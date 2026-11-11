#!/usr/bin/env bash
# Starts the dev stack one process at a time (this Mac has little free RAM):
#   1. llama-server  127.0.0.1:8742  (Gemma 4 E2B Q4_0)      [skip: JS_NO_LLM=1]
#   2. speech-worker 127.0.0.1:8743  (IndicConformer + Supertonic-3)
#   3. device-agent  127.0.0.1:8730  (config/device.dev.json, kiosk UI at /)
# PIDs go to var/run/*.pid, logs to var/log/*.log. Stop with scripts/dev/down.sh.
#
# Env: JS_NO_LLM=1, JS_CONFIG (default config/device.dev.json),
#      JS_AGENT_BIN (default bin/device-agent; built if missing).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
cd "$ROOT"

CONFIG="${JS_CONFIG:-config/device.dev.json}"
AGENT_BIN="${JS_AGENT_BIN:-bin/device-agent}"
WORKER_BIN="services/speech-worker/bin/speech-worker"
MODELS="models/weights"

trap 'echo "up.sh failed; stopping what was started"; "$ROOT/scripts/dev/down.sh"' ERR

if [[ "${JS_NO_LLM:-0}" != "1" ]]; then
  if pid_alive llama; then
    echo "llama-server already running (pid $(cat "$RUN_DIR/llama.pid"))"
  else
    echo "==> starting llama-server"
    llama-server -m "$MODELS/llm/gemma-4-E2B-it-Q4_0.gguf" -c 4096 -np 1 -fa on \
      --reasoning off -ngl 99 --cache-reuse 256 --host 127.0.0.1 --port 8742 --no-webui \
      >"$LOG_DIR/llama.log" 2>&1 &
    echo $! >"$RUN_DIR/llama.pid"
    wait_http http://127.0.0.1:8742/health 120 || { echo "llama-server not healthy; see var/log/llama.log" >&2; false; }
    echo "    llama-server healthy (pid $(cat "$RUN_DIR/llama.pid"))"
  fi
fi

if pid_alive speech; then
  echo "speech-worker already running (pid $(cat "$RUN_DIR/speech.pid"))"
else
  if [[ ! -x "$WORKER_BIN" ]]; then
    echo "==> building speech-worker (cgo)"
    (cd services/speech-worker && go build -trimpath -o bin/speech-worker ./cmd/speech-worker)
  fi
  echo "==> starting speech-worker"
  "$WORKER_BIN" -models-dir "$MODELS" >"$LOG_DIR/speech.log" 2>&1 &
  echo $! >"$RUN_DIR/speech.pid"
  wait_http http://127.0.0.1:8743/health 90 || { echo "speech-worker not healthy; see var/log/speech.log" >&2; false; }
  echo "    speech-worker healthy (pid $(cat "$RUN_DIR/speech.pid"))"
fi

if pid_alive agent; then
  echo "device-agent already running (pid $(cat "$RUN_DIR/agent.pid"))"
else
  if [[ ! -x "$AGENT_BIN" ]]; then
    echo "==> building device-agent (run 'make build' to embed the kiosk UI)"
    go build -o "$AGENT_BIN" ./services/device-agent/cmd/device-agent
  fi
  echo "==> starting device-agent"
  "$AGENT_BIN" -config "$CONFIG" >"$LOG_DIR/agent.log" 2>&1 &
  echo $! >"$RUN_DIR/agent.pid"
  wait_http http://127.0.0.1:8730/api/v1/status 30 || { echo "device-agent not serving; see var/log/agent.log" >&2; false; }
  echo "    device-agent serving http://127.0.0.1:8730 (pid $(cat "$RUN_DIR/agent.pid"))"
fi
trap - ERR
echo "dev stack up. Kiosk UI: http://127.0.0.1:8730/  Stop: scripts/dev/down.sh"
