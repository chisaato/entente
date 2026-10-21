package reconcile

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/docker"
)

func mustConfig(t *testing.T, extra string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(`
node_id: envoy
route_timeout: 60s
xds: {socket: /tmp/x.sock}
entrypoints: [https, http]
docker: {host: unix:///var/run/docker.sock}
` + extra))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ctr(name string, nets map[string]string, kv ...string) docker.Container {
	c := docker.Container{ID: name + "-id", Name: name, Labels: map[string]string{}, Networks: map[string]netip.Addr{}}
	for n, ip := range nets {
		if ip == "" {
			c.Networks[n] = netip.Addr{}
		} else {
			c.Networks[n] = netip.MustParseAddr(ip)
		}
	}
	for i := 0; i < len(kv); i += 2 {
		c.Labels[kv[i]] = kv[i+1]
	}
	return c
}

func exposed(name, host, eps string, nets map[string]string, extra ...string) docker.Container {
	kv := append([]string{
		"entente.enable", "true",
		"entente.entrypoints", eps,
		"entente.host", host,
		"entente.port", "80",
	}, extra...)
	return ctr(name, nets, kv...)
}

var oneNet = map[string]string{"web": "172.18.0.2"}

func rejectedSubjects(rs []Rejection) map[string]string {
	m := map[string]string{}
	for _, r := range rs {
		m[r.Subject] = r.Reason
	}
	return m
}

func TestBuildDocker(t *testing.T) {
	cfg := mustConfig(t, "")
	m, rej := Build(cfg, []docker.Container{
		exposed("whoami", "whoami.example.com", "https,http", oneNet),
		// 没有 label:静默忽略
		ctr("plain", oneNet),
	})
	if len(rej) != 0 {
		t.Fatalf("unexpected rejections: %v", rej)
	}
	if len(m.Routes) != 1 || m.Routes[0].Name != "docker/whoami" || m.Routes[0].Backend != "docker/whoami" {
		t.Fatalf("routes = %+v", m.Routes)
	}
	if got := m.Backends[0].Endpoints[0].String(); got != "172.18.0.2:80" {
		t.Errorf("endpoint = %s", got)
	}
	if strings.Join(m.Entrypoints, ",") != "http,https" {
		t.Errorf("entrypoints = %v", m.Entrypoints)
	}
}

func TestBuildDockerDisabledIgnoresContainers(t *testing.T) {
	cfg := mustConfig(t, "")
	cfg.Docker = nil
	m, rej := Build(cfg, []docker.Container{exposed("a", "a.example.com", "https", oneNet)})
	if len(m.Routes) != 0 || len(rej) != 0 {
		t.Fatalf("routes=%v rejected=%v", m.Routes, rej)
	}
}

func TestBuildRejectionsAreIsolated(t *testing.T) {
	cfg := mustConfig(t, "")
	twoNets := map[string]string{"web": "172.18.0.3", "db": "172.19.0.3"}
	m, rej := Build(cfg, []docker.Container{
		exposed("good", "good.example.com", "https", oneNet),
		exposed("undeclared", "u.example.com", "admin", oneNet),
		exposed("policy", "p.example.com", "https", oneNet, "entente.policy", "protect"),
		exposed("ambiguous", "amb.example.com", "https", twoNets),
		exposed("chosen", "chosen.example.com", "https", twoNets, "entente.network", "db"),
		exposed("wrongnet", "w.example.com", "https", oneNet, "entente.network", "db"),
		exposed("hostnet", "h.example.com", "https", map[string]string{"host": ""}),
		ctr("typo", oneNet, "entente.enable", "true", "entente.hots", "x"),
	})

	got := rejectedSubjects(rej)
	want := map[string]string{
		"docker/undeclared":   `entrypoint "admin" is not declared`,
		"docker/policy":       "policy layer is not implemented",
		"container/ambiguous": "attached to 2 networks",
		"container/wrongnet":  `not attached to network "db"`,
		"container/hostnet":   `no IP address on network "host"`,
		"container/typo":      "unknown labels entente.hots",
	}
	for subj, frag := range want {
		if !strings.Contains(got[subj], frag) {
			t.Errorf("rejection for %s = %q, want it to contain %q", subj, got[subj], frag)
		}
	}
	if len(got) != len(want) {
		t.Errorf("rejections = %v", rej)
	}

	var names []string
	for _, r := range m.Routes {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "docker/chosen,docker/good" {
		t.Errorf("accepted routes = %v", names)
	}
	for _, b := range m.Backends {
		if b.Name == "docker/chosen" && b.Endpoints[0].Addr().String() != "172.19.0.3" {
			t.Errorf("chosen network not used: %v", b.Endpoints)
		}
	}
}

func TestBuildHostConflictRejectsAllClaimants(t *testing.T) {
	cfg := mustConfig(t, `
static:
  backends: [{name: legacy, endpoints: ["10.0.0.5:8080"]}]
  routes:
    - {name: legacy, entrypoints: [https], host: dup.example.com, backend: legacy}
`)
	m, rej := Build(cfg, []docker.Container{
		exposed("a", "dup.example.com", "https", oneNet),
		// 不同的 entrypoint:不冲突
		exposed("b", "dup.example.com", "http", oneNet),
	})
	got := rejectedSubjects(rej)
	if !strings.Contains(got["docker/a"], "static/legacy") || !strings.Contains(got["static/legacy"], "docker/a") {
		t.Errorf("rejections = %v", rej)
	}
	if len(m.Routes) != 1 || m.Routes[0].Name != "docker/b" {
		t.Errorf("routes = %+v", m.Routes)
	}
	if len(m.Backends) != 1 {
		t.Errorf("unreferenced backends must be dropped, got %+v", m.Backends)
	}
}

func TestBuildStatic(t *testing.T) {
	cfg := mustConfig(t, `
static:
  backends: [{name: api, endpoints: ["10.0.0.5:8080", "10.0.0.6:8080"]}]
  routes:
    - {name: api, entrypoints: [https], host: api.example.com, backend: api}
    - {name: missing, entrypoints: [https], host: m.example.com, backend: nope}
    - {name: guarded, entrypoints: [https], host: g.example.com, backend: api, policy: protect}
    - {name: nohost, entrypoints: [https], host: "", backend: api}
    - {name: noep, host: n.example.com, backend: api}
`)
	m, rej := Build(cfg, nil)
	got := rejectedSubjects(rej)
	for subj, frag := range map[string]string{
		"static/missing": `backend "static/nope" does not exist`,
		"static/guarded": "policy",
		"static/nohost":  "not a lower-case hostname",
		"static/noep":    "no entrypoints",
	} {
		if !strings.Contains(got[subj], frag) {
			t.Errorf("rejection for %s = %q, want %q", subj, got[subj], frag)
		}
	}
	if len(m.Routes) != 1 || len(m.Backends[0].Endpoints) != 2 {
		t.Errorf("model = %+v", m)
	}
}

func TestBuildRouteTimeout(t *testing.T) {
	for _, global := range []string{"60s", "0s"} {
		t.Run(global, func(t *testing.T) {
			cfg := mustConfig(t, `
static:
  backends: [{name: api, endpoints: ["10.0.0.5:8080"]}]
  routes:
    - {name: inherited, entrypoints: [https], host: inherited.example.com, backend: api}
    - {name: unlimited, entrypoints: [https], host: unlimited.example.com, backend: api, timeout: "0s"}
    - {name: custom, entrypoints: [https], host: custom.example.com, backend: api, timeout: "2m"}
`)
			// 从文件重新解析以验证热重载后的顶层值。
			data := strings.Replace("node_id: envoy\nxds: {socket: /tmp/x.sock}\nentrypoints: [https]\nroute_timeout: 60s\n", "60s", global, 1)
			reloaded, err := config.Parse([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			reloaded.Static, reloaded.Docker = cfg.Static, cfg.Docker
			m, rej := Build(reloaded, []docker.Container{
				exposed("inherited", "docker-inherited.example.com", "https", oneNet),
				exposed("unlimited", "docker-unlimited.example.com", "https", oneNet, "entente.timeout", "0s"),
				exposed("custom", "docker-custom.example.com", "https", oneNet, "entente.timeout", "2m"),
				exposed("invalid", "invalid.example.com", "https", oneNet, "entente.timeout", "-1s"),
			})
			if len(rej) != 1 || rej[0].Subject != "container/invalid" || !strings.Contains(rej[0].Reason, "timeout") {
				t.Fatalf("rejections = %v", rej)
			}
			if len(m.Routes) != 6 {
				t.Fatalf("routes = %+v", m.Routes)
			}
			inherited, _ := time.ParseDuration(global)
			for _, r := range m.Routes {
				want := inherited
				if strings.HasSuffix(r.Name, "/custom") {
					want = 2 * time.Minute
				} else if strings.HasSuffix(r.Name, "/unlimited") {
					want = 0
				}
				if r.Timeout != want {
					t.Errorf("%s timeout = %v, want %v", r.Name, r.Timeout, want)
				}
			}
		})
	}
}
