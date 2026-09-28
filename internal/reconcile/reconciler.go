package reconcile

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/docker"
	"github.com/chisaato/entente/internal/xds"
)

// Reconciler owns the latest input from every source and publishes a new
// snapshot whenever that input changes.
type Reconciler struct {
	snapshots  cache.SnapshotCache
	routeNames *xds.RouteNames
	log        *slog.Logger
	debounce   time.Duration

	mu           sync.Mutex
	cfg          *config.Config
	containers   []docker.Container
	dockerSynced bool

	kick        chan struct{}
	lastVersion string
}

// New creates a Reconciler for the initial config. routeNames supplies the
// route_config_names Envoy has requested; New subscribes to its changes.
func New(cfg *config.Config, snapshots cache.SnapshotCache, routeNames *xds.RouteNames, log *slog.Logger) *Reconciler {
	r := &Reconciler{
		snapshots:  snapshots,
		routeNames: routeNames,
		log:        log,
		debounce:   100 * time.Millisecond,
		cfg:        cfg,
		kick:       make(chan struct{}, 1),
	}
	routeNames.SetOnChange(r.trigger)
	return r
}

// SetConfig replaces the config. Fields that need a restart must already have
// been checked by the caller.
func (r *Reconciler) SetConfig(cfg *config.Config) {
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
	r.trigger()
}

// SetContainers replaces the known Docker containers. It is the Docker
// watcher's onSync callback.
func (r *Reconciler) SetContainers(cs []docker.Container) {
	r.mu.Lock()
	r.containers = cs
	r.dockerSynced = true
	r.mu.Unlock()
	r.trigger()
}

func (r *Reconciler) trigger() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run reconciles on every change until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	r.trigger()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
		}
		// Coalesce bursts (e.g. a config reload racing a Docker sync).
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.debounce):
		}
		r.reconcileOnce(ctx)
	}
}

func (r *Reconciler) reconcileOnce(ctx context.Context) {
	r.mu.Lock()
	cfg, containers, synced := r.cfg, r.containers, r.dockerSynced
	r.mu.Unlock()

	// Until every enabled source has reported once, publishing would push a
	// partial view: after a control plane restart Envoy would drop all Docker
	// routes until the first sync. Envoy keeps its last config meanwhile.
	if cfg.Docker != nil && !synced {
		r.log.Info("waiting for first docker sync before publishing")
		return
	}

	m, rejected := Build(cfg, containers)
	for _, rej := range rejected {
		r.log.Error("route rejected", "subject", rej.Subject, "reason", rej.Reason)
	}

	requested := r.routeNames.Names()
	for _, n := range requested {
		if !slices.Contains(m.Entrypoints, n) {
			r.log.Warn("envoy requests a route_config_name that is not a declared entrypoint; serving it an empty route config",
				"route_config_name", n)
		}
	}

	res := xds.Translate(m, requested)
	version, err := res.Version()
	if err != nil {
		r.log.Error("hashing snapshot", "err", err)
		return
	}
	if version == r.lastVersion {
		r.log.Debug("snapshot unchanged", "version", version)
		return
	}
	snap, err := res.Snapshot(version)
	if err != nil {
		// Build only emits validated models, so this is a bug, not bad input.
		r.log.Error("inconsistent snapshot; keeping previous one", "version", version, "err", err)
		return
	}
	if err := r.snapshots.SetSnapshot(ctx, cfg.NodeID, snap); err != nil {
		r.log.Error("setting snapshot", "version", version, "err", err)
		return
	}
	r.lastVersion = version
	r.log.Info("snapshot published", "version", version,
		"entrypoints", len(m.Entrypoints), "routes", len(m.Routes), "backends", len(m.Backends), "rejected", len(rejected))
}
