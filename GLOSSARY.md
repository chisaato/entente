# Entente

Envoy 的外挂控制平面。它把 Docker 容器 label 和 YAML 静态配置收成同一套路由模型，再经 xDS 推给同机的 Envoy，供不跑 Kubernetes 的单机使用。

## Language

### 路由模型

**入口（Entrypoint）**:
控制平面承诺为其推送路由的一个名字，与 Envoy bootstrap 里某个 HttpConnectionManager 的 `rds.route_config_name` 相同。它不是 Listener 的名字，也不代表控制平面在监听端口。
_Avoid_: Listener、端口、默认入口

**路由（Route）**:
把一个 host 挂到一个或多个入口、并指向一个后端的声明。来源是容器 label，或 YAML 里的静态声明。
_Avoid_: virtual host（那是下发给 Envoy 的表示）、规则、ingress

**后端（Backend）**:
路由指向的上游。来源是一个 Docker 容器，或 YAML 里写明的静态地址。
_Avoid_: Service、upstream、Cluster（Cluster 是下发给 Envoy 的表示）

**策略（Policy）**:
运维在 YAML 里持有的安全决策，例如鉴权和放行例外。Label 不能表达它。第一版没有实现。
_Avoid_: 中间件（指安全类）、auth label、annotation

**策略引用（PolicyRef）**:
路由上指向某个具名策略的名字。空引用表示这条路由不附带策略。
_Avoid_: `<prefix>.policy` 当作已生效的配置、targetRef

### 配置来源

**label 前缀**:
Docker label 上、用来区分本控制平面实例的那一段名字，默认 `entente`，可以改。前缀对不上的 label 不属于本实例。
_Avoid_: 写死的 `entente.`、provider 名

**静态来源**:
YAML 里直接声明的路由、后端和入口，不依赖 Docker。
_Avoid_: file provider（把它当成与 Docker 隔离的另一套配置）

**Docker 来源**:
从本机 Docker 容器的 label 和网络信息得到的路由与后端。可以整段关掉。
_Avoid_: Docker provider（把它当成唯一来源，或当成与静态配置互斥的来源）

### xDS

**快照（Snapshot）**:
控制平面一次交给 Envoy 的全量 xDS 资源集合。它的版本只由内容决定。
_Avoid_: 增量 xDS、用时间戳当版本

**证书 secret**:
一份可下发给 Envoy 的 TLS 证书材料，由名字标识。材料来自 KV，或来自本地 PEM 文件，二者择一。
_Avoid_: 证书文件（下发前不落盘）、把 OpenBao 路径当作 Envoy 侧的名字

**兜底链（sink）**:
接住未知 SNI 和不带 SNI 的连接的那条 filter chain。它用 bootstrap 里的静态证书完成握手，并返回明确错误；它不是真证书缺失时的退路。
_Avoid_: 默认证书、fallback 证书（指真证书缺失时自动换上）

**TLS 绑定**:
一个 listener 上「一组 server name → 一个证书 secret」的对应关系。安全相关，只由运维在 YAML 里声明。
_Avoid_: 用 label 声明证书或 SNI、把绑定当成路由

**候选**:
一次 YAML 变更准备取代当前生效配置的那一整份提案。它要么整份成为新的生效配置，要么整份被拒绝。
_Avoid_: 热重载中间态、部分生效的配置

**生成代**:
某一时刻正在生效的那一整份配置，用内容哈希标识。候选只有在准备完成并通过之后才会取代它。
_Avoid_: 修订号、用时间区分的版本
