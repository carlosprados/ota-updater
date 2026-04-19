#!/usr/bin/env bash
# demo/setup.sh — bootstrap the OTA updater demo in /tmp/ota-demo.
#
# Idempotent: running it twice picks up where it left off except for the
# slot symlink, which is always reset to the v1 binary so the demo starts
# from a known state. To wipe everything use demo/cleanup.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEMO_DIR="/tmp/ota-demo"

echo "==> Preparing $DEMO_DIR"
mkdir -p \
  "$DEMO_DIR/keys" \
  "$DEMO_DIR/store/binaries" \
  "$DEMO_DIR/store/deltas" \
  "$DEMO_DIR/agent/slots" \
  "$DEMO_DIR/apps" \
  "$DEMO_DIR/configs" \
  "$DEMO_DIR/logs"

echo "==> Copying configs"
cp "$ROOT/demo/configs/server.yaml" "$DEMO_DIR/configs/server.yaml"
cp "$ROOT/demo/configs/agent.yaml"  "$DEMO_DIR/configs/agent.yaml"

echo "==> Generating Ed25519 keypair (if missing)"
if [[ ! -f "$DEMO_DIR/keys/server.key" ]]; then
  ( cd "$ROOT" && go run ./tools/keygen -out "$DEMO_DIR/keys" )
else
  echo "    keys already present, skipping"
fi

echo "==> Building update-server"
( cd "$ROOT" && CGO_ENABLED=0 go build -ldflags="-s -w" -o "$DEMO_DIR/update-server" ./cmd/update-server )

echo "==> Building demo apps v1, v2, v3"
for v in v1 v2 v3; do
  ( cd "$ROOT" && CGO_ENABLED=0 go build -ldflags="-s -w" -o "$DEMO_DIR/apps/$v" ./demo/apps/$v )
  echo "    $v: $(du -h "$DEMO_DIR/apps/$v" | cut -f1)"
done

echo "==> Publishing v1 as the initial target"
cp "$DEMO_DIR/apps/v1" "$DEMO_DIR/target.bin"

echo "==> Seeding the agent's A/B slots with v1"
cp "$DEMO_DIR/apps/v1" "$DEMO_DIR/agent/slots/A"
cp "$DEMO_DIR/apps/v1" "$DEMO_DIR/agent/slots/B"
chmod +x "$DEMO_DIR/agent/slots/A" "$DEMO_DIR/agent/slots/B"
ln -sfn "$DEMO_DIR/agent/slots/A" "$DEMO_DIR/agent/current"

echo ""
echo "Setup complete. Next steps (each in its own terminal):"
echo "  1) demo/run-server.sh      # starts the update-server"
echo "  2) demo/run-app.sh         # starts the demo app (embedded agent)"
echo "  3) open http://127.0.0.1:7000/"
echo "  Then use demo/publish-version.sh v2|v3 to trigger updates."
