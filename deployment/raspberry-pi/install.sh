#!/usr/bin/env bash
# Installs Jan Sahayak on a Raspberry Pi 5 (Raspberry Pi OS Lite 64-bit,
# bookworm). Run ON THE PI as root from an unpacked release directory:
#
#   sudo ./install.sh /path/to/release
#
# The release directory must contain:
#   bin/device-agent            (make build-pi on a dev machine)
#   bin/speech-worker           (built natively on the Pi, see README.md)
#   bin/llama-server            (llama.cpp, built on the Pi)
#   lib/libsherpa-onnx-c-api.so lib/libonnxruntime.so
#   models/...                  (scripts/models/install.sh --verify pi5)
#   config/device.pi5.json      deployment/systemd/*  deployment/raspberry-pi/*
#   share/bihar_districts.json
#
# Idempotent: re-running upgrades binaries/units and keeps /var/lib data and
# an existing /etc/jan-sahayak/device.json. It never deletes device data.
set -euo pipefail

REL="${1:?usage: install.sh RELEASE_DIR}"
[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }
[[ "$(uname -m)" == aarch64 ]] || { echo "this installer is for the Raspberry Pi (aarch64)" >&2; exit 1; }
for f in bin/device-agent bin/speech-worker bin/llama-server config/device.pi5.json; do
  [[ -e "$REL/$f" ]] || { echo "missing $REL/$f" >&2; exit 1; }
done

echo "==> verify release (SHA256SUMS, signature if an allowed-signers file is installed)"
# Prefer the verifier installed by a previous release: a verifier shipped in
# the bundle could be replaced together with the bundle. First install has
# no choice but the bundled copy (use known-good media).
VERIFY=/opt/jan-sahayak/bin/verify-release.sh
[[ -x "$VERIFY" ]] || VERIFY="$REL/scripts/release/verify-release.sh"
[[ -x "$VERIFY" ]] || { echo "no verify-release.sh found (release must ship scripts/release/)" >&2; exit 1; }
SIGNERS=/etc/jan-sahayak/release-allowed-signers
if [[ -f "$SIGNERS" ]]; then
  "$VERIFY" --allowed-signers "$SIGNERS" --require-signature "$REL"
else
  echo "    WARNING: $SIGNERS not present — integrity check only (no signature). See docs/DECISION_LOG.md D-025."
  "$VERIFY" "$REL"
fi

echo "==> packages (cage compositor, chromium, curl, tesseract OCR with Hindi + English)"
apt-get install -y --no-install-recommends cage chromium curl tesseract-ocr tesseract-ocr-eng tesseract-ocr-hin

echo "==> users"
id jansahayak >/dev/null 2>&1 || useradd --system --home /var/lib/jan-sahayak --shell /usr/sbin/nologin jansahayak
id jansahayak-ai >/dev/null 2>&1 || useradd --system --home /nonexistent --shell /usr/sbin/nologin jansahayak-ai
id kiosk >/dev/null 2>&1 || useradd --create-home --shell /usr/sbin/nologin kiosk
usermod -aG gpio,video jansahayak
usermod -aG audio,video,input,render kiosk

echo "==> files"
install -d -m 0755 /opt/jan-sahayak/bin /opt/jan-sahayak/lib /opt/jan-sahayak/share /opt/jan-sahayak/models
install -m 0755 "$REL/bin/device-agent" "$REL/bin/speech-worker" "$REL/bin/llama-server" /opt/jan-sahayak/bin/
install -m 0755 "$REL/scripts/release/verify-release.sh" /opt/jan-sahayak/bin/verify-release.sh
if compgen -G "$REL/lib/*.so*" >/dev/null; then install -m 0644 "$REL"/lib/*.so* /opt/jan-sahayak/lib/; fi
if [[ -d "$REL/models" ]]; then cp -R "$REL/models/." /opt/jan-sahayak/models/; fi
chown -R root:jansahayak-ai /opt/jan-sahayak/models && chmod -R u=rwX,g=rX,o= /opt/jan-sahayak/models
if [[ -f "$REL/share/bihar_districts.json" ]]; then install -m 0644 "$REL/share/bihar_districts.json" /opt/jan-sahayak/share/; fi
install -d -m 0750 -o jansahayak -g jansahayak /var/lib/jan-sahayak /var/lib/jan-sahayak/knowledge
install -d -m 0750 -o root -g jansahayak /etc/jan-sahayak
if [[ ! -f /etc/jan-sahayak/device.json ]]; then
  install -m 0640 -o root -g jansahayak "$REL/config/device.pi5.json" /etc/jan-sahayak/device.json
  echo "    wrote /etc/jan-sahayak/device.json — add sync.server_keys and sync.base_url before starting"
fi
if [[ ! -s /etc/jan-sahayak/llm-api-key ]]; then
  # Bearer key shared by llama-server (--api-key-file) and the agent. Root-only
  # here; systemd hands each unit its own copy (LoadCredential=), readable
  # only by that unit's user.
  ( umask 077; head -c 32 /dev/urandom | base64 | tr -d '=+/\n' > /etc/jan-sahayak/llm-api-key )
  echo "    generated /etc/jan-sahayak/llm-api-key"
fi
chown root:root /etc/jan-sahayak/llm-api-key && chmod 0600 /etc/jan-sahayak/llm-api-key
install -d /etc/chromium/policies/managed
install -m 0644 "$REL/deployment/raspberry-pi/chromium-policy.json" /etc/chromium/policies/managed/jan-sahayak.json

echo "==> systemd (needs >= 250 for LoadCredential %d specifiers; bookworm ships 252)"
sdver="$(systemctl --version | awk 'NR==1{print $2}')"
[[ "${sdver:-0}" -ge 250 ]] || { echo "systemd $sdver is too old (need >= 250)" >&2; exit 1; }
llama_help="$(/opt/jan-sahayak/bin/llama-server --help 2>&1 || true)"
for flag in --api-key-file --no-webui; do
  grep -q -- "$flag" <<<"$llama_help" || { echo "llama-server lacks $flag; build a newer llama.cpp" >&2; exit 1; }
done
install -m 0644 "$REL"/deployment/systemd/jan-sahayak-*.service /etc/systemd/system/
install -d /etc/systemd/journald.conf.d
install -m 0644 "$REL/deployment/systemd/journald.conf.d/jan-sahayak.conf" /etc/systemd/journald.conf.d/
systemctl daemon-reload
systemctl restart systemd-journald

echo "==> checks"
sudo -u jansahayak /opt/jan-sahayak/bin/device-agent -config /etc/jan-sahayak/device.json -check-config || {
  echo "config not valid yet: fix /etc/jan-sahayak/device.json, then: systemctl enable --now jan-sahayak-llm jan-sahayak-speech jan-sahayak-agent jan-sahayak-kiosk"
  exit 0
}
systemctl enable --now jan-sahayak-llm.service jan-sahayak-speech.service jan-sahayak-agent.service
systemctl enable jan-sahayak-kiosk.service
systemctl set-default graphical.target
echo "==> device identity (register with central-admin register-device):"
sudo -u jansahayak /opt/jan-sahayak/bin/device-agent -config /etc/jan-sahayak/device.json -print-identity
echo "done. Reboot to start the kiosk display."
