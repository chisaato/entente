Status: needs-triage

# 具名 Policy 层与 protect 中间件

来源：原设计文档第 4 节与第 9 节（文件已删除，原文在 git 历史）。

策略层第一版只预留了 `PolicyRef`，非空即拒绝路由（ADR-0006）。尚未实现具名 Policy：由运维在 YAML 中持有，例如 `protect` 这类带参数的中间件，背后是 ext_authz 加 OAuth / OIDC（对接 Zitadel、oauth2-proxy 等）。bypass 是策略参数（如 `protect` 的 `bypass_paths`），不是 label。
