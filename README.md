# Entente

Envoy 的外挂控制平面。它读取 Docker 容器 label 和静态 YAML,通过 Unix socket 上的 xDS(CDS / EDS / RDS / SDS / LDS)
把路由和证书推给 Envoy。证书来自 OpenBao KV,或本地 PEM 文件。效果类似 Traefik 的 Docker provider:给容器贴上 label,路由就自动出现;数据面是 Envoy。

面向不跑 Kubernetes 的单机部署。设计见 [docs/adr/](docs/adr/)，定位见 [ADR-0001](docs/adr/0001-standalone-envoy-control-plane.md)。

## 分工

- **Envoy bootstrap(运维手写)**:配置 xDS 连接、admin、兜底证书和不交给 Entente 的静态 listener。
- **Entente**:每个声明的入口推送一份 RouteConfiguration,每个后端推送一个 EDS cluster,
  从 OpenBao KV v2 或本地 PEM 文件读取证书并通过 SDS 下发；还可通过受限 LDS 管理 YAML 显式声明的 TLS listener。

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
静态 listener 的 WebSocket 升级由 bootstrap HCM 的 `upgrade_configs` 配置；托管 TLS listener 用显式 `hcm.websocket` 配置。
`node_id`、`xds.*`、Docker 来源开关、`docker.host` 和 `openbao.*` 修改后需要重启。

`entente --config <file> --check` 只校验配置,不启动。`--config` 的短选项是 `-c`,`--log-level` 的短选项是 `-l`;`--check` 没有短选项。旧的单横线长名(`-config` 等)不再接受。

### h2c 与同域名路径路由

`h2c: true` 显式启用上游明文 HTTP/2（prior knowledge，不回退 HTTP/1.1）。它与 listener 的 `alpn: [h2, http/1.1]` 不同：前者控制到后端的连接，后者控制客户端到 Envoy 的 TLS 协商。

```yaml
static:
  backends:
    - {name: api, endpoints: ["127.0.0.1:8085"], h2c: true}
    - {name: login, endpoints: ["127.0.0.1:3006"]}
  routes:
    - {name: api-prefix, entrypoints: [https], host: sso.example.com, backend: api, path: /api/, path_type: prefix, rewrite: /, priority: 10, timeout: 0s}
    - {name: login, entrypoints: [https], host: sso.example.com, backend: login, path: /ui/v2/login, path_type: prefix, priority: 20}
    - {name: root, entrypoints: [https], host: sso.example.com, backend: login, path: /, path_type: exact, rewrite: /ui/v2/login/, priority: 30}
    - {name: fallback, entrypoints: [https], host: sso.example.com, backend: api, path: /, path_type: prefix, priority: 40, timeout: 0s}
```

`path` 与 `path_type` 必须成对；多路径路由显式声明唯一 `priority`，越小越先，不能依赖声明顺序。`rewrite` 是内部前缀替换，不产生 302：上述 `/api/users` 转为 `/users`，精确 `/` 转为 `/ui/v2/login/`；`/api` 不匹配 `/api/`。重复匹配条件或优先级拒绝，旧的无路径单后端配置保持兼容。路径路由仅由 YAML 声明，不支持 label。见 [ADR-0021](docs/adr/0021-explicit-h2c-and-static-path-routes.md)。

### 站点级响应压缩

仅在需要压缩的 `listeners[].tls_bindings[]` 上添加：

```yaml
compression:
  encodings: [zstd, gzip]
  min_length: 512
  content_types: [text/html, text/plain, text/css, application/javascript, application/json, image/svg+xml]
```

这些字段都要显式给出；编码顺序决定偏好，MIME 必须精确枚举，不支持 `text/*`。其他绑定与兜底链不受影响。同一证书可被多个绑定复用，以区分压缩站点与非压缩站点。这不保证与 Caddy 的通配 MIME 或所有编码细节完全一致。见 [ADR-0022](docs/adr/0022-tls-binding-response-compression.md)。

## SDS / OpenBao 证书

在配置中显式声明 `openbao` 和 `secrets`,并在 Envoy bootstrap 中用同名的
`tls_certificate_sds_secret_configs` 引用证书,见上述示例。除可选的 `openbao.ca_file` 外,
这些段的所有字段都必填。证书链与私钥来自 KV v2 字段(示例为 `fullchain` / `key`),
经同一条 ADS 流下发,Entente 不把它们落盘,Envoy 不需要挂载真证书目录。

Entente 每次请求前重新读取 token 文件,不实现登录;生产环境可由 bao agent 用 AppRole 登录并维护 token sink。
若 agent 通过替换文件更新 token,应挂载 sink 所在目录,避免单文件 bind mount 继续引用旧 inode。
首次拉取失败时不下发该 secret,对应 TLS 入口保持 warming;后续失败保留上一份有效证书并重试。
`secrets` 列表支持热重载,`openbao.*` 需要重启。完整语义见 [ADR-0014](docs/adr/0014-sds-reads-kv-v2-with-a-token-file.md) 与 [ADR-0015](docs/adr/0015-per-secret-certificate-failure.md)。

### 本地 PEM 文件

不使用 OpenBao 时,删掉 `openbao` 段,改为:

```yaml
secrets:
  - name: demo-cert
    file:
      certificate_chain: /etc/acme/demo.example.com/fullchain.pem
      private_key: /etc/acme/demo.example.com/key.pem
```

两个路径都必须是绝对路径。每个 secret 只能有一种来源:KV 字段和 `file` 不能同时写,名字也不能重复。
只有存在 KV 来源时才需要 `openbao`;配了 `openbao` 但一个 secret 都没有仍然是错误。

Entente 启动时立即读取,并监听这两个文件的父目录,因此 acme.sh 原地覆盖或先写临时文件再 rename 都会被看到。
证书和私钥必须成对通过校验后才下发;只更新了其中一个时保留上一份有效证书。目录或文件稍后出现会重试恢复,不依赖某一次文件系统事件。
内容不变不推新快照;从配置删除该 secret 会立刻撤销。私钥不会写入日志,也不会为此发起 HTTP 请求。
语义见 [ADR-0018](docs/adr/0018-local-pem-certificate-files.md)。

### SNI 兜底与自签证书

示例 bootstrap 只对 `whoami.example.com` 和 `legacy.example.com` 的 SNI 出示 SDS 真证书。
未知或缺失 SNI 走 `default_filter_chain`,用静态 secret `sink` 完成握手并返回 421。
兜底不依赖 Entente 或 OpenBao;匹配域名但真证书缺失时仍握手失败,不退到兜底。
详见 [ADR-0016](docs/adr/0016-sni-demultiplex-and-sink-chain-in-bootstrap.md)。

```sh
entente gen-cert --name sink                     # 输出可贴进 static_resources.secrets 的 YAML 列表项
entente gen-cert --name sink --out-dir ./sink    # 写入 tls.crt(0644)、tls.key(0600),拒绝覆盖
```

该子命令不读取 `--config`,不连接服务。`--name` 始终必填;默认 CN / DNS SAN 为 `invalid`,
ECDSA P-256 自签证书有效期默认 3650 天,可用 `--common-name` 和 `--days` 调整。
示例的真证书来自 SDS,仅兜底证书挂载 `examples/sink/` 文件。

只有 Envoy、控制平面还没连上时,`initial_fetch_timeout` 默认为 15s:CDS 先等 15s,RDS 和 SDS 再各等 15s,所以前约 30s 端口不监听。要缩短这段时间,在 bootstrap 里显式设置 `initial_fetch_timeout`。

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

取值规则:

- `enable` 只接受 `"true"` / `"false"`;缺失或 `"false"` 表示不暴露,其他值记错误
- `host` 必须是小写主机名,最多一个前导 `*.` 通配
- `port` 必填,不从 `EXPOSE` 推断
- `network`:只挂一张网络时可省略;挂多张时必填。容器在该网络上没有 IP(如 `host` 网络)时拒绝
- 只暴露状态为 `running` 的容器,`paused` 不暴露

以下情况会拒绝该路由并打日志,不影响其他路由:

- 已启用的容器上有未知的 `entente.*` label(比如拼错成 `entente.entrypoint`)
- 设置了 `entente.policy`:策略层尚未实现,静默忽略就等于 fail open
- 同一入口上有多个容器或无路径路由争用相同 host，或容器路由与该 host 的静态路径集合冲突

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
