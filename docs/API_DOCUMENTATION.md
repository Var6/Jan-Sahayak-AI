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
* JSON bodies are limited to 16 KB; audio uploads to 1 MB.
* Errors: `{"error": {"code": "string", "message": "string"}}` with an
  appropriate HTTP status. Codes: `no_session`, `wrong_session`,
  `bad_state`, `bad_request`, `too_large`, `bad_audio`, `unavailable`,
  `button_disabled`, `internal`.

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
  dropped) and puts the critical `outbox` component in `REQUIRES_ATTENTION`.
* Health details (`components[].detail`) are fixed reason codes such as
  `stt_request_failed`, `db_write_failed`, `outbox_full`, `disk_low`,
  `speech_engine_hung`; error text goes only to the local log.

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
| `GET /v1/admin/analytics/summary?from&to` | viewer | Sessions, categories, languages, resolution rates. |
| `GET /v1/admin/devices`, `GET /v1/admin/devices/{id}` | viewer | Device registry, last heartbeat, sync status, versions. |
| `POST /v1/admin/devices` | device_admin | Register a device (public key from provisioning). |
| `POST /v1/admin/devices/{id}/revoke` | device_admin | Revoke a device key. |
| `GET /v1/admin/schemes`, `GET /v1/admin/schemes/{id}` | viewer | Knowledge records and history. |
| `PUT /v1/admin/schemes/{id}` | scheme_editor | Create/update a draft (status forced to NEEDS_REVIEW). |
| `POST /v1/admin/schemes/{id}/verify` | scheme_reviewer | Human verification (cannot be the same user as the last editor). |
| `POST /v1/admin/bundles` | scheme_reviewer | Publish a signed bundle for devices. |
| `GET /v1/admin/audit` | auditor | Audit log. |

Full request/response schemas are in `services/central-api/openapi.yaml`.
