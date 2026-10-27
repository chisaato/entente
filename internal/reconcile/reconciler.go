package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/docker"
	"github.com/chisaato/entente/internal/model"
	"github.com/chisaato/entente/internal/secrets"
	"github.com/chisaato/entente/internal/xds"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
)

// Reconciler 将完整候选提交为生效代,运行时事件只使用生效配置。
type Reconciler struct {
	snapshots              cache.SnapshotCache
	routeNames             *xds.RouteNames
	log                    *slog.Logger
	debounce               time.Duration
	mu                     sync.Mutex
	cfg                    *config.Config
	pending                *config.Config
	containers             []docker.Container
	dockerSynced           bool
	certificates           []secrets.Certificate
	secretsSynced          bool
	manager                *secrets.Manager
	kick                   chan struct{}
	lastVersion            string
	retry                  time.Duration
	retryAt                time.Time
	retiring               []secrets.Certificate
	listenerRemovalVersion string
	fatal                  chan error
	publisher              func(context.Context, string, cache.ResourceSnapshot) error
}

func New(cfg *config.Config, snapshots cache.SnapshotCache, names *xds.RouteNames, log *slog.Logger) *Reconciler {
	r := &Reconciler{cfg: cfg, snapshots: snapshots, routeNames: names, log: log, debounce: 100 * time.Millisecond, kick: make(chan struct{}, 1), fatal: make(chan error, 1)}
	names.SetOnChange(r.trigger)
	return r
}

// Errors 报告无法提交首代的配置错误,由主程序停止进程。
func (r *Reconciler) Errors() <-chan error { return r.fatal }

// SetPublisher 可接入 xDS 服务的发布观测回调,必须在 Run 前设置。
func (r *Reconciler) SetPublisher(publish func(context.Context, string, cache.ResourceSnapshot) error) {
	r.publisher = publish
}

// SetSecretManager 把候选读取与提交后的任务切换接到同一个读取器。
// 必须在 Run 和配置 watcher 启动之前调用。
func (r *Reconciler) SetSecretManager(m *secrets.Manager) {
	r.mu.Lock()
	r.manager = m
	r.mu.Unlock()
	m.SetValidator(r.validateRenewal)
}

func (r *Reconciler) validateRenewal(c secrets.Certificate) error {
	r.mu.Lock()
	cfg := r.cfg
	r.mu.Unlock()
	leaf, err := validateCertificate(c, time.Now())
	if err != nil {
		return err
	}
	var errs []error
	for _, l := range cfg.Listeners {
		for _, b := range l.TLSBindings {
			if b.Secret == c.Name {
				for _, h := range b.ServerNames {
					if !certificateCovers(leaf, h) {
						errs = append(errs, errors.New("renewed certificate SAN does not cover active TLS binding"))
					}
				}
			}
		}
	}
	return errors.Join(errs...)
}

// SetConfig 只登记候选,不替换生效代,也不切换证书任务。
func (r *Reconciler) SetConfig(cfg *config.Config) {
	r.mu.Lock()
	if fields := config.RestartRequired(r.cfg, cfg); len(fields) > 0 {
		r.mu.Unlock()
		r.log.Error("validation_failed; configuration requires restart", "fields", fields)
		return
	}
	r.pending = cfg
	r.retry = 0
	r.retryAt = time.Time{}
	r.mu.Unlock()
	r.trigger()
}

func (r *Reconciler) SetSecrets(cs []secrets.Certificate, synced bool) {
	r.mu.Lock()
	r.certificates = cs
	r.secretsSynced = synced
	r.mu.Unlock()
	r.trigger()
}

// SetSecretsForGeneration 丢弃上一代的异步结果。
func (r *Reconciler) SetSecretsForGeneration(generation string, cs []secrets.Certificate, synced bool) {
	r.mu.Lock()
	if generation == r.lastVersion {
		r.certificates = cs
		r.secretsSynced = synced
	}
	r.mu.Unlock()
	r.trigger()
}
func (r *Reconciler) SetContainers(cs []docker.Container) {
	r.mu.Lock()
	r.containers = cs
	r.dockerSynced = true
	r.mu.Unlock()
	r.trigger()
}

// ListenerAcknowledged 在 Envoy 接受移除引用的 LDS 后才允许删除旧 SDS 材料。
func (r *Reconciler) ListenerAcknowledged(version string) {
	r.mu.Lock()
	if version == r.listenerRemovalVersion {
		r.retiring = nil
		r.listenerRemovalVersion = ""
	}
	r.mu.Unlock()
	r.trigger()
}
func (r *Reconciler) trigger() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}
func (r *Reconciler) Run(ctx context.Context) {
	r.trigger()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.debounce):
		}
		r.reconcileOnce(ctx)
	}
}

func materialView(cs []secrets.Certificate) map[string]Material {
	out := map[string]Material{}
	for _, c := range cs {
		out[c.Name] = Material{Certificate: c}
	}
	return out
}

func (r *Reconciler) reconcileOnce(ctx context.Context) {
	r.mu.Lock()
	cfg, pending := r.cfg, r.pending
	containers, certificates := r.containers, r.certificates
	synced, secretsSynced := r.dockerSynced, r.secretsSynced
	manager := r.manager
	cold := r.lastVersion == ""
	retryAt := r.retryAt
	r.mu.Unlock()
	if cfg.Docker != nil && !synced {
		return
	}
	if len(cfg.Secrets) > 0 && !secretsSynced {
		return
	}
	// 失败候选不会阻断生效代的 Docker 更新和证书续期。
	if pending != nil && !time.Now().Before(retryAt) {
		materials := materialView(certificates)
		transient := false
		for _, d := range pending.Secrets {
			unchanged := false
			for _, old := range cfg.Secrets {
				if old.Equal(d) {
					unchanged = true
					break
				}
			}
			_, present := materials[d.Name]
			if unchanged && present {
				continue
			}
			if manager == nil {
				materials[d.Name] = Material{Err: errors.New("certificate reader not attached")}
				transient = true
				continue
			}
			c, err := manager.Read(ctx, d)
			materials[d.Name] = Material{Certificate: c, Err: err}
			transient = transient || err != nil
		}
		prepared, err := Prepare(pending, containers, materials, PrepareOptions{Now: time.Now(), Placeholders: r.routeNames.Names()})
		r.log.Info("candidate_received", "stage", "candidate_received", "generation_id", prepared.Generation)
		if err == nil {
			newCerts := []secrets.Certificate{}
			for _, d := range pending.Secrets {
				c := materials[d.Name].Certificate
				c.Name = d.Name
				newCerts = append(newCerts, c)
			}
			r.mu.Lock()
			if r.pending != pending {
				r.mu.Unlock()
				r.trigger()
				return
			}
			previousRetiring := r.retiring
			for _, c := range certificates {
				if referencesSecret(cfg, c.Name) && !referencesSecret(pending, c.Name) && !slices.ContainsFunc(newCerts, func(n secrets.Certificate) bool { return n.Name == c.Name }) {
					r.retiring = append(slices.Clone(r.retiring), c)
				}
			}
			if err = r.publish(ctx, pending, prepared); err == nil {
				r.warnListenerChanges(cfg, pending)
				r.cfg = pending
				r.pending = nil
				r.certificates = newCerts
				r.secretsSynced = true
				r.retry = 0
			}
			if err != nil {
				r.retiring = previousRetiring
			}
			r.mu.Unlock()
			if err == nil {
				for _, rejection := range prepared.Report.Rejected {
					r.log.Error("route rejected", "generation_id", prepared.Generation, "subject", rejection.Subject, "reason", rejection.Reason)
				}
				for _, warning := range prepared.Report.Warnings {
					r.log.Warn("candidate warning", "generation_id", prepared.Generation, "reason", warning)
				}
				r.log.Info("committed", "stage", "committed", "generation_id", prepared.Generation)
				r.log.Info("config reloaded", "generation_id", prepared.Generation)
				if manager != nil {
					manager.Commit(pending.Secrets, newCerts)
				}
				return
			}
		}
		r.log.Error("validation_failed; keeping active generation", "stage", "validation_failed", "generation_id", prepared.Generation, "active_generation_id", r.lastVersion, "err", err)
		r.mu.Lock()
		if r.pending == pending {
			if transient {
				if r.retry == 0 {
					r.retry = time.Second
				} else {
					r.retry = min(2*r.retry, 30*time.Second)
				}
				r.retryAt = time.Now().Add(r.retry)
				time.AfterFunc(r.retry, r.trigger)
			} else {
				r.pending = nil
			}
		}
		r.mu.Unlock()
	}
	p, err := Prepare(cfg, containers, materialView(certificates), PrepareOptions{Now: time.Now(), ColdStart: cold, Runtime: true, Placeholders: r.routeNames.Names()})
	if err != nil {
		r.log.Error("validation_failed; keeping previous snapshot", "stage", "validation_failed", "generation_id", p.Generation, "err", err)
		if cold {
			select {
			case r.fatal <- err:
			default:
			}
		}
		return
	}
	for _, warning := range p.Report.Warnings {
		r.log.Warn("candidate warning", "generation_id", p.Generation, "reason", warning)
	}
	for _, reason := range p.Report.Degraded {
		r.log.Error("certificate binding degraded", "generation_id", p.Generation, "reason", reason)
	}
	for _, rej := range p.Report.Rejected {
		r.log.Error("route rejected", "subject", rej.Subject, "reason", rej.Reason)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfg != cfg {
		return
	}
	if err := r.publish(ctx, cfg, p); err != nil {
		r.log.Error("publishing snapshot", "err", err)
	}
}

// publish 在锁内切换整个快照,失败时调用方不修改生效配置。
func (r *Reconciler) publish(ctx context.Context, cfg *config.Config, p *Prepared) error {
	if len(r.retiring) > 0 {
		retained := xds.Translate(&model.Model{}, nil, r.retiring)
		for _, s := range retained.Secrets {
			if !slices.ContainsFunc(p.Resources.Secrets, func(n types.Resource) bool { return n.(*tlsv3.Secret).Name == s.(*tlsv3.Secret).Name }) {
				p.Resources.Secrets = append(p.Resources.Secrets, s)
			}
		}
		slices.SortFunc(p.Resources.Secrets, func(a, b types.Resource) int { return strings.Compare(a.(*tlsv3.Secret).Name, b.(*tlsv3.Secret).Name) })
		version, err := p.Resources.Version()
		if err != nil {
			return err
		}
		p.Generation = version
	}
	if p.Generation == r.lastVersion {
		return nil
	}
	snap, err := p.Resources.Snapshot(p.Generation)
	if err != nil {
		return err
	}
	if r.publisher != nil {
		err = r.publisher(ctx, p.Generation, snap)
	} else {
		err = r.snapshots.SetSnapshot(ctx, cfg.NodeID, snap)
	}
	if err != nil {
		return err
	}
	r.lastVersion = p.Generation
	if len(r.retiring) > 0 {
		r.listenerRemovalVersion = p.Generation
	}
	r.log.Info("published", "stage", "published", "generation_id", p.Generation)
	return nil
}

func referencesSecret(cfg *config.Config, name string) bool {
	for _, l := range cfg.Listeners {
		if l.Fallback.Secret == name {
			return true
		}
		for _, b := range l.TLSBindings {
			if b.Secret == name {
				return true
			}
		}
	}
	return false
}

func (r *Reconciler) warnListenerChanges(old, next *config.Config) {
	for _, n := range next.Listeners {
		for _, o := range old.Listeners {
			if n.Name != o.Name {
				continue
			}
			n.TLSBindings = nil
			o.TLSBindings = nil
			a, _ := json.Marshal(n)
			b, _ := json.Marshal(o)
			if !slices.Equal(a, b) {
				r.log.Warn("listener-level configuration changed; entire listener will drain", "listener", n.Name)
			}
		}
	}
}
