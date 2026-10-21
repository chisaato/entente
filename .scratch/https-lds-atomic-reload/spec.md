# HTTPS 受限 LDS、事务式重载与分层校验

Status: ready-for-agent

## Problem Statement

运维要接入一个新域名(新证书来自 OpenBao KV 或本地文件,外加静态路由)时,目前要同时改三处:Envoy bootstrap 的 filter chain(`server_names` + SDS 引用)、Entente 配置(`secrets` + `static.routes`)、以及后端。改 bootstrap 就得重启 Envoy,在 Podman 容器里这意味着断连接。

同时,Entente 自己的热重载不是事务:

- 路由级错误只丢掉那一条路由,其余照常发布,半份配置就这样生效了。
- `config reloaded` 日志在资源生成、证书准备之前就打印,不代表 Envoy 已经生效。
- 新增证书首次读取失败时,照样会发布缺证书的快照。
- Envoy NACK 只记一条日志,没有关联到是哪一次发布。

`--check` 只做文件级校验,打印 `config OK` 并不能说明这份配置运行起来可用。

运维想要 nginx / Caddy 那样的体验:先校验,通过了才生效,失败就保持旧配置不动;生产环境里全部自动完成,不需要人工介入。

另外,隧道类后端(例如 gost 的公共入口)要按多个域名挂静态路由,但静态 backend 只接受 IP 字面量,容器 IP 又是动态的。

## Solution

1. **HTTPS listener 改走 LDS(受限)**。按 nginx / Traefik / Caddy 的分工:端口绑定是静态配置,"SNI → 证书 / vhost"是可热加载的配置。运维在 Entente YAML 里显式声明 listener 的骨架(地址、对应入口、HCM 选项、兜底证书)和 **TLS 绑定**(`server_names → secret`)。Entente 据此生成 Listener,经 ADS 下发。每个 TLS 绑定生成一条独立的 filter chain,新增绑定只会新增一条 chain,不打断已有连接。HTTP 等其他 listener 可以继续留在 bootstrap 里。
2. **事务式重载**。每次 YAML 变化都产生一个"候选"。候选必须完整准备好才能原子地替换当前生效的那一代,包括拉取并校验全部证书材料、交叉引用和 SAN 覆盖检查、生成并校验全部 xDS 资源。任何一步失败,整份候选就被拒绝,生效中的那一代保持不变,失败原因写进日志。
3. **分层校验**。`--check` 和热重载走同一条候选准备流程,区别只在数据来源。默认离线;显式加参数才联网拉 OpenBao 证书和 Docker 快照。没有检查的项目要在输出里明确标出,不再无条件打印 `config OK`。
4. **静态 backend 的 Docker 来源**。静态 backend 可以按容器名、网络和端口引用一个 Docker 容器,由 Docker watcher 解析出 IP,再经 EDS 下发。
5. **发布可观测**。每一代都有 generation id(内容哈希)。按 stream、type URL、nonce 关联 ACK / NACK,并在日志里给出明确的阶段。

## User Stories

1. 作为运维,我想在 Entente YAML 里新增一个 TLS 绑定和对应 secret,保存后自动生效,这样接入新的根域名时不用改 bootstrap,也不用重启 Envoy。
2. 作为运维,我想让新增一个域名不打断其他域名上已有的长连接(WebSocket、gost 隧道控制连接),这样改配置不影响在线业务。
3. 作为运维,我想让 YAML 里任何一处错误都使整份修改不生效,并在日志里看到全部错误,这样不会出现半份配置。
4. 作为运维,我想在新 secret 证书拉不到(OpenBao 不可达、文件不存在、PEM 坏了、私钥不匹配)时整份修改不生效,这样不会发布一个握手必然失败的域名。
5. 作为运维,我想让 Entente 在证书 SAN 不覆盖 TLS 绑定的某个 `server_names` 时拒绝候选,这样配错证书在发布前就会被发现。
6. 作为运维,我想让已过期或尚未生效的证书使候选被拒绝,7 天内过期的只打 WARN,这样不会上线一张已经失效的证书。
7. 作为运维,我想让候选因为暂时性故障(例如 OpenBao 不可达)被拒绝后,Entente 自动退避重试同一份候选,直到成功或 YAML 再次变化,这样故障恢复后无需人工介入。
8. 作为运维,我想让生效中的这一代在候选失败期间继续工作,包括已有证书的续期,这样一次失败的修改不会冻结其他自动化。
9. 作为运维,我想让 `entente --check` 默认离线运行,能在 CI 里校验配置文件本身、静态路由、交叉引用和本地证书文件。
10. 作为运维,我想用一个显式参数让 `--check` 联网拉 OpenBao 证书并读一次 Docker 快照,做和生产重载同等的完整检查。
11. 作为运维,我想在 `--check` 的输出里看到哪些项目已检查、哪些未检查,这样不会把"没检查"误当成"通过"。
12. 作为运维,我想让 `--check` 用退出码区分通过、配置无效和参数错误,这样可以把它接进脚本和 CI。
13. 作为运维,我想在日志里看到每一代配置的 generation id,以及候选收到、校验失败(附原因)、已提交、已发布、各资源类型的 ACK / NACK 这几个阶段,这样我能确认改动到底有没有在 Envoy 上生效。
14. 作为运维,我想在 Envoy NACK 时看到被拒绝的 generation id、资源类型和 Envoy 给出的错误详情,这样能定位问题。
15. 作为运维,我想让未知 SNI 和不带 SNI 的连接继续落到兜底链(静态 sink 证书 + 421),这样防 SNI 探测的行为不变。
16. 作为运维,我想让兜底证书可以继续用 bootstrap 里的静态 secret,这样 Entente 拿不到证书时兜底链也不依赖它。
17. 作为运维,我想在 listener 上显式写出 WebSocket 升级、去掉 Host 端口等 HCM 选项,这样 Entente 生成的 HCM 不含隐含默认值,行为和今天手写的 bootstrap 一致。
18. 作为运维,我想让 Docker 来源的路由挂在 LDS 管理的 TLS 入口上时,如果 host 不在任何 TLS 绑定的 `server_names` 覆盖范围内,就只拒绝这一条路由,这样一个容器贴错 label 不会影响其他路由。
19. 作为运维,我想让静态路由出现同样的问题时整份 YAML 被拒绝,这样静态配置保持原子性。
20. 作为运维,我想让通配路由 host(`*.example.com`)只能被同样通配的绑定覆盖,不能被某个具体子域覆盖,这样覆盖检查不会误放行。
21. 作为运维,我想让静态 backend 写 `docker: {container, network, port}` 来引用容器,这样 gost 之类的容器可以被多条静态路由按多个域名引用,不必固定 IP。
22. 作为运维,我想让 Docker 来源的静态 backend 在容器没运行或在指定网络上没有 IP 时,只拒绝引用它的路由,并在容器出现后自动恢复,这样 YAML 是否有效不取决于容器此刻的运行状态。
23. 作为运维,我想让没启用 Docker 来源时出现 Docker 型静态 backend 被当作文件级错误,这样不会静默失效。
24. 作为运维,我想让 Entente 冷启动时,某个绑定的证书首次拉取失败只让这一个绑定暂不下发(它的 SNI 落到兜底链)并打 ERROR,其他绑定照常上线,证书拿到后自动补上,这样一张证书出问题不会让整个 HTTPS listener 卡在 warming。
25. 作为运维,我想让 Entente 永远不下发引用了快照里不存在的 secret 的 listener,这样 Envoy 不会因为缺 SDS 而让整个 listener 卡在 warming。
26. 作为运维,我想让已有证书的续期(内容变化)继续走 per-secret 的"保留最后一份有效证书"语义,并且同样做 SAN 和有效期校验,这样续期失败不会中断正在工作的 TLS。
27. 作为运维,我想让修改 listener 级别的字段(地址、HCM 选项、兜底)时,在日志里明确提示这会让整个 listener drain,这样我知道这类改动会断开已有连接。
28. 作为运维,我想让 YAML 里声明的 listener 和 bootstrap 里的 listener 在名字上不冲突,冲突时在启动日志里能看到(Envoy 会 NACK),这样迁移时不会两边各起一个。
29. 作为开发者,我想让 `--check` 和热重载调用同一个候选准备函数,这样两条路径的校验结果永远一致。
30. 作为开发者,我想让候选准备是纯计算(输入配置、Docker 快照、证书材料,输出资源集和报告),这样大部分行为都能用单元测试覆盖。

## Implementation Decisions

### 推翻"不使用 LDS"(受限)

- 只有在 Entente YAML 里声明的 listener 走 LDS,用于需要按 SNI 动态增减证书的 TLS 入口。bootstrap 仍然可以有静态 listener(例如 HTTP),两者共存。
- bootstrap 需要加 `lds_config: { ads: {} }`。同一个 listener 不能既写在 `static_resources` 里又经 LDS 下发,因为静态 listener 不能被 LDS 修改。
- 入口模型不变:listener 的 HCM 依旧用 RDS `route_config_name` 等于入口名;`entrypoints` 仍然要显式声明,listener 引用的入口必须已声明。
- label 不能创建或修改 listener、TLS 绑定和证书引用。这些都属于运维在 YAML 里持有的安全配置。
- 需要同步修订:`AGENTS.md` 的 "No LDS" 原则,`docs/design.md` 第 2、3、7 节和 12.10,并新增一个 12.x 小节记录本次决定和理由(对照 nginx / Traefik / Caddy 的分工、filter-chain-only update 行为、冷启动不再有 421 兜底的代价)。

### Listener 与 TLS 绑定的配置模型

- 顶层新增 listener 列表。每个 listener 显式声明:名字、监听地址、对应入口、HCM 选项(WebSocket 升级开关、是否去掉 Host 端口)、ALPN、兜底证书来源,以及 TLS 绑定列表。这些字段都必填,没有默认值。
- 兜底证书来源二选一,用不同的键区分:引用 bootstrap 静态 secret 的名字(Entente 无法校验其内容,在 `--check` 输出里标为未检查),或者引用 Entente 管理的 secret。
- TLS 绑定:名字、`server_names` 列表、secret 名。名字在整个 listener 内唯一,并用作 filter chain 名。`server_names` 允许精确名和一个前导 `*.` 的通配,规则复用现有的 host 校验函数。同一 listener 内的 `server_names` 不允许重复。
- 生成 Listener 时 listener filter 固定是 `tls_inspector`。每个绑定一条 filter chain(`server_names` 匹配、DownstreamTlsContext 引用该 secret 的 SDS、HCM 走 RDS)。`default_filter_chain` 是兜底证书加内联 route config,对所有请求 `direct_response` 421。
- filter chain 的顺序和内容要确定,以保证"只新增绑定"是 filter-chain-only update;快照版本仍然是内容哈希。

### 候选准备(主接缝)

- 输入:解析后的配置、Docker 容器快照(可以为空,表示未检查)、证书材料视图(每个 secret 的 PEM 或获取失败的原因)。
- 输出:完整的 xDS 资源集(CDS / EDS / RDS / SDS / LDS)和校验报告;或者拒绝,附带全部错误。
- 校验内容:
  - 现有 `Build` 的全部规则。
  - 交叉引用:绑定引用的 secret 存在;listener 引用的入口已声明。
  - 证书:通过 key pair 校验;已过期或尚未生效即为错误;7 天内过期为警告。
  - SAN 覆盖:每个绑定的 `server_names` 都被其证书的 DNS SAN 覆盖(精确匹配,或一级通配)。
  - 路由覆盖:LDS 管理的入口上,路由 host 必须被该 listener 某个绑定的 `server_names` 覆盖;通配 host 只能被同样的通配绑定覆盖。
  - 对生成的每个 proto 调用 `ValidateAll`,再做内部一致性检查。
- 错误分级:
  - YAML 触发的候选:**任何错误**(包括静态路由级错误)都导致整份拒绝。
  - Docker 事件:基于当前生效的 YAML 那一代重新生成,Docker 路由仍然按单条路由 fail closed。
  - 证书续期:按单个 secret 保留最后一份有效证书,不影响其他资源。

### 事务式重载与生成代

- Reconciler 持有"当前生效的那一代"(配置、证书材料、资源集、generation id)。YAML 变化时构造候选:先为新出现或换了来源的 secret 拉取材料,再调用候选准备;全部通过后才原子替换生效的那一代,然后才变更 secret watcher 集合。
- 候选失败:生效代不变,旧 secret watcher 不取消,快照不覆盖,打 ERROR 并列出全部原因。如果失败原因是暂时性的(证书获取失败),就按退避重试同一份候选;YAML 再次变化时丢弃旧候选。
- 异步回调(证书刷新、Docker 同步)要带 generation id,防止上一个候选的结果覆盖新状态。校验和提交使用同一份内存数据,不重复读取(避免 TOCTOU)。
- 需要重启才能改的字段(`node_id`、`xds.*`、Docker 开关、`docker.host`、`openbao.*`)维持现状:拒绝并提示需要重启。
- 冷启动时没有上一代:YAML 无效则退出。证书首次失败时按单个绑定降级:该绑定不进 listener,ERROR,证书拿到后补上。listener 绝不引用快照中不存在的 secret。
- 修改 listener 级字段时,打 WARN 说明整个 listener 会 drain。

### `--check` 分层

- 默认离线:配置文件、候选准备(Docker 快照为空)、本地文件证书(读取并完整校验)。KV 证书和 Docker 路由标为未检查。
- 联网参数(名字在实现时确定,只用 pflag 原生写法,不引入新的默认值):额外拉取 OpenBao KV 证书,读一次 Docker 容器列表,并把结果送进同一个候选准备函数。
- 输出分别列出已检查、未检查和警告;只有全部检查项都通过才算成功。退出码:0 通过,1 配置或检查失败,2 参数错误。

### 静态 backend 的 Docker 来源

- 静态 backend 二选一:`endpoints`(IP:port 字面量,现状)或 `docker`(容器名、网络、端口,三项都必填)。两个都写或都不写是文件级错误;没启用 Docker 来源却出现 `docker` 也是文件级错误。
- 解析交给 Docker watcher 的快照:容器必须是 running,而且在指定网络上有 IP。解析失败时,引用它的路由按单条路由拒绝;容器出现后自动恢复。这属于运行时状态,不影响 YAML 候选是否有效。
- cluster 命名沿用 `static/<name>`,仍走 EDS。

### 发布可观测

- 日志阶段:`candidate_received`、`validation_failed`(附原因)、`committed`、`published`、每种资源类型的 `ack` / `nack`,都带 generation id。
- ACK / NACK 关联 stream、type URL、response nonce 和发送的版本。NACK 请求里的 `version_info` 是 Envoy 此前接受的版本,不能当成被拒的版本。
- NACK 时打 ERROR(包含 Envoy 的 error_detail)。Envoy 会保留之前接受的资源;Entente 不自动回滚快照。
- 现有的 `config reloaded` 日志改为只在提交成功后打印。

## Testing Decisions

- 好的测试只断言外部行为:给定配置、Docker 快照和证书材料,断言产出的资源或拒绝原因;不测内部数据结构。
- **第一层:候选准备(主接缝,绝大多数用例)**。在现有 `reconcile` Build 测试和 `xds` translate 测试的基础上扩展。覆盖:TLS 绑定生成的 filter chain 和兜底链、SAN 覆盖(精确、通配、一级通配不跨两级)、路由覆盖(通配只能被通配覆盖)、过期 / 未生效 / 临期、key pair 不匹配、缺失 secret、YAML 任何错误都整份拒绝、Docker 路由单条拒绝、Docker 型静态 backend 的解析和缺失、冷启动时证书缺失的绑定降级、新增绑定只新增 chain(其余 chain 的 proto 不变)。证书用测试里现场生成的自签证书,参考现有 gencert / secrets 测试的做法。
- **第二层:真实 UDS 上的 xDS 往返**。在现有 xDS server 测试上扩展:LDS 资源下发、候选失败时快照版本不变、ACK / NACK 和 nonce 的关联、NACK 的日志内容。
- **第三层:e2e(真 Envoy,现有 `test/e2e`)**:运行中新增 TLS 绑定和 secret 后,新域名可以访问;新增期间一条已建立的长连接不断;坏 YAML 时旧配置保持可用;未知 SNI 和无 SNI 返回 421;新 secret 拉取失败时整份修改不生效。
- `--check` 测试:离线模式下 KV 证书和 Docker 标为未检查;退出码三种情况;本地证书 SAN 不覆盖时失败。
- 只在最后集中跑一次全量回归(`go test ./...`、`go vet ./...`、e2e)。

## Out of Scope

- 只读的 status / metrics HTTP 端点(仍是第 9 节的后续阶段)。本次只做日志可观测。
- NACK 后自动回滚到上一个已 ACK 的快照。
- 增量 xDS(Delta)和 Envoy 的 on-demand 证书选择器。
- 由 Entente 校验 bootstrap 文件、或调用 `envoy --mode validate`。
- XFF、`use_remote_address` 等 HCM 的真实 IP 相关选项。
- TCP / SNI 透传路由(`tcp_proxy`)。
- 把 tx-gz、station-gdfs 的线上部署迁移到新模型(实现完成后另行处理)。
- gost 配置的生成或管理。

## Further Notes

- 来源:本次对话中的 gost 内网穿透讨论(tx-gz:Envoy 按 SNI 分流,HTTP 族流量经静态路由送到 gost 公共入口),以及 oracle 对热加载方案的调研(Envoy 1.39 多证书选择规则、filter-chain-only update、ECDS 和 on-demand 证书选择器的边界)。
- 代价已确认:Entente 没起来时,冷启动的 Envoy 上 LDS 管理的端口不监听(连接被拒绝),不再有 421 兜底。今天 Entente 不在时真证书本来也握手失败。
- 需要在 e2e 里实测确认:LDS listener 的 filter chain 引用缺失 SDS secret 时,是否让整个 listener 卡在 warming;filter-chain-only update 时,已有连接在未变的 chain 上确实保留。
- SotW 下从快照删除 secret 不等于 Envoy 撤销证书。删除绑定时,先从 listener 移除引用,再移除 secret(make-before-break 的反向)。
