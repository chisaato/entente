package reconcile

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/docker"
	"github.com/chisaato/entente/internal/secrets"
	"github.com/chisaato/entente/internal/xds"
	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	gzip "github.com/envoyproxy/go-control-plane/envoy/extensions/compression/gzip/compressor/v3"
	zstd "github.com/envoyproxy/go-control-plane/envoy/extensions/compression/zstd/compressor/v3"
	compressor "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/compressor/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	httpproto "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"google.golang.org/protobuf/proto"
)

const staticFeatureYAML = `
static:
  backends:
    - {name: api, endpoints: [127.0.0.1:8085], h2c: true}
    - {name: ui, endpoints: [127.0.0.1:3006]}
  routes:
    - {name: fallback, host: sso.example.com, entrypoints: [https], backend: api, path: /, path_type: prefix, priority: 40, timeout: 0s}
    - {name: root, host: sso.example.com, entrypoints: [https], backend: ui, path: /, path_type: exact, rewrite: /ui/v2/login/, priority: 30}
    - {name: login, host: sso.example.com, entrypoints: [https], backend: ui, path: /ui/v2/login, path_type: prefix, priority: 20}
    - {name: api, host: sso.example.com, entrypoints: [https], backend: api, path: /api/, path_type: prefix, rewrite: /, priority: 0, timeout: 0s}
    - {name: legacy, host: old.example.com, entrypoints: [https], backend: ui}
`

func TestStaticFeaturesH2CPathsAndDeterminism(t *testing.T) {
	cfg := mustConfig(t, staticFeatureYAML)
	m, rejected := Build(cfg, nil)
	if len(rejected) != 0 || len(m.Routes) != 5 {
		t.Fatalf("routes=%v rejected=%v", m.Routes, rejected)
	}
	res := xds.Translate(m, nil, nil)
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, raw := range res.Clusters {
		c := raw.(*cluster.Cluster)
		a := c.TypedExtensionProtocolOptions["envoy.extensions.upstreams.http.v3.HttpProtocolOptions"]
		if c.Name == "static/api" {
			p := &httpproto.HttpProtocolOptions{}
			if a == nil {
				t.Fatal("h2c protocol options absent")
			}
			if err := a.UnmarshalTo(p); err != nil {
				t.Fatal(err)
			}
			if p.GetExplicitHttpConfig().GetHttp2ProtocolOptions() == nil || c.TransportSocket != nil {
				t.Fatal("not cleartext HTTP/2 prior knowledge")
			}
			if err := p.ValidateAll(); err != nil {
				t.Fatal(err)
			}
		} else if a != nil {
			t.Fatal("ordinary backend protocol changed")
		}
	}
	rc := res.Routes[1].(*route.RouteConfiguration)
	if len(rc.VirtualHosts) != 2 {
		t.Fatalf("virtual hosts=%v", rc.VirtualHosts)
	}
	var routes []*route.Route
	for _, vh := range rc.VirtualHosts {
		if vh.Domains[0] == "sso.example.com" {
			routes = vh.Routes
		} else if vh.Name != "static/legacy" || vh.Routes[0].Match.GetPrefix() != "/" {
			t.Fatal("legacy virtual host changed")
		}
	}
	if len(routes) != 4 {
		t.Fatalf("routes=%v", routes)
	}
	for i, want := range []struct {
		path, rewrite, backend string
		exact, zero            bool
	}{
		{"/api/", "/", "static/api", false, true},
		{"/ui/v2/login", "", "static/ui", false, false},
		{"/", "/ui/v2/login/", "static/ui", true, false},
		{"/", "", "static/api", false, true},
	} {
		r := routes[i]
		p := r.Match.GetPrefix()
		if want.exact {
			p = r.Match.GetPath()
		}
		a := r.GetRoute()
		if p != want.path || a.PrefixRewrite != want.rewrite || a.GetCluster() != want.backend || a.Timeout == nil || (a.Timeout.AsDuration() == 0) != want.zero || r.GetRedirect() != nil {
			t.Fatalf("route[%d]=%v", i, r)
		}
	}
	v, _ := res.Version()
	slices.Reverse(cfg.Static.Routes)
	slices.Reverse(cfg.Static.Backends)
	m, rejected = Build(cfg, nil)
	v2, _ := xds.Translate(m, nil, nil).Version()
	if v != v2 || len(rejected) != 0 {
		t.Fatalf("non deterministic: %s != %s, %v", v, v2, rejected)
	}
}

func TestStaticFeaturesRouteConflicts(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		rejected       int
	}{
		{"missing priority", ", priority: 20", "", 4},
		{"duplicate priority", "priority: 20", "priority: 30", 4},
		{"duplicate match", "path: /ui/v2/login", "path: /api/", 4},
		{"legacy mixed", "path: /ui/v2/login, path_type: prefix, priority: 20", "", 4},
		{"invalid pair", "path_type: exact", "path_type: nonsense", 1},
		{"missing path type", "path_type: exact, ", "", 1},
		{"relative rewrite", "rewrite: /ui/v2/login/", "rewrite: relative", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustConfig(t, strings.Replace(staticFeatureYAML, tc.from, tc.to, 1))
			m, rej := Build(cfg, nil)
			if len(rej) != tc.rejected || len(m.Routes) != 5-tc.rejected {
				t.Fatalf("routes=%v rejected=%v", m.Routes, rej)
			}
		})
	}
	t.Run("label cannot override paths", func(t *testing.T) {
		cfg := mustConfig(t, staticFeatureYAML)
		m, rej := Build(cfg, []docker.Container{exposed("claim", "sso.example.com", "https", oneNet)})
		if len(rej) != 5 || len(m.Routes) != 1 {
			t.Fatalf("routes=%v rejected=%v", m.Routes, rej)
		}
	})
}

func TestStaticFeaturesDockerH2C(t *testing.T) {
	cfg := mustConfig(t, strings.Replace(staticFeatureYAML, "endpoints: [127.0.0.1:8085]", "docker: {container: api, network: web, port: 8085}", 1))
	m, rej := Build(cfg, []docker.Container{ctr("api", oneNet)})
	if len(rej) != 0 {
		t.Fatal(rej)
	}
	res := xds.Translate(m, nil, nil)
	if len(res.Clusters[0].(*cluster.Cluster).TypedExtensionProtocolOptions) != 1 {
		t.Fatal("docker-backed static h2c not propagated")
	}
}

func TestStaticFeaturesPrepareRejectsCandidate(t *testing.T) {
	for _, options := range []PrepareOptions{
		{Now: time.Now()},
		{Now: time.Now(), ColdStart: true, Runtime: true},
	} {
		cfg := mustConfig(t, strings.Replace(staticFeatureYAML, "priority: 20", "priority: 30", 1))
		p, err := Prepare(cfg, nil, nil, options)
		if err == nil || len(p.Report.Rejected) != 4 {
			t.Fatalf("invalid candidate accepted: %v, %+v", err, p.Report)
		}
	}
}

func TestStaticFeaturesHostIsScopedToEntrypoint(t *testing.T) {
	cfg := mustConfig(t, strings.Replace(staticFeatureYAML, "name: legacy, host: old.example.com, entrypoints: [https]", "name: legacy, host: sso.example.com, entrypoints: [http]", 1))
	m, rejected := Build(cfg, nil)
	if len(rejected) != 0 || len(m.Routes) != 5 {
		t.Fatalf("routes=%v rejected=%v", m.Routes, rejected)
	}
	if err := xds.Translate(m, nil, nil).Validate(); err != nil {
		t.Fatal(err)
	}
}

const bindingFeatureYAML = `
secrets:
  - {name: cert, file: {certificate_chain: /cert.pem, private_key: /key.pem}}
listeners:
  - name: https
    address: 127.0.0.1:443
    entrypoint: https
    hcm: {websocket: false, strip_any_host_port: false}
    alpn: [h2, http/1.1]
    fallback: {static_secret: sink}
    tls_bindings:
      - name: compressed
        server_names: [sso.example.com]
        secret: cert
        compression: {encodings: [zstd, gzip], min_length: 512, content_types: [text/plain, application/json]}
      - {name: plain, server_names: [old.example.com], secret: cert}
`

func TestStaticFeaturesBindingCompression(t *testing.T) {
	cfg := mustConfig(t, bindingFeatureYAML)
	m, rej := Build(cfg, nil)
	if len(rej) != 0 {
		t.Fatal(rej)
	}
	res := xds.Translate(m, nil, []secrets.Certificate{{Name: "cert"}})
	l := res.Listeners[0].(*listener.Listener)
	for _, chain := range append(slices.Clone(l.FilterChains), l.DefaultFilterChain) {
		h := &hcm.HttpConnectionManager{}
		if err := chain.Filters[0].GetTypedConfig().UnmarshalTo(h); err != nil {
			t.Fatal(err)
		}
		if chain.Name != "compressed" {
			if len(h.HttpFilters) != 1 {
				t.Fatalf("compression leaked to %s", chain.Name)
			}
			continue
		}
		if len(h.HttpFilters) != 3 || h.HttpFilters[2].Name != "envoy.filters.http.router" {
			t.Fatalf("filters=%v", h.HttpFilters)
		}
		for i, encoding := range []string{"zstd", "gzip"} {
			p := &compressor.Compressor{}
			if err := h.HttpFilters[i].GetTypedConfig().UnmarshalTo(p); err != nil {
				t.Fatal(err)
			}
			if err := p.ValidateAll(); err != nil {
				t.Fatal(err)
			}
			c := p.GetResponseDirectionConfig().GetCommonConfig()
			if p.GetCompressorLibrary().Name != encoding || p.ChooseFirst != (i == 0) || c.GetMinContentLength().GetValue() != 512 || !slices.Equal(c.ContentType, []string{"text/plain", "application/json"}) {
				t.Fatalf("compressor=%v", p)
			}
			if i == 0 {
				library := &zstd.Zstd{}
				if err := p.CompressorLibrary.GetTypedConfig().UnmarshalTo(library); err != nil {
					t.Fatal(err)
				}
				if err := library.ValidateAll(); err != nil {
					t.Fatal(err)
				}
			} else {
				library := &gzip.Gzip{}
				if err := p.CompressorLibrary.GetTypedConfig().UnmarshalTo(library); err != nil {
					t.Fatal(err)
				}
				if err := library.ValidateAll(); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	v, _ := res.Version()
	slices.Reverse(cfg.Listeners[0].TLSBindings)
	m, _ = Build(cfg, nil)
	v2, _ := xds.Translate(m, nil, []secrets.Certificate{{Name: "cert"}}).Version()
	if v != v2 {
		t.Fatal("binding order changed snapshot")
	}
	for i := range cfg.Listeners[0].TLSBindings {
		cfg.Listeners[0].TLSBindings[i].Compression = nil
	}
	m, _ = Build(cfg, nil)
	plain := xds.Translate(m, nil, []secrets.Certificate{{Name: "cert"}}).Listeners[0].(*listener.Listener)
	if !proto.Equal(l.FilterChains[1], plain.FilterChains[1]) || !proto.Equal(l.DefaultFilterChain, plain.DefaultFilterChain) {
		t.Fatal("compression changed another chain or sink")
	}
}

func TestStaticFeaturesPrepareInvalidCompressionDoesNotPanic(t *testing.T) {
	cfg := mustConfig(t, bindingFeatureYAML)
	cfg.Listeners[0].TLSBindings[0].Compression.MinLength = nil
	p, err := Prepare(cfg, nil, nil, PrepareOptions{Now: time.Now(), Offline: true})
	if err == nil || len(p.Report.Errors) == 0 {
		t.Fatal("invalid in-memory compression accepted")
	}
}

func TestStaticFeaturesStrictConfiguration(t *testing.T) {
	for _, tc := range []struct{ from, to string }{
		{"encodings: [zstd, gzip]", "encodings: [br]"},
		{"encodings: [zstd, gzip]", "encodings: [gzip, gzip]"},
		{"encodings: [zstd, gzip]", "encodings: []"},
		{"min_length: 512, ", ""},
		{"min_length: 512", "min_length: -1"},
		{"text/plain", "text/*"},
		{"text/plain", "not-a-mime"},
		{"content_types: [text/plain, application/json]", "content_types: []"},
		{"min_length: 512", "min_lenght: 512"},
	} {
		t.Run(tc.to, func(t *testing.T) {
			_, err := config.Parse([]byte("node_id: envoy\nxds: {socket: /tmp/x.sock}\nroute_timeout: 60s\nentrypoints: [https]\n" + strings.Replace(bindingFeatureYAML, tc.from, tc.to, 1)))
			if err == nil {
				t.Fatal("invalid compression accepted")
			}
		})
	}
	for _, field := range []string{"h2cc: true", "paths: /", "prioritiy: 1"} {
		_, err := config.Parse([]byte("node_id: envoy\nxds: {socket: /tmp/x.sock}\nroute_timeout: 60s\nentrypoints: [https]\nstatic:\n  backends: [{name: api, endpoints: [127.0.0.1:80], " + field + "}]"))
		if err == nil {
			t.Fatalf("unknown field accepted: %s", field)
		}
	}
	for _, tc := range []struct{ from, to string }{
		{"path: /api/", "paths: /api/"},
		{"path_type: exact", "path_typ: exact"},
		{"priority: 20", "prioritiy: 20"},
		{"rewrite: /ui/v2/login/", "redirect: /ui/v2/login/"},
	} {
		_, err := config.Parse([]byte("node_id: envoy\nxds: {socket: /tmp/x.sock}\nroute_timeout: 60s\nentrypoints: [https]\n" + strings.Replace(staticFeatureYAML, tc.from, tc.to, 1)))
		if err == nil {
			t.Fatalf("unknown route field accepted: %s", tc.to)
		}
	}
}
