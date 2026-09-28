// Package docker watches the Docker Engine and reports the running containers.
//
// The watcher is level-triggered: every relevant event only marks the state
// dirty, and a debounced full ContainerList produces the new picture. This
// makes event storms cheap and missed events harmless.
package docker

import (
	"context"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

// Container is the subset of container state the reconciler needs.
type Container struct {
	ID     string
	Name   string
	Labels map[string]string
	// Networks maps Docker network name to the container's IPv4 (or, when no
	// IPv4 is assigned, global IPv6) address on it. Networks without an
	// address (e.g. "host" or "none") map to an invalid netip.Addr.
	Networks map[string]netip.Addr
}

// Watcher lists running containers and re-lists them whenever Docker reports
// a relevant change.
type Watcher struct {
	cli      *client.Client
	log      *slog.Logger
	debounce time.Duration
	onSync   func([]Container)
}

// NewWatcher connects to the Docker Engine at host. onSync is called from the
// watcher goroutine with the full list of running containers after every
// successful sync.
func NewWatcher(host string, log *slog.Logger, onSync func([]Container)) (*Watcher, error) {
	cli, err := client.New(client.WithHost(host))
	if err != nil {
		return nil, err
	}
	return &Watcher{cli: cli, log: log, debounce: 250 * time.Millisecond, onSync: onSync}, nil
}

// Close releases the Docker client.
func (w *Watcher) Close() error { return w.cli.Close() }

// Run watches until ctx is cancelled. Connection failures are retried with
// backoff; the last successful sync stays in effect meanwhile.
func (w *Watcher) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := w.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		w.log.Error("docker event stream lost; keeping last known containers", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// relevantActions are the events after which the set of running containers,
// their labels or their addresses may have changed.
var relevantActions = map[events.Action]bool{
	events.ActionStart:      true,
	events.ActionRestart:    true,
	events.ActionStop:       true,
	events.ActionDie:        true,
	events.ActionKill:       true,
	events.ActionDestroy:    true,
	events.ActionPause:      true,
	events.ActionUnPause:    true,
	events.ActionUpdate:     true,
	events.ActionRename:     true,
	events.ActionConnect:    true,
	events.ActionDisconnect: true,
}

// session subscribes to events, performs an initial sync, then re-syncs on
// relevant events until the stream fails.
func (w *Watcher) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	filters := client.Filters{}.
		Add("type", string(events.ContainerEventType), string(events.NetworkEventType))
	// Subscribe before listing so nothing that happens during the list is lost.
	sub := w.cli.Events(ctx, client.EventsListOptions{Filters: filters})

	if err := w.sync(ctx); err != nil {
		return err
	}

	var timer <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-sub.Err:
			return err
		case msg := <-sub.Messages:
			if !relevantActions[msg.Action] {
				continue
			}
			w.log.Debug("docker event", "type", msg.Type, "action", msg.Action, "id", shortID(msg.Actor.ID))
			if timer == nil {
				timer = time.After(w.debounce)
			}
		case <-timer:
			timer = nil
			if err := w.sync(ctx); err != nil {
				return err
			}
		}
	}
}

func (w *Watcher) sync(ctx context.Context) error {
	res, err := w.cli.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return err
	}
	out := make([]Container, 0, len(res.Items))
	for _, s := range res.Items {
		if s.State != container.StateRunning {
			continue
		}
		out = append(out, fromSummary(s))
	}
	slices.SortFunc(out, func(a, b Container) int { return strings.Compare(a.Name, b.Name) })
	w.log.Debug("docker sync", "running", len(out))
	w.onSync(out)
	return nil
}

func fromSummary(s container.Summary) Container {
	c := Container{ID: s.ID, Labels: s.Labels, Networks: map[string]netip.Addr{}}
	if len(s.Names) > 0 {
		c.Name = strings.TrimPrefix(s.Names[0], "/")
	}
	if s.NetworkSettings != nil {
		for name, ep := range s.NetworkSettings.Networks {
			if ep == nil {
				continue
			}
			addr := ep.IPAddress
			if !addr.IsValid() {
				addr = ep.GlobalIPv6Address
			}
			c.Networks[name] = addr
		}
	}
	return c
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
