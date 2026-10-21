# Entente

Envoy 的外挂控制平面。它读取 Docker 容器 label 和静态 YAML,通过 Unix socket 上的 xDS(CDS / EDS / RDS / SDS)
把路由和 OpenBao 中的证书推给 Envoy。效果类似 Traefik 的 Docker provider:给容器贴上 label,路由就自动出现;数据面是 Envoy。

面向不跑 Kubernetes 的单机部署。设计见 [docs/design.md](docs/design.md)。

## 分工

- **Envoy bootstrap(运维手写)**:监听端口、TLS、filter chain。Entente 不使用 LDS,也不创建端口。
- **Entente**:每个声明的入口(即 bootstrap 里的 `rds.route_config_name`)推送一份 RouteConfiguration,
  每个后端推送一个 EDS cluster,从 OpenBao KV v2 读取证书并通过 SDS 下发。

没有任何默认入口,也没有隐含默认值:容器不写 `entrypoints` 就不会被暴露,引用了未声明入口的路由会被拒绝。

## 快速开始

```sh
test/e2e/run.sh          # 需要 Docker、curl、openssl 和 timeout;启动示例并验证
```

或者手动启动(仅供演示的 OpenBao dev 模式,固定 root token 不可用于生产):

```sh
test/e2e/gencert.sh examples/certs
docker compose -f examples/compose.yaml build entente
mkdir -p examples/sink
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/examples/sink:/out" entente:dev gen-cert --name sink --out-dir /out
# 仅用于演示,允许 Envoy 的 uid 101 读取;生产请配置专用组权限。
chmod 0644 examples/sink/tls.key
(umask 077; printf '%s\n' entente-demo-token > examples/bao-token)
docker compose -f examples/compose.yaml up -d openbao
# 等到 status 成功,再写入证书并启动其余服务。
docker compose -f examples/compose.yaml exec -T openbao bao status -address=http://127.0.0.1:8200
docker compose -f examples/compose.yaml exec -T -e BAO_TOKEN=entente-demo-token openbao bao kv put -address=http://127.0.0.1:8200 secret/entente/demo fullchain=@/certs/tls.crt key=@/certs/tls.key
docker compose -f examples/compose.yaml up -d --build
curl --resolve whoami.example.com:8080:127.0.0.1 http://whoami.example.com:8080/
curl -k --resolve legacy.example.com:8443:127.0.0.1 https://legacy.example.com:8443/
```

## 配置

完整带注释的示例见 [`examples/entente.yaml`](examples/entente.yaml),配套的 Envoy bootstrap 见
[`examples/envoy.yaml`](examples/envoy.yaml)。

```yaml
node_id: envoy                    # 与 bootstrap 的 node.id 一致
route_timeout: 60s                # 必填,非负 Go duration;0s 表示不限时
xds:
  socket: /run/entente/xds.sock
  socket_mode: "0660"             # 可选,默认 "0600"
entrypoints: [http, https]        # 与 bootstrap 的 route_config_name 一致
docker:                           # 整段删掉即关闭 Docker 来源
  host: unix:///var/run/docker.sock
  # label_prefix: entente         # 可选
static:
  backends:
    - { name: legacy, endpoints: ["10.0.0.5:8080"] }
  routes:
    - { name: legacy, entrypoints: [https], host: legacy.example.com, backend: legacy }
```

未知字段会被拒绝。配置文件变化时自动热重载;新配置无效时继续使用旧配置,并打错误日志。
`route_timeout` 支持热重载;静态路由的 `timeout` 或容器 label 的 `timeout` 可覆盖它,
省略覆盖值才继承顶层值,显式 `0s` 表示不限时。非法超时会拒绝整份 YAML 或对应容器路由。
WebSocket 升级由 Envoy bootstrap HCM 的 `upgrade_configs` 配置,Entente 不管理它。
`node_id`、`xds.*`、Docker 来源开关、`docker.host` 和 `openbao.*` 修改后需要重启。

`entente --config <file> --check` 只校验配置,不启动。`--config` 的短选项是 `-c`,`--log-level` 的短选项是 `-l`;`--check` 没有短选项。旧的单横线长名(`-config` 等)不再接受。

## SDS / OpenBao 证书

在配置中显式声明 `openbao` 和 `secrets`,并在 Envoy bootstrap 中用同名的
`tls_certificate_sds_secret_configs` 引用证书,见上述示例。除可选的 `openbao.ca_file` 外,
这些段的所有字段都必填。证书链与私钥来自 KV v2 字段(示例为 `fullchain` / `key`),
经同一条 ADS 流下发,Entente 不把它们落盘,Envoy 不需要挂载真证书目录。

Entente 每次请求前重新读取 token 文件,不实现登录;生产环境可由 bao agent 用 AppRole 登录并维护 token sink。
若 agent 通过替换文件更新 token,应挂载 sink 所在目录,避免单文件 bind mount 继续引用旧 inode。
首次拉取失败时不下发该 secret,对应 TLS 入口保持 warming;后续失败保留上一份有效证书并重试。
`secrets` 列表支持热重载,`openbao.*` 需要重启。完整语义见[设计文档 12.8 节](docs/design.md#128-sds从-openbao-kv-读证书)。

### SNI 兜底与自签证书

示例 bootstrap 只对 `whoami.example.com` 和 `legacy.example.com` 的 SNI 出示 SDS 真证书。
未知或缺失 SNI 走 `default_filter_chain`,用静态 secret `sink` 完成握手并返回 421。
兜底不依赖 Entente 或 OpenBao;匹配域名但真证书缺失时仍握手失败,不退到兜底。
详见[设计文档 12.10 节](docs/design.md#1210-sni-分流与兜底证书-建议)。

```sh
entente gen-cert --name sink                     # 输出可贴进 static_resources.secrets 的 YAML 列表项
entente gen-cert --name sink --out-dir ./sink    # 写入 tls.crt(0644)、tls.key(0600),拒绝覆盖
```

该子命令不读取 `--config`,不连接服务。`--name` 始终必填;默认 CN / DNS SAN 为 `invalid`,
ECDSA P-256 自签证书有效期默认 3650 天,可用 `--common-name` 和 `--days` 调整。
示例的真证书来自 SDS,仅兜底证书挂载 `examples/sink/` 文件。

## 容器 label

```yaml
labels:
  entente.enable: "true"
  entente.entrypoints: "http,https"   # 必填,没有默认值
  entente.host: "app.example.com"     # 必填
  entente.port: "8080"                # 必填,容器内端口
  entente.network: "edge"             # 容器挂在多张网络上时必填
  entente.timeout: "0s"               # 可选,覆盖 route_timeout;0s 表示不限时
```

以下情况会拒绝该路由并打日志,不影响其他路由:

- 已启用的容器上有未知的 `entente.*` label(比如拼错成 `entente.entrypoint`)
- 设置了 `entente.policy`:策略层尚未实现,静默忽略就等于 fail open
- 同一入口上已有别的路由声明了相同的 host

## 开发

```sh
go test ./...
go vet ./...
go build ./cmd/entente
```

代码结构:

| 包 | 职责 |
|---|---|
| `cmd/entente` | 入口、组装各组件 |
| `internal/config` | YAML 加载、校验、热重载 |
| `internal/labels` | 唯一的 label 解析入口 |
| `internal/docker` | Docker 事件监听 + 防抖后全量 list |
| `internal/model` | 与来源无关的 Entrypoint / Route / Backend 模型 |
| `internal/reconcile` | 合并各来源 → 校验(fail closed)→ 发布快照 |
| `internal/xds` | 模型 → CDS/EDS/RDS/SDS,基于 UDS 的 gRPC 服务 |
