#!/usr/bin/env bash
# 独立 LDS/事务重载回归。只清理本次进程和容器,不修改 examples 配置。
# 依赖 Docker、Go、Python 3、curl、openssl;Envoy 使用 host 网络访问宿主测试后端。
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
image=${E2E_ENVOY_IMAGE:-envoyproxy/envoy:v1.39-latest}
port=${E2E_LDS_PORT:-18444}
backend_port=${E2E_BACKEND_PORT:-18081}
docker info >/dev/null
for candidate in "$port" "$backend_port"; do
  if ss -ltn | grep -Eq ":${candidate}([^0-9]|$)"; then
    echo "FAIL 端口 $candidate 已占用,拒绝复用其他服务" >&2
    exit 1
  fi
done
work=$(mktemp -d /tmp/opencode/entente-lds.XXXXXX)
entente_pid=""; backend_pid=""; connection_pid=""; envoy_id=""
cleanup() {
  local result=$?
  for pid in "$connection_pid" "$entente_pid" "$backend_pid"; do
    if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  done
  if [[ "$result" != 0 ]]; then
    cat "$work/entente.log" >&2 2>/dev/null || true
    [[ -z "$envoy_id" ]] || docker logs "$envoy_id" >&2 2>/dev/null || true
    cat "$work/connection.log" >&2 2>/dev/null || true
  fi
  [[ -z "$envoy_id" ]] || docker rm -f "$envoy_id" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$work/run" "$work/sink"
# 仅 socket、bootstrap 与兜底证书需要容器 uid 101 读取。
chmod 0755 "$work" "$work/run" "$work/sink"
go build -o "$work/entente" "$root/cmd/entente"
for name in old new sink; do
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
    -subj "/CN=${name}.example.com" -addext "subjectAltName=DNS:${name}.example.com" \
    -keyout "$work/$name.key" -out "$work/$name.crt" >/dev/null 2>&1
done
cp "$work/sink.crt" "$work/sink/tls.crt"
cp "$work/sink.key" "$work/sink/tls.key"
chmod 0644 "$work/sink/"*

# 长连接客户端始终复用同一个 TLS socket,绝不重连;每次请求都必须完整返回。
cat >"$work/connection.py" <<'PY'
import http.client, pathlib, ssl, sys, time
work, port = pathlib.Path(sys.argv[1]), int(sys.argv[2])
ctx = ssl._create_unverified_context()
connection = http.client.HTTPSConnection("127.0.0.1", port, context=ctx, timeout=5)
import socket
connection.sock = ctx.wrap_socket(socket.create_connection(("127.0.0.1", port), timeout=5), server_hostname="old.example.com")
original = connection.sock
count = 0
while True:
    connection.request("GET", "/", headers={"Host": "old.example.com"})
    response = connection.getresponse()
    assert response.status == 200 and response.read() == b"backend ok\n"
    assert connection.sock is original, "TLS connection was replaced"
    count += 1
    (work / "connection.ready").touch()
    if (work / "connection.stop").exists():
        break
    time.sleep(0.1)
assert count >= 2
print(f"same TLS socket survived {count} requests", flush=True)
connection.close()
PY
python3 -u - "$backend_port" >"$work/backend.log" 2>&1 <<'PY' &
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import sys
class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def do_GET(self):
        body = b"backend ok\n"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
PY
backend_pid=$!

write_config() {
  local include_new=$1 broken_secret=${2:-false}
  cat >"$work/config.next" <<EOF
node_id: lds-e2e
route_timeout: 0s
xds: {socket: $work/run/xds.sock, socket_mode: "0666"}
entrypoints: [https]
secrets:
  - name: old
    file: {certificate_chain: $work/old.crt, private_key: $work/old.key}
EOF
  if [[ "$include_new" == true ]]; then
    local cert="$work/new.crt"
    [[ "$broken_secret" == false ]] || cert="$work/missing.crt"
    cat >>"$work/config.next" <<EOF
  - name: new
    file: {certificate_chain: $cert, private_key: $work/new.key}
EOF
  fi
  cat >>"$work/config.next" <<EOF
listeners:
  - name: tls
    address: 127.0.0.1:$port
    entrypoint: https
    hcm: {websocket: true, strip_any_host_port: true}
    alpn: [http/1.1]
    fallback: {static_secret: sink}
    tls_bindings:
      - {name: old, server_names: [old.example.com], secret: old}
EOF
  if [[ "$include_new" == true ]]; then
    echo '      - {name: new, server_names: [new.example.com], secret: new}' >>"$work/config.next"
  fi
  cat >>"$work/config.next" <<EOF
static:
  backends:
    - {name: backend, endpoints: ["127.0.0.1:$backend_port"]}
  routes:
    - {name: old, host: old.example.com, entrypoints: [https], backend: backend}
EOF
  if [[ "$include_new" == true ]]; then
    echo '    - {name: new, host: new.example.com, entrypoints: [https], backend: backend}' >>"$work/config.next"
  fi
  mv "$work/config.next" "$work/entente.yaml"
}
write_config false
cat >"$work/envoy.yaml" <<EOF
node: {id: lds-e2e, cluster: lds-e2e}
dynamic_resources:
  ads_config:
    api_type: GRPC
    transport_api_version: V3
    grpc_services:
      - envoy_grpc: {cluster_name: entente_xds}
  cds_config: {resource_api_version: V3, ads: {}}
  lds_config: {resource_api_version: V3, ads: {}}
static_resources:
  secrets:
    - name: sink
      tls_certificate:
        certificate_chain: {filename: /sink/tls.crt}
        private_key: {filename: /sink/tls.key}
  clusters:
    - name: entente_xds
      type: STATIC
      connect_timeout: 1s
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config: {http2_protocol_options: {}}
      load_assignment:
        cluster_name: entente_xds
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address: {pipe: {path: /run/entente/xds.sock}}
EOF
"$work/entente" --config "$work/entente.yaml" >"$work/entente.log" 2>&1 &
entente_pid=$!
if ! docker image inspect "$image" >/dev/null 2>&1; then docker pull "$image" >/dev/null; fi
envoy_id=$(docker run -d --network host \
  -v "$work/run:/run/entente" -v "$work/envoy.yaml:/etc/envoy/envoy.yaml:ro" \
  -v "$work/sink:/sink:ro" "$image" envoy -c /etc/envoy/envoy.yaml)
status() {
  curl -ks --noproxy '*' --max-time 2 --resolve "$1:$port:127.0.0.1" \
    -o /dev/null -w '%{http_code}' "https://$1:$port/" || true
}
expect() {
  local host=$1 code=$2 got=""
  for _ in $(seq 1 60); do
    got=$(status "$host")
    if [[ "$got" == "$code" ]]; then echo "ok $host -> $code"; return; fi
    sleep 0.25
  done
  echo "FAIL $host -> $got, want $code" >&2; exit 1
}
expect old.example.com 200
expect unknown.example.com 421
no_sni=$(printf 'GET / HTTP/1.1\r\nHost: old.example.com\r\nConnection: close\r\n\r\n' \
  | timeout 5 openssl s_client -quiet -connect "127.0.0.1:$port" -noservername 2>/dev/null || true)
grep -q 'HTTP/1.1 421' <<<"$no_sni"
echo 'ok no SNI -> 421'
python3 "$work/connection.py" "$work" "$port" >"$work/connection.log" 2>&1 &
connection_pid=$!
for _ in $(seq 1 50); do [[ ! -e "$work/connection.ready" ]] || break; sleep 0.1; done
[[ -e "$work/connection.ready" ]]

# 先提交缺材料候选,新域名必须仍落 sink,旧域名不受影响。
write_config true true
sleep 2
expect old.example.com 200
expect new.example.com 421
grep -q validation_failed "$work/entente.log"
write_config true
expect new.example.com 200
sleep 1
touch "$work/connection.stop"
wait "$connection_pid"
connection_pid=""
cat "$work/connection.log"

# 非法 YAML 和合法但路由错误的候选都不能替换生效代。
cp "$work/entente.yaml" "$work/good.yaml"
printf 'listeners: [\n' >"$work/bad.yaml"
mv "$work/bad.yaml" "$work/entente.yaml"
sleep 1
expect old.example.com 200
expect new.example.com 200
sed 's/backend: backend/backend: missing/g' "$work/good.yaml" >"$work/bad.yaml"
mv "$work/bad.yaml" "$work/entente.yaml"
sleep 1
expect old.example.com 200
expect new.example.com 200
expect unknown.example.com 421
echo 'LDS atomic reload e2e passed'
