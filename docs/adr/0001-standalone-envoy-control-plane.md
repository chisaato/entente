---
Status: accepted
---

# 独立的 Envoy 外挂控制平面，面向资源受限单机

Entente 是独立部署的 Envoy 外挂控制平面：监听 Docker 事件、读取容器 label，并与 YAML 静态配置一起生成 xDS，效果类似 Traefik Docker provider，数据面换成 Envoy。它不与 HimariConnect 或任何具体平台耦合；目标环境是资源受限的单机（如腾讯云），不安装 K3S / Kubernetes；用 Go 和 `go-control-plane` 实现；不做 WebUI，配置保持纯声明式（YAML，变更走 Git）。

## Consequences

第一版明确不做：内网穿透 / 隧道层（gost 等，那是另一条线）；任何带参数的中间件和鉴权策略（第一版只做反代基础配置）；增量 xDS（单机规模用全量 snapshot）。SDS / OpenBao 证书对接原列为第一版非目标，已提前实现，见 ADR-0014、ADR-0018。需要可视化时，只做只读的 status / metrics 端点，第一版未做。
