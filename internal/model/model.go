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
)

// Source 标识 Route 或 Backend 的来源。
type Source string

const (
	SourceDocker Source = "docker"
	SourceStatic Source = "static"
)

// Route 把一个 host 挂到一个或多个 entrypoint,并将其全部流量
// (path 前缀 "/")转发到单个 Backend。
type Route struct {
	// Name 在所有来源中唯一,例如 "docker/whoami" 或 "static/legacy"。
	Name   string
	Source Source

	Entrypoints []string
	Host        string
	Backend     string
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
	Endpoints []netip.AddrPort
}

// Model 是一次 reconcile 的校验结果:其中的内容都可以安全地翻译成 xDS 资源。
type Model struct {
	Entrypoints []string
	Routes      []Route
	Backends    []Backend
}
