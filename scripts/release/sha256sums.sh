#!/usr/bin/env bash
# Write SHA256SUMS for a release directory (and optionally sign it).
#
#   scripts/release/sha256sums.sh RELEASE_DIR [-k SIGNING_KEY]
#
# Lists every regular file under RELEASE_DIR (binaries, .so libraries,
# models, config, units, scripts) as "<sha256>  <relative path>", sorted, in
# the format `sha256sum -c` reads. SHA256SUMS and SHA256SUMS.sig themselves
# are not listed. The file is written atomically.
#
# Refuses symlinks, device files and file names with characters outside
# [A-Za-z0-9._/+@-] (spaces, newlines, quotes), so the list is unambiguous and
# the Pi installer cannot be tricked into copying something that is not
# covered.
#
# -k SIGNING_KEY: also writes SHA256SUMS.sig with OpenSSH signing
# (`ssh-keygen -Y sign`, namespace "jan-sahayak-release"; ships with
# OpenSSH >= 8.1 on macOS and Raspberry Pi OS — no new dependency). The
# matching public key goes into the allowed-signers file the Pi uses with
# scripts/release/verify-release.sh. Keep the private key off build
# machines that are shared; who holds it is a human decision.
set -euo pipefail

usage() { echo "usage: sha256sums.sh RELEASE_DIR [-k SIGNING_KEY]" >&2; exit 2; }
[[ $# -ge 1 ]] || usage
REL="$1"; shift
KEY=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -k) KEY="${2:?-k needs a key file}"; shift 2 ;;
    *) usage ;;
  esac
done
[[ -d "$REL" ]] || { echo "not a directory: $REL" >&2; exit 2; }
REL="$(cd "$REL" && pwd)"
NAMESPACE="jan-sahayak-release"

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

cd "$REL"
# Anything that is not a regular file or directory is refused.
odd="$(find . \( -type l -o \( ! -type f ! -type d \) \) -print | head -n 5)"
[[ -z "$odd" ]] || { echo "refusing: symlinks or special files in release:" >&2; echo "$odd" >&2; exit 1; }

tmp="$(mktemp "$REL/.SHA256SUMS.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
n=0
while IFS= read -r -d '' f; do
  p="${f#./}"
  case "$p" in SHA256SUMS|SHA256SUMS.sig|.SHA256SUMS.*) continue ;; esac
  [[ "$p" =~ ^[A-Za-z0-9._/+@-]+$ && "$p" != *..* ]] || { echo "refusing file name: $p" >&2; exit 1; }
  printf '%s  %s\n' "$(sha256 "$p")" "$p" >> "$tmp"
  n=$((n + 1))
done < <(find . -type f -print0 | LC_ALL=C sort -z)
[[ $n -gt 0 ]] || { echo "no files in $REL" >&2; exit 1; }

chmod 0644 "$tmp"
rm -f "$REL/SHA256SUMS.sig"   # a signature of the previous list must not survive
mv "$tmp" "$REL/SHA256SUMS"
trap - EXIT
echo "wrote $REL/SHA256SUMS ($n files)"

if [[ -n "$KEY" ]]; then
  ssh-keygen -Y sign -q -f "$KEY" -n "$NAMESPACE" "$REL/SHA256SUMS"
  echo "wrote $REL/SHA256SUMS.sig (namespace $NAMESPACE)"
else
  echo "NOTE: SHA256SUMS is not signed (no -k). It detects corruption and partial copies, not a malicious replacement of the whole bundle."
fi
