---
Status: accepted
---

# 静态 backend 只接受 IP 字面量

静态 backend 的 endpoint 只接受 IP 字面量，因为它们走 EDS，EDS 里是地址不是域名。需要 DNS 的后端将来另开 cluster 类型，而不是在这里放宽。

`.scratch/https-lds-atomic-reload/spec.md` 提议增加 Docker 来源的静态 backend（按容器名、网络和端口引用容器，再经 EDS 下发）。
