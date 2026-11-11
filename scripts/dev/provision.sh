#!/usr/bin/env bash
# Dev provisioning: connects a local device agent to the local central API.
# Idempotent — safe to run again; it never replaces keys or users that exist.
#
#   scripts/dev/provision.sh [--text-only] [--knowledge-from-central]
#
#   1. builds central-admin and device-agent (into var/central/bin, bin/),
#      migrates the dev database;
#   2. creates the central server signing key if missing (var/central/server.key);
#   3. creates/reads the device identity (device-agent -print-identity) and
#      registers its public key with central (skipped if already registered;
#      refused if the id is registered with a different key);
#   4. creates dev users dev-admin (admin), dev-editor (scheme_editor) and
#      dev-reviewer (scheme_reviewer) with random passwords, written once to
#      var/central/dev-users.json (mode 0600);
#   5. writes the device config override $JS_DEVICE_DATA_DIR/provisioned.json:
#      the base dev config plus sync.base_url, the pinned server public key
#      and data_dir.
#
# Then run the agent with:  bin/device-agent -config <data dir>/provisioned.json
# (central must be running: scripts/dev/central.sh).
#
# Flags:
#   --text-only               no AI runtimes (stt_engine none, TTS and LLM off)
#   --knowledge-from-central  knowledge.path "" — scheme records come only from
#                             signed central bundles (not the local drafts dir)
# Env:
#   JS_DATABASE_URL     (default postgres://jansahayak@127.0.0.1:54329/jansahayak?sslmode=disable)
#   JS_CENTRAL_URL      (default http://127.0.0.1:8750) — written into the device config
#   JS_DEV_DIR          (default var/central)
#   JS_DEVICE_DATA_DIR  (default var/device)
#   JS_BASE_CONFIG      (default config/device.dev.json)
#
# Dev only: the generated config uses plain http to loopback, which the agent
# accepts only on dev-* profiles.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

TEXT_ONLY=0
CENTRAL_KNOWLEDGE=0
for a in "$@"; do
  case "$a" in
    --text-only) TEXT_ONLY=1 ;;
    --knowledge-from-central) CENTRAL_KNOWLEDGE=1 ;;
    -h | --help) sed -n '2,35p' "$0"; exit 0 ;;
    *) echo "unknown flag: $a" >&2; exit 2 ;;
  esac
done

command -v jq >/dev/null || { echo "provision.sh needs jq" >&2; exit 1; }

export JS_DATABASE_URL="${JS_DATABASE_URL:-postgres://jansahayak@127.0.0.1:54329/jansahayak?sslmode=disable}"
CENTRAL_URL="${JS_CENTRAL_URL:-http://127.0.0.1:8750}"
DEV_DIR="${JS_DEV_DIR:-$ROOT/var/central}"
DATA_DIR="${JS_DEVICE_DATA_DIR:-$ROOT/var/device}"
BASE_CONFIG="${JS_BASE_CONFIG:-$ROOT/config/device.dev.json}"
KEY="$DEV_DIR/server.key"
ADMIN="$DEV_DIR/bin/central-admin"
AGENT="$ROOT/bin/device-agent"
CREDS="$DEV_DIR/dev-users.json"
OUT="$DATA_DIR/provisioned.json"

mkdir -p "$DEV_DIR/bin" "$DATA_DIR"
chmod 700 "$DEV_DIR"

echo "==> building central-admin and device-agent"
go build -o "$ADMIN" ./services/central-api/cmd/central-admin
go build -o "$AGENT" ./services/device-agent/cmd/device-agent

echo "==> migrating the central database"
"$ADMIN" migrate

if [[ ! -f "$KEY" ]]; then
  echo "==> generating the dev server signing key ($KEY)"
  "$ADMIN" gen-server-key -out "$KEY" >/dev/null
fi
PUB_JSON="$("$ADMIN" server-public-key -key "$KEY")"
KEY_ID="$(jq -er .key_id <<<"$PUB_JSON")"
KEY_PUB="$(jq -er .public_key <<<"$PUB_JSON")"
echo "    server key id $KEY_ID"

echo "==> device identity ($DATA_DIR/identity.json)"
ID_JSON="$(JS_DATA_DIR="$DATA_DIR" "$AGENT" -config "$BASE_CONFIG" -print-identity)"
DEVICE_ID="$(jq -er .device_id <<<"$ID_JSON")"
DEVICE_PUB="$(jq -er .public_key <<<"$ID_JSON")"
echo "    device id $DEVICE_ID"

REGISTERED_PUB="$("$ADMIN" list-devices -json | jq -r --arg id "$DEVICE_ID" '.[]? | select(.id == $id) | .public_key')"
if [[ -z "$REGISTERED_PUB" ]]; then
  echo "==> registering the device with central"
  "$ADMIN" register-device -id "$DEVICE_ID" -public-key "$DEVICE_PUB" \
    -name "dev-kiosk-${DEVICE_ID:0:8}" -district "DEV (FIXTURE)"
elif [[ "$REGISTERED_PUB" != "$DEVICE_PUB" ]]; then
  echo "device $DEVICE_ID is registered with a different public key; refusing (revoke it or use a new data dir)" >&2
  exit 1
else
  echo "    already registered"
fi

echo "==> dev users"
[[ -f "$CREDS" ]] || { umask 077; echo '{}' >"$CREDS"; }
chmod 600 "$CREDS"
create_user() { # username role
  local u=$1 role=$2 pw out
  pw="$(openssl rand -base64 18 | tr -d '/+=' | cut -c1-20)"
  if out="$(printf '%s\n' "$pw" | "$ADMIN" create-user -username "$u" -display-name "$u (dev)" 2>&1)"; then
    jq --arg u "$u" --arg p "$pw" '.[$u] = $p' "$CREDS" >"$CREDS.tmp" && mv "$CREDS.tmp" "$CREDS"
    chmod 600 "$CREDS"
    echo "    created $u"
  elif grep -q "username exists" <<<"$out"; then
    if jq -e --arg u "$u" 'has($u)' "$CREDS" >/dev/null; then
      echo "    $u exists"
    else
      echo "    $u exists but its password is not in $CREDS (created elsewhere); left unchanged" >&2
    fi
  else
    echo "$out" >&2
    return 1
  fi
  "$ADMIN" grant-role -username "$u" -role "$role" >/dev/null
}
create_user dev-admin admin
create_user dev-editor scheme_editor
create_user dev-reviewer scheme_reviewer
echo "    passwords: $CREDS"

echo "==> writing $OUT"
jq --arg url "$CENTRAL_URL" --arg kid "$KEY_ID" --arg pub "$KEY_PUB" --arg dd "$DATA_DIR" \
  --argjson text "$TEXT_ONLY" --argjson ck "$CENTRAL_KNOWLEDGE" '
  .data_dir = $dd
  | .sync.base_url = $url
  | .sync.server_keys = {($kid): $pub}
  | if $text == 1 then .speech.stt_engine = "none" | .speech.tts_enabled = false | .llm.enabled = false else . end
  | if $ck == 1 then .knowledge.path = "" else . end
' "$BASE_CONFIG" >"$OUT.tmp"
mv "$OUT.tmp" "$OUT"
"$AGENT" -config "$OUT" -check-config
echo "done. Start the agent: $AGENT -config $OUT"
