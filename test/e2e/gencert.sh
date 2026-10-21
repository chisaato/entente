#!/bin/sh
# 为演示用的 https listener 生成一张一次性自签证书。
set -eu
dir=${1:?usage: gencert.sh <dir>}
mkdir -p "$dir"
[ -f "$dir/tls.crt" ] && exit 0
openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
  -subj "/CN=entente-demo" \
  -addext "subjectAltName=DNS:*.example.com" \
  -keyout "$dir/tls.key" -out "$dir/tls.crt" 2>/dev/null
# 演示时由 OpenBao 容器中的 CLI 读取,只用于初始化 KV。
chmod 0644 "$dir/tls.key"
