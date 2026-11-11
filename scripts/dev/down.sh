#!/usr/bin/env bash
# Stops everything scripts/dev/up.sh started (agent first, so it can record
# the end of any open session while the database is still there).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
for name in agent speech llama; do
  stop_one "$name"
done
echo "dev stack down."
