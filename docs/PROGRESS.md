# Progress

_Last updated: 2026-11-09, end of session 1._

## Current phase

Phases 0–2 done. Phases 3–9 implemented, reviewed adversarially, fixed and
re-verified on the dev Mac. Next: Phase 10 (admin dashboard), then Phase 12
soak/fault-injection, then Phase 13 on real Pi hardware (needs approval).

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
7. Admin dashboard (Phase 10) and document assistance/OCR (Phase 11) not built.
8. Release signing key custody undecided (integrity checks only until then).
9. 8-hour and 72-hour soaks, power-cut injection, upgrade/rollback tests pending.
10. No git commits yet.

## Next three actions

1. Phase 10: Next.js static-export admin dashboard on the central API
   (analytics, devices, scheme review/verify/withdraw, audit).
2. Phase 12: 8-hour dev soak with fault injection (kill runtimes, network
   flaps, disk pressure) + upgrade/rollback test of device migration 0002.
3. Build a consented real-speech test set plan and a re-export of
   IndicConformer; prepare the Pi 5 benchmark kit list for approval.

## Decisions requiring approval

See the end-of-session summary; also listed in `DECISION_LOG.md`
(D-013, D-020, D-022, D-025) and `AI_GUARDRAILS.md` ("pending sign-off").
