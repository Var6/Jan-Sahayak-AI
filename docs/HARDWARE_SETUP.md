# Hardware Setup

No hardware has been purchased. Everything runs on the development Mac with a
simulated button (`hal.button = "sim"`). Buying the pilot kit needs approval
(see `PROGRESS.md`).

## Proposed pilot kit (one kiosk)

| Item | Recommendation | Why |
|---|---|---|
| Computer | Raspberry Pi 5, **8 GB** | Full AI stack needs ~3.5–4 GB plus Chromium/OS; 4 GB unproven (D-013). |
| Storage | NVMe SSD (128–256 GB) on an M.2 HAT, *or* industrial/high-endurance microSD (≥ 64 GB) | SD wear and power-loss resilience. |
| Cooling | Official Active Cooler (or case with fan) | LLM inference throttles a passively cooled Pi. |
| Power | Official 27 W USB-C PSU; optional UPS HAT | Under-voltage causes throttling and corruption risk. |
| Display | 10–15" touchscreen (HDMI + USB touch), ≥ 1024×600 | UI tested at 1024×600, 1280×800, 1920×1080, 800×1280. |
| Microphone | USB microphone with good near-field pickup (or a small USB mic array) | Noisy public spaces. |
| Speaker | Powered speaker (3.5 mm via USB DAC, or USB speaker) | Pi 5 has no analog audio out. |
| Camera (Phase 11) | USB UVC camera, ≥ 5 MP, fixed focus at ~30 cm, or a document camera | Only used after consent. |
| Button | Large illuminated arcade push-button on GPIO (e.g. GPIO17 + GND, LED on GPIO27 via transistor) | Physical session start. |
| Enclosure | Vandal-resistant kiosk enclosure with ventilation | Public location. |

## Software image (Phase 13)

1. Raspberry Pi OS Lite 64-bit (Bookworm or later).
2. `deployment/raspberry-pi/` install scripts: users, directories
   (`/opt/jan-sahayak`, `/var/lib/jan-sahayak`), systemd units, journald
   limits, hardware watchdog, `noatime`, tmpfs `/tmp`, Chromium kiosk.
3. Build natively on the Pi (or arm64 container): llama.cpp v0.9.0 with
   `-DGGML_NATIVE=ON`; speech worker (cgo + sherpa-onnx libs).
4. Cross-compile the Go agent on the Mac:
   `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 make build-pi`.
5. `scripts/models/install.sh pi5`.
6. Provision device identity → register public key with central-api.

## Pi benchmark plan (before finalising model sizes)

| Measure | Pass criterion (initial) |
|---|---|
| STT (IndicConformer) on 5 s utterance | ≤ 1.5 s |
| TTS (Supertonic, 4 steps) RTF | ≤ 0.5 (else 3 steps, else re-evaluate) |
| LLM decode / prompt tok/s, Gemma 4 E2B Q4_0 | ≥ 4 tok/s decode; rephrase ≤ 6 s |
| End-to-end turn (speech in → speech out start) | p50 ≤ 4 s, p95 ≤ 8 s |
| Total RAM in steady state | ≤ 6 GB |
| CPU temperature under 30 min load | < 80 °C, no throttling flags |
| 72 h soak | no RSS growth trend, no restarts except injected faults |
