package labels

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	valid := map[string]string{
		"entente.enable":      "true",
		"entente.entrypoints": "https, http",
		"entente.host":        "app.example.com",
		"entente.port":        "8080",
	}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range valid {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			if kv[i+1] == "<delete>" {
				delete(m, kv[i])
			} else {
				m[kv[i]] = kv[i+1]
			}
		}
		return m
	}

	tests := []struct {
		name        string
		prefix      string
		labels      map[string]string
		wantEnabled bool
		want        Spec
		wantErr     string
	}{
		{
			name: "valid", prefix: "entente", labels: valid, wantEnabled: true,
			want: Spec{Entrypoints: []string{"https", "http"}, Host: "app.example.com", Port: 8080},
		},
		{name: "no labels", prefix: "entente", labels: nil},
		{name: "other prefix is ignored", prefix: "edge", labels: valid},
		{name: "enable absent", prefix: "entente", labels: with("entente.enable", "<delete>")},
		{name: "enable false", prefix: "entente", labels: with("entente.enable", "false")},
		{name: "enable garbage", prefix: "entente", labels: with("entente.enable", "yes"), wantErr: `want "true" or "false"`},
		{name: "missing entrypoints", prefix: "entente", labels: with("entente.entrypoints", "<delete>"), wantEnabled: true, wantErr: "no default entrypoint"},
		{name: "empty entrypoint", prefix: "entente", labels: with("entente.entrypoints", "https,,http"), wantEnabled: true, wantErr: "empty entrypoint"},
		{name: "duplicate entrypoint", prefix: "entente", labels: with("entente.entrypoints", "https,https"), wantEnabled: true, wantErr: "listed twice"},
		{name: "missing host", prefix: "entente", labels: with("entente.host", "<delete>"), wantEnabled: true, wantErr: "entente.host is required"},
		{name: "upper-case host", prefix: "entente", labels: with("entente.host", "App.example.com"), wantEnabled: true, wantErr: "not a lower-case hostname"},
		{name: "missing port", prefix: "entente", labels: with("entente.port", "<delete>"), wantEnabled: true, wantErr: "entente.port is required"},
		{name: "bad port", prefix: "entente", labels: with("entente.port", "70000"), wantEnabled: true, wantErr: "not a port number"},
		{name: "zero port", prefix: "entente", labels: with("entente.port", "0"), wantEnabled: true, wantErr: "not a port number"},
		{name: "typo is rejected", prefix: "entente", labels: with("entente.entrypoint", "https"), wantEnabled: true, wantErr: "unknown labels entente.entrypoint"},
		{
			name: "wildcard host, network and policy pass through", prefix: "entente",
			labels:      with("entente.host", "*.example.com", "entente.network", "web", "entente.policy", "protect"),
			wantEnabled: true,
			want:        Spec{Entrypoints: []string{"https", "http"}, Host: "*.example.com", Port: 8080, Network: "web", Policy: "protect"},
		},
		{
			name: "custom prefix", prefix: "edge.v2",
			labels: map[string]string{
				"edge.v2.enable": "true", "edge.v2.entrypoints": "http", "edge.v2.host": "a.b", "edge.v2.port": "80",
				"entente.enable": "true",
			},
			wantEnabled: true,
			want:        Spec{Entrypoints: []string{"http"}, Host: "a.b", Port: 80},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, enabled, err := Parse(tt.prefix, tt.labels)
			if enabled != tt.wantEnabled {
				t.Errorf("enabled = %v, want %v", enabled, tt.wantEnabled)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("spec = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseTimeout(t *testing.T) {
	for _, value := range []string{"60s", "0s", "1.5s", "", "-1s", "forever", "999999999999h"} {
		t.Run(value, func(t *testing.T) {
			spec, enabled, err := Parse("edge.v2", map[string]string{
				"edge.v2.enable": "true", "edge.v2.entrypoints": "https",
				"edge.v2.host": "app.example.com", "edge.v2.port": "80", "edge.v2.timeout": value,
			})
			want, parseErr := time.ParseDuration(value)
			if !enabled {
				t.Fatal("enabled container was ignored")
			}
			if parseErr != nil || want < 0 {
				if err == nil || !strings.Contains(err.Error(), "edge.v2.timeout") {
					t.Fatalf("err = %v, want timeout error", err)
				}
			} else if err != nil || spec.Timeout == nil || *spec.Timeout != want {
				t.Fatalf("timeout = %v, err = %v, want %v", spec.Timeout, err, want)
			}
		})
	}
}
