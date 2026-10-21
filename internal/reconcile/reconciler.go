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
	"github.com/chisaato/entente/internal/secrets"
	"github.com/chisaato/entente/internal/xds"
)

// Reconciler 持有每个来源的最新输入,并在输入变化时发布新快照。
type Reconciler struct {
	snapshots  cache.SnapshotCache
	routeNames *xds.RouteNames
	log        *slog.Logger
	debounce   time.Duration

	mu            sync.Mutex
	cfg           *config.Config
	containers    []docker.Container
	dockerSynced  bool
	certificates  []secrets.Certificate
	secretsSynced bool

	kick        chan struct{}
	lastVersion string
}

// New 为初始配置创建一个 Reconciler。routeNames 提供 Envoy 请求过的
// route_config_name;New 会订阅它的变化。
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

// SetConfig 替换配置。需要重启才能生效的字段必须已由调用方检查过。
func (r *Reconciler) SetConfig(cfg *config.Config) {
	r.mu.Lock()
	// Secret.File 是指针,按内容比较,避免一次无变化的重载把已同步的文件证书打回未尝试。
	if !config.SecretsEqual(r.cfg.Secrets, cfg.Secrets) {
		r.secretsSynced = false
	}
	r.cfg = cfg
	r.mu.Unlock()
	r.trigger()
}

// SetSecrets 接收当前有效证书,以及所有配置项是否都已完成首次尝试。
func (r *Reconciler) SetSecrets(certificates []secrets.Certificate, synced bool) {
	r.mu.Lock()
	r.certificates = certificates
	r.secretsSynced = synced
	r.mu.Unlock()
	r.trigger()
}

// SetContainers 替换已知的 Docker 容器。它是 Docker watcher 的 onSync 回调。
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

// Run 在每次变化时 reconcile,直到 ctx 被取消。
func (r *Reconciler) Run(ctx context.Context) {
	r.trigger()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
		}
		// 合并突发变化(例如配置重载与 Docker 同步撞在一起)。
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
	certificates, secretsSynced := r.certificates, r.secretsSynced
	r.mu.Unlock()

	// 在每个已启用的来源都至少报告过一次之前就发布,会推出不完整的视图:
	// 控制平面重启后,Envoy 会丢掉全部 Docker 路由,直到第一次同步。
	// 在此期间 Envoy 继续使用上一份配置。
	if cfg.Docker != nil && !synced {
		r.log.Info("waiting for first docker sync before publishing")
		return
	}
	if len(cfg.Secrets) > 0 && !secretsSynced {
		r.log.Info("waiting for first secrets fetch before publishing")
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

	res := xds.Translate(m, requested, certificates)
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
		// Build 只产出已校验的模型,所以这是 bug,不是坏输入。
		r.log.Error("inconsistent snapshot; keeping previous one", "version", version, "err", err)
		return
	}
	if err := r.snapshots.SetSnapshot(ctx, cfg.NodeID, snap); err != nil {
		r.log.Error("setting snapshot", "version", version, "err", err)
		return
	}
	r.lastVersion = version
	r.log.Info("snapshot published", "version", version,
		"entrypoints", len(m.Entrypoints), "routes", len(m.Routes), "backends", len(m.Backends), "secrets", len(res.Secrets), "rejected", len(rejected))
}
