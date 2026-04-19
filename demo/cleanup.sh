#!/usr/bin/env bash
# demo/cleanup.sh — kill demo processes and wipe /tmp/ota-demo.
# Safe to re-run anytime. Does NOT touch the repo.

set -euo pipefail

DEMO_DIR="/tmp/ota-demo"

echo "==> Killing demo processes (update-server, demo apps)"
pkill -f "$DEMO_DIR/update-server" 2>/dev/null || true
pkill -f "$DEMO_DIR/agent/current" 2>/dev/null || true
pkill -f "$DEMO_DIR/agent/slots/" 2>/dev/null || true
pkill -f "$DEMO_DIR/apps/" 2>/dev/null || true

echo "==> Removing $DEMO_DIR"
rm -rf "$DEMO_DIR"

echo "done."
