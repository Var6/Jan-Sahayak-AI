#!/usr/bin/env bash
# Dev runner for the central API: builds the binaries, migrates the dev
# database, creates a dev signing key on first run, and starts the server on
# 127.0.0.1:8750 in the foreground (Ctrl-C stops it).
#
#   scripts/dev/central.sh
#
# Environment overrides:
#   JS_DATABASE_URL  (default postgres://jansahayak@127.0.0.1:54329/jansahayak?sslmode=disable)
#   JS_ADDR          (default 127.0.0.1:8750)
#   JS_DEV_DIR       (default var/central) — holds bin/ and server.key
#
# First admin user (password read from stdin, never from argv):
#   printf '%s\n' "$PASSWORD" | var/central/bin/central-admin create-user -username admin -display-name Admin
#   var/central/bin/central-admin grant-role -username admin -role admin
#
# The admin cookie is sent without the Secure flag here (plain-HTTP localhost).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

export JS_DATABASE_URL="${JS_DATABASE_URL:-postgres://jansahayak@127.0.0.1:54329/jansahayak?sslmode=disable}"
ADDR="${JS_ADDR:-127.0.0.1:8750}"
DEV_DIR="${JS_DEV_DIR:-$ROOT/var/central}"
BIN="$DEV_DIR/bin"
KEY="$DEV_DIR/server.key"

mkdir -p "$BIN"
chmod 700 "$DEV_DIR"

echo "==> building central-api and central-admin"
go build -o "$BIN/central-api" ./services/central-api/cmd/central-api
go build -o "$BIN/central-admin" ./services/central-api/cmd/central-admin

echo "==> migrating $JS_DATABASE_URL"
"$BIN/central-admin" migrate

if [[ ! -f "$KEY" ]]; then
  echo "==> generating dev server signing key at $KEY"
  "$BIN/central-admin" gen-server-key -out "$KEY"
fi
echo "==> server public key (pin this in device config):"
"$BIN/central-admin" server-public-key -key "$KEY"

echo "==> starting central-api on http://$ADDR"
exec "$BIN/central-api" -addr "$ADDR" -server-key "$KEY" -cookie-secure=false
