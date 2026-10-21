---
Status: accepted
---

# 路由超时必须显式配置，允许 label 覆盖

顶层 `route_timeout` 必填，是非负 Go duration，不沿用 Envoy 隐式的 15s。隐含超时违背明确配置（ADR-0002），也可能截断耗时请求。静态路由的 `timeout` 或 Docker label `<prefix>.timeout` 可以覆盖顶层值；只有省略覆盖值才继承。`0s` 表示不限时。无论是否为零，都显式下发 Envoy RouteAction 的 `timeout`。

YAML 里缺顶层值，或任一超时非法，是文件级错误。label 超时非法只拒绝该容器路由。顶层值和静态覆盖值都支持热重载。WebSocket 由 bootstrap HCM 的 `upgrade_configs` 负责，Entente 不管理升级配置。超时是路由行为，不是安全决策，所以允许写在 label 里，不受 ADR-0006 的 Policy 所有权限制。
