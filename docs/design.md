# Entente 设计文档

> 来源:claude.ai 上的 Entente 设计文档 Artifact(由前序对话整理)。本文件是它在仓库里的副本,
> 并在第 12 节追加了实现过程中做出的决定。两者冲突时以本文件为准,改设计请直接改这里。

读作 `/ɑ̃.tɑ̃t/`(法语),英语常读 `/ɒnˈtɒnt/`,中文近似"昂-唐特"。Envoy 的外挂控制平面,
前序对话里的说明用语是 "envoy docker provider"。

标记说明:

- **[已定]** 用户明确决定过
- **[建议]** 方向已认可,细节可调整
- **[待核实]** 凭记忆写的,必须对照当前官方文档确认

## 1. 目标与定位

一个**独立部署的 Envoy 外挂控制平面**。它监听 Docker 事件,读取容器 label,动态生成 Envoy 的
xDS 配置,效果类似 Traefik Docker provider 的"贴 label 自动出现路由",数据面换成 Envoy。
Docker 只是其中一个配置来源,YAML 静态配置是另一个(见第 4、8 节),所以项目名保持中立。

- **[已定]** 作为独立组件,不与 HimariConnect 或任何具体平台强耦合。
- **[已定]** 使用场景是资源受限的单机(如腾讯云),不安装 K3S / Kubernetes。
- **[已定]** 用 Go 实现,基于 `go-control-plane`。
- **[已定]** 不做 WebUI。配置纯声明式(YAML),变更走 Git。需要可视化时,只做只读的 status / metrics 端点。
- **[已定]** 哲学:upstream-first、声明式、**明确配置,不要隐含默认值**,宁可麻烦也要看得清楚。

### 第一版的非目标

- 内网穿透 / 隧道层(gost 等)。这是另一条线,不是本项目重点。
- 任何带参数的中间件和鉴权策略(OAuth、ext_authz 等)。第一版只做反代基础配置。
- SDS / OpenBao 证书对接,见第 9 节。(已提前实现 KV 读取,见 12.8;本地 PEM 文件来源见 12.12。)
- 增量 xDS。单机规模用全量 snapshot 即可。

## 2. 总体架构

```
Docker Events ─┐
               ├─> Reconciler ─> 内部统一模型 ─> SnapshotCache ─(xDS over UDS)─> Envoy
YAML (fsnotify)┘
```

Envoy **先以静态 bootstrap 启动**,之后才连接到本控制平面。控制平面**不负责创建监听端口**。

### Envoy 与控制平面的分工 [已定]

| 内容 | 由谁管 | 方式 |
|---|---|---|
| 监听端口(80 / 443 / 8443 等)、TLS、filter chain 结构 | 运维事先写好 | bootstrap 的 `static_resources.listeners` |
| 路由(host / path → backend) | 控制平面 | RDS |
| 后端 Cluster、Endpoint | 控制平面 | CDS + EDS |
| Listener 的动态变更 | 不做 | **不使用 LDS**。改 filter chain 有连接中断风险,常驻端口本来也不需要动态 |

### 传输 [已定]

xDS 走 **Unix Domain Socket**,Envoy 与控制平面同机。权限边界即信任边界(0600 + 同 owner / group),
不叠 mTLS 或 token。

## 3. 入口(Entrypoint)模型 [已定]

> 这是与 Traefik 最大的设计差异。Traefik 由配置决定"监听哪些端口"(entrypoints `web` / `websecure`
> 带默认值)。本项目抛弃这一点:监听器全在 Envoy bootstrap 里,控制平面只通过 **RDS 名字**把路由挂上去。

**接入点是 `route_config_name`,不是 Listener 的 `name`。** bootstrap 里每个 Listener 的
HttpConnectionManager 这样写:

```yaml
rds:
  route_config_name: "https"     # 自己起的名字,控制平面推送同名 RouteConfiguration
  config_source:
    # 指向 bootstrap 里静态定义的、连到控制平面 UDS 的 xDS cluster
    ...
```

控制平面 YAML 里**显式声明**自己管理哪些入口:

```yaml
entrypoints:
  - https
  - https-alt
  - http
```

### 规则

- 不存在任何默认入口。容器 label 不写入口,就**不暴露**。
- label 引用了 YAML 里没声明的入口,拒绝该路由并记录日志(fail closed)。
- 控制平面不探测 Envoy 里实际有哪些 Listener。bootstrap 与 YAML 的名字是否一致由运维保证。这是"明确配置"的代价,已接受。
- **每个声明的入口,即使没有任何路由,也必须推送一份 RouteConfiguration(空 virtual_hosts)。**
  否则引用了 RDS 的 Listener 会在等待首份路由配置时处于 warming。
  **[待核实]** `initial_fetch_timeout` 超时后的具体行为(印象是继续启动并返回 404),以官方文档为准。
  (实现后已不依赖该行为,见 12.3。)
- 同一 host 可挂在多个入口:往多份 RouteConfiguration 里各加一个 virtual host,互不影响。

## 4. 内部统一模型 [建议]

模型借鉴 Kubernetes Gateway API 的**分层与所有权划分**,不引入 CRD,不依赖 Kubernetes。

| 内部对象 | 对应的 Gateway API 概念 | 来源 |
|---|---|---|
| `Entrypoint` | Gateway / Listener(仅取"入口"含义) | YAML 声明 + bootstrap |
| `Route` | HTTPRoute | Docker label,或 YAML 静态声明 |
| `Backend` | backendRef / Service | Docker 容器信息,或 YAML 静态地址 |
| `PolicyRef`(预留) | Policy attachment(`targetRef`) | label 只引用名字 |
| `Policy`(预留,第一版不实现) | Policy,如 Envoy Gateway 的 SecurityPolicy | YAML |

### 所有权原则

- Route 层(label)只描述"我是谁、后端在哪、挂哪个入口"。
- 安全相关决策(鉴权、放行例外)属于 Policy 层,由运维在 YAML 中持有,**不允许在 label 里写**。
- 因此 `auth bypass` 之类的例外将来是**策略参数**(如 `protect` 中间件的 `bypass_paths`),不是 label。
- 第一版:`Route` 上预留空的 `PolicyRef` 字段。reconciler 遇到非空 `PolicyRef` 时**报错并拒绝该路由**,
  不要静默忽略,否则等于 fail open。

将来如果加纯转换类的 Filter(rewrite、加 header),才可以做成 Traefik 风格的 label 中间件。
涉及安全的东西不放进 label。

## 5. Label 约定 [已定] [建议]

- **[已定]** **前缀可配置**,配置项 `docker.label_prefix`,做成"藏得比较深"的选项,让用户选自己喜欢的前缀。
- **[建议]** 默认前缀 `entente`,与项目名一致(Traefik 的做法也是如此)。
  **所有 label 解析必须经过同一个函数(`internal/labels.Parse`),禁止在其他地方硬编码前缀字符串。**
- 前缀不匹配的容器一律静默忽略(不暴露),方向是安全的。
- 同一 Docker 主机可跑多个实例,各用不同前缀,互不干扰。

label 语义(前缀记作 `<prefix>`):

```
<prefix>.enable       = "true"
<prefix>.entrypoints  = "https"              # 无默认,必须显式写;逗号分隔
<prefix>.host         = "app.example.com"
<prefix>.port         = "8080"               # 容器内端口
<prefix>.network      = "<docker network>"   # 容器挂在多张网络上时必填
<prefix>.timeout      = "60s"                # 可选,非负 Go duration;0s 表示不限时
# 预留:<prefix>.policy = "<name>"  第一版遇到即拒绝
```

path 匹配等在第一版保持最小:host 匹配加 `/` 前缀。是否需要更多 label 由实现者按需提出,
但**不要引入隐含默认值**。

## 6. xDS 实现要点

- 使用 `SnapshotCache`,每次 reconcile 后生成新 snapshot。
- **snapshot version 用内容 hash 或单调递增计数器,不要用 wall clock**,避免时钟回拨导致 Envoy 认为版本未变。
- 资源:CDS(每个 Backend 一个 Cluster)、EDS(容器 IP:port)、RDS(每个 Entrypoint 一份)。
- Cluster 类型倾向 EDS 而不是 STRICT_DNS,这样容器 IP 变化只需推 EDS。
- 一致性校验:reconcile 时做一次完整校验。任何一条 Route 校验失败只影响它自己(拒绝并记录日志),不影响其他路由。
- 需要考虑:Docker 事件风暴时的合并 / 防抖;控制平面重启后 Envoy 重连并收到全量 snapshot 的行为。

## 7. 与 Envoy 配套的 bootstrap

第一版交付物包含一份**最小可运行的 bootstrap 示例**(`examples/envoy.yaml`):

- 一个静态 xDS cluster,指向控制平面的 UDS(需 HTTP/2)。
- `dynamic_resources`:CDS 走该 cluster,不配 LDS。
- 至少两个示例 Listener(http、https),各引用一个 `route_config_name`。
- 示例证书用文件方式加载,SDS 是后续阶段。

## 8. 第一版(MVP)范围与顺序

1. Docker watcher(容器 start / stop / die / label 变化)→ 内部模型(`Entrypoint` / `Route` / `Backend`)。
   同时 YAML 也能声明静态 `Route` 和 `Backend`(指向非 Docker 的地址),与 Docker 来源走同一条 Reconciler 路径。
   证书第一版维持 bootstrap 里文件加载。
2. CDS + EDS + RDS 推送(含空 RouteConfiguration),配套最小 bootstrap 示例。
3. YAML 配置读取(`entrypoints`、`label_prefix`、UDS 路径),fsnotify 热重载。
4. 一致性校验与 fail closed 行为、日志。
5. 基本测试:用 `docker compose` 起一个 Envoy + 控制平面 + 若干示例容器,curl 验证。

## 9. 后续阶段(不在第一版)

- 具名 Policy 层与 `protect` 之类的带参数中间件(ext_authz + OAuth / OIDC,例如对接 Zitadel、oauth2-proxy)。bypass 作为策略参数。
- SDS:让控制平面直接从 OpenBao 读证书,经 gRPC SDS 推给 Envoy,不落盘。原则:控制平面**只消费一个 token 文件**
  (由 bao agent 或用户自己的机制维护),不实现任何登录方式。
- 只读的 status / metrics 端点。
- 隧道集成(gost 等)。这是另一条线,届时作为普通 upstream Cluster 接入即可。

## 10. 需要自行调研的外部参考

请用搜索 / 抓取工具**获取最新文档**,不要依赖本文档或记忆。

- **Traefik**:Docker provider、entrypoints、label 语法、middleware、file provider 的多来源限制。
  这是我们要解决的痛点:各 provider 相互隔离,无法合并。
- **Kubernetes Gateway API**:Gateway / HTTPRoute 的分层与角色划分、`allowedRoutes`、Policy attachment
  (`targetRef`、default / override 语义)、引用解析失败时的 status 行为。同时确认其外部认证相关能力的当前状态。**[待核实]**
- **Envoy Gateway**(CNCF):`SecurityPolicy` 的设计,作为 Policy 层的参考。
- **Envoy 官方文档**:xDS 协议(state-of-the-world)、RDS 与 `initial_fetch_timeout`、Listener warming、
  bootstrap 的 `dynamic_resources` 写法。
- **go-control-plane**:`SnapshotCache` 用法、示例 server、UDS 上的 gRPC 监听。
- **kolumbus**(dkfbasel/kolumbus):早期的 Envoy + Docker label 控制平面,可作参考实现,注意其维护状态。
- **EnRoute**(saarasio/enroute):基于 Envoy 的网关,带 standalone Docker 部署和 ACME。README 称 2025 年起
  开源版不再更新,可作参考,**命名上要避开 `enroute` 及近似写法**。
- **Envoy Gateway 的 standalone 模式**:官方简介写着 "Standalone or Kubernetes-based"。请确认它的单机模式
  能否通过 Docker 或文件发现后端,评估是否与本项目重复。**[待核实]**

## 11. 待定事项

- path 匹配、多端口容器、健康检查等 label 的细节。
- 第一版是否需要 HTTP→HTTPS 重定向等常见模式(倾向留给后续的 Filter 层)。
- 控制平面部署形态(容器 / systemd),以及与 Envoy 共享 UDS 的挂载方式。
  (`examples/compose.yaml` 给了一种容器形态:共享 named volume + `user: "0:101"` + `socket_mode: "0660"`。)
- 同一入口上多个路由声明同一 host 时的处理。当前实现是**全部拒绝**(见 12.5);如果将来需要支持
  `docker compose --scale` 式的多副本,要另行设计显式的合并规则。

## 12. 实现记录

MVP 实现过程中做出的决定。标 **[建议]** 的是实现者的判断,可以推翻。

### 12.1 依赖与版本

- Go 1.25(`go-control-plane` v0.14 要求)。
- `github.com/envoyproxy/go-control-plane` v0.14.0 + `.../envoy` v1.39.0(拆分后的新模块)。
- Docker 客户端用 `github.com/moby/moby/client`(`github.com/docker/docker` 已停在 v28,不再更新)。
- 配置解析 `go.yaml.in/yaml/v3`,热重载 `github.com/fsnotify/fsnotify`。
- 命令行入口使用 `github.com/spf13/cobra`,只接受 pflag 原生写法。长选项是 `--config`(必填,无默认路径;短选项 `-c`)、`--log-level`(默认 `info`;短选项 `-l`)、`--check`(默认 false,无短选项)。不保留旧的单横线长名(`-config` 等),也不注册 `completion` 子命令,短选项不引入新的默认值。
- 端到端测试用 `envoyproxy/envoy:v1.39-latest`(实测 1.39.1)。

### 12.2 所有动态资源走 ADS **[建议]**

bootstrap 用一条 ADS 流承载 CDS / EDS / RDS(`ads_config` + 各处 `ads: {}`),只有一个静态 xDS cluster。
好处是只有一条连接,Envoy 侧有 CDS→EDS→RDS 的顺序保证。

但 `SnapshotCache` 以 **`ads=false`** 创建。go-control-plane 在 `ads=true` 时,只有 Envoy 请求的 RDS 名字
覆盖快照里**全部** RouteConfiguration 才会响应。于是只要 YAML 多声明了一个 bootstrap 里没有的入口,
**所有**入口的路由都会卡住。`ads=false` 时按请求的名字取子集返回,互不影响。

### 12.3 未声明入口:推空 RouteConfiguration **[建议]**

控制平面记录 Envoy 通过 RDS 请求过的所有 `route_config_name`。对其中**没有**在 YAML 里声明的名字,
推送一份空的 RouteConfiguration(返回 404),并打 WARN 日志。原因:

- SotW 下 RDS 不会因为资源从响应里消失就删除它。如果从 YAML 删掉一个入口,对应 Listener 会保留旧路由,
  而旧路由指向的 cluster 已被 CDS 删除,结果是 **503**。现在会变成 404,方向是 fail closed。
- bootstrap 里引用了未声明入口的 Listener 不会停在 warming,也就不再依赖 `initial_fetch_timeout` 的行为。

这只让未声明的入口"什么都不暴露",没有引入任何路由上的默认值。

### 12.4 Label 细节 **[建议]**

- `enable` 只接受 `"true"` / `"false"`。缺失或 `"false"` 视为不暴露;其他值记错误。
- 已启用的容器上出现**未知的 `<prefix>.*` label 时拒绝该容器**,防止 `entrypoint`(少了 s)之类的拼写错误被静默忽略。
- `host` 必须是小写主机名,允许一个前导 `*.` 通配。
- `port` 必填,不从 EXPOSE 推断。
- `timeout` 可选,必须是非负 Go duration;非法值拒绝该容器路由,省略时继承顶层 `route_timeout`。
- `network`:容器只挂一张网络时可省略;挂多张时必填,否则拒绝。容器在该网络上没有 IP(如 `host` 网络)时拒绝。
- 只看状态为 `running` 的容器,`paused` 的不暴露。

### 12.5 校验与冲突 **[建议]**

- 文件级错误(缺必填项、未知 key、名字非法、静态 endpoint 不是 `IP:port`)让**整份配置**被拒绝;
  热重载时保留旧配置。
- 路由级错误(未声明的入口、不存在的 backend、非空 policy、Docker 网络问题)只拒绝该路由。
- 同一入口上两个及以上路由声明同一 host 时,**全部拒绝**并在日志里互相点名。挑一个"赢家"本身就是隐含规则。
  另外,Envoy 也会整份 NACK 含重复 domain 的 RouteConfiguration。
- 静态 backend 只接受 IP 字面量:它们走 EDS,EDS 里是地址不是域名。需要 DNS 的后端将来另开 cluster 类型。

### 12.6 命名

- Route / Cluster 名:`docker/<容器名>`、`static/<name>`。Virtual host 名等于 route 名。
- 每个生成的 cluster 显式设置 `connect_timeout: 5s`、`ROUND_ROBIN`,不依赖 Envoy 的内置默认值。

### 12.7 运行时行为

- **Docker watcher 是 level-triggered 的**:相关事件只标记"脏",250ms 防抖后做一次完整 `ContainerList`。
  先订阅事件再 list,不会漏事件。事件流断开时按 1s→30s 退避重连,期间保留最后一次成功同步的结果。
- **启用了 Docker 来源时,在第一次同步完成前不发布快照**。否则控制平面重启后会先推一份不含 Docker 路由的快照。
  Envoy 在这期间继续使用旧配置(实测重启控制平面期间 40/40 请求返回 200)。
- **snapshot version** 是全部资源确定性序列化后的 sha256 前 16 位。内容不变则不重发;重启后生成的版本与 Envoy 已有的一致。
- **热重载**:`entrypoints`、`route_timeout`、`static`、`docker.label_prefix` 可热更新;`node_id`、`xds.*`、Docker 来源的开关和
  `docker.host` 需要重启,改了会被拒绝并打日志。
- 发现 NACK(Envoy 拒绝配置)和 node id 不匹配时打日志。
- UDS 在 umask 0177 下创建,再 chmod 成 `socket_mode`,不会短暂出现比配置更宽的权限。
  启动时只会删除遗留的 socket 文件,路径上是普通文件时拒绝启动。

### 12.8 SDS:从 OpenBao KV 读证书 **[建议]**

第 9 节的 SDS 提前实现,只做 **KV v2 读取**,形态对照运维现有的 bao agent 用法
(agent 用 AppRole 自动登录并把 token 写到 sink 文件,证书以 `fullchain` / `key` 两个字段存在 KV 里)。

- **认证**:Entente 只读一个 token 文件(`openbao.token_file`),每次请求前重新读取,以跟上 agent 的续期 / 重登。
  不实现任何登录方式。请求头 `X-Vault-Token`。
- **读取**:`GET {address}/v1/{kv_mount}/data/{kv_path}`,取 `.data.data[certificate_field]` 和
  `.data.data[private_key_field]`。只用标准库 `net/http`,不引入 OpenBao / Vault SDK。
  `openbao.ca_file` 可选,缺省时用系统根证书。单次请求超时是代码里的常量 10s。
- **配置全部显式**:`openbao.address`、`openbao.token_file`、`openbao.refresh_interval`,以及每个 KV secret 的
  `name`、`kv_mount`、`kv_path`、`certificate_field`、`private_key_field` 都是必填,没有默认值。
  有 KV 来源必须有 `openbao`;有 `openbao` 但没有任何 secret 也是配置错误。
  只从本地文件读证书时不要配置 `openbao`,见 12.12。
- **下发**:每个 secret 生成一个 `envoy.extensions.transport_sockets.tls.v3.Secret`(`tls_certificate`,
  证书链与私钥用 `inline_bytes`),名字等于配置里的 `name`,和其他资源走同一条 ADS 流。
  bootstrap 用 `sds_config: { ads: {} }` 按名字引用。证书只在内存里,不落盘;私钥绝不写日志。
- **校验**:拉到的内容必须通过 `tls.X509KeyPair`(PEM 能解析,私钥与证书匹配),否则视为拉取失败。
  证书已过期或 7 天内过期时打 WARN。
- **失败处理(per secret,fail closed)**:
  - 从未成功拉到过:快照里没有这个 secret。引用它的 Listener / filter chain 在 Envoy 侧保持 warming,不对外提供 TLS。
  - 之前成功过、这次失败:**保留上一份有效内容**并打 ERROR。证书还没过期时,丢掉它只会让正在工作的 TLS 中断。
  - 一个 secret 失败不影响其他 secret 和路由。
- **刷新**:成功后按 `refresh_interval` 轮询(KV v2 没有推送)。失败后按 1s→`refresh_interval` 退避重试。
  内容不变时快照版本不变(版本哈希包含 secret),不会重复推送。
- **启动**:与 Docker 一样,配置了 secrets 时,要等每个 secret 都完成第一次拉取(成功或失败)后才发布快照,
  避免控制平面重启后先推一份缺证书的快照。
- **热重载**:`secrets` 列表可热更新(新增立即拉取,删除即从快照移除);`openbao.*` 需要重启。

### 12.9 启动顺序与证书缺失(实测,Envoy 1.39)

socket 由 Entente 监听,Envoy 是 gRPC 客户端,连不上会自己退避重连,所以两者的启动顺序无关。
用 `examples/` 这套栈实测(Envoy 先起,`initial_fetch_timeout` 用默认值 15s):

| 阶段 | http(无 TLS) | https(SDS 证书) |
|---|---|---|
| 只有 Envoy,0–30s | 端口未监听 | 端口未监听 |
| 只有 Envoy,30s 后 | 404 | 握手失败 |
| Entente 已起,KV 里没有证书 | 约 3s 后 200 | 握手失败 |
| 写入 KV 后 | 200 | 约 9s 后 200(等 Entente 的重试退避) |

- 只有 Envoy 时,前 30s 端口不监听:CDS 的 initial fetch 先超时 15s,之后 RDS 和 SDS 再各等 15s。
  想缩短这段时间,要在 bootstrap 里显式设置 `initial_fetch_timeout`。
- 缺失的 SDS 证书只让引用它的 filter chain 握手失败,不影响同一个 Envoy 里的其他 listener。

### 12.10 SNI 分流与兜底证书 **[建议]**

目标:防 SNI 探测(扫描器拿不到真证书和它的 SAN),以及兜底(控制平面或 OpenBao 不可用时,443 仍能完成握手,
返回明确的错误,而不是连不上)。

- **全部在 Envoy bootstrap 里做,Entente 不参与**(listener 是静态的,见第 2 节):
  - 真证书:每条 filter chain 用 `filter_chain_match.server_names` 匹配域名,证书走 SDS(12.8)。
  - 兜底:`default_filter_chain` 接住 SNI 不匹配和不带 SNI 的连接。它的证书是 bootstrap 里的**静态 secret**,
    HCM 用内联 `route_config`,对所有请求 `direct_response` 返回 **421 Misdirected Request**。
    兜底链不依赖 Entente、不依赖 OpenBao,Envoy 一监听就能用。
- **兜底证书不走 SDS**:12.9 实测表明,Entente 没起来时 SDS 证书拿不到,走 SDS 的兜底链也会握手失败。
- 真证书缺失时**不会**退到兜底证书:那条 filter chain 自己握手失败(fail closed),不掩盖故障。
- **`entente gen-cert` 子命令**生成兜底用的随机自签证书,省去手写 openssl 命令:
  - ECDSA P-256 私钥,序列号随机。CN 和 SAN 都是 `--common-name`,默认 `invalid`
    (RFC 6761 保留域名,不会撞上真实域名)。有效期 `--days`,默认 3650。
    这里有默认值:证书本身不承载任何安全或路由决策,"明确配置"原则针对的是路由和安全。
  - 默认向 stdout 输出一段可直接贴进 bootstrap `static_resources.secrets` 的 YAML(`inline_string`),
    secret 名由必填的 `--name` 指定。
  - `--out-dir <dir>` 改为写 `tls.crt`(0644)和 `tls.key`(0600)两个 PEM 文件,目标文件已存在则拒绝覆盖。
  - 不需要 `--config`,不连接任何服务。

### 12.11 路由超时 **[建议]**

- 顶层 `route_timeout` 必填,使用非负 Go duration 字符串,不沿用 Envoy 隐式的 15s:
  隐含超时违背明确配置原则,也可能截断 Vaultwarden 大附件等耗时请求。
- 优先级:静态路由 `static.routes[].timeout` 或 Docker `<prefix>.timeout` 覆盖顶层 `route_timeout`;
  只有省略覆盖值才继承。`0s` 表示不限时,始终显式下发 Envoy RouteAction 的 `timeout`,包括零值。
- YAML 中缺失顶层值或任意超时非法属于文件级错误;label 超时非法只拒绝该容器路由。
  顶层值与静态覆盖值均支持热重载,内容变化自然改变快照哈希。
- WebSocket 由 bootstrap HCM 的 `upgrade_configs` 负责,Entente 不管理升级配置。
- 超时属于路由行为而非安全决策,因此允许通过 label 配置,不受 Policy 所有权限制。

### 12.12 SDS:从本地 PEM 文件读证书 **[已定]**

给不跑 OpenBao 的部署用,例如 acme.sh 把 `fullchain.pem` / `key.pem` 写到磁盘,Entente 读入内存后经同一条 ADS SDS 流推给 Envoy。
文件来源不发 HTTP,也不连接 OpenBao。KV 来源的行为仍是 12.8,两种来源可以列在同一份 `secrets` 里,但每个 secret 只能有一种。

```yaml
secrets:
  - name: demo-cert
    file:
      certificate_chain: /etc/acme/demo.example.com/fullchain.pem
      private_key: /etc/acme/demo.example.com/key.pem
```

- **来源互斥**:原有 KV 字段保持不变。`file` 与任一 KV 字段同时出现则拒绝;两个都没有也拒绝。
  `name` 在整份列表里唯一,和来源无关。
- **路径必填且必须是绝对路径**。相对路径会跟着进程工作目录变,等于隐含配置。
  不在配置里写轮询间隔或防抖时间;下面的时间是代码常量,和 OpenBao 的 10s 请求超时同一类。
- **OpenBao**:只有存在 KV 来源时才必填。纯 file 配置不创建 HTTP 客户端。
  配了 `openbao` 但一个 secret 都没有,仍然是配置错误。`openbao.*` 继续需要重启;文件路径跟 `secrets` 列表一样热重载。
- **读取**:启动后立刻读,不等文件系统事件。监听的是两个文件的父目录,所以原地覆盖和
  `写入临时文件再 rename` 都能看见。同一目录只监听一次。相关事件按 250ms 合并后再读,避免证书链和私钥
  各写一次就读到半对新。
- **不能只等一个事件**:父目录还不存在时 `inotify` 无事可看。读取或监听失败后按 1s→30s 退避重试,
  目录或文件后来出现就会补上监听并重新读取。监听正常时每 30s 也重读一次,防止目录被整个替换后盯着旧 inode。
- **校验和失败(per secret,与 12.8 相同)**:两个文件都要能通过 `tls.X509KeyPair` 才成对下发。
  只换了证书、私钥还是旧的,视为失败。首次失败不下发;已有一份有效证书时保留它。一个 secret 失败不影响其他 secret。
  日志只写 secret 名、路径和固定错误,不写 PEM。
- **版本**:证书链或私钥字节变了,快照哈希才变。文件时间戳变了但内容相同,不推新版本。
  从配置里删掉该 secret 会立刻从快照移除,不等待文件删除。
- **热重载路径**:同名 secret 换路径时,新文件还没读成功就继续下发上一份有效内容。每个 secret 一个 goroutine,
  取消配置即关闭它的 watcher。
