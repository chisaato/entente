---
Status: accepted
---

# SNI 分流与兜底链由 listener 所有者声明

目标仍是防 SNI 探测:未知 SNI 和无 SNI 不获得业务证书。ADR-0020 取代 ADR-0004 后,YAML 声明的 TLS listener 经 LDS 管理,其余静态 listener 仍由 bootstrap 管理:

- 真证书：每条 filter chain 用 `filter_chain_match.server_names` 匹配域名，证书走 SDS（ADR-0014）。
- 兜底:`default_filter_chain` 接住 SNI 不匹配和不带 SNI 的连接,HCM 用内联 `route_config`,对所有请求返回 421。YAML 必须二选一显式引用 bootstrap 静态 secret 或 Entente 管理的 secret;静态 secret 的内容无法由 Entente 校验。

冷启动时,首次材料获取失败的绑定不生成 filter chain,其 SNI 因而落入 421 兜底,并记录 ERROR;材料恢复后补上绑定。热重载缺失材料则拒绝整个候选。LDS listener 绝不引用快照里不存在的动态 secret。`entente gen-cert` 仍用于生成兜底自签证书。

LDS 管理的端口在控制平面尚未成功发布 listener 时不监听,即使兜底引用 bootstrap 静态 secret 也不例外。下面的历史实测记录只描述旧版静态 listener,不能据此推断 LDS 的冷启动行为。

## Consequences

Envoy 1.39 上用 `examples/` 实测过（Envoy 先起，`initial_fetch_timeout` 用默认 15s）。socket 由 Entente 监听，Envoy 是 gRPC 客户端，连不上会自己退避重连，两者启动顺序无关。

| 阶段 | http（无 TLS） | https（SDS 证书） |
|---|---|---|
| 只有 Envoy，0–30s | 端口未监听 | 端口未监听 |
| 只有 Envoy，30s 后 | 404 | 握手失败 |
| Entente 已起，KV 里没有证书 | 约 3s 后 200 | 握手失败 |
| 写入 KV 后 | 200 | 约 9s 后 200（等 Entente 的重试退避） |

只有 Envoy 时，前 30s 端口不监听：CDS 的 initial fetch 先超时 15s，之后 RDS 和 SDS 再各等 15s。想缩短这段时间，要在 bootstrap 里显式设置 `initial_fetch_timeout`。缺失的 SDS 证书只让引用它的 filter chain 握手失败，不影响同一个 Envoy 里的其他 listener。这也是兜底链不能走 SDS 的证据：Entente 没起来时 SDS 证书拿不到，走 SDS 的兜底链也会握手失败。
