# Progress

_Last updated: 2026-11-09 (session 1, in progress)_

## Current phase

Phases 0–1 done. Phase 2 models downloaded and verified. Phases 3–9 being
built in parallel (build workflow `jan-sahayak-build-1` running: speech
worker, dialogue engine, kiosk UI, central API, knowledge drafts → device
agent integration → independent review).

## Completed (by lead engineer, tested)

* Machine audit: Apple M1, 8 GB RAM (≈9 GB swap in use — heavy memory
  pressure), 64 GB free, macOS 26.6.2, Go 1.26.4, Node 26.5, pnpm 11.20,
  Apple clang 21, Homebrew.
* Toolchain installed: cmake 4.4.5, llama.cpp 0.9.0 (Metal), whisper.cpp
  1.9.5, PostgreSQL 17.11, sqlc 1.31.1, goose 3.29.0. Project-local Postgres
  on 127.0.0.1:54329 (`var/pgdata`, not a login service).
* Repo: git initialised (no commits yet), pnpm workspace, Go module
  `github.com/janmanindia/jan-sahayak`.
* Research (6 agents + skeptic): model, runtime, library and source choices
  with licences, sizes and SHA-256 — see `DECISION_LOG.md`.
* Models downloaded + SHA-256 verified (`scripts/models/install.sh --verify dev-mac` → all ok):
  IndicConformer-Hindi int8 (STT), Silero VAD, Supertonic-3 int8 (TTS),
  Gemma 4 E2B-it Q4_0 + MTP draft (LLM), Whisper large-v3-turbo q5_0 and
  small q5_1 (baselines).
* Go packages (all `go test -race`, 3× repeated, passing):
  `internal/ids`, `internal/syncproto`, `internal/scheme`,
  `device-agent/internal/{store, outbox, syncer, session, health, identity, ai}`.
* Docs: SYSTEM_ARCHITECTURE, LANGUAGE_AND_RUNTIME_DECISIONS, DECISION_LOG,
  LOCAL_AI_MODELS, OFFLINE_SYNC, SECURITY_AND_PRIVACY, LONG_TERM_RELIABILITY,
  HARDWARE_SETUP, TESTING_STRATEGY, DEVELOPMENT_ROADMAP, PROJECT_OVERVIEW,
  API_DOCUMENTATION.

## Tests run (lead engineer)

| Command | Result |
|---|---|
| `go test -race -count=3 ./services/device-agent/... ./internal/...` | all packages ok |
| `scripts/models/install.sh --verify dev-mac` | 7/7 ok |
| whisper-cli turbo vs small on 4 synthetic Hindi clips | see LOCAL_AI_MODELS.md |
| whisper-server turbo steady-state | 2.1–2.3 s/utterance, 618 MB RSS |

## Decisions requiring approval

See the end-of-session summary (to be finalised when the build workflow
completes).
