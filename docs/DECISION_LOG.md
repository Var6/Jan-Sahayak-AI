# Decision Log

Format: ID · date · decision · why · alternatives considered · status.

### D-001 · 2026-11-09 · Go for the device agent and the central API
**Why:** compiled single binary, bounded concurrency with contexts, pure-Go
cross-compile to arm64, one language for both ends of the sync protocol
(shared `internal/syncproto`). **Alternatives:** Node/TS central (only gain was
shared TS types — solved with OpenAPI-generated types). **Status:** accepted.

### D-002 · 2026-11-09 · `modernc.org/sqlite` (pure Go) on the device
**Why:** builds for linux/arm64 with `CGO_ENABLED=0`; measured performance on
par with ncruces and the cgo driver (100k inserts / tx: 277 ms on M1).
**Alternatives:** mattn/go-sqlite3 (cgo — fails to cross-compile without a C
toolchain), ncruces (fallback). **Status:** accepted.

### D-003 · 2026-11-09 · `synchronous=FULL` + WAL for the device DB
**Why:** outbox rows are promises we must not lose on power cuts; write
volume is a few rows per citizen session so the fsync cost is negligible.
**Alternative:** NORMAL (research suggestion; can lose last transactions on
power loss). **Status:** accepted.

### D-004 · 2026-11-09 · Outbox deletion guarded by SQLite triggers
**Why:** "never delete unacknowledged records" must hold even if future code
has a bug. Triggers refuse DELETE unless `state='DELETE_ELIGIBLE'`, refuse
illegal state transitions, require a receipt for ACKNOWLEDGED, and make
payloads immutable. Proven by tests including a 3,000-step randomised test.
**Status:** accepted, implemented.

### D-005 · 2026-11-09 · Signed acknowledgements (Ed25519) for sync
**Why:** HTTP 200 is not proof of storage. The server signs per-event results
(with payload hash + receipt); the device verifies signature, batch id,
device id, event membership and hash before marking ACKNOWLEDGED. Device
requests are Ed25519-signed with timestamp + nonce (replay protection).
**Alternatives:** mTLS only (proves transport, not per-event storage);
JWT (unnecessary). **Status:** accepted, implemented (device side).

### D-006 · 2026-11-09 · STT = sherpa-onnx + AI4Bharat IndicConformer-Hindi (int8 CTC)
**Why:** measured fastest and most accurate on Bihar district names on the
M1 (RTF ≈ 0.03, 441 MB); expected conversational on Pi 5 CPU (est. RTF
0.1–0.2, unverified). **Risks:** community ONNX export with ~0 downloads (SHA
pinned; re-export ourselves is a TODO); no WER published for this exact
120M model (the 13.2 figure belongs to the 600M multilingual sibling).
**Alternatives:** whisper.cpp large-v3-turbo (good on dev Metal, ~2.2 s/turn,
too slow on Pi CPU, poor on names), whisper small (bad on names), vasista22
hi-small (accurate but ~RTF 2–4 on Pi), Vosk (weaker). **Status:** accepted
pending real-speech WER test.

### D-007 · 2026-11-09 · TTS = Supertonic-3 int8 on sherpa-onnx v2.0.1
**Why:** word-perfect Hindi in round-trip tests, RTF 0.135 on M1, raw
Devanagari input (no GPL espeak-ng). **Licence:** OpenRAIL-M weights — must
disclose machine voice; no automated legal-rights decisions, medical advice,
law enforcement / justice administration; no exploiting vulnerable groups to
distort behaviour; pass restrictions on. Kiosk use (information only) is
compatible; counsel review requested. **Rejected:** Piper pratham/priyamvada
(CC-BY-NC-SA), Piper rohan (lessac research-only ancestry — legal grey;
its phonemiser wrapper has no licence), MMS-TTS (CC-BY-NC), Kokoro (no Hindi
path in sherpa v2), IndicF5/Parler (too heavy). **Status:** accepted.

### D-008 · 2026-11-09 · LLM = Gemma 4 E2B-it Q4_0 via llama.cpp `llama-server`
**Why:** best Hindi of five small models tested hands-on; Apache-2.0;
0.8–1.5 GB RAM; JSON-schema constrained output works (with the
`response_format.json_schema` request shape — the README's shape is silently
ignored by build 12110). **Role:** classification fallback + rephrasing of
verified text only; validator rejects any added fact. **Alternatives:**
Qwen3.5-2B (classification-only fallback if RAM-constrained), Llama 3.2,
Gemma 3, Phi-4-mini, Sarvam (licence). **Status:** accepted.

### D-009 · 2026-11-09 · AI runtimes run as separate processes
**Why:** a native crash or OOM in llama.cpp / onnxruntime must never take
down the controller or the database. The speech worker is the only cgo code
and lives in its own Go module and process. **Status:** accepted.

### D-010 · 2026-11-09 · Admin dashboard = Next.js static export
**Why:** the brief requires Next.js; 2026 saw multiple RSC/Server-Function
CVEs. `output: 'export'` keeps Next.js tooling with no Node server in
production; the Go central API serves the static files. **Status:** accepted.

### D-011 · 2026-11-09 · Knowledge base built from department sources, not myScheme API
**Why:** myScheme's Terms of Use forbid bots/scrapers/automated copying
without written NeGD permission; using the API key embedded in its JS bundle
would be unauthorised access. Records are drafted from official department
PDFs/portals with verbatim excerpts, start as UNVERIFIED and become VERIFIED
only through a human reviewer (editor ≠ reviewer). **Action for Janman:**
request written permission from NeGD. **Status:** accepted.

### D-012 · 2026-11-09 · Logs → stdout → journald (no own rotation)
**Why:** journald rotates and caps (`SystemMaxUse=50M`), reducing SD wear and
code. lumberjack is unmaintained. **Status:** accepted.

### D-013 · 2026-11-09 · Production hardware target = Raspberry Pi 5 **8 GB**
**Why:** the full stack (LLM ~1.5 GB, STT ~0.45 GB, TTS ~0.4 GB, Chromium,
OS) does not demonstrably fit 4 GB; Raspberry Pi's own Gemma 4 E2B llama.cpp
test peaked at 4.4 GB on an 8 GB board (default context). 4 GB only after a
measured run. **Status:** proposed — purchase needs approval.

### D-014 · 2026-11-09 · Unverified knowledge only visible in `dev-*` profiles
**Why:** developers need to exercise flows before human review completes;
production must never present unverified facts. The flag is refused at
startup unless the profile starts with `dev`, and every such answer carries
a "not yet verified" banner. **Status:** accepted.

### D-016 · 2026-11-09 · Only per-event server verdicts consume sync attempts
**Why:** review showed a kiosk offline ~10 days stranded every event in FAILED
forever. Network/HTTP-level failures now never count; FAILED (not CONFLICT)
events are auto-requeued every 24 h; operators can requeue manually.
**Status:** accepted, being implemented (fix workflow).

### D-017 · 2026-11-09 · Non-corruption DB open errors fail startup, never quarantine
**Why:** a permissions/I/O error was treated as corruption and the live DB was
moved aside. Only `quick_check` failures, SQLITE_CORRUPT or SQLITE_NOTADB
quarantine; anything else fails loudly so systemd retries and an operator
sees it. **Status:** accepted, being implemented.

### D-018 · 2026-11-09 · LLM rephrase validator is an allowlist; rephrase off on pi5 by default
**Why:** the blocklist validator accepted invented number words, deadlines,
eligibility verdicts, guarantees, documents and offices. The allowlist only
permits content tokens already present in the verified source. Rephrasing is
disabled on production profiles until field validation; LLM classification
(fixed enum, schema-constrained) stays on. **Status:** accepted.

### D-019 · 2026-11-09 · Crisis detection precedes every other gate
**Why:** low STT confidence and the 500-char cap were hiding crisis input.
Crisis matching now runs on the raw input first; fuzzy matching is disabled
for short romanised tokens (`pati` ≠ `jati`). The phrase corpus must be
reviewed by a counsellor before pilot. **Status:** accepted.

### D-020 · 2026-11-09 · 112 is a hard-coded emergency fallback
**Why:** India's national emergency number must be shown even if the
helpline file is missing or unverified. Recorded exception to "facts only from
records"; needs Janman sign-off. **Status:** proposed.

### D-021 · 2026-11-09 · Self-healing process policy
**Why:** systemd's default start limit left units dead after ~30 s of boot
failures. Units use `StartLimitIntervalSec=0` + `RestartSec=5`; missing GPIO
degrades instead of failing; a speech worker whose inference exceeds 3× its
timeout exits so systemd restarts it. **Status:** accepted.

### D-022 · 2026-11-09 · Privacy hardening of analytics
**Why:** small counts by device/day could single out a visit. Device rounds
session timestamps to the minute; viewer-role analytics suppress cells < 5;
health details sent centrally are fixed reason codes. Threshold to be
confirmed with Janman/legal (DPDP). **Status:** accepted (threshold provisional).

### D-023 · 2026-11-09 · Central DB owner and runtime roles are separate
**Why:** the app role could disable the audit-log triggers it owned. Runtime
role gets no ownership and only SELECT/INSERT on append-only tables.
**Status:** accepted, being implemented.

### D-024 · 2026-11-09 · Only `session.ended` and `dialogue.turn` analytics events
**Why:** the central API stores only event types with strict schemas (no free
text can slip in); a separate `session.started` event added nothing
(`session.ended` carries the start time). New event types require a server
update first. **Status:** accepted.

### D-025 · 2026-11-09 · Release integrity: SHA256SUMS + optional SSH signature
**Why:** the Pi installer copied binaries, libraries and models unchecked.
Releases now ship `SHA256SUMS`; `verify-release.sh` (installed copy preferred
over the bundled one) refuses mismatches and unlisted files, and requires an
`ssh-keygen -Y` signature when an allowed-signers file exists on the device.
**Open:** who holds the release signing key and how the allowed-signers file
is provisioned — needs a Janman decision. **Status:** implemented
(integrity); signing pending.

### D-026 · 2026-11-09 · llama-server requires an API key, no web UI
**Why:** any local process (including the kiosk browser) could otherwise use
the LLM directly. llama-server runs with `--api-key-file` and `--no-webui`;
the key is handed to units via systemd `LoadCredential`; the agent sends it
via `ai.LlamaConfig.APIKey`. **Status:** implemented (untested on Pi).

### D-015 · 2026-11-09 · Keep Go 1.26 (not 1.27)
**Why:** 1.27's only relevant gain is a stdlib `uuid`; our own UUIDv7 is ~60
lines and tested. Avoids a system toolchain change mid-project. **Status:**
accepted; revisit at next toolchain refresh.

### D-027 · 2026-11-11 · Document OCR language order is `eng+hin`
**Why:** on the synthetic fixtures `hin+eng` dropped the digit 1 on every
image ("1234" → "234"); `eng+hin` read both scripts correctly. Configurable
in `documents.languages`; re-check on real, consented phone photos.
**Status:** accepted (Phase 11).

### D-028 · 2026-11-11 · Sessions left open by a killed agent are closed at the next start
**Why:** the Phase 12 fault soak SIGKILLed the agent mid-session: the turns
were recorded, but the `sessions` row stayed open and no `session.ended`
event was ever sent. At startup, before any session can start, the agent
now closes every open row with `end_reason = "error"` and enqueues its
`session.ended` in the same transaction (`api.CloseOrphanedSessions`). The
real end time is unknown, so `ended_at` = `started_at` (minute); analytics
must read "error, zero length" as "end not observed". **Status:** accepted.

### D-029 · 2026-11-11 · Verified backup before every device schema migration
**Why:** a rollback to the previous binary needs a database at the old
schema (a binary refuses a newer one). `store.Open` now writes
`backups/pre-migration-v<N>-<time>.db` (VACUUM INTO, fsynced, quick_check)
before migrating, and refuses to migrate if it cannot — the DB then stays at
the old version and the old binary still works. These copies are not
rotated (one per schema change). Procedure: `LONG_TERM_RELIABILITY.md` §7a.
**Status:** accepted.

### D-030 · 2026-11-11 · Heartbeat health details are reason codes only, enforced on both sides
**Why:** the "fixed reason codes" rule was a convention; a future check that
put an error string (which can quote citizen input) into a detail would have
shipped it centrally. `deviceapi.ReasonCode` keeps a detail only if it is
1–4 codes matching `[a-z0-9_]{1,64}` joined by `; `, else `other`. The device
applies it before sending, central before storing (so an older device cannot
store free text either). Two health details with spaces were renamed
(`not_yet_checked`, `check_timed_out`). **Status:** accepted.

### D-031 · 2026-11-11 · A tesseract OOM must not restart the agent
**Why:** tesseract runs inside the agent's cgroup (`MemoryMax=256M`); with
systemd's default `OOMPolicy=stop` an OOM kill of tesseract stopped and
restarted the whole agent, ending the citizen's session. The agent unit now
sets `OOMPolicy=continue`, and the agent sets tesseract's `oom_score_adj` to
1000 so the OOM killer picks it first. A separate scope/slice for OCR was
considered; it needs D-Bus access from a hardened unit and is not worth it
for one short-lived child. **Status:** accepted; untested on a Pi.

### D-032 · 2026-11-11 · `issuing_authority` is dropped when it holds more than 4 digits
**Why:** OCR can put any separator between digit groups (dots, slashes,
dashes) and misread digits as letters, so masking alone let a full
Aadhaar-shaped number reach the kiosk screen in the office field. An office
name does not need more than 4 digits; dropping the field is safer than
trying to mask every OCR variant (masking was also made looser, as defence
in depth). The unlabelled fallback also skips address and relation lines.
**Status:** accepted; legal review of what may be displayed (DPDP, UIDAI
rules) still pending.
