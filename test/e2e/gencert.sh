#!/bin/sh
# Generates a throwaway self-signed certificate for the demo https listener.
set -eu
dir=${1:?usage: gencert.sh <dir>}
mkdir -p "$dir"
[ -f "$dir/tls.crt" ] && exit 0
openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
  -subj "/CN=entente-demo" \
  -addext "subjectAltName=DNS:*.example.com" \
  -keyout "$dir/tls.key" -out "$dir/tls.crt" 2>/dev/null
# Envoy runs as uid 101 in the official image.
chmod 0644 "$dir/tls.key"
