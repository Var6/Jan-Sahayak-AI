#!/usr/bin/env bash
# Builds the citizen kiosk UI (apps/citizen-kiosk) and copies dist/ into the
# device agent's embed directory so `go build` bakes it into the binary.
#
#   scripts/dev/build-webui.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
DEST="services/device-agent/internal/webui/assets/dist"

echo "==> building citizen-kiosk"
pnpm --dir apps/citizen-kiosk build

if [[ ! -f apps/citizen-kiosk/dist/index.html ]]; then
  echo "build produced no dist/index.html" >&2
  exit 1
fi
rm -rf "$DEST"
mkdir -p "$DEST"
cp -R apps/citizen-kiosk/dist/. "$DEST/"
echo "==> embedded UI: $(find "$DEST" -type f | wc -l | tr -d ' ') files, $(du -sh "$DEST" | cut -f1)"
