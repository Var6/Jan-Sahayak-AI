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

### D-015 · 2026-11-09 · Keep Go 1.26 (not 1.27)
**Why:** 1.27's only relevant gain is a stdlib `uuid`; our own UUIDv7 is ~60
lines and tested. Avoids a system toolchain change mid-project. **Status:**
accepted; revisit at next toolchain refresh.
