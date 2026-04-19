#!/usr/bin/env bash
# demo/publish-version.sh <v1|v2|v3> — publish a demo binary as the new
# target. The update-server's fsnotify watcher picks up the write within
# a few hundred ms and invalidates the manifest cache. The agent sees
# the change on its next heartbeat (default 5s in the demo config).

set -euo pipefail

DEMO_DIR="/tmp/ota-demo"

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <v1|v2|v3>" >&2
  exit 64
fi
WHICH="$1"
SRC="$DEMO_DIR/apps/$WHICH"

if [[ ! -x "$SRC" ]]; then
  echo "unknown version '$WHICH' (no binary at $SRC). Run demo/setup.sh." >&2
  exit 1
fi

DST="$DEMO_DIR/target.bin"
TMP="${DST}.publishing"

echo "==> publishing $WHICH as $DST"
cp "$SRC" "$TMP"
mv "$TMP" "$DST"    # atomic rename → fsnotify sees one CREATE/RENAME event
echo "    sha256: $(sha256sum "$DST" | cut -d' ' -f1)"
echo "    the agent will pick it up on its next heartbeat (~5s)."
