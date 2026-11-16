package config

import (
	"fmt"
	"mime"
	"net/netip"
	"slices"
	"strings"

	"github.com/chisaato/entente/internal/labels"
)

// Listener 的安全配置只来自 YAML,不接受 label 覆盖。
type Listener struct {
	Name        string       `yaml:"name"`
	Address     string       `yaml:"address"`
	IPv4Compat  *bool        `yaml:"ipv4_compat,omitempty"`
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
	Name        string       `yaml:"name"`
	ServerNames []string     `yaml:"server_names"`
	Secret      string       `yaml:"secret"`
	Compression *Compression `yaml:"compression"`
}

// Compression 只作用于此 TLS binding 的业务 HCM,不影响其他绑定或 sink。
type Compression struct {
	Encodings    []string `yaml:"encodings"`
	MinLength    *uint32  `yaml:"min_length"`
	ContentTypes []string `yaml:"content_types"`
}

func (c *Compression) Validate() error {
	if c == nil {
		return nil
	}
	if len(c.Encodings) == 0 || c.MinLength == nil || len(c.ContentTypes) == 0 {
		return fmt.Errorf("compression requires encodings, min_length and content_types")
	}
	seen := map[string]bool{}
	for _, e := range c.Encodings {
		if (e != "zstd" && e != "gzip") || seen[e] {
			return fmt.Errorf("compression: unknown or duplicate encoding %q", e)
		}
		seen[e] = true
	}
	seen = map[string]bool{}
	for _, value := range c.ContentTypes {
		media, params, err := mime.ParseMediaType(value)
		if err != nil || !strings.Contains(media, "/") || strings.Contains(value, "*") || len(params) != 0 || media != value || seen[value] {
			return fmt.Errorf("compression: content type %q must be a unique exact MIME type without parameters or wildcards", value)
		}
		seen[value] = true
	}
	return nil
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
		} else if l.IPv4Compat != nil && *l.IPv4Compat && !ap.Addr().Is6() {
			add("listener %q: ipv4_compat can only be enabled for IPv6 addresses", l.Name)
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
			if err := b.Compression.Validate(); err != nil {
				add("listener %q binding %q: %v", l.Name, b.Name, err)
			}
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
