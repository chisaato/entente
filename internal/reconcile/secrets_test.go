package reconcile

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/secrets"
	"github.com/chisaato/entente/internal/xds"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

func TestSecretsFirstAttemptBarrier(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{NodeID: "envoy", Entrypoints: []string{"https"}, Secrets: []config.Secret{{Name: "cert"}}}
	snapshots := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	r := New(cfg, snapshots, xds.NewRouteNames(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.reconcileOnce(ctx)
	if _, err := snapshots.GetSnapshot("envoy"); err == nil {
		t.Fatal("published before first secret attempt")
	}
	// 首次失败也解除屏障,不阻塞正常路由。
	r.SetSecrets(nil, true)
	r.reconcileOnce(ctx)
	snap, err := snapshots.GetSnapshot("envoy")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.GetResources(resource.SecretType)) != 0 || len(snap.GetResources(resource.RouteType)) != 1 {
		t.Fatal("first failure must publish routes without secrets")
	}
	r.SetSecrets([]secrets.Certificate{{Name: "cert", Certificate: []byte("chain"), PrivateKey: []byte("key")}}, true)
	r.reconcileOnce(ctx)
	snap, _ = snapshots.GetSnapshot("envoy")
	if len(snap.GetResources(resource.SecretType)) != 1 {
		t.Fatal("missing recovered certificate")
	}
	version := snap.GetVersion(resource.SecretType)
	next := *cfg
	next.Secrets = append([]config.Secret{{Name: "added"}}, cfg.Secrets...)
	r.SetConfig(&next)
	r.reconcileOnce(ctx)
	snap, _ = snapshots.GetSnapshot("envoy")
	if snap.GetVersion(resource.SecretType) != version {
		t.Fatal("published before hot-added secret first attempt")
	}
	next.Secrets = nil
	r.SetConfig(&next)
	r.SetSecrets(nil, true)
	r.reconcileOnce(ctx)
	snap, _ = snapshots.GetSnapshot("envoy")
	if len(snap.GetResources(resource.SecretType)) != 0 {
		t.Fatal("deleted certificate retained")
	}
}
