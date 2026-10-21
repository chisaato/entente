// Package docker 监视 Docker Engine 并报告正在运行的容器。
//
// watcher 是 level-triggered 的:每个相关事件只把状态标脏,
// 防抖后的一次完整 ContainerList 给出新画面。
// 这样事件风暴代价很低,漏掉的事件也无害。
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

// Container 是 reconciler 需要的那一部分容器状态。
type Container struct {
	ID     string
	Name   string
	Labels map[string]string
	// Networks 把 Docker 网络名映射到容器在该网络上的 IPv4
	// (没有 IPv4 时则用全局 IPv6)地址。没有地址的网络
	// (例如 "host" 或 "none")映射为无效的 netip.Addr。
	Networks map[string]netip.Addr
}

// Watcher 列出正在运行的容器,并在 Docker 报告相关变化时重新列出。
type Watcher struct {
	cli      *client.Client
	log      *slog.Logger
	debounce time.Duration
	onSync   func([]Container)
}

// NewWatcher 连接到 host 上的 Docker Engine。每次同步成功后,
// onSync 在 watcher goroutine 里被调用,参数是全部正在运行的容器。
func NewWatcher(host string, log *slog.Logger, onSync func([]Container)) (*Watcher, error) {
	cli, err := client.New(client.WithHost(host))
	if err != nil {
		return nil, err
	}
	return &Watcher{cli: cli, log: log, debounce: 250 * time.Millisecond, onSync: onSync}, nil
}

// Close 释放 Docker 客户端。
func (w *Watcher) Close() error { return w.cli.Close() }

// Run 一直监视到 ctx 被取消。连接失败会按退避重试;
// 在此期间保留上一次成功同步的结果。
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

// relevantActions 是那些之后,正在运行的容器集合、它们的 label 或地址可能已变化的事件。
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

// session 订阅事件,先做一次初始同步,然后在相关事件上重新同步,直到流失败。
func (w *Watcher) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	filters := client.Filters{}.
		Add("type", string(events.ContainerEventType), string(events.NetworkEventType))
	// 先订阅再 list,这样 list 期间发生的变化不会丢。
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
