// Package config loads and validates Entente's YAML configuration.
//
// Validation here is file-level only: required fields, name syntax,
// uniqueness, endpoint syntax. Whether a static Route references a declared
// entrypoint or an existing backend is checked per Route by the reconciler,
// so one bad Route never takes the others down with it.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// DefaultLabelPrefix is used when docker.label_prefix is not set.
const DefaultLabelPrefix = "entente"

// DefaultSocketMode is used when xds.socket_mode is not set. It is the most
// restrictive mode that still lets a same-owner Envoy connect.
const DefaultSocketMode os.FileMode = 0o600

type Config struct {
	// NodeID must equal node.id in the Envoy bootstrap. Snapshots are only
	// published for this node.
	NodeID string `yaml:"node_id"`

	XDS XDS `yaml:"xds"`

	// Entrypoints lists the RDS route_config_names this control plane owns.
	// There is no default entrypoint.
	Entrypoints []string `yaml:"entrypoints"`

	// Docker enables the Docker source. Absent means disabled.
	Docker *Docker `yaml:"docker"`

	Static Static `yaml:"static"`
}

type XDS struct {
	// Socket is the Unix domain socket path the xDS gRPC server listens on.
	Socket string `yaml:"socket"`
	// SocketMode is an octal permission string such as "0600" or "0660".
	SocketMode string `yaml:"socket_mode"`
}

type Docker struct {
	// Host is the Docker Engine API address, e.g. unix:///var/run/docker.sock.
	Host string `yaml:"host"`
	// LabelPrefix namespaces the container labels this instance reads.
	// Defaults to DefaultLabelPrefix.
	LabelPrefix string `yaml:"label_prefix"`
}

type Static struct {
	Backends []StaticBackend `yaml:"backends"`
	Routes   []StaticRoute   `yaml:"routes"`
}

type StaticBackend struct {
	Name string `yaml:"name"`
	// Endpoints are "IP:port" literals. Hostnames are not accepted because
	// backends are served over EDS, which carries addresses, not names.
	Endpoints []string `yaml:"endpoints"`

	parsed []netip.AddrPort
}

// ParsedEndpoints returns the endpoints validated by Load.
func (b StaticBackend) ParsedEndpoints() []netip.AddrPort { return b.parsed }

type StaticRoute struct {
	Name        string   `yaml:"name"`
	Entrypoints []string `yaml:"entrypoints"`
	Host        string   `yaml:"host"`
	Backend     string   `yaml:"backend"`
	// Policy is reserved; any non-empty value makes the route rejected.
	Policy string `yaml:"policy"`
}

// SocketFileMode returns the parsed xds.socket_mode.
func (c *Config) SocketFileMode() os.FileMode {
	if c.XDS.SocketMode == "" {
		return DefaultSocketMode
	}
	m, _ := strconv.ParseUint(c.XDS.SocketMode, 8, 32) // validated in Load
	return os.FileMode(m)
}

// Load reads, strictly decodes and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse strictly decodes and validates YAML config. Unknown keys are errors.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config is empty")
		}
		return nil, err
	}
	if c.Docker != nil && c.Docker.LabelPrefix == "" {
		c.Docker.LabelPrefix = DefaultLabelPrefix
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

var (
	// nameRe covers entrypoint, backend and route names.
	nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)
	// LabelPrefixRe is the accepted syntax for docker.label_prefix.
	LabelPrefixRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)
)

func (c *Config) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.NodeID == "" {
		add("node_id is required and must match node.id in the Envoy bootstrap")
	}
	if c.XDS.Socket == "" {
		add("xds.socket is required")
	}
	if c.XDS.SocketMode != "" {
		m, err := strconv.ParseUint(c.XDS.SocketMode, 8, 32)
		if err != nil || m > 0o777 {
			add("xds.socket_mode %q is not an octal permission like \"0600\"", c.XDS.SocketMode)
		}
	}

	if len(c.Entrypoints) == 0 {
		add("entrypoints must list at least one RDS route_config_name")
	}
	seen := map[string]bool{}
	for _, ep := range c.Entrypoints {
		if !nameRe.MatchString(ep) {
			add("entrypoint %q: invalid name (want %s)", ep, nameRe)
		}
		if seen[ep] {
			add("entrypoint %q declared twice", ep)
		}
		seen[ep] = true
	}

	if c.Docker != nil {
		if c.Docker.Host == "" {
			add("docker.host is required when the docker section is present")
		}
		if !LabelPrefixRe.MatchString(c.Docker.LabelPrefix) {
			add("docker.label_prefix %q: invalid prefix (want %s)", c.Docker.LabelPrefix, LabelPrefixRe)
		}
	}

	seen = map[string]bool{}
	for i := range c.Static.Backends {
		b := &c.Static.Backends[i]
		if !nameRe.MatchString(b.Name) {
			add("static.backends[%d]: invalid name %q (want %s)", i, b.Name, nameRe)
		}
		if seen[b.Name] {
			add("static backend %q declared twice", b.Name)
		}
		seen[b.Name] = true
		if len(b.Endpoints) == 0 {
			add("static backend %q: endpoints must not be empty", b.Name)
		}
		b.parsed = b.parsed[:0]
		for _, e := range b.Endpoints {
			ap, err := netip.ParseAddrPort(e)
			if err != nil || ap.Port() == 0 {
				add("static backend %q: endpoint %q must be an IP:port literal", b.Name, e)
				continue
			}
			b.parsed = append(b.parsed, ap)
		}
	}

	seen = map[string]bool{}
	for i, r := range c.Static.Routes {
		if !nameRe.MatchString(r.Name) {
			add("static.routes[%d]: invalid name %q (want %s)", i, r.Name, nameRe)
		}
		if seen[r.Name] {
			add("static route %q declared twice", r.Name)
		}
		seen[r.Name] = true
	}

	return errors.Join(errs...)
}

// RestartRequired reports the fields that differ between old and new but
// cannot be applied without restarting the process.
func RestartRequired(old, new *Config) []string {
	var diff []string
	if old.NodeID != new.NodeID {
		diff = append(diff, "node_id")
	}
	if old.XDS != new.XDS {
		diff = append(diff, "xds")
	}
	if (old.Docker == nil) != (new.Docker == nil) {
		diff = append(diff, "docker (enabled/disabled)")
	} else if old.Docker != nil && old.Docker.Host != new.Docker.Host {
		diff = append(diff, "docker.host")
	}
	return diff
}
