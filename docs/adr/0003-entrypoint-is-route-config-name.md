---
Status: accepted
---

# 入口是 RDS 的 route_config_name，不是 Listener 名字

与 Traefik 不同：监听器全在 Envoy bootstrap 里，控制平面只通过 RDS 名字把路由挂上去。接入点是 HttpConnectionManager 的 `rds.route_config_name`，不是 Listener 的 `name`。YAML 里用 `entrypoints` 显式声明自己管理哪些入口；没有默认入口。label 引用了未声明的入口就拒绝该路由并记日志。控制平面不探测 Envoy 里实际有哪些 Listener，bootstrap 与 YAML 的名字是否一致由运维保证，这是明确配置的代价。

同一 host 可以挂在多个入口上：往多份 RouteConfiguration 里各加一个 virtual host，互不影响。
