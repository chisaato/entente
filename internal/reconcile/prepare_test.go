package reconcile

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/secrets"
	"github.com/chisaato/entente/internal/xds"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/proto"
)

func candidateCertificate(t *testing.T, names []string, before, after time.Time) secrets.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: names, NotBefore: before, NotAfter: after, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return secrets.Certificate{Name: "cert", Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})}
}

func candidateConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`node_id: envoy
xds: {socket: /tmp/entente-test.sock}
route_timeout: 0s
entrypoints: [https]
secrets:
  - name: cert
    file: {certificate_chain: /cert.pem, private_key: /key.pem}
listeners:
  - name: https
    address: "0.0.0.0:443"
    entrypoint: https
    hcm: {websocket: true, strip_any_host_port: true}
    alpn: [h2, http/1.1]
    fallback: {static_secret: sink}
    tls_bindings:
      - {name: example, server_names: ["*.example.com"], secret: cert}
static:
  backends: [{name: app, endpoints: ["127.0.0.1:80"]}]
  routes: [{name: app, host: app.example.com, entrypoints: [https], backend: app}]
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestPrepareListenerAndStableChains(t *testing.T) {
	now := time.Now()
	cfg := candidateConfig(t)
	c := candidateCertificate(t, []string{"*.example.com"}, now.Add(-time.Hour), now.Add(30*24*time.Hour))
	view := map[string]Material{"cert": {Certificate: c}}
	p, err := Prepare(cfg, nil, view, PrepareOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	l := p.Resources.Listeners[0].(*listener.Listener)
	if len(l.FilterChains) != 1 || l.DefaultFilterChain == nil || l.ListenerFilters[0].Name != "envoy.filters.listener.tls_inspector" {
		t.Fatal("missing TLS chains")
	}
	cfg.Listeners[0].TLSBindings = append(cfg.Listeners[0].TLSBindings, config.TLSBinding{Name: "second", ServerNames: []string{"second.example.com"}, Secret: "cert"})
	next, err := Prepare(cfg, nil, view, PrepareOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	nl := next.Resources.Listeners[0].(*listener.Listener)
	if len(nl.FilterChains) != 2 || !proto.Equal(l.FilterChains[0], nl.FilterChains[0]) || !proto.Equal(l.DefaultFilterChain, nl.DefaultFilterChain) {
		t.Fatal("adding binding changed existing chains")
	}
}

func TestPrepareCertificateAndCoverage(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name          string
		sans          []string
		before, after time.Time
		host          string
		bad           bool
		warn          bool
	}{
		{"wildcard", []string{"*.example.com"}, now.Add(-time.Hour), now.Add(30 * 24 * time.Hour), "app.example.com", false, false},
		{"exact cannot cover wildcard binding", []string{"app.example.com"}, now.Add(-time.Hour), now.Add(30 * 24 * time.Hour), "app.example.com", true, false},
		{"wildcard cannot cross labels", []string{"*.example.com"}, now.Add(-time.Hour), now.Add(30 * 24 * time.Hour), "a.b.example.com", true, false},
		{"expired", []string{"*.example.com"}, now.Add(-2 * time.Hour), now.Add(-time.Hour), "app.example.com", true, false},
		{"future", []string{"*.example.com"}, now.Add(time.Hour), now.Add(30 * 24 * time.Hour), "app.example.com", true, false},
		{"soon", []string{"*.example.com"}, now.Add(-time.Hour), now.Add(time.Hour), "app.example.com", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := candidateConfig(t)
			cfg.Static.Routes[0].Host = tc.host
			c := candidateCertificate(t, tc.sans, tc.before, tc.after)
			p, err := Prepare(cfg, nil, map[string]Material{"cert": {Certificate: c}}, PrepareOptions{Now: now})
			if (err != nil) != tc.bad {
				t.Fatalf("err=%v", err)
			}
			if tc.warn && len(p.Report.Warnings) == 0 {
				t.Fatal("missing expiry warning")
			}
		})
	}
}

func TestPrepareMissingAndMismatchedCertificate(t *testing.T) {
	now := time.Now()
	cfg := candidateConfig(t)
	if _, err := Prepare(cfg, nil, nil, PrepareOptions{Now: now}); err == nil {
		t.Fatal("hot candidate accepted missing material")
	}
	p, err := Prepare(cfg, nil, nil, PrepareOptions{Now: now, ColdStart: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Resources.Secrets) != 0 || len(p.Resources.Listeners[0].(*listener.Listener).FilterChains) != 0 {
		t.Fatal("cold start references absent SDS secret")
	}
	c := candidateCertificate(t, []string{"*.example.com"}, now.Add(-time.Hour), now.Add(time.Hour))
	other := candidateCertificate(t, []string{"*.example.com"}, now.Add(-time.Hour), now.Add(time.Hour))
	c.PrivateKey = other.PrivateKey
	if _, err := Prepare(cfg, nil, map[string]Material{"cert": {Certificate: c}}, PrepareOptions{Now: now}); err == nil {
		t.Fatal("mismatched key accepted")
	}
}

func TestCandidateRejectionAndSecretRemovalOrdering(t *testing.T) {
	now := time.Now()
	cfg := candidateConfig(t)
	c := candidateCertificate(t, []string{"*.example.com"}, now.Add(-time.Hour), now.Add(30*24*time.Hour))
	cache := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	r := New(cfg, cache, xds.NewRouteNames(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.SetSecrets([]secrets.Certificate{c}, true)
	r.reconcileOnce(context.Background())
	old := r.lastVersion
	bad := *cfg
	bad.Static = cfg.Static
	bad.Static.Routes = slices.Clone(cfg.Static.Routes)
	bad.Static.Routes[0].Backend = "absent"
	r.SetConfig(&bad)
	r.reconcileOnce(context.Background())
	if r.cfg != cfg || r.lastVersion != old {
		t.Fatal("failed candidate changed active generation")
	}
	removed := *cfg
	removed.Secrets = nil
	removed.Listeners = slices.Clone(cfg.Listeners)
	removed.Listeners[0].TLSBindings = []config.TLSBinding{}
	removed.Static = config.Static{}
	r.SetConfig(&removed)
	r.reconcileOnce(context.Background())
	snap, err := cache.GetSnapshot("envoy")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.GetResources(resource.SecretType)) != 1 {
		t.Fatal("secret removed before LDS ACK")
	}
	l := snap.GetResources(resource.ListenerType)["https"].(*listener.Listener)
	if len(l.FilterChains) != 0 {
		t.Fatal("binding not removed first")
	}
	r.ListenerAcknowledged(r.lastVersion)
	r.reconcileOnce(context.Background())
	snap, _ = cache.GetSnapshot("envoy")
	if len(snap.GetResources(resource.SecretType)) != 0 {
		t.Fatal("secret retained after LDS ACK")
	}
}

func TestRouteWildcardCoverage(t *testing.T) {
	if covers("exact.example.com", "*.example.com") || covers("*.example.com", "a.b.example.com") || !covers("*.example.com", "*.example.com") {
		t.Fatal("incorrect wildcard coverage")
	}
	cfg := candidateConfig(t)
	cfg.Static.Routes[0].Backend = "missing"
	cfg.Static.Routes = append(cfg.Static.Routes, config.StaticRoute{Name: "another", Host: "bad", Backend: "missing", Entrypoints: []string{"https"}})
	_, err := Prepare(cfg, nil, nil, PrepareOptions{Now: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "static/app") || !strings.Contains(err.Error(), "static/another") {
		t.Fatalf("missing aggregate errors: %v", err)
	}
}

func TestCandidateRetriesWithoutSwitchingActiveWatchers(t *testing.T) {
	now := time.Now()
	cfg := candidateConfig(t)
	c := candidateCertificate(t, []string{"*.example.com"}, now.Add(-time.Hour), now.Add(30*24*time.Hour))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	snapshots := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	r := New(cfg, snapshots, xds.NewRouteNames(nil), log)
	m, err := secrets.New(nil, cfg.Secrets, log, r.SetSecrets)
	if err != nil {
		t.Fatal(err)
	}
	r.SetSecretManager(m)
	r.SetSecrets([]secrets.Certificate{c}, true)
	r.reconcileOnce(context.Background())
	old := r.lastVersion
	next := *cfg
	next.Secrets = slices.Clone(cfg.Secrets)
	dir := t.TempDir()
	chain, key := filepath.Join(dir, "chain.pem"), filepath.Join(dir, "key.pem")
	next.Secrets[0].File = &config.SecretFile{CertificateChain: chain, PrivateKey: key}
	r.SetConfig(&next)
	r.reconcileOnce(context.Background())
	if r.cfg != cfg || r.lastVersion != old || r.pending == nil || r.retry == 0 {
		t.Fatal("transient failure did not preserve generation and schedule retry")
	}
	if err := os.WriteFile(chain, c.Certificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, c.PrivateKey, 0600); err != nil {
		t.Fatal(err)
	}
	r.retryAt = time.Time{}
	r.reconcileOnce(context.Background())
	if r.cfg != &next || r.pending != nil {
		t.Fatal("recovered candidate was not committed")
	}
}

func TestColdStartInvalidStaticRouteReportsFatal(t *testing.T) {
	cfg := candidateConfig(t)
	cfg.Static.Routes[0].Backend = "missing"
	r := New(cfg, cache.NewSnapshotCache(false, cache.IDHash{}, nil), xds.NewRouteNames(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.SetSecrets(nil, true)
	r.reconcileOnce(context.Background())
	select {
	case err := <-r.Errors():
		if err == nil {
			t.Fatal("missing fatal reason")
		}
	default:
		t.Fatal("invalid first generation did not report fatal error")
	}
}
