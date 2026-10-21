---
Status: accepted
---

# 内部模型借鉴 Gateway API 的分层，安全决策不进 label

内部模型借鉴 Kubernetes Gateway API 的分层与所有权，不引入 CRD，也不依赖 Kubernetes。对象是 Entrypoint、Route、Backend，以及预留的 PolicyRef 和 Policy。Route 层（label 或静态声明）只描述「我是谁、后端在哪、挂哪个入口」。鉴权、放行例外等安全决策属于 Policy 层，由运维在 YAML 里持有，不允许写在 label 里。因此 `auth bypass` 之类的例外将来是策略参数，不是 label。

第一版 Policy 层不实现。`Route` 上预留空的 `PolicyRef`；reconciler 遇到非空 `PolicyRef` 必须报错并拒绝该路由，不能静默忽略，否则等于 fail open。纯转换类 Filter（rewrite、加 header）将来可以做成 label 中间件；涉及安全的东西不放进 label。
