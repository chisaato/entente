// Package model 定义 Entente 内部、与来源无关的路由模型。
//
// 分层借鉴 Kubernetes Gateway API:Entrypoint 是挂载点
// (与 Envoy bootstrap 里的 RDS route_config_name 一一对应),
// Route 表示"哪个 host、挂在哪些 entrypoint、转到哪个 backend",
// Backend 是一组具体 endpoint。安全决策属于单独的 Policy 层,尚未实现;
// Route 只携带一个目前必须保持为空的预留 PolicyRef。
package model

import (
	"net/netip"
	"time"

	"github.com/chisaato/entente/internal/config"
)

// Source 标识 Route 或 Backend 的来源。
type Source string

const (
	SourceDocker Source = "docker"
	SourceStatic Source = "static"
)

// Route 把一个 host 的路径挂到一个或多个 entrypoint,转发到单个 Backend。
// 未声明路径的旧路由匹配前缀 "/",不能与同 host 的其他路由混用。
type Route struct {
	// Name 在所有来源中唯一,例如 "docker/whoami" 或 "static/legacy"。
	Name   string
	Source Source

	Entrypoints []string
	Host        string
	Backend     string
	Path        string
	PathType    string
	Rewrite     string
	Priority    *int
	// Timeout 已合并来源覆盖值与顶层配置,0 表示不限时。
	Timeout time.Duration

	// PolicyRef 预留给未来的 Policy 层。非空值会被 reconciler 拒绝;
	// 静默忽略等于 fail open。
	PolicyRef string
}

// Backend 是一组带名字的 endpoint。它在 Envoy 里变成一个 EDS Cluster。
type Backend struct {
	// Name 在所有来源中唯一,同时用作 Envoy cluster 名。
	Name      string
	Source    Source
	H2C       bool
	Endpoints []netip.AddrPort
}

// PathError 校验合并模型中的路由路径;候选准备再决定是否拒绝整份配置。
func (r Route) PathError() string {
	if r.Path == "" && r.PathType == "" {
		if r.Rewrite != "" || r.Priority != nil {
			return "rewrite and priority require explicit path and path_type"
		}
		return ""
	}
	if !absolutePath(r.Path) || (r.PathType != "prefix" && r.PathType != "exact") {
		return "path and path_type must be declared together: absolute path and prefix or exact"
	}
	if r.Rewrite != "" && !absolutePath(r.Rewrite) {
		return "rewrite must be an absolute path"
	}
	return ""
}

func absolutePath(p string) bool {
	if len(p) == 0 || p[0] != '/' || (len(p) > 1 && p[1] == '/') {
		return false
	}
	for _, c := range p {
		if c <= ' ' || c == 127 || c == '?' || c == '#' || c == '\\' {
			return false
		}
	}
	return true
}

// Model 是一次 reconcile 的校验结果:其中的内容都可以安全地翻译成 xDS 资源。
type Model struct {
	Listeners   []config.Listener
	Entrypoints []string
	Routes      []Route
	Backends    []Backend
}
