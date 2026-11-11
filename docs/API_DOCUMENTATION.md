# API Documentation

Two APIs:

1. **Device local API** — Go device agent ↔ citizen kiosk UI, on the device only.
2. **Central API** — devices and the admin dashboard ↔ central server.

---

## 1. Device local API (v1)

* Base: `http://127.0.0.1:8730` — bound to loopback only.
* The kiosk UI is served from the same origin (`/`), so no CORS.
* Every request must carry `Host: 127.0.0.1:8730` or `localhost:8730`
  (DNS-rebinding protection). Mutating requests must carry
  `X-JS-Kiosk: 1` and must not carry a cross-site `Origin`/`Sec-Fetch-Site`
  (CSRF protection via `http.CrossOriginProtection`).
* JSON bodies are limited to 16 KB; audio uploads to 1 MB; document photos
  to 2 MB.
* Errors: `{"error": {"code": "string", "message": "string"}}` with an
  appropriate HTTP status. Codes: `no_session`, `wrong_session`,
  `bad_state`, `bad_request`, `too_large`, `bad_audio`, `unavailable`,
  `button_disabled`, `no_consent`, `bad_image`, `internal`.

### Types

```ts
type Activity = "IDLE" | "READY" | "LISTENING" | "PROCESSING" | "SPEAKING";
type Lang = "hi" | "en";

interface SessionSnapshot {
  id?: string; activity: Activity; language?: Lang;
  started_at?: string; last_active_at?: string; idle_deadline?: string;
  camera_consent: boolean; turns: number;
}

interface Option { id: string; label: string }
interface CardSection { title: string; items: string[] }
interface Card {
  scheme_id: string; title: string; verified: boolean; last_verified?: string;
  sections: CardSection[]; official_url?: string; helpline?: string; disclaimer?: string;
}
type ReplyKind = "greeting" | "question" | "answer" | "repeat" | "redirect"
               | "escalate" | "crisis" | "goodbye";

interface Reply {
  kind: ReplyKind; text: string; speech?: string; options?: Option[];
  cards?: Card[]; listen_next: boolean; end_session: boolean; used_llm: boolean;
}

interface TurnResult {
  session: SessionSnapshot;
  transcript?: { text: string; confidence: number; no_speech: boolean };
  reply: Reply;
  audio_url?: string;   // GET this to play the spoken reply (WAV); absent if TTS unavailable
}

interface DeviceStatus {
  device_id: string; version: string; overall: HealthState;
  capabilities: { voice_input: boolean; voice_output: boolean; llm: boolean; sim_button: boolean; camera: boolean };
  session: SessionSnapshot;
}
type HealthState = "HEALTHY" | "DEGRADED" | "UNAVAILABLE" | "RECOVERING" | "REQUIRES_ATTENTION";
```

### Endpoints

| Method & path | Body | Response | Notes |
|---|---|---|---|
| `GET /api/v1/status` | — | `DeviceStatus` | Poll-free: UI also listens to SSE. |
| `GET /api/v1/events` | — | `text/event-stream` | Events: `session` (`{type, session, end_reason?, reply?, audio_url?}` where type ∈ `session.started`, `session.activity`, `session.reminder`, `session.final_warning`, `session.ended`, `session.language`, `session.camera_consent`), `health` (`{overall, capabilities}`), `ping` every 15 s. At most `api.sse_max_clients` (4) streams; one slot is reserved for the kiosk page (a same-origin browser request, `Sec-Fetch-Site: same-origin`), other clients get 503 `unavailable` beyond the rest. When all slots are taken a kiosk stream evicts the oldest stream (old kiosk streams first). |
| `POST /api/v1/session/start` | `{language?: Lang}` | `{session, reply: Reply, audio_url?}` | **Simulated button.** Enabled only when `hal.button = "sim"` (PC). On the Pi the GPIO button starts sessions and this returns 403 `button_disabled`. |
| `POST /api/v1/session/{id}/listen` | — | `{session}` | Moves to `LISTENING`. The UI may open the mic only after this succeeds. |
| `POST /api/v1/session/{id}/audio` | WAV body (`audio/wav`, 16 kHz mono 16-bit, ≤ 20 s) | `TurnResult` | Rejected (409 `bad_state`) unless the session is `LISTENING` both before the upload and when the turn starts (checked again under the turn lock, so audio that overlapped another turn is discarded, never transcribed). Audio is processed in memory and never written to disk. `transcript.text` is shown on screen with sensitive values (Aadhaar, phone, account, OTP/PIN/password…) masked as `[#]`. |
| `POST /api/v1/session/{id}/text` | `{text: string}` (≤ 500 chars) | `TurnResult` | Text fallback / keyboard. |
| `POST /api/v1/session/{id}/option` | `{option_id: string}` | `TurnResult` | Tapped an option button. |
| `POST /api/v1/session/{id}/speaking-done` | — | `{session}` | UI finished playing audio → `READY`, or `LISTENING` if `listen_next`. |
| `POST /api/v1/session/{id}/language` | `{language: Lang}` | `{session}` | Also pushes a `health` event: `voice_output` depends on the session language. |
| `POST /api/v1/session/{id}/greeting` | — | `{session, reply: Reply, audio_url?}` | The greeting again, in the session's current language, spoken if the TTS voice speaks it. The UI calls it after the citizen changes language on the welcome screens (the greeting it holds is in the old language), or when it finds an open session without a greeting (reload). Refused with 409 `bad_state` while a turn is running or the session is `PROCESSING`; from `LISTENING` the session first returns to `READY`. With audio the session goes to `SPEAKING` (release it with `speaking-done`). |
| `POST /api/v1/session/{id}/touch` | — | `{session}` | Citizen interacted (resets inactivity clock). |
| `POST /api/v1/session/{id}/end` | — | `{session, reply: Reply, audio_url?}` | Visible "End session" button. Returns goodbye. |
| `GET /api/v1/session/{id}/audio/{n}.wav` | — | `audio/wav` | Reply audio for turn n. Kept in RAM (bounded LRU, ≤ 8 clips) and dropped at session end. |
| `GET /api/v1/health` | — | health snapshot | Detailed component health (for technicians). |
| `POST /api/v1/session/{id}/camera-consent` | `{consent: boolean}` | `{session}` | Explicit per-session consent for the document check (see below). `true` is refused with 503 `unavailable` unless `capabilities.camera`. `false` withdraws it and forgets every document result of the session. |
| `POST /api/v1/session/{id}/document` | JPEG body (`image/jpeg`, ≤ 2 MB, long side ≤ 1600 px, short side ≥ 200 px) | `DocumentResult` | Requires consent (403 `no_consent`) and the session in `READY` or `LISTENING` (409 `bad_state` otherwise). Read once in memory by OCR, then dropped. |
| `POST /api/v1/session/{id}/document/confirm` | `{correct: boolean}` | `ConfirmResult` | The citizen's answer to "is this a <type>?". 409 `bad_state` when no photo awaits confirmation. |

### Agent behaviour (implemented in `services/device-agent/internal/api`)

* After a reply: with audio → `SPEAKING`; without audio and `end_session` →
  session ends; else `LISTENING` if `listen_next` and voice input works,
  otherwise `READY`. `speaking-done` (only valid in `SPEAKING`) applies the
  same rule, ending the session if the reply asked for it.
* `/text` and `/option` are also accepted in `LISTENING` (recording discarded)
  and `SPEAKING` (reply interrupted); a second concurrent turn gets 409 `bad_state`.
* Sessions started by the physical button carry the greeting on the SSE
  `session.started` event as extra fields `reply` and `audio_url`; the UI
  shows and plays it after the privacy notice.
* A physical button press during a session counts as interaction, unless the
  session looks abandoned (in `PROCESSING`/`SPEAKING` with no activity for
  2 min, or idle for 9 min): then it ends with reason `abandoned`
  (`inactivity_timeout` on the wire) and a fresh session starts, so the next
  citizen never inherits the previous conversation. Presses are handled on
  their own goroutine, single-flight.
* `capabilities.voice_output` is false for a session whose language the TTS
  voice does not speak (`speech.tts_langs`, default `["hi"]`: English
  sessions have no audio).
* A turn that panics returns the session to `READY` (500 `internal`); the
  session manager also caps `PROCESSING` and `SPEAKING` so nothing can wedge
  the kiosk. Speech synthesis gets its own minimum budget (10 s) even when
  the turn used most of `api.turn_timeout`; a reply cut short that way, or
  one "busy" answer, does not mark TTS unavailable (three busy answers in a
  row do).
* The goodbye clip returned by `/end` stays playable for `api.goodbye_grace`
  (20 s) after the session ended; all other clips are dropped at session end.
* Degraded: STT down → `voice_input=false`, `/listen` and `/audio` 503
  `unavailable`; TTS down → no `audio_url`; LLM down → deterministic replies;
  database down → `REQUIRES_ATTENTION` and session start refused with 503
  `unavailable` (sessions could not be recorded safely).
* Outbox events: `dialogue.turn` per turn and `session.ended` per session
  (schemas in `internal/deviceapi`), each in the same transaction as the
  `sessions` row update. No `session.started` event is sent because the central
  API rejects unknown types. A turn that finishes after its session ended
  (End pressed mid-turn) is not recorded. `started_at`/`ended_at` are
  truncated to the minute, on the device, both in `session.ended` and in the
  local `sessions` table. A full outbox refuses new events (none are
  dropped) and puts the critical `outbox` component in `REQUIRES_ATTENTION`;
  the session row is still closed, the refusal is counted in
  `outbox_refused` (`GET /api/v1/health`) and logged only at the 1st, 10th,
  100th and every 1,000th refusal. At startup the agent records the end of
  any session a killed predecessor left open (`end_reason` `error`, with its
  `session.ended` event).
* Health details (`components[].detail`) are fixed reason codes such as
  `stt_request_failed`, `db_write_failed`, `outbox_full`, `disk_low`,
  `speech_engine_hung`; error text goes only to the local log. The heartbeat
  enforces this: a detail that is not 1–4 codes matching `[a-z0-9_]{1,64}`
  (joined by `; `) is sent as `other` (`deviceapi.ReasonCode`), and central
  applies the same rule before storing.

### Document check (implemented in `internal/api/documents.go`, `internal/docassist`)

Purpose: help a citizen see which of the documents required by the scheme
being discussed they already have. It is **not** authentication and **not**
storage: OCR cannot tell a genuine document from a forged one, and every
response says so (`notice`).

```ts
type DocType = "aadhaar" | "ration_card" | "caste_certificate" | "income_certificate"
  | "residence_certificate" | "bank_passbook" | "voter_id" | "pan" | "land_record"
  | "birth_certificate" | "school_certificate" | "unknown";

interface DocumentResult {
  session: SessionSnapshot;
  document: {
    doc_type: DocType; label: string; confidence: "high" | "low" | "none";
    name?: string;               // only when clearly labelled ("नाम:", "Name:")
    issuing_authority?: string;  // labelled issuer, or a line naming an office/department
    issue_date?: string;         // DD/MM/YYYY, only next to an issue-date label (never a birth date)
    masked_ids?: { kind: "aadhaar" | "vid" | "pan" | "epic" | "account" | "number"; value: string }[];
                                 // last 4 characters only: "XXXX XXXX 1234"
  };
  text: string;        // "यह <type> लगता है — क्या यह सही है?" or how to retake
  can_confirm: boolean; // false for "unknown" (offer only "retake")
  notice: string;      // OCR does not verify authenticity; the photo was not kept
}

interface ConfirmResult {
  session: SessionSnapshot;
  text: string;
  confirmed: { doc_type: DocType; label: string }[];
  coverage?: {                 // absent when no scheme is being discussed
    scheme_id: string; scheme_name: string;
    mode: "factual" | "unverified_dev" | "name_only"; verified: boolean;
    items?: { name: string; note?: string; optional: boolean;
              status: "seen" | "not_seen" | "not_checkable" }[];
    message?: string;          // name_only: "verified details not available …" instead of items
    disclaimer?: string;       // unverified_dev: the "not verified (test mode)" warning
  };
  caveat?: string;             // "this compares only the kind of document …"
  notice: string;
}
```

Flow and rules:

* `capabilities.camera` is true only when document assistance is enabled
  (`documents.enabled`) and the `ocr` health component is usable. That check
  runs `tesseract --version`, confirms the configured languages are
  installed, and reads a tiny embedded self-test image. It runs every
  `documents.health_interval` (2 min).
* The UI offers "दस्तावेज़ जाँचें / Check my documents" while a scheme card
  is on screen. It shows a consent screen first, posts `camera-consent`, and
  opens `getUserMedia` (video only) only after that succeeds. It stops the
  video tracks right after the photo and whenever the screen closes. Closing
  the screen withdraws the consent.
* Upload: the agent moves the session to `PROCESSING` (a turn cannot run
  meanwhile) and pipes the JPEG into
  `tesseract - - -l eng+hin --psm 3` on stdin. Nothing is written to disk.
  The process runs with a minimal environment, `OMP_THREAD_LIMIT=1`, its own
  process group (killed on timeout, 15 s) and, on Linux, `RLIMIT_AS`
  (`documents.max_memory_mb`, 512), `RLIMIT_CORE` 0 (no core dump),
  `oom_score_adj` 1000 and a parent-death signal. Stdout is capped at 64 KB.
  One upload runs at a time per session: a second upload in the same session
  while one is being read gets 409 `bad_state`; an upload while the health
  self-test holds the engine gets 503 `unavailable`. The photo is read into
  one buffer (sized from `Content-Length`) that is zeroed afterwards. Then the
  session returns to `READY`.
* Language order: `eng+hin`, not `hin+eng`. On the synthetic fixtures,
  `hin+eng` dropped the digit 1 (`1234` → `234`); `eng+hin` read both
  scripts correctly. Configurable in `documents.languages`.
* The OCR text is classified by keyword rules (`internal/docassist/classify.go`;
  ties or weak evidence give `unknown`) and only the fields above leave the
  agent. Identifiers are masked, and every free-text field is scrubbed of
  long digit runs (whatever the separators: spaces, dots, slashes, dashes,
  commas, colons, underscores) and PAN/EPIC-shaped tokens (any case, with
  O/I/l read as digits). `issuing_authority` is left out when it holds more
  than four digits, and the unlabelled fallback skips address and relation
  lines (पता, ग्राम, पिन, के पास, S/O, Address, Near …). The raw OCR text and
  the document type are never logged (any level), stored or sent centrally.
* If consent is withdrawn or the session ends while OCR runs, the result is
  discarded (403 `no_consent` / 404 `no_session`).
* Coverage follows the knowledge display policy. The scheme record's
  `mandatory_docs` / `optional_docs` are compared only for a `factual` record,
  or for an `unverified_dev` record shown with its disclaimer. Otherwise the
  response says verification is required (`message`). Matching is by document
  kind only (`TypesFor`): a requirement naming a bank account maps to the
  passbook even if it also says "Aadhaar". A bare "bank" (for example "6
  months' bank statement") in a requirement that also names another kind
  of paper does not make the passbook count; passbook, account number or
  IFSC always do. Requirements a camera cannot check
  (forms, photos, affidavits) are `not_checkable`.
* Session end (or consent withdrawal) drops every document result. Camera
  consent never carries over to the next session.
* Failures: OCR timeout or busy → 503 `unavailable` (camera stays offered);
  a photo the engine cannot decode (for example a truncated JPEG that passed
  the header check) → 400 `bad_image` (camera stays offered);
  OCR process failure → 503 and `ocr` reported `UNAVAILABLE`
  (`ocr_failed`), so `capabilities.camera` turns false until the next
  passing health check.

---

## 2. Central API (v1)

Base: `https://<central-host>`.

### Device endpoints (Ed25519-signed, see `internal/syncproto`)

| Method & path | Purpose |
|---|---|
| `POST /v1/sync/events` | Batch upload of outbox events. Response is a signed `BatchAck`. |
| `POST /v1/devices/heartbeat` | Signed. Device health snapshot, versions, outbox counts, storage, temperature. |
| `GET /v1/devices/knowledge-bundle?since=<bundle_version>` | Signed. Latest signed scheme bundle (only VERIFIED/NEEDS_REVIEW records with status). |

### Admin endpoints (session cookie + CSRF protection)

| Method & path | Role | Purpose |
|---|---|---|
| `POST /v1/admin/login`, `POST /v1/admin/logout` | any | Server-side sessions (argon2id passwords). |
| `GET /v1/admin/me` | any | Current user and roles (the dashboard's session check). |
| `GET /v1/admin/analytics/summary?from&to` | viewer | Sessions, categories, languages, resolution rates. |
| `GET /v1/admin/devices`, `GET /v1/admin/devices/{id}` | viewer | Device registry, last heartbeat, sync status, versions. |
| `POST /v1/admin/devices` | device_admin | Register a device (public key from provisioning). |
| `POST /v1/admin/devices/{id}/revoke` | device_admin | Revoke a device key. |
| `GET /v1/admin/schemes`, `GET /v1/admin/schemes/{id}` | viewer | Knowledge records and history. |
| `GET /v1/admin/schemes/{id}/versions/{version}` | viewer | One version with its full record (used for diffs). |
| `PUT /v1/admin/schemes/{id}` | scheme_editor | Create/update a draft (status forced to NEEDS_REVIEW). |
| `POST /v1/admin/schemes/{id}/verify` | scheme_reviewer | Human verification (cannot be the same user as the last editor). |
| `POST /v1/admin/schemes/{id}/withdraw` | scheme_reviewer | Mark the live VERIFIED version OUTDATED (reason required). |
| `GET /v1/admin/bundles` | viewer | Published bundles, newest first. |
| `POST /v1/admin/bundles` | scheme_reviewer | Publish a signed bundle for devices. |
| `GET /v1/admin/audit` | auditor | Audit log. |

Full request/response schemas are in `services/central-api/openapi.yaml`.

### Admin dashboard (static UI)

`apps/admin-dashboard` is a Next.js 16 App Router app built as a pure static
export (`output: "export"`, `basePath: "/admin"`, `trailingSlash: true`): no
route handlers, Server Functions, middleware or image optimisation. central-api
serves the `out/` directory at `/admin/` when started with
`-admin-ui-dir` / `JS_ADMIN_UI_DIR` (GET/HEAD only, `os.Root`-confined, no
listings). The browser calls the admin API above on the same origin; the
`js_admin` cookie (HttpOnly, `Path=/v1/admin`) is never readable by page code.
Pages that need an id take it as a query parameter
(`/admin/schemes/detail/?id=…`, `/admin/devices/detail/?id=…`) because a
static export cannot pre-render unknown ids.

**CSP and inline scripts.** Next.js bootstraps every exported page with
inline `<script>` elements (`self.__next_f.push(...)`). The UI CSP stays
`script-src 'self'` with no `'unsafe-inline'` and no nonces (a static file
server cannot mint them). Instead `pnpm build` runs
`scripts/csp-hashes.mjs`, which writes `out/csp-hashes.json`
(`{"version":1,"files":{"<page>.html":["sha256-…"]}}`) with the SHA-256 of
each page's inline scripts. central-api reads it once at startup
(`static_ui.go`: ≤ 4 MiB, ≤ 2000 pages, ≤ 256 hashes per page, strict
`sha256-<base64>` format; a malformed file fails startup) and serves each HTML
page with `script-src 'self' '<its own hashes>'`. A page not listed, or a
build without the file, gets plain `script-src 'self'`, so its inline scripts
are blocked and it does not hydrate. **Restart central-api after deploying a
new dashboard build** so the new hashes are loaded. The hashes add no trust
beyond the files themselves: whoever can write the HTML can write the
manifest, exactly as with any served script.

Build and run locally:

```sh
pnpm --filter admin-dashboard build          # out/ + out/csp-hashes.json
scripts/dev/central.sh                       # or: central-api ... -admin-ui-dir apps/admin-dashboard/out
open http://127.0.0.1:8750/admin/
```

`pnpm dev:admin` starts `next dev` on :8751 for layout work only; it does not
proxy `/v1/admin`, so API calls need the built export behind central-api.

**Behaviour.** The UI hides actions the user's roles do not allow (mirror
of `roleReq.satisfiedBy`; `admin` satisfies all); the server remains the
authority. Verify requires the reviewer to tick a statement that they
personally checked every fact against the listed sources; the four-eyes rule
is explained before the server refuses with `own_edit`. Analytics counts the
server suppressed (`null`) are shown as `<5` and drawn as hatched bars, never
as zero. Devices count as online when the last heartbeat is under 15 minutes
old: three heartbeat intervals (the device agent sends one every 5 minutes,
±10 % jitter), so one lost heartbeat does not show a device offline.
The device list fetches per-device detail (3 at a time, visible page of 25
only) for disk, memory, temperature and last sync, because the list endpoint
carries only a heartbeat summary.
