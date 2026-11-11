# Jan Sahayak AI

Offline-first, voice-first Hindi/Hinglish kiosk that helps citizens in Bihar
understand government welfare schemes and public services. Speech
recognition, speech synthesis and the language model all run locally.
Built for Janman People's Foundation. **Not an official government service.**

> Status: development. Scheme records are drafts marked **UNVERIFIED** and
> must be checked by Janman reviewers before any citizen sees them as fact.
> See [docs/PROGRESS.md](docs/PROGRESS.md).

## Layout

```
apps/citizen-kiosk/        React + Vite + TS kiosk UI (embedded into the agent)
services/device-agent/     Go device agent: API, sessions, dialogue, sync, health
services/speech-worker/    Go + cgo sherpa-onnx: Hindi STT, VAD, TTS (own module)
services/central-api/      Go + PostgreSQL central server
internal/                  shared Go contracts (sync protocol, scheme records, ids)
packages/scheme-knowledge/ scheme records, helplines, Bihar gazetteer
models/manifests/          model registry (weights are downloaded, not committed)
deployment/                systemd units, Raspberry Pi installer, server config
docs/                      architecture, decisions, reliability, security, progress
```

## Quick start (macOS dev)

```
brew install go cmake llama.cpp postgresql@17   # Go 1.26+, Node 22+, pnpm
pnpm install
scripts/models/install.sh dev-mac               # downloads + SHA-256-verifies models
make build                                      # kiosk UI + agent
make speech-worker
scripts/dev/up.sh                               # llama-server, speech worker, agent
open http://127.0.0.1:8730
scripts/dev/down.sh
```

Tests: `go test -race ./...`, `(cd services/speech-worker && go test ./...)`,
`(cd apps/citizen-kiosk && pnpm test)`. See [docs/TESTING_STRATEGY.md](docs/TESTING_STRATEGY.md).

## Key documents

[System architecture](docs/SYSTEM_ARCHITECTURE.md) ·
[Language & runtime decisions](docs/LANGUAGE_AND_RUNTIME_DECISIONS.md) ·
[Decision log](docs/DECISION_LOG.md) ·
[Local AI models](docs/LOCAL_AI_MODELS.md) ·
[AI guardrails](docs/AI_GUARDRAILS.md) ·
[Offline sync](docs/OFFLINE_SYNC.md) ·
[Security & privacy](docs/SECURITY_AND_PRIVACY.md) ·
[Long-term reliability](docs/LONG_TERM_RELIABILITY.md) ·
[Hardware](docs/HARDWARE_SETUP.md)

## Model licences

Gemma 4 E2B (Apache-2.0), AI4Bharat IndicConformer (MIT), Silero VAD (MIT),
Supertonic-3 (OpenRAIL-M — use restrictions apply: disclose machine-generated
speech; no medical advice, law enforcement or automated legal decisions).
