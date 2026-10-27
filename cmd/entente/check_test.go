package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func executeCheck(t *testing.T, contents string, extra ...string) (string, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "entente.yaml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"--config", path, "--check"}, extra...))
	err := root.Execute()
	if err == nil {
		return out.String(), 0
	}
	var exit exitError
	if errors.As(err, &exit) {
		return out.String(), exit.code
	}
	return out.String(), 2
}

const minimalCheckConfig = `node_id: test
route_timeout: 0s
xds: {socket: /tmp/entente-check.sock}
entrypoints: [https]
`

func TestCheckExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		args         []string
		code         int
	}{
		{"valid", minimalCheckConfig, nil, 0},
		{"invalid", minimalCheckConfig + "unknown: true\n", nil, 1},
		{"invalid-route", minimalCheckConfig + "static:\n  routes:\n    - {name: bad, host: example.com, entrypoints: [https], backend: absent}\n", nil, 1},
		{"bad-flag", minimalCheckConfig, []string{"--no-such-flag"}, 2},
		{"bad-level", minimalCheckConfig, []string{"--log-level=not-a-level"}, 2},
		{"positional", minimalCheckConfig, []string{"unexpected"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out, code := executeCheck(t, tc.config, tc.args...); code != tc.code {
				t.Fatalf("code=%d want=%d output=%s", code, tc.code, out)
			}
		})
	}
}

func TestCheckOfflineReportsUnchecked(t *testing.T) {
	text := minimalCheckConfig + `docker: {host: "unix:///no-such-docker.sock"}
openbao:
  address: http://127.0.0.1:1
  token_file: /no-such-token
  refresh_interval: 1m
secrets:
  - {name: kv-cert, kv_mount: secret, kv_path: cert, certificate_field: cert, private_key_field: key}
`
	out, code := executeCheck(t, text)
	if code != 0 || !strings.Contains(out, "未检查: secret/kv-cert") || !strings.Contains(out, "未检查: docker snapshot") || !strings.Contains(out, "已检查:") {
		t.Fatalf("code=%d output=%s", code, out)
	}
}

func TestCheckLocalCertificateSAN(t *testing.T) {
	dir := t.TempDir()
	cert, key, err := generateCertificate("good.example.com", 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCertificate(dir, cert, key); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"good.example.com", "wrong.example.com"} {
		text := minimalCheckConfig + fmt.Sprintf(`secrets:
  - name: cert
    file: {certificate_chain: %q, private_key: %q}
listeners:
  - name: tls
    address: 127.0.0.1:18443
    entrypoint: https
    hcm: {websocket: true, strip_any_host_port: true}
    alpn: [http/1.1]
    fallback: {static_secret: sink}
    tls_bindings:
      - {name: binding, server_names: [%s], secret: cert}
`, filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), host)
		out, code := executeCheck(t, text)
		if host == "good.example.com" && (code != 0 || !strings.Contains(out, "已检查: secret/cert")) {
			t.Fatalf("valid SAN: code=%d output=%s", code, out)
		}
		if host == "wrong.example.com" && (code != 1 || !strings.Contains(out, "SAN does not cover")) {
			t.Fatalf("invalid SAN: code=%d output=%s", code, out)
		}
	}
}
