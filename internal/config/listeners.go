package config

import (
	"fmt"
	"net/netip"
	"slices"

	"github.com/chisaato/entente/internal/labels"
)

// Listener 的安全配置只来自 YAML,不接受 label 覆盖。
type Listener struct {
	Name        string       `yaml:"name"`
	Address     string       `yaml:"address"`
	Entrypoint  string       `yaml:"entrypoint"`
	HCM         HCM          `yaml:"hcm"`
	ALPN        []string     `yaml:"alpn"`
	Fallback    Fallback     `yaml:"fallback"`
	TLSBindings []TLSBinding `yaml:"tls_bindings"`
}

type HCM struct {
	Websocket        *bool `yaml:"websocket"`
	StripAnyHostPort *bool `yaml:"strip_any_host_port"`
}

type Fallback struct {
	StaticSecret string `yaml:"static_secret"`
	Secret       string `yaml:"secret"`
}

type TLSBinding struct {
	Name        string   `yaml:"name"`
	ServerNames []string `yaml:"server_names"`
	Secret      string   `yaml:"secret"`
}

func (c *Config) validateListeners() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	secrets := map[string]bool{}
	for _, s := range c.Secrets {
		secrets[s.Name] = true
	}
	names := map[string]bool{}
	for _, l := range c.Listeners {
		if !nameRe.MatchString(l.Name) || names[l.Name] {
			add("listener %q: invalid or duplicate name", l.Name)
		}
		names[l.Name] = true
		if ap, err := netip.ParseAddrPort(l.Address); err != nil || ap.Port() == 0 {
			add("listener %q: address must be an IP:port literal", l.Name)
		}
		if !slices.Contains(c.Entrypoints, l.Entrypoint) {
			add("listener %q: entrypoint %q is not declared", l.Name, l.Entrypoint)
		}
		if l.HCM.Websocket == nil || l.HCM.StripAnyHostPort == nil {
			add("listener %q: all hcm options are required", l.Name)
		}
		if len(l.ALPN) == 0 {
			add("listener %q: alpn is required", l.Name)
		}
		seenALPN := map[string]bool{}
		for _, p := range l.ALPN {
			if p == "" || len(p) > 255 || seenALPN[p] {
				add("listener %q: invalid or duplicate ALPN", l.Name)
			}
			seenALPN[p] = true
		}
		if (l.Fallback.Secret == "") == (l.Fallback.StaticSecret == "") {
			add("listener %q: exactly one fallback certificate source is required", l.Name)
		}
		if l.Fallback.Secret != "" && !secrets[l.Fallback.Secret] {
			add("listener %q: unknown fallback secret %q", l.Name, l.Fallback.Secret)
		}
		if l.TLSBindings == nil {
			add("listener %q: tls_bindings is required", l.Name)
		}
		bindings, hosts := map[string]bool{}, map[string]bool{}
		for _, b := range l.TLSBindings {
			if !nameRe.MatchString(b.Name) || bindings[b.Name] {
				add("listener %q: invalid or duplicate binding %q", l.Name, b.Name)
			}
			bindings[b.Name] = true
			if !secrets[b.Secret] {
				add("listener %q binding %q: unknown secret %q", l.Name, b.Name, b.Secret)
			}
			if len(b.ServerNames) == 0 {
				add("listener %q binding %q: server_names is required", l.Name, b.Name)
			}
			for _, h := range b.ServerNames {
				if !labels.ValidHost(h) || hosts[h] {
					add("listener %q: invalid or duplicate server name %q", l.Name, h)
				}
				hosts[h] = true
			}
		}
	}
	return errs
}
