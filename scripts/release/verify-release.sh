#!/usr/bin/env bash
# Verify a release directory against its SHA256SUMS before anything is
# installed from it.
#
#   scripts/release/verify-release.sh [--allowed-signers FILE] [--require-signature] RELEASE_DIR
#
# Checks, and exits 1 on the first class of failure (all problems are listed):
#   * SHA256SUMS exists and every line is "<64 hex>  <safe relative path>".
#   * Every listed file exists, is a regular file (not a symlink) and has
#     that SHA-256.
#   * No file in the release is missing from SHA256SUMS (an extra .so or
#     binary would otherwise be copied unchecked). SHA256SUMS(.sig) excepted.
#   * Signature: with --allowed-signers FILE (or env
#     JS_RELEASE_ALLOWED_SIGNERS), SHA256SUMS.sig must exist and verify with
#     `ssh-keygen -Y verify`, identity "jan-sahayak-release", namespace
#     "jan-sahayak-release". --require-signature fails when no allowed-signers
#     file is given. Without either, the check is integrity-only and says so.
#
# Trust: a verifier inside the bundle can be replaced together with the
# bundle. Run a trusted copy (one installed by a previous release at
# /opt/jan-sahayak/bin/verify-release.sh, or from known-good media) and keep
# the allowed-signers file on the device, not in the bundle.
#
# Exit codes: 0 verified, 1 verification failed, 2 usage error.
set -euo pipefail

usage() { echo "usage: verify-release.sh [--allowed-signers FILE] [--require-signature] RELEASE_DIR" >&2; exit 2; }
SIGNERS="${JS_RELEASE_ALLOWED_SIGNERS:-}"
REQUIRE_SIG=0
REL=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --allowed-signers) SIGNERS="${2:?--allowed-signers needs a file}"; shift 2 ;;
    --require-signature) REQUIRE_SIG=1; shift ;;
    -*) usage ;;
    *) [[ -z "$REL" ]] || usage; REL="$1"; shift ;;
  esac
done
[[ -n "$REL" && -d "$REL" ]] || usage
REL="$(cd "$REL" && pwd)"
IDENTITY="jan-sahayak-release"
NAMESPACE="jan-sahayak-release"
SUMS="$REL/SHA256SUMS"

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

fail() { echo "FAIL      $*"; bad=1; }
bad=0

[[ -f "$SUMS" && ! -L "$SUMS" ]] || { echo "FAIL      $SUMS missing — refusing to install an unverified release"; exit 1; }

# 1. Signature (authenticity) first: no point hashing files listed by a
#    forged SHA256SUMS.
if [[ -n "$SIGNERS" ]]; then
  [[ -f "$SIGNERS" ]] || { echo "FAIL      allowed-signers file $SIGNERS not found"; exit 1; }
  [[ -f "$SUMS.sig" ]] || { echo "FAIL      SHA256SUMS.sig missing but a signature is required"; exit 1; }
  if ssh-keygen -Y verify -f "$SIGNERS" -I "$IDENTITY" -n "$NAMESPACE" -s "$SUMS.sig" < "$SUMS" >/dev/null 2>&1; then
    echo "ok        SHA256SUMS signature ($IDENTITY)"
  else
    echo "FAIL      SHA256SUMS signature does not verify against $SIGNERS"; exit 1
  fi
elif [[ $REQUIRE_SIG == 1 ]]; then
  echo "FAIL      --require-signature given but no allowed-signers file (--allowed-signers or JS_RELEASE_ALLOWED_SIGNERS)"; exit 1
else
  echo "WARNING   no allowed-signers file: checking integrity only (corruption, partial copy, swapped files), not who built the release"
fi

# 2. Every listed file matches. Bounded: at most 20000 entries.
#    (Plain bash 3.2 constructs: macOS still ships bash 3.2.)
WORK="$(mktemp -d "${TMPDIR:-/tmp}/js-verify.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
: > "$WORK/listed"
lines=0
while IFS= read -r line || [[ -n "$line" ]]; do
  lines=$((lines + 1))
  [[ $lines -le 20000 ]] || { fail "SHA256SUMS has more than 20000 lines"; break; }
  if [[ ! "$line" =~ ^([0-9a-f]{64})\ \ ([A-Za-z0-9._/+@-]+)$ ]]; then fail "malformed SHA256SUMS line $lines"; continue; fi
  sum="${BASH_REMATCH[1]}"; p="${BASH_REMATCH[2]}"
  if [[ "$p" == /* || "$p" == *..* ]]; then fail "unsafe path $p"; continue; fi
  printf '%s\n' "$p" >> "$WORK/listed"
  f="$REL/$p"
  if [[ -L "$f" ]]; then fail "$p is a symlink"; continue; fi
  if [[ ! -f "$f" ]]; then fail "$p missing"; continue; fi
  got="$(sha256 "$f")"
  [[ "$got" == "$sum" ]] || fail "$p hash mismatch (have $got)"
done < "$SUMS"
nlisted="$(wc -l < "$WORK/listed" | tr -d ' ')"
[[ "$nlisted" -gt 0 ]] || fail "SHA256SUMS lists no files"
LC_ALL=C sort "$WORK/listed" > "$WORK/listed.sorted"
while IFS= read -r p; do fail "duplicate entry $p"; done < <(LC_ALL=C uniq -d "$WORK/listed.sorted")

# 3. Nothing unlisted (and no symlinks / special files anywhere).
: > "$WORK/present"
while IFS= read -r -d '' f; do
  p="${f#"$REL"/}"
  case "$p" in SHA256SUMS|SHA256SUMS.sig) continue ;; esac
  if [[ -L "$f" ]]; then fail "$p is a symlink"; continue; fi
  if [[ ! -f "$f" ]]; then fail "$p is not a regular file"; continue; fi
  printf '%s\n' "$p" >> "$WORK/present"
done < <(find "$REL" ! -type d -print0)
LC_ALL=C sort -u "$WORK/present" > "$WORK/present.sorted"
LC_ALL=C sort -u "$WORK/listed.sorted" > "$WORK/listed.uniq"
while IFS= read -r p; do fail "$p is not listed in SHA256SUMS"; done < <(LC_ALL=C comm -23 "$WORK/present.sorted" "$WORK/listed.uniq")

if [[ $bad == 0 ]]; then
  echo "verified  $nlisted files in $REL"
  exit 0
fi
echo "release verification FAILED — do not install from $REL"
exit 1
