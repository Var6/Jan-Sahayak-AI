#!/usr/bin/env bash
# Runs the fault-injection soak against a fully owned dev stack:
# central-api (dev DB, admin UI), llama-server, speech worker and a device
# agent provisioned with scripts/dev/provision.sh into $JS_SOAK_DIR.
#
#   JS_SOAK_DIR=var/soak/device scripts/dev/faultsoak/run.sh -duration 60m
#   scripts/dev/faultsoak/run.sh -duration 8h -faults 'speech@30m,llama@1h,central@2h,agent@3h,…'
#
# Extra arguments go to the faultsoak binary (see its -h). Nothing else may
# be listening on 8730/8739/8742/8743/8750. Output: var/soak/<run id>/.
#
# One-time setup of the soak device (fresh identity, registered centrally):
#   JS_DEVICE_DATA_DIR=$PWD/var/soak/device scripts/dev/provision.sh
#   jq '.pprof_addr="127.0.0.1:8739" | .sync.heartbeat_interval="1m"' \
#     var/soak/device/provisioned.json > var/soak/device/soak.json
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$ROOT"

SOAK_DIR="${JS_SOAK_DIR:-$ROOT/var/soak/device}"
CONFIG="${JS_SOAK_CONFIG:-$SOAK_DIR/soak.json}"
export JS_DATABASE_URL="${JS_DATABASE_URL:-postgres://jansahayak@127.0.0.1:54329/jansahayak?sslmode=disable}"
MODELS="$ROOT/models/weights"

[[ -f "$CONFIG" ]] || { echo "missing $CONFIG (see the setup notes at the top of this script)" >&2; exit 1; }
DEVICE_ID="$(jq -er .device_id "$SOAK_DIR/identity.json")"

go build -o bin/faultsoak ./scripts/dev/faultsoak
[[ -x bin/device-agent ]] || make build
[[ -x services/speech-worker/bin/speech-worker ]] || make speech-worker
go build -o var/central/bin/central-api ./services/central-api/cmd/central-api

ADMIN_UI=""
[[ -f apps/admin-dashboard/out/csp-hashes.json ]] && ADMIN_UI="-admin-ui-dir $ROOT/apps/admin-dashboard/out"

exec bin/faultsoak \
  -data-dir "$SOAK_DIR" -device-id "$DEVICE_ID" -pg "$JS_DATABASE_URL" -pprof http://127.0.0.1:8739 \
  -central-cmd "$ROOT/var/central/bin/central-api -addr 127.0.0.1:8750 -server-key $ROOT/var/central/server.key -cookie-secure=false $ADMIN_UI" \
  -llama-cmd "llama-server -m $MODELS/llm/gemma-4-E2B-it-Q4_0.gguf -c 4096 -np 1 -fa on --reasoning off -ngl 99 --cache-reuse 256 --host 127.0.0.1 --port 8742 --no-webui" \
  -speech-cmd "$ROOT/services/speech-worker/bin/speech-worker -models-dir $MODELS" \
  -agent-cmd "$ROOT/bin/device-agent -config $CONFIG" \
  "$@"
