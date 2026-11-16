---
Status: accepted
Extends: ADR-0020
---

# listener 支持 ipv4_compat 双栈监听

listener 可显式声明 `ipv4_compat: true`（布尔指针，区分省略与 false）。仅对 IPv6 地址合法：地址是 IPv4 或 IPv4-mapped 以外的形态时，文件级校验直接拒绝（"ipv4_compat can only be enabled for IPv6 addresses"）。省略时保持现状语义——Envoy 对 IPv6 监听地址默认 `IPV6_V6ONLY=1`，纯 IPv6。

## 背景

容器平台（Podman 6 / Netavark 2.1）在 `enable_ipv6: true` 的桥接网络上会为同一 `ports:` 声明同时生成 IPv4 与 IPv6 两条 DNAT：v4 流量 DNAT 到容器 v4 地址，v6 流量 DNAT 到容器 v6 地址。容器内的 listener socket 必须是 dual-stack（`::` + `IPV6_V6ONLY=0`）才能同时接收两条路径；此前 listener 只能绑 `0.0.0.0`（纯 v4）或 `::`（纯 v6），无法双栈，逼出过 socat 旁路容器。

Envoy 的 `SocketAddress.ipv4_compat` 正是这个开关：`protobufAddressToAddress` 把它转成 `v6only=!ipv4_compat`，socket 创建时据此设置 `IPV6_V6ONLY`。Entente 只需透传：`translateListeners` 把 `*bool` 拷入 `SocketAddress.Ipv4Compat`。

## 决定

- 字段名与 Envoy proto 一致：`ipv4_compat`。安全语义（TLS 绑定、fallback）不变，属于 listener 级配置。
- 默认省略 = false = 现状。不为 IPv4 地址开启（校验拒绝），不猜。

## 运维注意（实测）

Envoy 1.39 对已运行 listener 的 `ipv4_compat` 变化：LDS 会 ACK（配置已更新，config_dump 可见 `ipv4_compat: true`），但既有 listen socket 的 v6only 状态不随 drain 重建——`::` + compat 的 socket 在热更新后仍拒绝 IPv4。对 Envoy 容器做冷重启后 socket 按新配置创建，v4/v6 同时生效。部署时：改 `ipv4_compat` 后重启 Envoy 容器。

## Consequences

- `tx-gz` 的 entente 栈：listener `address: "[::]:8443"` + `ipv4_compat: true`，删除 socat（`entente-ipv6-https`）旁路容器，443/8443/7785 经 Netavark 双栈 DNAT 直接对外。
- listener 级 drain 的 WARN 规则（ADR-0020）同样适用于本字段变化。
