#!/usr/bin/env bash
# 端到端检查:通过 examples/compose.yaml 拉起 Envoy + Entente + OpenBao + 容器。
#
#   test/e2e/run.sh
#
# E2E_COMPOSE_OVERRIDE 可以指定额外的 compose 文件(例如把 entente 镜像换成
# 本地编译的二进制)。E2E_KEEP=1 时结束后保留这套栈。
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"

compose=(docker compose -f examples/compose.yaml)
if [[ -n "${E2E_COMPOSE_OVERRIDE:-}" ]]; then
  compose+=(-f "$E2E_COMPOSE_OVERRIDE")
fi

cleanup() {
  if [[ "${E2E_KEEP:-}" != 1 ]]; then
    "${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

dump() {
  echo "--- openbao logs" >&2; "${compose[@]}" logs openbao >&2 || true
  echo "--- entente logs" >&2; "${compose[@]}" logs entente >&2 || true
  echo "--- envoy logs" >&2; "${compose[@]}" logs envoy | tail -40 >&2 || true
}

test/e2e/gencert.sh examples/certs
# 演示用 dev root token;生产环境应由 bao agent 等机制维护 token 文件。
(umask 077; printf '%s\n' entente-demo-token > examples/bao-token)
"${compose[@]}" build entente
mkdir -p examples/sink
if [[ ! -e examples/sink/tls.crt || ! -e examples/sink/tls.key ]]; then
  # compose run 同样遵循 E2E_COMPOSE_OVERRIDE 中的本地二进制挂载。
  "${compose[@]}" run --rm --no-deps --user "$(id -u):$(id -g)" \
    -v "$PWD/examples/sink:/out" entente gen-cert --name sink --out-dir /out
fi
# 仅用于演示:Envoy 的 uid 101 需要读取此私钥,生产应配置专用组权限。
chmod 0644 examples/sink/tls.key
"${compose[@]}" up -d --quiet-pull openbao
ready=0
for _ in $(seq 1 60); do
  if "${compose[@]}" exec -T openbao bao status -address=http://127.0.0.1:8200 >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.5
done
if [[ "$ready" != 1 ]]; then
  echo "FAIL OpenBao 未就绪" >&2
  dump
  exit 1
fi
# 先写入 KV,再启动 Entente,确保首次 SDS 拉取就能得到证书。
"${compose[@]}" exec -T -e BAO_TOKEN=entente-demo-token openbao bao kv put \
  -address=http://127.0.0.1:8200 secret/entente/demo \
  fullchain=@/certs/tls.crt key=@/certs/tls.key >/dev/null
"${compose[@]}" up -d --build --quiet-pull

# status <scheme> <host> <port>:打印 HTTP 状态码。
status() {
  curl -s -o /dev/null -w '%{http_code}' -k --max-time 3 --noproxy '*' \
    --resolve "$2:$3:127.0.0.1" "$1://$2:$3/" || true
}

# expect <code> <scheme> <host> <port>:最多等 30 秒,直到出现该状态码。
expect() {
  local want=$1 got=""
  for _ in $(seq 1 60); do
    got=$(status "$2" "$3" "$4")
    [[ "$got" == "$want" ]] && { echo "ok   $2://$3:$4 -> $got"; return 0; }
    sleep 0.5
  done
  echo "FAIL $2://$3:$4 -> $got, want $want" >&2
  dump
  exit 1
}

# Docker label,entrypoint 为 http
expect 200 http  whoami.example.com 8080
# Docker label,entrypoint 为 https
expect 200 https whoami.example.com 8443
# 静态路由
expect 200 https legacy.example.com 8443
# 确认 TLS 实际出示的是写入 KV、经 SDS 下发的证书。
expected_fingerprint=$(openssl x509 -in examples/certs/tls.crt -noout -fingerprint -sha256)
if ! actual_fingerprint=$(timeout 5 openssl s_client -connect 127.0.0.1:8443 \
  -servername legacy.example.com </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256); then
  echo "FAIL 无法读取 Envoy 的 TLS 证书" >&2
  dump
  exit 1
fi
if [[ "$actual_fingerprint" != "$expected_fingerprint" ]]; then
  echo "FAIL SDS 证书指纹不匹配: $actual_fingerprint, want $expected_fingerprint" >&2
  dump
  exit 1
fi
echo "ok   SDS 证书指纹与 KV 中的证书一致"

expect_sink_certificate() {
  local expected actual
  expected=$(openssl x509 -in examples/sink/tls.crt -noout -fingerprint -sha256)
  if ! actual=$(timeout 5 openssl s_client -connect 127.0.0.1:8443 "$@" \
    </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256); then
    echo "FAIL 无法读取兜底 TLS 证书 ($*)" >&2
    dump
    exit 1
  fi
  if [[ "$actual" != "$expected" ]]; then
    echo "FAIL 兜底证书指纹不匹配 ($*): $actual, want $expected" >&2
    dump
    exit 1
  fi
  echo "ok   兜底证书指纹一致 ($*)"
}
expect_sink_certificate -servername probe.invalid
expect_sink_certificate -noservername
expect 421 https probe.invalid 8443
# 静态路由只挂在 https
expect 404 http  legacy.example.com 8080
# 默认什么都不暴露
expect 404 http  unknown.example.com 8080

body=$(curl -sk --max-time 3 --noproxy '*' --resolve legacy.example.com:8443:127.0.0.1 https://legacy.example.com:8443/)
grep -q '^Name: legacy' <<<"$body" || { echo "FAIL legacy body: $body" >&2; exit 1; }
echo "ok   legacy body served by the static backend"

"${compose[@]}" stop whoami >/dev/null
# 容器消失后路由一并移除
expect 404 http whoami.example.com 8080

"${compose[@]}" start whoami >/dev/null
# 再恢复
expect 200 http whoami.example.com 8080

# 关闭控制平面后,静态兜底仍可握手和返回 421,旧动态配置仍可服务。
"${compose[@]}" stop entente >/dev/null
expect_sink_certificate -servername probe.invalid
expect_sink_certificate -noservername
expect 421 https probe.invalid 8443
expect 200 https legacy.example.com 8443

echo "e2e passed"
