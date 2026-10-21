---
Status: accepted
---

# 对 Envoy 请求过、但 YAML 未声明的入口推空 RouteConfiguration

控制平面记下 Envoy 通过 RDS 请求过的每个 `route_config_name`。其中没有在 YAML 里声明的名字，推一份空的 RouteConfiguration（返回 404），并打 WARN。这只让未声明入口什么都不暴露，没有引入路由上的默认值。

## Consequences

SotW 下，RDS 不会因为资源从响应里消失就删掉它。如果从 YAML 删掉一个入口又不推空配置，对应 Listener 会留着旧路由，而旧路由指向的 cluster 已被 CDS 删除，结果是 503。推空配置后变成 404，方向是 fail closed。bootstrap 里引用了未声明入口的 Listener 也不会停在 warming，因此不再依赖 `initial_fetch_timeout` 超时后的具体行为。
