#!/usr/bin/env bash
# demo/run-app.sh — foreground launcher for the demo application.
#
# The "app" is whatever binary the agent's active symlink points at. On
# first run that's v1 (seeded by setup.sh). After each successful OTA
# swap + exec, the same process image is replaced by the new binary and
# this script stays attached — you see the version flip in the logs.
#
# Ctrl+C stops it. cleanup.sh wipes all state.

set -euo pipefail

DEMO_DIR="/tmp/ota-demo"
APP="$DEMO_DIR/agent/current"
CFG="$DEMO_DIR/configs/agent.yaml"

if [[ ! -L "$APP" && ! -x "$APP" ]]; then
  echo "agent active symlink missing. Run demo/setup.sh first." >&2
  exit 1
fi

echo "==> demo app (embedded agent)"
echo "    Banner HTTP : 127.0.0.1:7000"
echo "    Config      : $CFG"
echo "    Active slot : $(readlink -f "$APP")"
echo ""
exec "$APP" -config "$CFG"
