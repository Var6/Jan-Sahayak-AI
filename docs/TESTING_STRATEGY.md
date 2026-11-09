# Testing Strategy

Rule: never claim a test passed unless it was executed; `PROGRESS.md` records
the exact commands and results of each session.

## Layers

| Layer | Tooling | Where |
|---|---|---|
| Go unit + integration | `go test -race ./...` (root module); `goleak` for goroutine leaks; `testing/synctest` or injected clocks for timers | `internal/**`, `services/device-agent/**`, `services/central-api/**` |
| Speech worker (cgo) | `cd services/speech-worker && go test ./...` | |
| Database | migration tests (fresh, re-open, refuse newer schema), trigger tests, corruption recovery | `store`, central migrations against local Postgres (`jansahayak_test` on :54329) |
| Sync | protocol tests against a fake signed server; randomised no-loss invariant; central ingest tests on real Postgres | `outbox`, `syncer`, `central-api` |
| Dialogue & guardrails | table-driven Hindi/Hinglish/English cases; adversarial suite; LLM fake for validator; opt-in real-LLM test (`JS_LLM_URL`) | `dialogue` |
| Speech | benchmark + round-trip (TTS → STT) checks on synthetic clips; real-speech WER set (to be recorded with consent) | `scripts/bench/` |
| Frontend | Vitest + Testing Library (happy-dom); Biome lint; `tsc --noEmit` | `apps/citizen-kiosk` |
| End-to-end | scripted session via curl against running agent + runtimes | `scripts/dev/` |
| Soak | long-running drivers sampling RSS/goroutines/FDs/DB size/latency | Phase 12 |
| C++ | none written by us; if ever added: ASan/UBSan/TSan builds in CI | — |

## Required scenarios (from the brief) → where covered

| Scenario | Coverage |
|---|---|
| Relevant welfare questions / unrelated questions / prompt injection / unsupported facts / outdated info / incomplete info | `dialogue` tests + adversarial review suite |
| Hindi / Hinglish speech, low-confidence recognition | dialogue tests with STT-garbled transcripts; speech bench; real WER set pending |
| Model crash, STT/TTS failure | `ai` gate/circuit-breaker tests; API degradation tests; soak with runtime kills (Phase 12) |
| Internet loss, server failure, duplicate sync, invalid acknowledgement | `syncer` tests (unreachable, 500, forged ack, lost response, idempotent resend) |
| Power interruption, DB corruption recovery | `outbox.RecoverInterrupted`, `store` corruption test; Pi power-cut soak (Phase 13) |
| Storage pressure | `outbox` full test; disk-fill soak (Phase 12) |
| Session timeout | `session` timeline tests (2/9/10 min) |
| Memory growth, goroutine leaks | goleak tests; soak trends |
| Long-running stability | Phase 12/13 soaks |

## Commands

```
go vet ./... && go test -race ./...            # root module
(cd services/speech-worker && go test ./...)    # cgo worker
(cd apps/citizen-kiosk && pnpm test && pnpm typecheck && pnpm lint)
scripts/models/install.sh --verify dev-mac      # model integrity
```
