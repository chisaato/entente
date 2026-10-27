---
Status: accepted
---

# 静态 backend 的显式地址或 Docker 来源

静态 backend 必须二选一声明 `endpoints` 或 `docker`。`endpoints` 只接受 IP 字面量,因为 EDS 携带的是地址而不是域名。需要 DNS 的后端将来另开 cluster 类型,不在这里放宽。

`docker` 必须显式声明容器名、网络、端口,并启用 Docker 来源。Docker watcher 的完整快照将它解析为地址,cluster 继续命名为 `static/<name>` 并走 EDS。容器未运行或指定网络没有 IP 是运行时状态,只拒绝引用它的路由,不使 YAML 候选无效;容器恢复后自动恢复路由。
