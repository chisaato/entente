---
Status: accepted
---

# 显式 h2c 后端与静态路径路由

迁移单机开发站点时，Zitadel 同一域名需要按路径访问 HTTP 登录页或 h2c API，且去前缀和内部改写不能变成客户端重定向。选择扩展现有 Backend / Route，而不是在 Envoy bootstrap 旁路维护另一套路由或继续串联旧网关。

静态后端通过 `h2c: true` 显式选择 HTTP/2 prior knowledge，不加 TLS，也不自动回退 HTTP/1.1；省略保持既有 HTTP/1.1 行为。静态 Docker 后端也可声明此传输约定，不增加协议 label。

静态路由使用成对的 `path` / `path_type`（`prefix` 或 `exact`）和可选的绝对路径 `rewrite`。路径改写只在转发内部生效，不产生重定向。同一入口与 host 的多路径路由必须显式声明互不重复的 `priority`，数值越小越先匹配；重复匹配条件或优先级拒绝，不能依靠 YAML 顺序、map 顺序或最长前缀猜测意图。它们下发为一个 virtual host 内的多条有序路由。

未声明路径的旧配置保持全路径单后端语义。容器 label 不增加路径或优先级能力，也不能与静态路径集合争夺同一 host。保留 ADR-0020 的校验边界：非法 YAML 候选整体拒绝并保留旧代；运行时 Docker 路由冲突逐组 fail closed，不选隐含赢家。每条路由的显式超时及 `0s` 不限时语义不变。
