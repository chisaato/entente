package config

import (
	"strings"
	"testing"
)

func TestDockerBackendConfiguration(t *testing.T) {
	for name, tc := range map[string]struct {
		backend  string
		disabled bool
		want     string
	}{
		"valid":                       {backend: "docker: {container: gost, network: public, port: 8443}"},
		"both":                        {backend: "endpoints: ['127.0.0.1:80'], docker: {container: gost, network: public, port: 80}", want: "exactly one"},
		"empty endpoints with docker": {backend: "endpoints: [], docker: {container: gost, network: public, port: 80}", want: "exactly one"},
		"neither":                     {want: "endpoints must not be empty"},
		"empty docker":                {backend: "docker: {}", want: "docker.container is required"},
		"missing container":           {backend: "docker: {network: public, port: 80}", want: "docker.container is required"},
		"missing network":             {backend: "docker: {container: gost, port: 80}", want: "docker.network is required"},
		"missing port":                {backend: "docker: {container: gost, network: public}", want: "docker.port"},
		"zero port":                   {backend: "docker: {container: gost, network: public, port: 0}", want: "docker.port"},
		"negative port":               {backend: "docker: {container: gost, network: public, port: -1}", want: "docker.port"},
		"large port":                  {backend: "docker: {container: gost, network: public, port: 65536}", want: "docker.port"},
		"disabled":                    {backend: "docker: {container: gost, network: public, port: 80}", disabled: true, want: "docker source must be enabled"},
	} {
		t.Run(name, func(t *testing.T) {
			data := minimal
			if !tc.disabled {
				data += "docker: {host: unix:///var/run/docker.sock}\n"
			}
			data += "static:\n  backends: [{name: tunnel, " + tc.backend + "}]\n"
			cfg, err := Parse([]byte(data))
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b := cfg.Static.Backends[0]
			if b.Docker.Container != "gost" || b.Docker.Network != "public" || b.Docker.Port != 8443 || len(b.ParsedEndpoints()) != 0 {
				t.Fatalf("backend = %+v", b)
			}
		})
	}
}
