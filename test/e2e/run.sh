#!/usr/bin/env bash
# End-to-end check: Envoy + Entente + containers via examples/compose.yaml.
#
#   test/e2e/run.sh
#
# E2E_COMPOSE_OVERRIDE may name an extra compose file (e.g. one that swaps the
# entente image for a locally built binary). E2E_KEEP=1 leaves the stack up.
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
  echo "--- entente logs" >&2; "${compose[@]}" logs entente >&2 || true
  echo "--- envoy logs" >&2; "${compose[@]}" logs envoy | tail -40 >&2 || true
}

test/e2e/gencert.sh examples/certs
"${compose[@]}" up -d --build --quiet-pull

# status <scheme> <host> <port>: prints the HTTP status code.
status() {
  curl -s -o /dev/null -w '%{http_code}' -k --max-time 3 --noproxy '*' \
    --resolve "$2:$3:127.0.0.1" "$1://$2:$3/" || true
}

# expect <code> <scheme> <host> <port>: waits up to 30s for the code.
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

expect 200 http  whoami.example.com 8080   # docker labels, entrypoint http
expect 200 https whoami.example.com 8443   # docker labels, entrypoint https
expect 200 https legacy.example.com 8443   # static route
expect 404 http  legacy.example.com 8080   # static route is https-only
expect 404 http  unknown.example.com 8080  # nothing exposed by default

body=$(curl -sk --max-time 3 --noproxy '*' --resolve legacy.example.com:8443:127.0.0.1 https://legacy.example.com:8443/)
grep -q '^Name: legacy' <<<"$body" || { echo "FAIL legacy body: $body" >&2; exit 1; }
echo "ok   legacy body served by the static backend"

"${compose[@]}" stop whoami >/dev/null
expect 404 http whoami.example.com 8080    # route removed with the container

"${compose[@]}" start whoami >/dev/null
expect 200 http whoami.example.com 8080    # and back

echo "e2e passed"
