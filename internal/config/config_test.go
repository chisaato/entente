package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

const minimal = `
node_id: envoy
route_timeout: 60s
xds:
  socket: /run/entente/xds.sock
entrypoints: [https, http]
`

func TestParseMinimal(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Docker != nil {
		t.Error("docker source must be disabled when the section is absent")
	}
	if c.SocketFileMode() != 0o600 {
		t.Errorf("socket mode = %#o, want 0600", c.SocketFileMode())
	}
}

func TestParseFull(t *testing.T) {
	c, err := Parse([]byte(`
node_id: envoy
route_timeout: 60s
xds:
  socket: /run/entente/xds.sock
  socket_mode: "0660"
entrypoints: [https]
docker:
  host: unix:///var/run/docker.sock
static:
  backends:
    - name: legacy
      endpoints: ["10.0.0.5:8080", "[fd00::1]:80"]
  routes:
    - name: legacy
      entrypoints: [https]
      host: legacy.example.com
      backend: legacy
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Docker.LabelPrefix != DefaultLabelPrefix {
		t.Errorf("label prefix = %q, want default %q", c.Docker.LabelPrefix, DefaultLabelPrefix)
	}
	if c.SocketFileMode() != 0o660 {
		t.Errorf("socket mode = %#o", c.SocketFileMode())
	}
	eps := c.Static.Backends[0].ParsedEndpoints()
	if len(eps) != 2 || eps[1].Port() != 80 {
		t.Errorf("parsed endpoints = %v", eps)
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]struct{ yaml, want string }{
		"empty":             {"", "config is empty"},
		"unknown key":       {minimal + "listen: 1\n", "field listen not found"},
		"no node":           {"xds: {socket: /x}\nentrypoints: [a]\n", "node_id is required"},
		"no socket":         {"node_id: e\nentrypoints: [a]\n", "xds.socket is required"},
		"bad mode":          {"node_id: e\nxds: {socket: /x, socket_mode: rw}\nentrypoints: [a]\n", "socket_mode"},
		"no entrypoints":    {"node_id: e\nxds: {socket: /x}\n", "at least one"},
		"dup entrypoint":    {"node_id: e\nxds: {socket: /x}\nentrypoints: [a, a]\n", "declared twice"},
		"bad entrypoint":    {"node_id: e\nxds: {socket: /x}\nentrypoints: [A]\n", "invalid name"},
		"docker no host":    {minimal + "docker: {}\n", "docker.host is required"},
		"bad prefix":        {minimal + "docker: {host: unix:///d, label_prefix: Foo}\n", "label_prefix"},
		"hostname endpoint": {minimal + "static:\n  backends: [{name: b, endpoints: [\"db:5432\"]}]\n", "IP:port literal"},
		"no endpoints":      {minimal + "static:\n  backends: [{name: b}]\n", "must not be empty"},
		"dup route":         {minimal + "static:\n  routes: [{name: r}, {name: r}]\n", "declared twice"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestRestartRequired(t *testing.T) {
	a, _ := Parse([]byte(minimal))
	b, _ := Parse([]byte(strings.Replace(minimal, "[https, http]", "[https]", 1)))
	if d := RestartRequired(a, b); len(d) != 0 {
		t.Errorf("entrypoint change must be hot-reloadable, got %v", d)
	}
	c, _ := Parse([]byte(strings.Replace(minimal, "envoy", "envoy-2", 1) + "docker: {host: unix:///d}\n"))
	if d := RestartRequired(a, c); len(d) != 2 {
		t.Errorf("RestartRequired = %v, want node_id and docker", d)
	}
}

func TestRouteTimeout(t *testing.T) {
	for _, value := range []string{"60s", "0s", "1.5s"} {
		t.Run(value, func(t *testing.T) {
			c, err := Parse([]byte(strings.Replace(minimal, "60s", value, 1) + `
static:
  routes:
    - {name: inherited}
    - {name: unlimited, timeout: "0s"}
    - {name: custom, timeout: "2m"}
`))
			if err != nil {
				t.Fatal(err)
			}
			want, _ := time.ParseDuration(value)
			if c.ParsedRouteTimeout() != want || c.Static.Routes[0].ParsedTimeout() != nil ||
				c.Static.Routes[1].ParsedTimeout() == nil || *c.Static.Routes[1].ParsedTimeout() != 0 ||
				c.Static.Routes[2].ParsedTimeout() == nil || *c.Static.Routes[2].ParsedTimeout() != 2*time.Minute {
				t.Fatalf("unexpected parsed timeouts: %+v", c)
			}
			old, err := Parse([]byte(minimal))
			if err != nil {
				t.Fatal(err)
			}
			if diff := RestartRequired(old, c); len(diff) != 0 {
				t.Errorf("timeout changes must be hot-reloadable: %v", diff)
			}
		})
	}
}

func TestInvalidRouteTimeout(t *testing.T) {
	for name, data := range map[string]string{
		"missing":         strings.Replace(minimal, "route_timeout: 60s\n", "", 1),
		"empty":           strings.Replace(minimal, "60s", `""`, 1),
		"invalid":         strings.Replace(minimal, "60s", "forever", 1),
		"negative":        strings.Replace(minimal, "60s", "-1s", 1),
		"overflow":        strings.Replace(minimal, "60s", "999999999999h", 1),
		"static empty":    minimal + "static: {routes: [{name: bad, timeout: \"\"}]}\n",
		"static invalid":  minimal + "static: {routes: [{name: bad, timeout: forever}]}\n",
		"static negative": minimal + "static: {routes: [{name: bad, timeout: -1s}]}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(data)); err == nil || !strings.Contains(err.Error(), "timeout") {
				t.Fatalf("err = %v, want timeout error", err)
			}
		})
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	for _, p := range []string{"../../examples/entente.yaml"} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if _, err := Load(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}
