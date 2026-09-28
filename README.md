# Entente

Envoy 的外挂控制平面。它读取 Docker 容器 label 和静态 YAML,通过 Unix socket 上的 xDS(CDS / EDS / RDS)
把路由推给 Envoy。效果类似 Traefik 的 Docker provider:给容器贴上 label,路由就自动出现;数据面是 Envoy。

面向不跑 Kubernetes 的单机部署。设计见 [docs/design.md](docs/design.md)。

## 分工

- **Envoy bootstrap(运维手写)**:监听端口、TLS、filter chain。Entente 不使用 LDS,也不创建端口。
- **Entente**:每个声明的入口(即 bootstrap 里的 `rds.route_config_name`)推送一份 RouteConfiguration,
  每个后端推送一个 EDS cluster。

没有任何默认入口,也没有隐含默认值:容器不写 `entrypoints` 就不会被暴露,引用了未声明入口的路由会被拒绝。

## 快速开始

```sh
test/e2e/run.sh          # 需要 Docker 和 openssl;启动 examples/compose.yaml 并用 curl 验证
```

或者手动启动:

```sh
test/e2e/gencert.sh examples/certs
docker compose -f examples/compose.yaml up --build
curl --resolve whoami.example.com:8080:127.0.0.1 http://whoami.example.com:8080/
curl -k --resolve legacy.example.com:8443:127.0.0.1 https://legacy.example.com:8443/
```

## 配置

完整带注释的示例见 [`examples/entente.yaml`](examples/entente.yaml),配套的 Envoy bootstrap 见
[`examples/envoy.yaml`](examples/envoy.yaml)。

```yaml
node_id: envoy                    # 与 bootstrap 的 node.id 一致
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
`node_id`、`xds.*` 和 `docker.host` 修改后需要重启。

`entente -config <file> -check` 只校验配置,不启动。

## 容器 label

```yaml
labels:
  entente.enable: "true"
  entente.entrypoints: "http,https"   # 必填,没有默认值
  entente.host: "app.example.com"     # 必填
  entente.port: "8080"                # 必填,容器内端口
  entente.network: "edge"             # 容器挂在多张网络上时必填
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
| `internal/xds` | 模型 → CDS/EDS/RDS,基于 UDS 的 gRPC 服务 |
