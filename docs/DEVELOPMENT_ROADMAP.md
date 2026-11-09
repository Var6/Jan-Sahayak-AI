# Development Roadmap

Phases follow the master brief. Status is updated in `PROGRESS.md`.

| Phase | Scope | Status (2026-11-09) |
|---|---|---|
| 0 Audit | machine, tools, workspace, architecture & decision docs | done |
| 1 Foundation | Go module, pnpm workspace, toolchain (cmake, llama.cpp, whisper.cpp, PostgreSQL 17, sqlc, goose), local Postgres, lint/test setup | done (frontend lint/test in progress with kiosk) |
| 2 Local AI | select + download + verify STT, VAD, TTS, LLM; benchmarks; manifest + installer | models downloaded & verified; speech-worker benchmarks in progress |
| 3 Go device agent | store, outbox, sync, session, health, identity, AI clients (done); API, config, HAL, resources, main (in progress) | in progress |
| 4 Kiosk UI | React/Vite PC-first UI on the local API | in progress |
| 5 Conversation engine & guardrails | topic guard, flows, retrieval, validator, LLM integration | in progress |
| 6 Bihar knowledge base | ~25 UNVERIFIED drafts from department sources; review checklist | in progress (human review needed) |
| 7 Voice integration | real STT/TTS in the turn loop; Hindi/Hinglish tests | in progress (speech worker) |
| 8 Central backend | Go + PostgreSQL: devices, ingest, admin, schemes, analytics | in progress |
| 9 Offline sync | device side done & tested; central ingest in progress; e2e | in progress |
| 10 Admin dashboard | Next.js static export | next |
| 11 Document assistance | consent, capture, Tesseract OCR (hin+eng), confirmation, cleanup | later |
| 12 Reliability & endurance | soaks, leak tests, fault injection, upgrade/rollback | partially (unit-level done) |
| 13 Raspberry Pi | buy kit (approval), image, cross-compile, benchmarks, 72 h soak | blocked on hardware approval |
| 14 Pilot | one authorised location, consent & privacy procedures, legal review | later |
