#!/usr/bin/env bash
# demo/run-server.sh — foreground launcher for the update-server.
# Ctrl+C stops it. Logs stream straight to the terminal so the audience
# sees the handshake + reload + delta-served lines live.

set -euo pipefail

DEMO_DIR="/tmp/ota-demo"
BIN="$DEMO_DIR/update-server"
CFG="$DEMO_DIR/configs/server.yaml"

if [[ ! -x "$BIN" ]]; then
  echo "update-server not built. Run demo/setup.sh first." >&2
  exit 1
fi

echo "==> update-server"
echo "    HTTP API : 127.0.0.1:18080"
echo "    CoAP     : 127.0.0.1:15683 (UDP)"
echo "    Metrics  : 127.0.0.1:19100 (+ /debug/pprof/)"
echo "    Config   : $CFG"
echo ""
exec "$BIN" -config "$CFG"
