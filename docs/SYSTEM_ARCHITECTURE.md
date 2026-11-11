# Jan Sahayak AI — System Architecture

Status: living document. Last updated 2026-11-11 (through Phase 12: speech worker, document check, heartbeat, bundle sync, admin dashboard).

## 1. Shape of the system

Two primary applications plus one central service:

```
                         ┌──────────────────────────────────────────────┐
                         │ CENTRAL (server / cloud VM)                  │
                         │  central-api (Go)  ── PostgreSQL             │
                         │  admin-dashboard (Next.js) → central-api     │
                         └───────────────▲──────────────────────────────┘
                                         │ HTTPS, Ed25519-signed device requests,
                                         │ idempotent event batches + signed acks
┌────────────────────────────────────────┴─────────────────────────────────────┐
│ KIOSK DEVICE (dev: macOS M1 · prod: Raspberry Pi 5, Pi OS Lite 64-bit)        │
│                                                                               │
│  Chromium (kiosk mode) ── http://127.0.0.1:8730 ──┐                           │
│    citizen-kiosk (React+Vite+TS, static files     │                           │
│    served by the agent; UI only, no device ops)   ▼                           │
│                                    ┌──────────────────────────────┐           │
│   physical button (GPIO) ───────▶  │ device-agent (Go, 1 binary)  │           │
│   USB mic / speaker / camera ◀──▶  │  api · session · dialogue    │           │
│                                    │  knowledge · store(SQLite)   │           │
│                                    │  sync · health · hal · logs  │           │
│                                    └──┬────────┬────────┬─────────┘           │
│                       localhost HTTP  │        │        │  subprocess/HTTP    │
│                       ┌───────────────▼┐ ┌─────▼──────┐ ┌▼──────────────┐     │
│                       │ speech-worker  │ │ llama-server│ │ tesseract    │     │
│                       │ (Go+sherpa-onnx│ │ (C++, LLM)  │ │ (per photo,  │     │
│                       │  STT, VAD, TTS)│ │             │ │  stdin/out)  │     │
│                       └────────────────┘ └────────────┘ └───────────────┘     │
│   systemd supervises every process; each has MemoryMax / restart policy.      │
└───────────────────────────────────────────────────────────────────────────────┘
```

Design rules:

1. **One Go device agent** owns all critical device behaviour: sessions, storage,
   sync, health, hardware, the conversation engine and the local API.
   It is a single modular binary, not a set of microservices.
2. **AI runtimes are separate native processes** (llama-server, the speech
   worker for STT/VAD/TTS, and one tesseract process per document photo).
   If one crashes or is OOM-killed, the agent marks it `UNAVAILABLE`, keeps the
   kiosk usable (text input, deterministic answers) and systemd restarts it.
   The agent never links model weights into its own address space, so a model
   bug cannot corrupt the database or the session state.
3. **The browser is a display.** It renders state pushed by the agent (SSE),
   captures microphone audio only when the agent says the session is
   `LISTENING`, and plays audio the agent returns. The agent re-checks session
   state on every request; the UI cannot start the mic or camera on its own
   authority.
4. **The LLM is never the source of facts.** Facts come from human-verified
   scheme records. The LLM may classify, ask clarifying questions and rephrase
   verified text; a validator rejects any output that introduces numbers, URLs,
   phone numbers or scheme names not present in the retrieved records.
5. **Offline first.** Every citizen interaction completes without internet.
   Events are written to SQLite first and synced later with acknowledgements.

## 2. Device agent internal packages

`services/device-agent/` (Go module `github.com/janmanindia/jan-sahayak`, see repo `go.mod`)

| Package | Responsibility |
|---|---|
| `cmd/device-agent` | Wiring, config load, signal handling, graceful shutdown, sd_notify. |
| `internal/config` | File + env config with validation and hardware profiles (`dev-mac`, `pi5`). |
| `internal/store` | SQLite (WAL, `synchronous=NORMAL`/`FULL` for outbox), embedded migrations, integrity check at boot, backup. |
| `internal/session` | One active citizen session; activities IDLE → READY ⇄ LISTENING → PROCESSING → SPEAKING → READY/LISTENING; 2/9/10-minute inactivity rules; caps on time spent in PROCESSING/SPEAKING so an abandoned or crashed turn cannot wedge the kiosk; cleanup hooks. |
| `internal/dialogue` | Conversation engine: topic guard, intent classifier, per-intent deterministic flows, slot filling, retrieval, response composition, LLM rephrase + validator, escalation. |
| `internal/knowledge` | Scheme records loaded from signed bundles into SQLite FTS5; only `VERIFIED` records produce factual answers. |
| `internal/ai` | Clients for STT/LLM/TTS runtimes with timeouts, concurrency limit of 1 per runtime, circuit breakers. |
| `internal/hal` | Hardware abstraction: button, audio in/out, camera, LEDs. `sim` implementation for PC, `linux` (GPIO via gpiochip character device, ALSA) for Pi. |
| `internal/docassist` | Document check: tesseract subprocess (image on stdin, text on stdout, nothing on disk, killed on timeout, `RLIMIT_AS`/`RLIMIT_CORE`), keyword classifier, masked identifiers, comparison with a scheme's document list (only for records the display policy allows). |
| `internal/heartbeat` | Signed heartbeat to central every 5 min (health with reason codes only, versions, outbox counts, resources); reads the latest bundle version from the reply. |
| `internal/bundlesync` | Fetches the signed knowledge bundle, verifies it against pinned server keys, refuses rollbacks, never swaps facts while a session is open, keeps the signed bytes on disk. |
| `internal/resources` | Memory, goroutines, disk, CPU temperature, throttling checks. |
| `internal/outbox` + `internal/syncer` | Event outbox with PENDING → UPLOADING → ACKNOWLEDGED → DELETE_ELIGIBLE / FAILED / CONFLICT; signed batch upload, ack verification, backoff. |
| `internal/health` | Component registry with states HEALTHY / DEGRADED / UNAVAILABLE / RECOVERING / REQUIRES_ATTENTION; resource monitor (memory, goroutines, disk, CPU temp, throttling). |
| `internal/api` | Loopback-only HTTP API + SSE for the kiosk UI; Host/Origin checks; request size limits. |
| `internal/logging` | `log/slog` JSON logs with size-bounded rotation. |

### Concurrency model

* A fixed set of long-lived goroutines, each started from `main` with a shared
  `context.Context` and tracked by a `sync.WaitGroup`: HTTP server, session
  timer, sync worker, health monitor, log rotator. No goroutine is spawned per
  event except per-HTTP-request handlers (bounded by `net/http` + limits).
* All queues are bounded channels or SQLite tables with size caps.
* Each AI runtime has a semaphore of 1 — a kiosk serves one citizen at a time.
* Every outbound call has a context deadline.

## 3. Process supervision (production)

| Unit | Restart | Memory cap (Pi 5 8 GB, provisional) |
|---|---|---|
| `jan-sahayak-agent.service` (Type=notify, WatchdogSec=30; tesseract runs in its cgroup, `OOMPolicy=continue`) | always, RestartSec=5, no start limit | 256 MB |
| `jan-sahayak-speech.service` (speech worker: STT, VAD, TTS) | always | 900 MB |
| `jan-sahayak-llm.service` (llama-server) | always | 2.5 GB |
| `jan-sahayak-kiosk.service` (Chromium `--kiosk` in cage) | always | 1 GB |

Every unit sets `MemorySwapMax=0` and `LimitCORE=0`: citizen audio, photos
and text are never swapped to the SD card or written to a core file.

The agent sends `WATCHDOG=1` only while its internal liveness probe (DB ping +
HTTP self-probe + per-loop heartbeats of the health, session and sync loops)
succeeds, so systemd restarts it on genuine hangs rather
than on a timer. Caps are finalised after Pi benchmarking (Phase 13).

## 4. Data flow of one citizen turn

1. Button press (GPIO or simulated) → `session.Start()` → UI shows privacy
   notice and greeting; the agent plays greeting audio.
2. Agent sets `LISTENING`; UI records ≤ 20 s of 16 kHz mono audio and POSTs it.
3. Agent: VAD/length checks → STT → transcript + confidence. Low confidence →
   ask to repeat.
4. Dialogue engine: topic guard → intent → flow step → retrieval → compose
   answer (template from verified facts) → optional LLM rephrase → validator.
5. TTS → audio returned to UI; text shown on screen simultaneously.
6. Anonymous event (session id, category, service, language, resolution) is
   written to the outbox in the same transaction as the session update.
7. Session end or timeout → temp audio deleted, in-memory transcript dropped.

Document check (only after the citizen's per-session camera consent): the UI
posts one JPEG → the agent checks the header, pipes it to tesseract, drops
the bytes, classifies the text, masks identifiers and returns the type and a
few fields for the citizen to confirm. Confirmed types are compared with the
scheme's document list. Nothing about the document is stored, logged or sent
centrally (`docs/API_DOCUMENTATION.md`, "Document check").

## 5. Central side

* `services/central-api` (Go, `net/http`, pgx, PostgreSQL): device registry,
  device auth (Ed25519 public keys), event ingest with idempotency, signed
  acknowledgements, scheme management + review workflow, analytics queries,
  admin auth (RBAC).
* `apps/admin-dashboard` (Next.js static export, served by central-api with a
  per-page script-hash CSP): analytics, device health, scheme review.
* Devices send a signed heartbeat every 5 minutes and fetch the signed
  knowledge bundle (every 6 hours, or sooner when a heartbeat reply announces
  a newer version).

## 6. Where things live in the repo

See `README.md` for the tree. Directories are created when they get real
content — no placeholder folders.
