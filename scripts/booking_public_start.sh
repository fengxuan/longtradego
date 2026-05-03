#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

exec go run . booking service start \
  --addr 127.0.0.1:18081 \
  --admin-addr 127.0.0.1:18082 \
  --max-port-fallback 0 \
  --security-keys conf/security_keys.json \
  "$@"
