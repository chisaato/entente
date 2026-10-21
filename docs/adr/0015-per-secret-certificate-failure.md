---
Status: accepted
---

# 证书按 secret 独立失败，失败不丢最后一份有效内容

证书拉取或读取按 secret 失败，不影响其他 secret，也不影响路由。某个 secret 从未成功过：快照里不下发它，引用它的 Listener / filter chain 在 Envoy 侧保持 warming，不对外提供 TLS。之前成功过、这次失败：保留上一份有效内容并打 ERROR——证书还没过期时丢掉它，只会让正在工作的 TLS 中断。

配置了 secrets 时，要等每个 secret 都完成第一次拉取或读取（成功或失败）后才发布快照，避免控制平面重启后先推一份缺证书的快照。本地 PEM 来源（ADR-0018）用同一套失败语义。
