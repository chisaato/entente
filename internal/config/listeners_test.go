package config

import (
	"strings"
	"testing"
)

const listenerYAML = `node_id: envoy
xds: {socket: /tmp/test.sock}
route_timeout: 0s
entrypoints: [https]
secrets:
  - name: cert
    file: {certificate_chain: /cert.pem, private_key: /key.pem}
listeners:
  - name: https
    address: "[::]:443"
    entrypoint: https
    hcm: {websocket: false, strip_any_host_port: false}
    alpn: [h2, http/1.1]
    fallback: {static_secret: sink}
    tls_bindings:
      - {name: example, server_names: ["*.example.com", example.com], secret: cert}
`

func TestListenerExplicitConfiguration(t *testing.T) {
	if _, err := Parse([]byte(listenerYAML)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, from, to string }{
		{"missing websocket", "websocket: false, ", ""},
		{"missing strip port", ", strip_any_host_port: false", ""},
		{"missing ALPN", "alpn: [h2, http/1.1]", "alpn: []"},
		{"missing address", "address: \"[::]:443\"", "address: \"\""},
		{"bad address", "[::]:443", "example.com:443"},
		{"unknown entrypoint", "entrypoint: https", "entrypoint: unknown"},
		{"two fallback sources", "static_secret: sink", "static_secret: sink, secret: cert"},
		{"no fallback source", "static_secret: sink", ""},
		{"unknown fallback", "static_secret: sink", "secret: missing"},
		{"unknown binding secret", "secret: cert", "secret: missing"},
		{"duplicate server name", "example.com]", "\"*.example.com\"]"},
		{"bad wildcard", "*.example.com", "*.*.example.com"},
		{"missing bindings", "    tls_bindings:\n      - {name: example, server_names: [\"*.example.com\", example.com], secret: cert}", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(strings.Replace(listenerYAML, tc.from, tc.to, 1))); err == nil {
				t.Fatal("invalid listener accepted")
			}
		})
	}
}
