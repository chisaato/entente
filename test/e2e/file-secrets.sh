#!/usr/bin/env bash
# 隔离 e2e:纯 file 证书来源,经真实 Entente 进程的 ADS/SDS 推给一只独立 Envoy。
# 不使用 OpenBao,不碰 examples/ 和 test/e2e/run.sh 的 compose 栈。
#
#   test/e2e/file-secrets.sh
#
# 断言:
#   1. 首次握手出示证书 A
#   2. 原地覆盖证书链、私钥仍是 A 时,继续出示 A
#   3. 写入与新证书匹配的私钥后,出示 B
#   4. 同目录 rename 覆盖证书链和私钥后,出示 C
#   5. 重启 Entente,并换一只没有 SDS 缓存的 Envoy 后,仍出示 C
#
# 每次运行使用独立临时目录和容器名。清理只针对本次启动成功后记下的
# 进程与容器,不读取、也不结束任何先前留下的 PID。
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# 避开 examples compose 的 8080/8443/9901。占用则直接失败,不抢端口。
host_port=18443
sni=files.example.com
image=envoyproxy/envoy:v1.39-latest

if ss -ltn | grep -Eq ":${host_port}([^0-9]|$)"; then
  echo "FAIL 127.0.0.1:${host_port} is already in use; refusing to share a port" >&2
  exit 1
fi

work=$(mktemp -d /tmp/opencode/entente-file-secrets.XXXXXX)
# 容器名跟这次目录走,避免删到另一次运行。
envoy_name=entente-e2e-file-secrets-$(basename "$work")
# 失败时把去掉 PEM 的日志放到临时目录之外。
log_keep=/tmp/opencode/$(basename "$work").log

bin=$work/bin/entente
run_dir=$work/run
cert_dir=$work/certs
stage=$work/stage
cfg=$work/etc/entente.yaml
envoy_cfg=$work/etc/envoy.yaml
log=$work/entente.log

# 只有本次启动成功后才记下。trap 不碰这两项之外的进程和容器。
entente_pid=""
envoy_id=""

redact_pem() {
  sed -E '/-----BEGIN /, /-----END /d'
}

cleanup() {
  local status=$?
  if [[ -n "${entente_pid}" ]]; then
    kill "$entente_pid" 2>/dev/null || true
    wait "$entente_pid" 2>/dev/null || true
  fi
  # 先留日志再删容器。副本只放临时目录外面,且去掉 PEM。
  if [[ "$status" -ne 0 ]]; then
    {
      echo "--- entente log"
      if [[ -f "$log" ]]; then
        redact_pem <"$log" || true
      fi
      echo "--- envoy log"
      if [[ -n "${envoy_id}" ]]; then
        docker logs "$envoy_id" 2>&1 | tail -40 | redact_pem || true
      fi
    } >"$log_keep" 2>/dev/null || true
    echo "kept redacted log at ${log_keep}" >&2
  fi
  if [[ -n "${envoy_id}" ]]; then
    docker rm -f "$envoy_id" >/dev/null 2>&1 || true
  fi
  # 证书、私钥和本次工作目录一起删除。
  if [[ "$work" == /tmp/opencode/entente-file-secrets.* ]]; then
    rm -rf "$work"
  fi
}
trap cleanup EXIT

fail() {
  echo "FAIL $*" >&2
  if [[ -f "$log" ]]; then
    echo "--- entente log (tail, PEM redacted)" >&2
    tail -80 "$log" | redact_pem >&2 || true
  fi
  if [[ -n "${envoy_id}" ]]; then
    echo "--- envoy log" >&2
    docker logs "$envoy_id" 2>&1 | tail -40 | redact_pem >&2 || true
  fi
  exit 1
}

fingerprint_file() {
  openssl x509 -in "$1" -noout -fingerprint -sha256
}

# 只打印证书指纹,不读取、不打印私钥。
presented_fingerprint() {
  timeout 5 openssl s_client -connect "127.0.0.1:${host_port}" \
    -servername "$sni" </dev/null 2>/dev/null \
    | openssl x509 -noout -fingerprint -sha256 2>/dev/null
}

wait_fingerprint() {
  local want=$1 label=$2 got=""
  for _ in $(seq 1 40); do
    got=$(presented_fingerprint || true)
    if [[ "$got" == "$want" ]]; then
      echo "ok   ${label}: ${got}"
      return 0
    fi
    sleep 0.5
  done
  fail "${label}: got '${got:-<handshake failed>}', want ${want}"
}

# 不匹配的证书必须保持上一份。窗口要盖过 250ms 去抖和 Envoy 收 SDS 的时间,
# 否则"还没更新"会被误当成"保留旧证书"。
assert_stays() {
  local want=$1 label=$2 got=""
  for _ in $(seq 1 8); do
    got=$(presented_fingerprint || true)
    if [[ "$got" != "$want" ]]; then
      fail "${label}: got '${got:-<handshake failed>}', want stay ${want}"
    fi
    sleep 0.5
  done
  echo "ok   ${label}: stayed ${want}"
}

# 生成一张一次性 ECC 证书。私钥只落在 0700 目录,stdout/stderr 都不留 PEM。
mint() {
  local id=$1
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
    -subj "/CN=${sni}" \
    -addext "subjectAltName=DNS:${sni}" \
    -keyout "${stage}/${id}.key" -out "${stage}/${id}.crt" >/dev/null 2>&1
  chmod 0600 "${stage}/${id}.key"
  [[ -s "${stage}/${id}.crt" && -s "${stage}/${id}.key" ]] || fail "mint ${id} produced empty files"
}

install_pair() {
  # 截断写回同一 inode,模拟 acme.sh 原地覆盖。
  cat "${stage}/$1.crt" > "${cert_dir}/fullchain.pem"
  cat "${stage}/$1.key" > "${cert_dir}/key.pem"
  chmod 0600 "${cert_dir}/key.pem"
}

wait_port_free() {
  local _
  for _ in $(seq 1 50); do
    if ! ss -ltn | grep -Eq ":${host_port}([^0-9]|$)"; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

start_entente() {
  local child _
  # 只结束本次已经启动成功的那一个进程。
  if [[ -n "${entente_pid}" ]]; then
    kill "$entente_pid" 2>/dev/null || true
    wait "$entente_pid" 2>/dev/null || true
    entente_pid=""
  fi
  # 关掉旧进程会删掉 socket;目录权限保持对容器内 uid 101 可进入。
  rm -f "${run_dir}/xds.sock"
  chmod 0755 "$run_dir"
  : >"$log"
  "$bin" --config "$cfg" >>"$log" 2>&1 &
  child=$!
  for _ in $(seq 1 50); do
    if [[ -S "${run_dir}/xds.sock" ]]; then
      entente_pid=$child
      return 0
    fi
    if ! kill -0 "$child" 2>/dev/null; then
      wait "$child" 2>/dev/null || true
      fail "entente exited before the xDS socket appeared"
    fi
    sleep 0.1
  done
  kill "$child" 2>/dev/null || true
  wait "$child" 2>/dev/null || true
  fail "xDS socket was not created"
}

start_envoy() {
  local cid
  # 重启时只删本次启动成功的那只容器,不按固定名字搜。
  if [[ -n "${envoy_id}" ]]; then
    docker rm -f "$envoy_id" >/dev/null
    envoy_id=""
    wait_port_free || fail "port ${host_port} stayed in use after removing this run's envoy"
  fi
  if ! cid=$(docker run -d --name "$envoy_name" \
    --pull never \
    -p "127.0.0.1:${host_port}:8443" \
    -v "${run_dir}:/run/entente" \
    -v "${envoy_cfg}:/etc/envoy/envoy.yaml:ro" \
    "$image" \
    envoy -c /etc/envoy/envoy.yaml); then
    # 这次 docker run 可能留下同名容器。名字只属于本次,删掉它,不记入 envoy_id。
    docker rm -f "$envoy_name" >/dev/null 2>&1 || true
    fail "envoy container did not start"
  fi
  envoy_id=$cid
}

mkdir -p "$work/bin" "$work/etc" "$run_dir"
chmod 0755 "$run_dir"
# 证书和中间材料只放在这次的私有临时目录。
mkdir -p "$cert_dir" "$stage"
chmod 0700 "$cert_dir" "$stage"

cat >"$cfg" <<EOF
# 本文件由 test/e2e/file-secrets.sh 生成。证书只来自本地 PEM,不声明外部密钥库。
node_id: file-secrets
route_timeout: 0s
xds:
  socket: ${run_dir}/xds.sock
  # Envoy 容器以 uid 101 连接,和本进程不是同一用户。
  socket_mode: "0666"
entrypoints:
  - https
secrets:
  - name: file-cert
    file:
      certificate_chain: ${cert_dir}/fullchain.pem
      private_key: ${cert_dir}/key.pem
EOF

if grep -Eq '^[[:space:]]*openbao:' "$cfg"; then
  fail "generated config contains an openbao section"
fi

cat >"$envoy_cfg" <<EOF
node:
  id: file-secrets
  cluster: file-secrets-e2e
admin:
  address:
    socket_address: { address: 127.0.0.1, port_value: 9901 }
dynamic_resources:
  ads_config:
    api_type: GRPC
    transport_api_version: V3
    set_node_on_first_message_only: true
    grpc_services:
      - envoy_grpc: { cluster_name: entente_xds }
  cds_config:
    resource_api_version: V3
    ads: {}
static_resources:
  clusters:
    - name: entente_xds
      type: STATIC
      connect_timeout: 1s
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http2_protocol_options: {}
      load_assignment:
        cluster_name: entente_xds
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    pipe: { path: /run/entente/xds.sock }
  listeners:
    - name: https
      address:
        socket_address: { address: 0.0.0.0, port_value: 8443 }
      listener_filters:
        - name: envoy.filters.listener.tls_inspector
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.filters.listener.tls_inspector.v3.TlsInspector
      filter_chains:
        - filter_chain_match:
            server_names: ["${sni}"]
          transport_socket:
            name: envoy.transport_sockets.tls
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext
              common_tls_context:
                tls_certificate_sds_secret_configs:
                  - name: file-cert
                    sds_config:
                      resource_api_version: V3
                      ads: {}
          filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: https
                rds:
                  route_config_name: https
                  config_source:
                    resource_api_version: V3
                    ads: {}
                http_filters:
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
EOF

# 证书不挂进 Envoy。若镜像里没有 envoy 或端口映射错了,下面的握手会失败。
if ! docker image inspect "$image" >/dev/null 2>&1; then
  docker pull "$image" >/dev/null
fi

echo "building entente into ${bin}"
(
  cd "$root"
  GOCACHE=/tmp/opencode/go-cache GOTMPDIR=/tmp/opencode \
    go build -mod=readonly -trimpath -o "$bin" ./cmd/entente
)

mint a
mint b
mint c
fp_a=$(fingerprint_file "${stage}/a.crt")
fp_b=$(fingerprint_file "${stage}/b.crt")
fp_c=$(fingerprint_file "${stage}/c.crt")
[[ "$fp_a" != "$fp_b" && "$fp_b" != "$fp_c" && "$fp_a" != "$fp_c" ]] \
  || fail "minted certificates are not distinct"

install_pair a
start_entente
start_envoy
wait_fingerprint "$fp_a" "first handshake is A"

# 只换证书链,私钥保持 A。X509KeyPair 失败,应保留 A。
cat "${stage}/b.crt" > "${cert_dir}/fullchain.pem"
assert_stays "$fp_a" "in-place cert B with key A still presents A"

cat "${stage}/b.key" > "${cert_dir}/key.pem"
chmod 0600 "${cert_dir}/key.pem"
wait_fingerprint "$fp_b" "matching key B presents B"

# 同目录先写临时文件再 rename,覆盖成 C。
cp -f "${stage}/c.crt" "${cert_dir}/fullchain.pem.next"
cp -f "${stage}/c.key" "${cert_dir}/key.pem.next"
chmod 0600 "${cert_dir}/key.pem.next"
mv -f "${cert_dir}/fullchain.pem.next" "${cert_dir}/fullchain.pem"
mv -f "${cert_dir}/key.pem.next" "${cert_dir}/key.pem"
wait_fingerprint "$fp_c" "rename to C presents C"

# 磁盘上仍是 C。重启控制平面,并丢掉 Envoy 的旧 SDS 缓存后再握一次。
on_disk=$(fingerprint_file "${cert_dir}/fullchain.pem")
[[ "$on_disk" == "$fp_c" ]] || fail "files on disk are not C before restart"
start_entente
start_envoy
wait_fingerprint "$fp_c" "restarted entente still serves C from files"

echo "file-secrets e2e passed"
echo "evidence: A=${fp_a}"
echo "evidence: B=${fp_b}"
echo "evidence: C=${fp_c}"
echo "evidence: envoy=${envoy_name} port=127.0.0.1:${host_port} sni=${sni}"
