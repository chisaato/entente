package reconcile

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/docker"
	"github.com/chisaato/entente/internal/xds"
	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

func dockerBackendConfig(t *testing.T) *config.Config {
	t.Helper()
	return mustConfig(t, `static:
  backends:
    - {name: tunnel, docker: {container: gost, network: public, port: 8443}}
    - {name: local, endpoints: ["127.0.0.1:8080"]}
  routes:
    - {name: first, host: first.example.com, entrypoints: [https], backend: tunnel}
    - {name: second, host: second.example.com, entrypoints: [https], backend: tunnel}
    - {name: local, host: local.example.com, entrypoints: [https], backend: local}
`)
}

func TestPrepareDockerBackend(t *testing.T) {
	for name, tc := range map[string]struct {
		containers []docker.Container
		address    string
		problem    string
	}{
		"not running":   {containers: []docker.Container{}, problem: "not running"},
		"offline":       {problem: "not running"},
		"wrong network": {containers: []docker.Container{ctr("gost", oneNet)}, problem: "not attached"},
		"no IP":         {containers: []docker.Container{ctr("gost", map[string]string{"public": ""})}, problem: "no IP"},
		"IPv4":          {containers: []docker.Container{ctr("gost", map[string]string{"public": "172.19.0.9", "other": "172.20.0.9"})}, address: "172.19.0.9"},
		"IPv6":          {containers: []docker.Container{ctr("gost", map[string]string{"public": "fd00::9"})}, address: "fd00::9"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, options := range []PrepareOptions{{Now: time.Now()}, {Now: time.Now(), Runtime: true, ColdStart: true}, {Now: time.Now(), Runtime: true}} {
				p, err := Prepare(dockerBackendConfig(t), tc.containers, nil, options)
				if err != nil {
					t.Fatal(err)
				}
				if tc.problem != "" {
					if len(p.Report.Rejected) != 2 || len(p.Resources.Clusters) != 1 {
						t.Fatalf("rejections = %v, clusters = %v", p.Report.Rejected, p.Resources.Clusters)
					}
					for _, r := range p.Report.Rejected {
						if !r.Runtime || !strings.Contains(r.Reason, tc.problem) {
							t.Fatalf("rejection = %+v", r)
						}
					}
					continue
				}
				if len(p.Report.Rejected) != 0 || len(p.Resources.Clusters) != 2 {
					t.Fatalf("rejections = %v, clusters = %v", p.Report.Rejected, p.Resources.Clusters)
				}
				c := p.Resources.Clusters[1].(*cluster.Cluster)
				if c.Name != "static/tunnel" || c.GetType() != cluster.Cluster_EDS || c.EdsClusterConfig.ServiceName != "static/tunnel" {
					t.Fatalf("cluster = %v", c)
				}
				e := p.Resources.Endpoints[1].(*endpoint.ClusterLoadAssignment).Endpoints[0].LbEndpoints[0].GetEndpoint().Address.GetSocketAddress()
				if e.Address != tc.address || e.GetPortValue() != 8443 {
					t.Fatalf("endpoint = %v", e)
				}
			}
		})
	}
}

func TestUnavailableDockerBackendDoesNotHideStaticErrors(t *testing.T) {
	for _, kind := range []string{"host", "entrypoint", "conflict", "TLS coverage"} {
		t.Run(kind, func(t *testing.T) {
			cfg := dockerBackendConfig(t)
			switch kind {
			case "host":
				cfg.Static.Routes[0].Host = "INVALID"
			case "entrypoint":
				cfg.Static.Routes[0].Entrypoints = []string{"unknown"}
			case "conflict":
				cfg.Static.Routes[0].Host = cfg.Static.Routes[1].Host
			case "TLS coverage":
				cfg.Listeners = candidateConfig(t).Listeners
				cfg.Listeners[0].TLSBindings = nil
				cfg.Static.Routes = cfg.Static.Routes[:1]
			}
			if _, err := Prepare(cfg, []docker.Container{}, nil, PrepareOptions{Now: time.Now()}); err == nil {
				t.Fatal("invalid static configuration accepted")
			}
		})
	}
}

func TestDockerBackendRecoveryAndCandidateCommit(t *testing.T) {
	ctx := context.Background()
	cfg := dockerBackendConfig(t)
	snapshots := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	r := New(cfg, snapshots, xds.NewRouteNames(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	// 容器缺失不阻止冷启动,只保留独立后端。
	r.SetContainers([]docker.Container{})
	r.reconcileOnce(ctx)
	assertClusters := func(want int) {
		t.Helper()
		snapshot, err := snapshots.GetSnapshot("envoy")
		if err != nil || len(snapshot.GetResources(resource.ClusterType)) != want {
			t.Fatalf("want %d clusters, snapshot = %v, err = %v", want, snapshot, err)
		}
	}
	assertClusters(1)
	// 同一份 YAML 的候选也必须能在容器缺失时提交。
	next := dockerBackendConfig(t)
	next.Static.Routes[2].Host = "updated.example.com"
	r.SetConfig(next)
	r.reconcileOnce(ctx)
	if r.cfg != next {
		t.Fatal("candidate rejected because Docker backend was unavailable")
	}
	r.SetContainers([]docker.Container{ctr("gost", map[string]string{"public": "172.19.0.9"})})
	r.reconcileOnce(ctx)
	assertClusters(2)
	r.SetContainers([]docker.Container{})
	r.reconcileOnce(ctx)
	assertClusters(1)
	r.SetContainers([]docker.Container{ctr("gost", map[string]string{"public": "172.19.0.10"})})
	r.reconcileOnce(ctx)
	assertClusters(2)
}
