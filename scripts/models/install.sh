#!/usr/bin/env bash
# Install local AI model weights for a hardware profile.
#
#   scripts/models/install.sh dev-mac      # or: pi5
#   scripts/models/install.sh --verify dev-mac
#
# Reads models/manifests/models.json. Every file in the manifest has a pinned
# SHA-256, including each file inside an archive.
#
# Install: downloads only what is missing or fails its hash. Single files and
# archives are SHA-256 checked BEFORE anything is moved into models/weights/.
# An archive is extracted into a private staging directory, every listed file
# is checked against its own hash, and only the listed files are moved into
# place (anything else in the archive is discarded with the staging dir).
# A file that fails its hash is renamed to <file>.bad.<time> for inspection,
# never silently overwritten.
#
# --verify: downloads nothing and re-hashes EVERY listed file (single files
# and archive members). Exit 1 if anything is missing or does not match.
#
# Never runs at application startup. Requires: curl, shasum or sha256sum,
# tar with bzip2, and a JSON reader (jq, or python3 as fallback).
#
# Env: JS_MODELS_DIR overrides the weights directory (default models/weights);
# JS_MODELS_MANIFEST overrides the manifest path (tests).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
MANIFEST="${JS_MODELS_MANIFEST:-$ROOT/models/manifests/models.json}"
WEIGHTS="${JS_MODELS_DIR:-$ROOT/models/weights}"
VERIFY_ONLY=0
if [[ "${1:-}" == "--verify" ]]; then VERIFY_ONLY=1; shift; fi
PROFILE="${1:?usage: install.sh [--verify] <dev-mac|pi5>}"

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

# Bounded downloads: give up if the connection cannot be made in 30 s or the
# transfer stays below 10 KiB/s for 2 minutes; refuse bodies much larger than
# the manifest says.
fetch() { # fetch <url> <out> <expected-bytes or 0>
  local url="$1" out="$2" bytes="$3" limit=()
  if [[ "$bytes" =~ ^[0-9]+$ && "$bytes" -gt 0 ]]; then limit=(--max-filesize "$((bytes + 1048576))"); fi
  curl -fL --retry 3 --retry-all-errors --proto '=https' --proto-redir '=https' -sS \
    --connect-timeout 30 --speed-limit 10240 --speed-time 120 \
    ${limit[@]+"${limit[@]}"} -o "$out" "$url"
}

# Emit TSV lines (no empty fields: "-" is the placeholder, because bash
# `read` collapses consecutive tabs):
#   file    <path> <url> <sha256> -          <bytes>
#   archive <id>   <url> <sha256> <extract>  <bytes>
#   member  <path> <id>  <sha256> <extract>  <bytes>
list_items() {
  if command -v jq >/dev/null; then
    jq -r --arg p "$PROFILE" '
      (.profiles[$p] // error("unknown profile \($p)")) as $ids
      | .models[] | select(.id as $i | $ids | index($i))
      | . as $m
      | if .archive then
          ("archive\t\(.id)\t\(.archive.url)\t\(.archive.sha256 // "-")\t\(.archive.extract_to)\t\(.archive.bytes // 0)"),
          (.files[] | "member\t\(.path)\t\($m.id)\t\(.sha256 // "-")\t\($m.archive.extract_to)\t\(.bytes // 0)")
        else (.files[] | "file\t\(.path)\t\(.url)\t\(.sha256 // "-")\t-\t\(.bytes // 0)") end' "$MANIFEST"
  else
    python3 -I - "$MANIFEST" "$PROFILE" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
if sys.argv[2] not in m["profiles"]:
    sys.exit("unknown profile " + sys.argv[2])
ids = m["profiles"][sys.argv[2]]
for x in m["models"]:
    if x["id"] not in ids:
        continue
    if "archive" in x:
        a = x["archive"]
        print("\t".join(["archive", x["id"], a["url"], a.get("sha256") or "-", a["extract_to"], str(a.get("bytes", 0))]))
        for f in x["files"]:
            print("\t".join(["member", f["path"], x["id"], f.get("sha256") or "-", a["extract_to"], str(f.get("bytes", 0))]))
    else:
        for f in x["files"]:
            print("\t".join(["file", f["path"], f["url"], f.get("sha256") or "-", "-", str(f.get("bytes", 0))]))
PY
  fi
}

# Manifest paths must be plain relative paths inside the weights dir.
safe_path() { [[ "$1" != /* && "$1" != *..* && "$1" =~ ^[A-Za-z0-9._/-]+$ ]]; }

ITEMS="$(list_items)"
[[ -n "$ITEMS" ]] || { echo "no models for profile $PROFILE"; exit 2; }

# Manifest sanity first: every entry needs a sha256 and a safe path.
bad_manifest=0
while IFS=$'\t' read -r kind path _ sum _ _; do
  if [[ ! "$sum" =~ ^[0-9a-f]{64}$ ]]; then echo "MANIFEST  $kind $path has no valid sha256"; bad_manifest=1; fi
  if [[ "$kind" != archive ]] && ! safe_path "$path"; then echo "MANIFEST  unsafe path $path"; bad_manifest=1; fi
done <<< "$ITEMS"
[[ $bad_manifest == 0 ]] || exit 2

mkdir -p "$WEIGHTS"
STAGE="$(mktemp -d "${TMPDIR:-/tmp}/js-models.XXXXXX")"
trap 'rm -rf "$STAGE"' EXIT
fail=0

# check_file <dest> <sum> <label>: prints ok/MISMATCH/MISSING; returns 0 if ok.
check_file() {
  local dest="$1" sum="$2" label="$3" got
  if [[ ! -f "$dest" ]]; then echo "MISSING   $label"; return 1; fi
  got="$(sha256 "$dest")"
  if [[ "$got" == "$sum" ]]; then echo "ok        $label"; return 0; fi
  echo "MISMATCH  $label (have $got, want $sum)"
  return 1
}

# set_aside <dest>: keep a bad file for inspection; never overwrite silently.
set_aside() { [[ -f "$1" ]] && mv "$1" "$1.bad.$(date +%s)"; return 0; }

# install_archive <id> <url> <sum> <extract_to> <bytes>
install_archive() {
  local id="$1" url="$2" sum="$3" extract="$4" bytes="$5"
  local tmp="$STAGE/$id.archive" x="$STAGE/$id.x" got path msum rel
  echo "download  $url"
  fetch "$url" "$tmp" "$bytes"
  got="$(sha256 "$tmp")"
  [[ "$got" == "$sum" ]] || { echo "CHECKSUM FAILED for archive $id: $got != $sum"; exit 2; }
  mkdir -p "$x"
  tar -xjf "$tmp" -C "$x" --no-same-owner
  rm -f "$tmp"
  # Check every member in staging before moving any of them.
  while IFS=$'\t' read -r kind path mid msum _ _; do
    [[ "$kind" == member && "$mid" == "$id" ]] || continue
    rel="${path#"$extract"}"
    [[ -f "$x/$rel" ]] || { echo "ARCHIVE $id lacks $rel"; exit 2; }
    got="$(sha256 "$x/$rel")"
    [[ "$got" == "$msum" ]] || { echo "CHECKSUM FAILED for $path (from archive $id): $got != $msum"; exit 2; }
  done <<< "$ITEMS"
  while IFS=$'\t' read -r kind path mid msum _ _; do
    [[ "$kind" == member && "$mid" == "$id" ]] || continue
    rel="${path#"$extract"}"
    if [[ -f "$WEIGHTS/$path" ]]; then
      # Keep members that already match; set aside only the bad ones.
      [[ "$(sha256 "$WEIGHTS/$path")" == "$msum" ]] && continue
      set_aside "$WEIGHTS/$path"
    fi
    mkdir -p "$(dirname "$WEIGHTS/$path")"
    mv "$x/$rel" "$WEIGHTS/$path"
    echo "installed $path"
  done <<< "$ITEMS"
  rm -rf "$x"
}

# Pass 1: single files, and decide which archives are needed.
need_archive=" "
while IFS=$'\t' read -r kind path url sum extract bytes; do
  case "$kind" in
    file)
      dest="$WEIGHTS/$path"
      if check_file "$dest" "$sum" "$path"; then continue; fi
      fail=1
      [[ $VERIFY_ONLY == 1 ]] && continue
      set_aside "$dest"
      echo "download  $path"
      tmp="$STAGE/$(basename "$path")"
      fetch "$url" "$tmp" "$bytes"
      got="$(sha256 "$tmp")"
      [[ "$got" == "$sum" ]] || { echo "CHECKSUM FAILED for $path: $got != $sum"; exit 2; }
      mkdir -p "$(dirname "$dest")"; mv "$tmp" "$dest"; echo "installed $path"
      ;;
    member)
      # url column holds the archive id for members.
      if ! check_file "$WEIGHTS/$path" "$sum" "$path"; then
        fail=1
        [[ "$need_archive" == *" $url "* ]] || need_archive+="$url "
      fi
      ;;
  esac
done <<< "$ITEMS"

if [[ $VERIFY_ONLY == 1 ]]; then
  [[ $fail == 0 ]] && echo "verify: all files match the manifest" || echo "verify: FAILED (see MISSING / MISMATCH above)"
  exit $fail
fi

# Pass 2: fetch and install archives whose members are missing or bad.
while IFS=$'\t' read -r kind id url sum extract bytes; do
  [[ "$kind" == archive && "$need_archive" == *" $id "* ]] || continue
  install_archive "$id" "$url" "$sum" "$extract" "$bytes"
done <<< "$ITEMS"

# Every failure above was repaired or aborted the script (exit 2).
exit 0
