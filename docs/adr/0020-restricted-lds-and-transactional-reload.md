---
Status: accepted
Supersedes: ADR-0004
---

# 受限 LDS 与事务式配置重载

仅 YAML 显式声明的 TLS listener 走 LDS,bootstrap 的其他静态 listener 可以共存。运维必须启用 ADS LDS,且同名 listener 不能同时存在于 bootstrap 与 LDS;Entente 不读取或验证 bootstrap,冲突由 Envoy NACK 报告。入口仍是 HCM 的 RDS `route_config_name`,不是监听端口或 listener 名。

listener 的 `name`、`address`(IP:port)、`entrypoint`、`hcm.websocket`、`hcm.strip_any_host_port`、`alpn`、`fallback`、`tls_bindings` 均显式声明。布尔选项使用指针区分省略和 false。`fallback.static_secret` 与 `fallback.secret` 二选一。TLS 绑定的 `name` 在 listener 内唯一,`server_names` 在 listener 内不重复,引用的入口和动态 secret 必须存在。安全配置不接受 label。

每个绑定生成一个具名 filter chain,按名字确定性排序,使用 tls_inspector 和 SDS,业务 HCM 使用 RDS。兜底链内联 421 路由。新增绑定不会改写其他 chain 的 proto。修改地址、入口、HCM、ALPN 或兜底时记录整个 listener drain 的 WARN。快照包含 CDS/EDS/RDS/SDS/LDS,版本仍为内容哈希。

候选准备是纯计算:输入配置、Docker 快照、证书材料和显式时间,输出资源与聚合报告。证书检查 key pair、有效期和 DNS SAN,临期七天警告。通配 SAN 只覆盖一级名称,通配路由必须有同样的通配绑定。所有生成的 proto 执行 ValidateAll,并检查内部引用。

YAML 候选完整准备成功才替换生效配置和快照,之后切换证书 watcher,提交时不重复读取材料。失败保留旧配置、旧任务和旧快照;获取材料的暂时性失败按 1s 到 30s 退避,更新 YAML 会替换旧候选。Docker 事件使用生效代且逐路由 fail closed;续期保存前校验生效绑定,失败保留每个 secret 的最后有效材料。

没有上一代时,获取失败的证书绑定暂不生成,避免 listener 因缺失 SDS 卡在 warming;托管兜底材料缺失时暂不生成整个 listener。材料恢复后重新生成。删除绑定与 secret 分两阶段:先发布移除 LDS 引用但保留旧 SDS 的快照,确认对应 LDS ACK 后才发布删除材料的快照。NACK 不自动回滚。

配置级路由错误在 YAML 候选和冷启动中拒绝整份,覆盖 ADR-0011 的静态配置部分;运行时 Docker 路由仍单独拒绝。失效或尚未生效证书拒绝使用,覆盖 ADR-0014 的仅警告行为。动态 listener 的冷启动降级覆盖 ADR-0015 的 warming 描述。静态 bootstrap listener 的依赖仍由运维负责。
