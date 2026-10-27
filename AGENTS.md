# Entente — notes for agents

Envoy control plane: Docker labels + static YAML → CDS/EDS/RDS over a Unix socket.
改行为前读 `GLOSSARY.md` 和 `docs/adr/`；做出或改变设计决定时新增 ADR。

## Principles (from the design, non-negotiable)

- Explicit configuration, no implicit defaults. No default entrypoint; a route that is unclear is
  rejected, not guessed. (ADR-0002)
- Fail closed, per route: a bad route is dropped and logged; other routes are unaffected. (ADR-0011)
- Restricted LDS: only YAML-declared TLS listeners are managed dynamically; other listeners stay in bootstrap. Entrypoint = RDS `route_config_name`. Labels never configure TLS bindings. (ADR-0020, ADR-0003)
- Security decisions never come from labels. `PolicyRef` is reserved and must be rejected while the
  policy layer does not exist. (ADR-0006)
- All label parsing goes through `internal/labels.Parse`. Never hard-code the label prefix elsewhere. (ADR-0007)

## Commands

```sh
go test ./...              # unit tests, including an xDS round trip over a real UDS
go vet ./...
test/e2e/run.sh            # docker compose: Envoy + entente + containers, checked with curl
```

`test/e2e/run.sh` builds the image from `Dockerfile`. If `go mod download` cannot reach the network
inside `docker build`, build the binary on the host and pass a compose override via
`E2E_COMPOSE_OVERRIDE` that mounts it into `gcr.io/distroless/static-debian12`.

## Gotchas

- go-control-plane's `SnapshotCache` must be created with `ads=false` (ADR-0009), and its
  `Snapshot.Consistent()` does not fit static listeners; use `xds.Resources.consistent`.
- Snapshot versions are content hashes; never use time. (ADR-0008)
- `github.com/docker/docker` 停在 v28，不再更新；Docker 客户端用 `github.com/moby/moby/client`。
- `go-control-plane` v0.14 要求 Go 1.25。
- 资源命名：Route / Cluster 名为 `docker/<容器名>`、`static/<name>`；virtual host 名等于 route 名。每个生成的 cluster 显式设置 `connect_timeout: 5s` 和 `ROUND_ROBIN`，不依赖 Envoy 的内置默认值。

## Agent skills

### Issue tracker

Local markdown: issues live under `.scratch/<feature>/`. See `docs/agents/issue-tracker.md`.

### Triage labels

The five default roles (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`), recorded on
each issue's `Status:` line. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context；`GLOSSARY.md` + `docs/adr/`。See `docs/agents/domain.md`.
