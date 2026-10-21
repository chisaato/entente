---
Status: accepted
---

# 动态资源走一条 ADS 流，但 SnapshotCache 用 ads=false

bootstrap 用一条 ADS 流承载 CDS / EDS / RDS（`ads_config`，各处 `ads: {}`），只有一个静态 xDS cluster。这样只有一条连接，Envoy 侧有 CDS→EDS→RDS 的顺序保证。

`SnapshotCache` 必须以 `ads=false` 创建。go-control-plane 在 `ads=true` 时，只有 Envoy 请求的 RDS 名字覆盖快照里全部 RouteConfiguration 才会响应；YAML 只要多声明一个 bootstrap 里没有的入口，所有入口的路由都会卡住。`ads=false` 时按请求的名字取子集返回，互不影响。
