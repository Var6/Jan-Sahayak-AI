#!/usr/bin/env bash
# Builds the speech worker (cgo + sherpa-onnx) and runs the speech
# benchmark: STT latency/RTF/accuracy on var/testaudio, TTS latency/RTF,
# a TTS→STT round trip, and the worker's resident memory.
# The benchmark starts the worker itself and always stops it at the end.
#
# Usage: scripts/bench/speech.sh [-threads N] [-runs N]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

echo "building speech-worker..."
(cd services/speech-worker && go build -trimpath -o bin/speech-worker ./cmd/speech-worker)

exec go run ./scripts/bench/speechbench \
  -worker services/speech-worker/bin/speech-worker \
  -models-dir models/weights \
  -audio-dir var/testaudio \
  "$@"
