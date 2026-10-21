---
Status: accepted
---

# SDS 只读 KV v2，认证只消费一个 token 文件

控制平面从 OpenBao KV 读证书，经同一条 ADS 上的 SDS 推给 Envoy，证书不落盘。只做 KV v2 读取，用标准库 `net/http`，不引入 OpenBao / Vault SDK。认证只消费一个 token 文件（由 bao agent 或用户自己的机制维护），不实现任何登录。地址、token 文件、刷新间隔，以及每个 secret 的挂载点、路径和字段名全部必填，没有默认值。

## Consequences

- 有 KV 来源就必须有 `openbao`；有 `openbao` 但没有任何 secret 也是配置错误。只从本地文件读证书时不要配置 `openbao`（ADR-0018）。`openbao.ca_file` 可选，缺省用系统根证书。
- 读取是 `GET {address}/v1/{kv_mount}/data/{kv_path}`，请求头 `X-Vault-Token`。每次请求前重新读 token 文件，以跟上续期。单次请求超时是代码常量 10s。
- 每个 secret 生成同名的 `tls_certificate`（`inline_bytes`），bootstrap 用 `sds_config: { ads: {} }` 按名字引用。私钥绝不写日志。
- 内容必须通过 `tls.X509KeyPair`，否则视为拉取失败。已过期或 7 天内过期时打 WARN。成功后按 `refresh_interval` 轮询（KV v2 没有推送）；失败后按 1s→`refresh_interval` 退避。内容不变则快照版本不变。
- 失败时是否下发见 ADR-0015。`secrets` 列表可热更新（新增立即拉取，删除即从快照移除）；`openbao.*` 需要重启。
