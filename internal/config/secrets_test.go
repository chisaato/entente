package config

import (
	"strings"
	"testing"
)

const baoYAML = "openbao: {address: 'https://bao.example.com', token_file: /run/token, refresh_interval: 5m}\n"
const secretsYAML = "secrets: [{name: example-cert, kv_mount: secret, kv_path: entente/demo, certificate_field: fullchain, private_key_field: key}]\n"

func TestSecretsValidation(t *testing.T) {
	valid := minimal + baoYAML + secretsYAML
	if _, err := Parse([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for name, yaml := range map[string]string{
		"missing openbao":           minimal + secretsYAML,
		"missing secrets":           minimal + baoYAML,
		"missing address":           strings.Replace(valid, "address: 'https://bao.example.com', ", "", 1),
		"missing token":             strings.Replace(valid, "token_file: /run/token, ", "", 1),
		"missing interval":          strings.Replace(valid, ", refresh_interval: 5m", "", 1),
		"invalid interval":          strings.Replace(valid, "5m", "tomorrow", 1),
		"zero interval":             strings.Replace(valid, "5m", "0s", 1),
		"negative interval":         strings.Replace(valid, "5m", "-1s", 1),
		"invalid name":              strings.Replace(valid, "example-cert", "INVALID", 1),
		"missing mount":             strings.Replace(valid, "kv_mount: secret, ", "", 1),
		"missing path":              strings.Replace(valid, "kv_path: entente/demo, ", "", 1),
		"missing certificate field": strings.Replace(valid, "certificate_field: fullchain, ", "", 1),
		"missing private key field": strings.Replace(valid, ", private_key_field: key", "", 1),
		"duplicate name":            minimal + baoYAML + "secrets: [{name: duplicate}, {name: duplicate}]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(yaml)); err == nil {
				t.Fatal("expected invalid configuration")
			}
		})
	}
}

func TestOpenBaoRestartRequired(t *testing.T) {
	old, _ := Parse([]byte(minimal + baoYAML + secretsYAML))
	for _, mutate := range []func(*OpenBao){
		func(b *OpenBao) { b.Address += "/other" },
		func(b *OpenBao) { b.TokenFile += ".new" },
		func(b *OpenBao) { b.CAFile = "/ca.pem" },
		func(b *OpenBao) { b.RefreshInterval = "10m" },
	} {
		next := *old
		bao := *old.OpenBao
		next.OpenBao = &bao
		mutate(next.OpenBao)
		if len(RestartRequired(old, &next)) == 0 {
			t.Error("openbao change must require restart")
		}
	}
	next := *old
	next.Secrets = nil
	if len(RestartRequired(old, &next)) != 0 {
		t.Error("secrets must allow hot reload")
	}
	next.OpenBao = nil
	if len(RestartRequired(old, &next)) == 0 {
		t.Error("disabling openbao must require restart")
	}
}
