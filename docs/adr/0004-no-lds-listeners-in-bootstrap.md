---
Status: accepted
---

# Listener 写在 Envoy bootstrap，不使用 LDS

Envoy 先以静态 bootstrap 启动，之后才连到控制平面；控制平面不创建监听端口。监听端口、TLS 和 filter chain 结构由运维写在 `static_resources.listeners` 里。路由走 RDS，后端 Cluster 和 Endpoint 走 CDS + EDS。不使用 LDS：改 filter chain 有连接中断风险，常驻端口本来也不需要动态变更。

`.scratch/https-lds-atomic-reload/spec.md` 提议对 HTTPS listener 改走受限 LDS。实现时会写一份新 ADR 取代本 ADR。
