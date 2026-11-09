# Language and Runtime Decisions

Last updated 2026-11-09. Each choice was checked against current releases on
that date (registry queries, licences, and hands-on runs on the dev M1).

## Final allocation

| Component | Language / runtime | Why |
|---|---|---|
| Device agent (controller, local API, sessions, sync, health, storage, dialogue engine) | **Go 1.26** | Single static binary; cross-compiles to linux/arm64 with `CGO_ENABLED=0`; goroutines + contexts give bounded concurrency with explicit cancellation; strong stdlib (net/http, crypto/ed25519, log/slog, embed); low, predictable memory (~20–40 MB RSS). |
| Device database | **SQLite** via `modernc.org/sqlite` (pure Go) | No cgo → reproducible ARM64 builds from a Mac. Same speed as the cgo driver in our benchmark (100k inserts in 277 ms on M1). WAL + `synchronous=FULL` for power-loss safety. |
| Speech-to-text | **sherpa-onnx (C++)** running AI4Bharat **IndicConformer-Hindi** (int8 ONNX) | Fastest *and* most accurate engine tested on the M1 (RTF ≈ 0.03, 441 MB). Expected RTF 0.1–0.2 on Pi 5. Native C++ with official Go bindings. |
| STT second opinion (dev only) | **whisper.cpp** `whisper-server` + large-v3-turbo q5_0 | Better with English words; supports name-biasing prompts; Metal on M1. Too heavy for Pi CPU. |
| Text-to-speech | **sherpa-onnx (C++)** running Piper VITS Hindi voices | See `LOCAL_AI_MODELS.md`. Same native runtime as STT → one speech worker. |
| Local LLM | **llama.cpp** `llama-server` (C/C++) + **Gemma 4 E2B-it Q4_0** | Best Hindi among small open models tested; Apache-2.0; 0.8–1.5 GB RAM; JSON-schema constrained output for classification. |
| Speech worker (hosts sherpa-onnx STT + VAD + TTS) | **Go + cgo** thin wrapper around sherpa-onnx's C API, run as a **separate process** | Keeps all C++ out of the device agent's address space. A native crash/OOM restarts only the worker (systemd), never the controller or the database. |
| Citizen kiosk UI | **React 19 + Vite + TypeScript** | Static files embedded in the agent binary and served on 127.0.0.1 to Chromium kiosk mode. No Node at runtime on the device. |
| Admin dashboard | **Next.js + React + TypeScript**, `output: 'export'` (static) | Meets the Next.js requirement while avoiding the 2026 React-Server-Components CVE series: no Node server, no Server Functions. Static build is served by the central API. |
| Central backend | **Go** (`net/http`, `pgx/v5`) | One language with the device; shares `internal/syncproto`, `internal/scheme`, `internal/ids` — the sync contract cannot drift. Single binary, small VPS footprint. Nothing argued for Node (TS types are generated from OpenAPI instead). |
| Central database | **PostgreSQL 17** | Mature, transactional, good JSON support for scheme records. |
| Dev utilities | Shell + Go; Python only for offline experiments | Python is never in the device runtime path. |

## Why not…

* **Python on the device** — interpreter overhead, dependency drift and
  memory growth in a process meant to run for years. Allowed only for offline
  tasks (e.g. re-exporting the IndicConformer ONNX model at build time).
* **cgo in the device agent** — would tie the controller's stability to C++
  libraries and complicate cross-compilation. The cgo code lives only in the
  speech worker, behind a loopback HTTP interface.
* **Linking llama.cpp / whisper.cpp in-process** — same reason. The runtimes'
  own servers (`llama-server`, `whisper-server`) are maintained upstream, keep
  models resident, and can be restarted independently.
* **Electron** — 150+ MB runtime for no benefit over Chromium kiosk mode.
* **Custom C++** — none written. Every native component is an established,
  maintained project (llama.cpp, whisper.cpp, sherpa-onnx, onnxruntime).
  If we ever add custom C++ it must build with `-fsanitize=address,undefined`
  in CI and use RAII/smart pointers only.
* **Next.js server mode** — 2026 saw several RSC DoS/RCE advisories; a static
  export removes that attack surface entirely.
* **JWT for admin auth** — server-side sessions (hashed random token in an
  HttpOnly SameSite=Strict cookie) are simpler to revoke.
* **goose / golang-migrate at runtime** — their module graphs pull in many
  database drivers. An 80-line embedded migration runner does the job and is
  covered by tests (including "refuse newer schema" for rollbacks).
* **lumberjack log rotation** — unmaintained; on the Pi, journald rotates and
  caps logs (`SystemMaxUse=50M`).

## Dependency budget (runtime)

| Binary | Third-party Go modules |
|---|---|
| device-agent | `modernc.org/sqlite` (+ its pinned transitive modules) |
| speech-worker | `github.com/k2-fsa/sherpa-onnx-go` |
| central-api | `github.com/jackc/pgx/v5`, `golang.org/x/crypto` |
| tests only | `go.uber.org/goleak` |

Rule: adding a runtime dependency requires an entry in `DECISION_LOG.md`.
