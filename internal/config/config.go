// Package config 加载并校验 Entente 的 YAML 配置。
//
// 这里只做文件级校验:必填字段、名字语法、唯一性、endpoint 语法。
// 静态 Route 是否引用了已声明的 entrypoint 或已存在的 backend,由 reconciler
// 按每条 Route 检查,因此一条坏路由不会把其他路由一起拖垮。
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
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultLabelPrefix 在未设置 docker.label_prefix 时使用。
const DefaultLabelPrefix = "entente"

// DefaultSocketMode 在未设置 xds.socket_mode 时使用。它是仍允许同一属主的
// Envoy 连上来的最严权限。
const DefaultSocketMode os.FileMode = 0o600

type Config struct {
	// NodeID 必须等于 Envoy bootstrap 里的 node.id。快照只发布给这个 node。
	NodeID string `yaml:"node_id"`

	XDS XDS `yaml:"xds"`

	// Entrypoints 列出本控制平面负责的 RDS route_config_name。
	// 没有默认 entrypoint。
	Entrypoints []string `yaml:"entrypoints"`

	// RouteTimeout 是必填的路由超时,0s 表示不限时。
	RouteTimeout string `yaml:"route_timeout"`
	routeTimeout time.Duration

	// Docker 启用 Docker 来源。缺省表示关闭。
	Docker *Docker `yaml:"docker"`

	Static  Static   `yaml:"static"`
	OpenBao *OpenBao `yaml:"openbao"`
	Secrets []Secret `yaml:"secrets"`
}

type OpenBao struct {
	Address         string `yaml:"address"`
	TokenFile       string `yaml:"token_file"`
	CAFile          string `yaml:"ca_file"`
	RefreshInterval string `yaml:"refresh_interval"`
}

type Secret struct {
	Name             string `yaml:"name"`
	KVMount          string `yaml:"kv_mount"`
	KVPath           string `yaml:"kv_path"`
	CertificateField string `yaml:"certificate_field"`
	PrivateKeyField  string `yaml:"private_key_field"`
}

type XDS struct {
	// Socket 是 xDS gRPC 服务器监听的 Unix domain socket 路径。
	Socket string `yaml:"socket"`
	// SocketMode 是八进制权限字符串,例如 "0600" 或 "0660"。
	SocketMode string `yaml:"socket_mode"`
}

type Docker struct {
	// Host 是 Docker Engine API 地址,例如 unix:///var/run/docker.sock。
	Host string `yaml:"host"`
	// LabelPrefix 限定本实例读取的容器 label 命名空间。
	// 默认值为 DefaultLabelPrefix。
	LabelPrefix string `yaml:"label_prefix"`
}

type Static struct {
	Backends []StaticBackend `yaml:"backends"`
	Routes   []StaticRoute   `yaml:"routes"`
}

type StaticBackend struct {
	Name string `yaml:"name"`
	// Endpoints 是 "IP:port" 字面量。不接受主机名,因为 backend 经 EDS 下发,
	// EDS 携带的是地址而不是名字。
	Endpoints []string `yaml:"endpoints"`

	parsed []netip.AddrPort
}

// ParsedEndpoints 返回 Load 校验过的 endpoints。
func (b StaticBackend) ParsedEndpoints() []netip.AddrPort { return b.parsed }

type StaticRoute struct {
	Name        string   `yaml:"name"`
	Entrypoints []string `yaml:"entrypoints"`
	Host        string   `yaml:"host"`
	Backend     string   `yaml:"backend"`
	// Timeout 可选,覆盖顶层 route_timeout;指针用于区分省略与显式空值。
	Timeout *string `yaml:"timeout"`
	timeout *time.Duration
	// Policy 为预留字段;任何非空值都会使该路由被拒绝。
	Policy string `yaml:"policy"`
}

// ParsedRouteTimeout 返回文件级校验过的路由超时。
func (c *Config) ParsedRouteTimeout() time.Duration { return c.routeTimeout }

// ParsedTimeout 返回文件级校验过的覆盖值;nil 表示继承顶层配置。
func (r StaticRoute) ParsedTimeout() *time.Duration { return r.timeout }

// SocketFileMode 返回解析后的 xds.socket_mode。
func (c *Config) SocketFileMode() os.FileMode {
	if c.XDS.SocketMode == "" {
		return DefaultSocketMode
	}
	// 已在 Load 中校验过。
	m, _ := strconv.ParseUint(c.XDS.SocketMode, 8, 32)
	return os.FileMode(m)
}

// Load 读取 path 处的配置文件,严格解码并校验。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse 严格解码并校验 YAML 配置。未知字段视为错误。
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
	// nameRe 覆盖 entrypoint、backend 和 route 的名字。
	nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)
	// LabelPrefixRe 是 docker.label_prefix 接受的语法。
	LabelPrefixRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)
)

func (c *Config) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.NodeID == "" {
		add("node_id is required and must match node.id in the Envoy bootstrap")
	}
	if d, err := time.ParseDuration(c.RouteTimeout); err != nil || d < 0 {
		add("route_timeout is required and must be a non-negative Go duration")
	} else {
		c.routeTimeout = d
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
	if len(c.Secrets) > 0 && c.OpenBao == nil {
		add("openbao is required when secrets are configured")
	}
	if c.OpenBao != nil {
		if len(c.Secrets) == 0 {
			add("openbao requires at least one secret")
		}
		if strings.TrimSpace(c.OpenBao.Address) == "" {
			add("openbao.address is required")
		}
		if strings.TrimSpace(c.OpenBao.TokenFile) == "" {
			add("openbao.token_file is required")
		}
		if d, err := time.ParseDuration(c.OpenBao.RefreshInterval); err != nil || d <= 0 {
			add("openbao.refresh_interval must be a positive Go duration")
		}
	}
	for i, s := range c.Secrets {
		if !nameRe.MatchString(s.Name) {
			add("secrets[%d]: invalid name %q", i, s.Name)
		}
		if seen[s.Name] {
			add("secret %q declared twice", s.Name)
		}
		seen[s.Name] = true
		for field, value := range map[string]string{"kv_mount": s.KVMount, "kv_path": s.KVPath, "certificate_field": s.CertificateField, "private_key_field": s.PrivateKeyField} {
			if strings.TrimSpace(value) == "" {
				add("secrets[%d].%s is required", i, field)
			}
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
	for i := range c.Static.Routes {
		r := &c.Static.Routes[i]
		if r.Timeout != nil {
			if d, err := time.ParseDuration(*r.Timeout); err != nil || d < 0 {
				add("static.routes[%d].timeout must be a non-negative Go duration", i)
			} else {
				r.timeout = &d
			}
		}
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

// RestartRequired 报告 old 与 new 之间有差异、但不重启进程就无法生效的字段。
func RestartRequired(old, new *Config) []string {
	var diff []string
	if (old.OpenBao == nil) != (new.OpenBao == nil) || (old.OpenBao != nil && new.OpenBao != nil && *old.OpenBao != *new.OpenBao) {
		diff = append(diff, "openbao")
	}
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
