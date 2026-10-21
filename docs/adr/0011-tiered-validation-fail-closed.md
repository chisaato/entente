---
Status: accepted
---

# 校验分级：文件级拒绝整份，路由级只拒绝该路由

文件级错误（缺必填项、未知 key、名字非法、静态 endpoint 不是 `IP:port`）拒绝整份配置；热重载时保留旧配置。路由级错误（未声明的入口、不存在的 backend、非空 policy、Docker 网络问题）只拒绝该路由并记日志，不影响其他路由。同一入口上两个及以上路由声明同一 host 时，全部拒绝，并在日志里互相点名——挑一个「赢家」本身就是隐含规则；Envoy 也会整份 NACK 含重复 domain 的 RouteConfiguration。
