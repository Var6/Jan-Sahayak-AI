# Raspberry Pi 5 deployment

Status: written and reviewed on the developer Mac. **Nothing here has been run on a
Pi yet**, including `install.sh`, the systemd units, the GPIO button code and the
Chromium kiosk set-up. Treat it as a checklist for the first hardware bring-up.

## Processes

| Unit | Binary | Listens | Memory cap (provisional) |
|---|---|---|---|
| `jan-sahayak-agent` (Type=notify, WatchdogSec=30) | `device-agent` (Go, static, CGO_ENABLED=0) | 127.0.0.1:8730 | MemoryMax 256M, GOMEMLIMIT 200 MB |
| `jan-sahayak-speech` | `speech-worker` (Go + cgo + sherpa-onnx) | 127.0.0.1:8743 | MemoryMax 900M (570 MiB peak measured on M1) |
| `jan-sahayak-llm` | `llama-server` (llama.cpp) | 127.0.0.1:8742 | MemoryMax 2560M |
| `jan-sahayak-kiosk` | `cage` + `chromium --kiosk` | — | MemoryMax 1G |

Every unit has `Restart=always`, `RestartSec=5` and `StartLimitIntervalSec=0`
(no start rate limit: a persistent startup error is retried forever instead of
leaving the unit failed until a reboot; bookworm's systemd 252 has no
`RestartSteps`, so the delay is fixed), and its own memory cap. The AI processes may
only talk to localhost (`IPAddressDeny=any`, `IPAddressAllow=localhost`).
journald is capped at 50 MB and 14 days (`journald.conf.d/jan-sahayak.conf`).

## Build

On the dev machine:

```sh
make build-pi        # kiosk UI + embed + linux/arm64 agent → bin/linux-arm64/device-agent
```

On the Pi itself (cgo cannot be cross-compiled from macOS — no Linux C
toolchain there):

```sh
sudo apt-get install -y golang-go build-essential cmake   # Go ≥ 1.26 needed: use the go.dev tarball if apt's is older
cd services/speech-worker && go build -trimpath -o bin/speech-worker ./cmd/speech-worker
# The sherpa-onnx shared libraries come from the Go module cache:
cp "$(go env GOMODCACHE)"/github.com/k2-fsa/sherpa-onnx-go-linux/v2@v2.0.1/lib/aarch64-unknown-linux-gnu/*.so release/lib/
# llama.cpp: build llama-server with cmake (CPU, -DGGML_NATIVE=ON) and copy build/bin/llama-server.
```

The speech worker's library search path must point at `/opt/jan-sahayak/lib`.
The unit sets `LD_LIBRARY_PATH`; linking with
`-ldflags='-extldflags=-Wl,-rpath,/opt/jan-sahayak/lib'` is the cleaner fix.

## Install

```sh
sudo ./deployment/raspberry-pi/install.sh /path/to/release
```

The script creates the users (`jansahayak` for the agent, `jansahayak-ai`
for the runtimes, `kiosk` for the display), installs the binaries, models, units,
journald drop-in and the Chromium policy, and keeps any existing
`/etc/jan-sahayak/device.json` and everything under `/var/lib/jan-sahayak`.

`config/device.pi5.json` **fails validation until it is provisioned**, on purpose:

* `sync.server_keys`: the central server's bundle and ack signing keys
  (`central-admin server-public-key -key …` prints `{"key_id", "public_key"}`).
  Without them the agent cannot verify a knowledge bundle, and production
  profiles accept only signed bundles.
* `sync.base_url`: the central API URL (https only).
* `knowledge.path`: a signed bundle at `/var/lib/jan-sahayak/knowledge/bundle.json`.

Then register the device's public key with the central server:

```sh
sudo -u jansahayak /opt/jan-sahayak/bin/device-agent -config /etc/jan-sahayak/device.json -print-identity
central-admin register-device -id <device_id> -public-key <public_key> -name … -district …
```

## Session button (GPIO)

`hal.button = "gpio"` reads a momentary push button through the gpiochip
character device (GPIO uAPI v2, kernel edge detection and debounce; no sysfs, no
polling). Default wiring: GPIO17 (header pin 11) to ground (pin 9), internal
pull-up on, active-low, 30 ms debounce, presses closer than 1 s ignored.

On current Pi 5 kernels the header GPIOs are `/dev/gpiochip0`; older 6.1
kernels exposed them as `/dev/gpiochip4`. Check with `gpioinfo` (package
`gpiod`) and adjust `hal.gpio_chip`, and the unit's `DeviceAllow=` line to match.

A press with no session open starts one: the greeting arrives in the SSE
`session.started` event. A press during a session counts as interaction. With
the GPIO button, `POST /api/v1/session/start` returns 403 `button_disabled`.

## Kiosk display

Chromium runs inside `cage` (a Wayland kiosk compositor) on tty1 with a
tmpfs profile (`/run/jan-sahayak-kiosk`), `--incognito`,
`--autoplay-policy=no-user-gesture-required` (button-started sessions have no tap
to unlock audio), and the managed policy `chromium-policy.json`:

* microphone and camera auto-granted for `http://127.0.0.1:8730` only (the
  UI opens the camera only after the citizen consents to the document check;
  the agent refuses document uploads without that consent);
* every other URL blocked;
* no password manager, autofill, sync, translate or dev tools.

The agent sends the Content-Security-Policy as a header and serves `.js` as
`text/javascript`, which the AudioWorklet needs.

## Degraded operation

The agent keeps serving citizens when a component fails:

| Failure | What the citizen sees | What the agent does |
|---|---|---|
| Speech worker down or loading | Keyboard and option buttons, no mic | `capabilities.voice_input/voice_output=false`; `/listen` and `/audio` return 503 `unavailable`; replies come without `audio_url` |
| TTS fails on a reply | Text on screen, no audio | Reply returned without `audio_url`; `tts` health reported |
| llama-server down | Same answers, no rephrasing | Gated LLM client returns "unavailable" without network calls; dialogue is deterministic |
| Database fails | Start screen says the kiosk needs attention | `database` goes REQUIRES_ATTENTION; session start (UI or button) refused with 503, because sessions and analytics could not be recorded safely |
| Agent hangs | Brief restart | No `WATCHDOG=1` while DB ping or HTTP self-probe fail, or (after a 90 s grace) the health, session or sync loop stops stamping its heartbeat; systemd restarts it after 30 s |
| Speech engine hangs inside a native call | Keyboard and options only for a moment | The worker's `/health` turns 503 `wedged` once an inference holds its slot for 3× its timeout, and the worker exits(1); systemd restarts it. Three "busy" answers in a row also mark STT/TTS unavailable in the agent |
| Speech worker stuck loading > 5 min | Keyboard and options | `speech_worker` goes REQUIRES_ATTENTION (`speech_loading_stuck`) |
| GPIO button missing or not ready | Start screen; no session can start | Agent starts anyway; `button` (critical) is UNAVAILABLE and escalates to REQUIRES_ATTENTION; the line is retried with backoff up to 30 s |
| Outbox full (100k events) | Nothing changes for the citizen | New analytics events are refused (none dropped); `outbox` (critical) goes REQUIRES_ATTENTION |

The agent backs up `device.db` every day at 03:00 local time, postponed while
a session is open, and keeps 7 copies in `/var/lib/jan-sahayak/backups`. A
corrupt database is quarantined at boot (never deleted) and the newest good
backup is restored.

## Local LLM access

`llama-server` runs with `--no-webui` and `--api-key-file`. `install.sh`
generates `/etc/jan-sahayak/llm-api-key` (root, 0600) once; both the LLM unit
and the agent unit load it with `LoadCredential=llm-api-key:…`, so each gets a
private copy under `$CREDENTIALS_DIRECTORY` (`%d`) readable only by its own
service user. The agent reads it via `JS_LLM_API_KEY_FILE` (or
`llm.api_key_file`) and refuses a key file readable by group/others on
non-dev profiles. To rotate: replace the file, then restart both units.

## Profiles on the Pi

`dev-*` profiles are refused on linux/arm64 unless `JS_ALLOW_DEV_PROFILE=1` is
set (bench devices only). `pprof_addr` and `hal.button = "sim"` are refused on
non-dev profiles. `llm.rephrase` is off in `device.pi5.json` until the
allowlist validator is field-validated; LLM classification stays on.

## Not done / to verify on hardware

* All units: run `systemd-analyze security` and `systemd-analyze verify`.
  Then confirm the syscall filters don't break onnxruntime or llama.cpp.
* GPIO: the ioctl structs are checked against the kernel's sizes in unit
  tests (592-byte request, 48-byte event), but the button has never been
  pressed on a real Pi.
* `vcgencmd get_throttled` requires `/dev/vcio` access (group `video`).
* Memory caps above are provisional until the Pi benchmark (roadmap Phase 13).

## Document check (OCR)

The document check (docs/API_DOCUMENTATION.md, "Document check") runs
`tesseract` as a short-lived subprocess of the agent. Each photo goes in on
stdin and the text comes out on stdout.

```sh
sudo apt-get install -y --no-install-recommends tesseract-ocr tesseract-ocr-eng tesseract-ocr-hin
tesseract --version          # bookworm ships 5.3.x; stdin input ("-") needs ≥ 4
tesseract --list-langs       # must list eng and hin
```

`install.sh` installs these packages. The agent finds `tesseract` on `PATH`
(`documents.tesseract_path`, default `tesseract`, which is
`/usr/bin/tesseract` on Pi OS). It uses the package's tessdata unless
`documents.tessdata_dir` is set. Without tesseract, or without the `hin`
data, the `ocr` health component is UNAVAILABLE and the kiosk simply does
not offer the document check (`capabilities.camera = false`). Nothing else
degrades.

Camera: any UVC USB camera, or the Pi camera exposed as a V4L2 device. The
`kiosk` user is in the `video` group and the Chromium policy grants video
capture to the kiosk origin only.

Limits (config `documents.*`): OCR timeout 15 s, one process at a time,
`OMP_THREAD_LIMIT=1`, stdout ≤ 64 KB, `RLIMIT_AS` 512 MB (applied with
`prlimit` before the image is sent), JPEG ≤ 2 MB and ≤ 1600 px.

**Memory budget (to verify on the Pi).** tesseract runs inside the agent's
cgroup, so it counts against `MemoryMax=256M` of `jan-sahayak-agent`.
Measured on the M1: agent RSS 24–26 MB during the check, and tesseract peak
RSS 48 MB for a 1000×560 page and 52 MB for a 1600×1524 page. Together that
fits under 256 MB. If the Pi shows tesseract being OOM-killed, the agent keeps
running: the cgroup OOM killer picks the largest process, which is
tesseract. The check fails with 503, `ocr` goes UNAVAILABLE until the next
self-test, and the cap should then be raised.

To verify on hardware:

* `strace -f -e trace=openat,creat -p <agent pid>` during a document check:
  no file may be created. Leptonica should decode from memory (`fmemopen`).
* tesseract runs under the agent's seccomp filter (`@system-service`) and
  `MemoryDenyWriteExecute=yes`. Run one check and look for EPERM in the
  journal.
* OCR latency on the Pi 5. M1: 0.13–0.28 s per synthetic page through the
  API, 0.6 s for a 1600-px page.

