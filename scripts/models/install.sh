#!/usr/bin/env bash
# Install local AI model weights for a hardware profile.
#
#   scripts/models/install.sh dev-mac      # or: pi5
#   scripts/models/install.sh --verify dev-mac
#
# Reads models/manifests/models.json. Downloads only files that are missing,
# verifies SHA-256 BEFORE moving anything into models/weights/, and never
# runs at application startup. Requires: curl, shasum or sha256sum, tar, and
# a JSON reader (jq, or python3/go as fallback — dev machines only).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
MANIFEST="$ROOT/models/manifests/models.json"
WEIGHTS="${JS_MODELS_DIR:-$ROOT/models/weights}"
VERIFY_ONLY=0
if [[ "${1:-}" == "--verify" ]]; then VERIFY_ONLY=1; shift; fi
PROFILE="${1:?usage: install.sh [--verify] <dev-mac|pi5>}"

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

# Emit TSV lines: kind \t path \t url \t sha256 \t extract_to
list_items() {
  if command -v jq >/dev/null; then
    jq -r --arg p "$PROFILE" '
      (.profiles[$p] // error("unknown profile")) as $ids
      | .models[] | select(.id as $i | $ids | index($i))
      | if .archive then "archive\t\(.files[0].path)\t\(.archive.url)\t\(.archive.sha256)\t\(.archive.extract_to)"
        else (.files[] | "file\t\(.path)\t\(.url)\t\(.sha256)\t-") end' "$MANIFEST"
  else
    python3 -I - "$MANIFEST" "$PROFILE" <<'PY'
import json, sys
m = json.load(open(sys.argv[1])); ids = m["profiles"][sys.argv[2]]
for x in m["models"]:
    if x["id"] not in ids: continue
    if "archive" in x:
        a = x["archive"]; print("\t".join(["archive", x["files"][0]["path"], a["url"], a["sha256"], a["extract_to"]]))
    else:
        for f in x["files"]: print("\t".join(["file", f["path"], f["url"], f["sha256"], "-"]))
PY
  fi
}

mkdir -p "$WEIGHTS"
STAGE="$(mktemp -d "${TMPDIR:-/tmp}/js-models.XXXXXX")"
trap 'rm -rf "$STAGE"' EXIT
fail=0

while IFS=$'\t' read -r kind path url sum extract; do
  dest="$WEIGHTS/$path"
  if [[ "$kind" == "file" ]]; then
    if [[ -f "$dest" ]]; then
      got="$(sha256 "$dest")"
      if [[ "$got" == "$sum" ]]; then echo "ok        $path"; continue; fi
      echo "MISMATCH  $path (have $got)"; fail=1
      [[ $VERIFY_ONLY == 1 ]] && continue
      mv "$dest" "$dest.bad.$(date +%s)"   # keep for inspection; never silently overwrite
    fi
    [[ $VERIFY_ONLY == 1 ]] && { echo "MISSING   $path"; fail=1; continue; }
    echo "download  $path"
    tmp="$STAGE/$(basename "$path")"
    curl -fL --retry 3 --proto '=https' -sS -o "$tmp" "$url"
    got="$(sha256 "$tmp")"
    [[ "$got" == "$sum" ]] || { echo "CHECKSUM FAILED for $path: $got != $sum"; exit 2; }
    mkdir -p "$(dirname "$dest")"; mv "$tmp" "$dest"; echo "installed $path"
  else
    if [[ -f "$dest" ]]; then echo "ok        $path (archive already extracted)"; continue; fi
    [[ $VERIFY_ONLY == 1 ]] && { echo "MISSING   $path"; fail=1; continue; }
    echo "download  $url"
    tmp="$STAGE/archive.tar.bz2"
    curl -fL --retry 3 --proto '=https' -sS -o "$tmp" "$url"
    got="$(sha256 "$tmp")"
    [[ "$got" == "$sum" ]] || { echo "CHECKSUM FAILED for $url"; exit 2; }
    mkdir -p "$WEIGHTS/$extract"
    tar -xjf "$tmp" -C "$WEIGHTS/$extract" --no-same-owner
    echo "installed $path"
  fi
done < <(list_items)

exit $fail
