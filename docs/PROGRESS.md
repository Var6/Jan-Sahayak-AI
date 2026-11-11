# Progress

_Last updated: 2026-11-11 (Phase 12 + fixes from the second independent review)._

## Current phase

Phases 0–12 done on the dev Mac: admin dashboard (10), document check (11),
heartbeat + signed bundle sync + dev provisioning, and Phase 12 integration,
60-minute fault-injection soak, storage-pressure and upgrade/rollback tests
(all pass; results in `LONG_TERM_RELIABILITY.md` §7a/§8). Next: 8-hour dev
soak with the same tool, then Phase 13 on real Pi hardware (needs approval).

## Completed

| Area | What exists (all tested) |
|---|---|
| Device agent (Go) | SQLite store (WAL, `synchronous=FULL`, trigger-guarded outbox, corruption quarantine + restore, verified backups), outbox + signed-ack sync worker, session manager (2/9/10-min rules, PROCESSING/SPEAKING caps), health monitor, resource monitor, device identity (Ed25519), loopback API + SSE per `API_DOCUMENTATION.md`, config profiles, GPIO/sim button HAL, sd_notify watchdog with per-loop heartbeats, embedded kiosk UI. Cross-compiles to linux/arm64 (18.8 MB static). |
| Speech worker (Go + cgo, sherpa-onnx 2.0.1) | IndicConformer-Hindi STT + Silero VAD + Supertonic-3 Hindi TTS on loopback HTTP; self-exits if an inference wedges. |
| Conversation engine | Crisis-first safety (stems, relative+violence pairs, self-harm, child marriage, dowry, medical emergency), sensitive-data redaction, prompt-injection folding/detection, deterministic topic classifier with LLM (Gemma 4 E2B) enum fallback, flows for loan/pension/certificate/etc., verified-records-only answers, allowlist validator for LLM rephrasing. |
| Kiosk UI (React 19 + Vite + TS) | All screens, mic capture → 16 kHz WAV in browser with VAD, SSE with resync, crisis panel, inactivity overlays, degraded modes, self-hosted fonts, no external calls. |
| Central API (Go + PostgreSQL 17) | Signed sync ingest (idempotent, nonce replay, skew, revocation in-tx, signed acks), heartbeat, signed knowledge bundles, admin auth (argon2id, sessions, throttling), RBAC, four-eyes scheme review, withdraw/OUTDATED, analytics with small-cell suppression, append-only audit log, owner/runtime DB roles, admin CLI. |
| Knowledge base | 24 Bihar + central scheme/service drafts from official department sources with verbatim excerpts, all **UNVERIFIED**; helplines file (UNVERIFIED); `kb` validator (strict: every number must appear in an excerpt); review checklist. |
| Models & supply chain | Manifest with per-file SHA-256 (archive members too), installer + `--verify`, release `SHA256SUMS` + verifier wired into the Pi installer. |
| Admin dashboard (Next.js 16 static export) | Served by central-api under `/admin` with per-page CSP script hashes; analytics, devices, scheme review/verify/withdraw (four-eyes), bundles, audit. |
| Connectivity | Signed heartbeat (5 min), signed bundle sync (6 h, rollback-refusing), `scripts/dev/provision.sh`. |
| Document check (OCR) | Local tesseract `eng+hin`, consent-gated, masked IDs, coverage vs. scheme documents; nothing stored or sent. |
| Reliability tooling | `scripts/dev/faultsoak` (fault-injection soak, CSV + trend/latency/recovery summary), `scripts/dev/upgradetest/run.sh` (v1 → v2 upgrade and rollback). Startup sweep closes sessions a killed agent left open (D-028); verified backup before every schema migration (D-029); outbox-full log rate-limited. |
| Deployment | systemd units (hardened, self-healing restart policy, MemoryMax), journald caps, Chromium kiosk under `cage`, Pi install script, TLS reverse-proxy example for central. |

## Models installed (all SHA-256 verified, 15/15 files)

IndicConformer-Hindi int8 (STT), Silero VAD, Supertonic-3 int8 (TTS),
Gemma 4 E2B-it Q4_0 + MTP draft (LLM), Whisper large-v3-turbo q5_0 and small
q5_1 (dev baselines). Details: `LOCAL_AI_MODELS.md`.

## Tests run at end of session (all executed)

| Command | Result |
|---|---|
| `gofmt -l` (all Go files) / `go vet ./...` | clean / clean |
| `go test -race -p 2 ./...` (root module, incl. central API on Postgres) | exit 0, 25 packages ok, 0 failures |
| `JS_REVIEW_STRICT=1 go test -race ./services/device-agent/internal/dialogue/` (adversarial guardrail suite) | ok |
| `go test -race -tags reliabilityrepro` store, ai, syncer, api | ok (all reliability repros pass) |
| `go test -race -tags securityreview` central server, identity | ok (all security repros pass) |
| `JS_REVIEW=1 go test -race` api, outbox | ok |
| speech worker `go test -race ./...` (+ reliabilityrepro) | ok (verifier run) |
| kiosk `pnpm typecheck && pnpm lint && pnpm test && pnpm build` | ok, 85/85 tests (verifier run) |
| `kb validate -strict` | 24 records valid, all UNVERIFIED |
| `scripts/models/install.sh --verify dev-mac` | 15/15 ok; tampered copy detected |
| `verify-release.sh` on clean / tampered release | exit 0 / exit 1 |
| Live smoke (agent + speech worker + llama-server) | pass: exact transcripts, crisis replies for "pati roz maarta hai" and "मैं आत्महत्या करना चाहता हूँ", Aadhaar digits never echoed, outbox has no text, timestamps minute-rounded, SPEAKING released at 120 s |
| 10-min soak (text sessions) | 68 sessions / 204 turns, 0 errors; p50 1.3 s, p95 2.4 s; agent RSS flat 19–26 MB; goroutines flat 12–19 |

## Phase 12 checks (2026-11-11, all executed)

| Check | Result |
|---|---|
| `gofmt -l`, `go vet ./...`, `go test -race -p 2 ./...` (root, incl. central on Postgres) | clean, clean, all ok (28 packages before the Phase 12 fixes, 29 after, incl. the new faultsoak tests) |
| speech worker `go vet`, `go test -race ./...` | ok (5 packages) |
| kiosk typecheck / lint / test / build | ok, 98/98 tests |
| admin dashboard typecheck / lint / test / build | ok, 40/40 tests, 11 pages, 74 CSP hashes |
| `kb validate -strict`, `kb helplines` | 24 records valid (all UNVERIFIED), 6 helplines valid |
| `scripts/models/install.sh --verify dev-mac` | 15/15 ok |
| Live stack (central + provision + llama + speech + agent synced) | voice turns transcribed, text + crisis replies, document check (403 without consent, income certificate read, Aadhaar masked, 403 after withdrawal), 11/11 events ACKNOWLEDGED with identical IDs centrally, heartbeats via admin API, `/admin/` served with per-page CSP and hydrating in headless Chrome |
| 60-min fault soak (9 faults) | 0 failed requests, 1,186/1,186 events ACKNOWLEDGED and identical centrally, no RSS/goroutine/FD growth, every fault recovered |
| Storage pressure (outbox 1,000 rows) | REQUIRES_ATTENTION, citizens still served, no stored event lost, full catch-up in 7 s |
| Upgrade/rollback v1 → v2 (`upgradetest/run.sh`) | all PASS |

## Review 2 fixes (2026-11-11, all executed)

Fixed from the security/privacy and correctness reviews of Phases 10–12:
issuing office never shows an ID (dropped above 4 digits, looser scrubbing,
case-insensitive PAN/EPIC with OCR mix-ups), address lines no longer taken
as the office, consent-withdrawal race closed, photo read into one zeroed
buffer, document type no longer logged, heartbeat details restricted to
reason codes on device and central (D-030), compound "bank statement"
requirement fixed, bundle checks no longer every 10 min after a restart,
bundle not swapped if a session opened during the download, truncated photo
→ 400 instead of turning OCR off, `OOMPolicy=continue` + tesseract
`oom_score_adj` 1000 (D-031), no core dumps (agent and tesseract
`RLIMIT_CORE=0`, `LimitCORE=0` on all units), kiosk `MemorySwapMax=0`,
Chromium capture policies `false` + allowlist, rollback SQL keeps the newer
session end, soak voice p95 corrected to 6.45 s, stale docs refreshed.

| Check | Result |
|---|---|
| `gofmt -l` / `go vet ./...` | clean / clean |
| `go test -race -p 2 ./...` (root, central on Postgres) | all ok |
| same with `-tags securityreview2`, `-tags review2`, `JS_REVIEW=1 -tags "reliabilityrepro securityreview"` | all ok (every reviewer repro/probe passes) |
| speech worker `go vet`, `go test -race ./...` | ok (5 packages) |
| kiosk typecheck / lint / test / build | ok, 98/98 |
| admin dashboard typecheck / lint / test / build | ok, 40/40, 11 pages, 74 CSP hashes, build peak RSS 347 MiB |
| `kb validate -strict`, `kb helplines` | 24 valid (all UNVERIFIED), 6 valid |
| `scripts/models/install.sh --verify dev-mac` | 15/15 ok |
| linux/arm64 `CGO_ENABLED=0` agent build | ok (19.2 MB, not stripped) |
| Live smoke (central + provision + llama + speech + agent) | voice turn 3.0 s (exact transcript), document check 0.29 s (`income_certificate`/high), truncated JPEG → 400 with camera still on, 2/2 events ACKNOWLEDGED and present centrally, heartbeat stored (reason-code details only); everything stopped (pgrep empty) |

## Performance (M1, 8 GB, heavily swapping — pessimistic)

| Measure | Value |
|---|---|
| STT (IndicConformer) | 31–111 ms per clip, RTF ≈ 0.025; CER 0.00–0.02 on synthetic clips |
| TTS (Supertonic, 4 steps) | RTF 0.12–0.15; round-trip similarity 1.00 |
| Speech worker RSS | 527 MiB loaded, 570 MiB peak; healthy in 1.8 s |
| LLM classifier (Gemma 4 E2B, Metal) | 10/10 correct, median 0.88 s |
| Device agent RSS / goroutines | 19–26 MB / 10–19 |
| Text turn latency (soak) | p50 1.3 s, p95 2.4 s |
| Central ingest | 200-event signed batch in 12 ms; idle RSS ~16 MB |

## Known issues / not yet done

1. Helplines and all 24 scheme records are UNVERIFIED → production shows only
   scheme names and only 112 in crisis replies until Janman reviewers sign off.
2. Crisis/injection vocabulary needs counsellor and field-staff review.
3. Nothing has run on a Raspberry Pi (GPIO, systemd 252 `LoadCredential`,
   llama key flags, real-load wedge detection, thermals, RAM fit).
4. Speech worker cannot be cross-compiled from the Mac (cgo); build on the Pi
   or in an arm64 container.
5. STT accuracy measured only on synthetic clips — no real-speech WER yet.
6. IndicConformer ONNX is a community export (SHA-pinned); re-export ourselves.
7. Document check tested only on clean synthetic images; no spoken audio or analytics for it.
8. Release signing key custody undecided (integrity checks only until then).
9. 8-hour (dev) and 72-hour (Pi) soaks and power-cut injection pending; the
   60-minute soak ran with the Mac sleeping ~23 min (lid closed on battery).
10. Rollback is a documented technician procedure; the A/B updater does not
    run it automatically yet.

## Next three actions

1. 8-hour dev soak on AC power with the lid open:
   `scripts/dev/faultsoak/run.sh -duration 8h -faults …`.
2. Phase 13 on a Pi 5 (needs the hardware): same soak for 72 h, power-cut
   injection, memory caps, tesseract temp-file check with `strace`,
   `OOMPolicy=continue` with a forced tesseract OOM, `oom_score_adj` and
   `RLIMIT_CORE` under the unit's sandbox, Chromium mic/camera with the
   tightened capture policy, and whether the Pi kernel enables the memory
   cgroup (`MemoryMax`/`MemorySwapMax` need it).
3. Consented real-speech and real-document test sets; IndicConformer re-export.

## Decisions requiring approval

See the end-of-session summary; also listed in `DECISION_LOG.md`
(D-013, D-020, D-022, D-025) and `AI_GUARDRAILS.md` ("pending sign-off").
