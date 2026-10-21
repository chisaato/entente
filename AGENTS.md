# Entente — notes for agents

Envoy control plane: Docker labels + static YAML → CDS/EDS/RDS over a Unix socket.
Read `docs/design.md` before changing behaviour; section 12 records decisions made during
implementation. Update that file when you make or change a design decision.

## Principles (from the design, non-negotiable)

- Explicit configuration, no implicit defaults. No default entrypoint; a route that is unclear is
  rejected, not guessed.
- Fail closed, per route: a bad route is dropped and logged; other routes are unaffected.
- No LDS. Listeners live in the Envoy bootstrap; entrypoint = RDS `route_config_name`.
- Security decisions never come from labels. `PolicyRef` is reserved and must be rejected while the
  policy layer does not exist.
- All label parsing goes through `internal/labels.Parse`. Never hard-code the label prefix elsewhere.

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

- go-control-plane's `SnapshotCache` must be created with `ads=false` (see design 12.2), and its
  `Snapshot.Consistent()` does not fit static listeners; use `xds.Resources.consistent`.
- Snapshot versions are content hashes; never use time.
