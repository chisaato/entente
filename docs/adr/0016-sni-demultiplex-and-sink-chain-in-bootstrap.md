---
Status: accepted
---

# SNI 分流与兜底链在 bootstrap 里做，兜底不走 SDS

目标是防 SNI 探测（扫描器拿不到真证书及其 SAN），以及在控制平面或 OpenBao 不可用时让 443 仍能完成握手并返回明确错误，而不是连不上。因为 listener 是静态的（ADR-0004），这件事全部在 Envoy bootstrap 里做，Entente 不参与：

- 真证书：每条 filter chain 用 `filter_chain_match.server_names` 匹配域名，证书走 SDS（ADR-0014）。
- 兜底：`default_filter_chain` 接住 SNI 不匹配和不带 SNI 的连接。证书是 bootstrap 里的静态 secret，HCM 用内联 `route_config`，对所有请求 `direct_response` 返回 421 Misdirected Request。兜底链不依赖 Entente，也不依赖 OpenBao。

兜底证书不走 SDS。真证书缺失时不会退到兜底证书：那条 filter chain 自己握手失败（fail closed），不掩盖故障。`entente gen-cert` 用来生成兜底用的自签证书；它的默认值是 ADR-0002 记下的例外。

## Consequences

Envoy 1.39 上用 `examples/` 实测过（Envoy 先起，`initial_fetch_timeout` 用默认 15s）。socket 由 Entente 监听，Envoy 是 gRPC 客户端，连不上会自己退避重连，两者启动顺序无关。

| 阶段 | http（无 TLS） | https（SDS 证书） |
|---|---|---|
| 只有 Envoy，0–30s | 端口未监听 | 端口未监听 |
| 只有 Envoy，30s 后 | 404 | 握手失败 |
| Entente 已起，KV 里没有证书 | 约 3s 后 200 | 握手失败 |
| 写入 KV 后 | 200 | 约 9s 后 200（等 Entente 的重试退避） |

只有 Envoy 时，前 30s 端口不监听：CDS 的 initial fetch 先超时 15s，之后 RDS 和 SDS 再各等 15s。想缩短这段时间，要在 bootstrap 里显式设置 `initial_fetch_timeout`。缺失的 SDS 证书只让引用它的 filter chain 握手失败，不影响同一个 Envoy 里的其他 listener。这也是兜底链不能走 SDS 的证据：Entente 没起来时 SDS 证书拿不到，走 SDS 的兜底链也会握手失败。
