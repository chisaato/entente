---
Status: accepted
---

# 全量 SotW 快照，版本是内容哈希

单机规模用全量 state-of-the-world snapshot，不做增量 xDS。每次 reconcile 后用 `SnapshotCache` 生成新快照：每个 Backend 一个 Cluster（走 EDS，这样容器 IP 变化只需推 EDS），每个 Entrypoint 一份 RDS。snapshot version 是全部资源确定性序列化后的 sha256 前 16 位，不用 wall clock，也不用时间：避免时钟回拨让 Envoy 认为版本没变。内容不变则不重发；进程重启后生成的版本与 Envoy 已有的一致。
