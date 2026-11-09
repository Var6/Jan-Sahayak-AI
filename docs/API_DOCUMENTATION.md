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
| `GET /api/v1/events` | — | `text/event-stream` | Events: `session` (`{type, session, end_reason?}` where type ∈ `session.started`, `session.activity`, `session.reminder`, `session.final_warning`, `session.ended`, `session.language`, `session.camera_consent`), `health` (`{overall, capabilities}`), `ping` every 15 s. |
| `POST /api/v1/session/start` | `{language?: Lang}` | `{session, reply: Reply, audio_url?}` | **Simulated button.** Enabled only when `hal.button = "sim"` (PC). On the Pi the GPIO button starts sessions and this returns 403 `button_disabled`. |
| `POST /api/v1/session/{id}/listen` | — | `{session}` | Moves to `LISTENING`. The UI may open the mic only after this succeeds. |
| `POST /api/v1/session/{id}/audio` | WAV body (`audio/wav`, 16 kHz mono 16-bit, ≤ 20 s) | `TurnResult` | Rejected unless the session is `LISTENING`. Audio is processed in memory and never written to disk. |
| `POST /api/v1/session/{id}/text` | `{text: string}` (≤ 500 chars) | `TurnResult` | Text fallback / keyboard. |
| `POST /api/v1/session/{id}/option` | `{option_id: string}` | `TurnResult` | Tapped an option button. |
| `POST /api/v1/session/{id}/speaking-done` | — | `{session}` | UI finished playing audio → `READY`, or `LISTENING` if `listen_next`. |
| `POST /api/v1/session/{id}/language` | `{language: Lang}` | `{session}` | |
| `POST /api/v1/session/{id}/touch` | — | `{session}` | Citizen interacted (resets inactivity clock). |
| `POST /api/v1/session/{id}/end` | — | `{session, reply: Reply, audio_url?}` | Visible "End session" button. Returns goodbye. |
| `GET /api/v1/session/{id}/audio/{n}.wav` | — | `audio/wav` | Reply audio for turn n. Kept in RAM (bounded LRU, ≤ 8 clips) and dropped at session end. |
| `GET /api/v1/health` | — | health snapshot | Detailed component health (for technicians). |

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
