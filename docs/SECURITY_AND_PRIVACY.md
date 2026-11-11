# Security and Privacy

Status: design + implemented controls as of 2026-11-09. Items marked
**[legal review]** need advice from a qualified lawyer before pilot.

## 1. Data minimisation (what we do not collect)

* No recording of visitors. Microphone opens only after a session is started
  by the physical button **and** the agent has put the session into
  `LISTENING`; the browser releases the mic after each utterance.
* Camera opens only after explicit per-session consent for document help
  (consent never carries over to the next session).
* No face recognition, no biometrics, no continuous sensing.
* The kiosk never asks for OTPs, passwords, PINs, bank credentials or full
  Aadhaar numbers. If a citizen starts saying them, the kiosk tells them not
  to share such details here and does not echo or store them.
* Raw audio is processed in memory and never written to disk. Transcripts
  and conversation state exist only in RAM for the session.
* Analytics (outbox) contain only: random session id, device id, timestamps,
  language, intent/category, scheme id, resolution, input source,
  whether the LLM was used. No free text.
* No document images stored, ever (Phase 11: OCR in memory, the citizen
  confirms the detected document type; see section 1a).

## 1a. Document check (Phase 11)

Purpose: tell a citizen which of the documents the scheme they are asking
about needs they seem to have already. It is **not** identity verification,
**not** an authenticity check and **not** storage.

| Step | Control |
|---|---|
| Consent | The UI shows a consent screen first. It says the camera is used only for this check, the photo is read once inside the kiosk and then discarded, and nothing is saved or sent. It also says to show an Aadhaar card only if needed, and preferably only its heading with the number covered. The agent records consent per session (`camera-consent`). Uploads without it get 403 `no_consent`, and consent never carries over to the next session. |
| Camera | `getUserMedia` with video only (no audio), opened only after the consent POST succeeded. Every track is stopped right after the photo and whenever the screen closes or the session ends. Closing the screen withdraws the consent. Chromium policy allows video capture for `http://127.0.0.1:8730` only, and the agent's `Permissions-Policy` is `camera=(self)`. |
| Photo | One downscaled JPEG (≤ 1600 px, ≤ 2 MB) per upload, held only in memory. The agent pipes it into a `tesseract` subprocess on stdin and reads the text from stdout; no file is written by the agent. The request buffer is zeroed after OCR. The browser keeps no copy (the canvas is cleared, no storage APIs). |
| OCR process | Minimal environment, one OpenMP thread, its own process group killed on timeout (15 s), stdout capped at 64 KB, one process at a time. On Linux it also gets `RLIMIT_AS` (512 MB) and a parent-death signal. It inherits the agent unit's sandbox (`ProtectSystem=strict`, `PrivateTmp`, seccomp `@system-service`, `MemoryMax`). |
| What leaves OCR | Only the document type, a clearly labelled name, the issuing office, a labelled issue date, and identifiers masked to their last 4 characters ("XXXX XXXX 1234"). Free-text fields are scrubbed of long digit runs and PAN/EPIC-shaped tokens. The raw OCR text is never returned to the UI, logged, put in analytics or sent centrally. |
| Retention | The pending result and the confirmed document types live in the session's RAM conversation. They are dropped when consent is withdrawn and when the session ends; a result that finishes after either is discarded. No outbox event is written for document checks. |
| Facts | Comparison with a scheme's document list follows the knowledge display policy. It is used only for human-verified (`factual`) records, or in dev with the "not verified" disclaimer. Otherwise the citizen is told verification is required. Matching is by document kind only, and the reply says that conditions (whose document, how recent) must be confirmed at the office. |

Residual risks / to verify **[legal review]** for the DPDP aspects:

* The name and masked identifiers are personal data shown on screen to the
  citizen holding the document. A bystander could see them, as with the
  paper document itself.
* Leptonica decodes images from memory (`fmemopen`) on the platforms checked,
  but this has not been traced on the Pi. Run
  `strace -f -e trace=openat,creat` during a document check on the Pi to
  confirm that no temp file is created.
* The kiosk cannot detect that someone is photographing another person's
  document.

## 2. Device security

| Control | Implementation |
|---|---|
| Local API exposure | Bound to 127.0.0.1 only; Host-header allow-list (DNS-rebinding); `http.CrossOriginProtection` + custom header on mutations (CSRF); body limits (16 KB JSON, 1 MB audio). |
| Mic/camera gating | Enforced server-side by `session.RequireListening` / `RequireCameraConsent`; UI cannot bypass. |
| Device identity | Ed25519 key generated on device at first boot, file mode 0600, never leaves the device; only the public key is registered centrally. |
| Sync integrity | Signed requests (timestamp + nonce), signed per-event acknowledgements, pinned server keys with key ids for rotation. |
| Process isolation | AI runtimes in separate processes; systemd hardening (`NoNewPrivileges`, `ProtectSystem=strict`, `PrivateTmp`, `ReadWritePaths=/var/lib/jan-sahayak`, `MemoryMax`). |
| LLM permissions | None. No tools, shell, filesystem, database or network. Receives only engine-built prompts; output validated. |
| Updates | Every release ships `SHA256SUMS`; the Pi installer runs `verify-release.sh` before copying anything and refuses on any mismatch or unlisted file. With `/etc/jan-sahayak/release-allowed-signers` present it also requires an SSH signature (`ssh-keygen -Y verify`). Signing-key custody pending (D-025). Rollback planned (Phase 12–13). |
| Model supply chain | Every model file — including each file inside an archive — has a pinned SHA-256; `scripts/models/install.sh` verifies before install and `--verify` re-hashes every file. |
| Encryption at rest | Pi: LUKS full-disk encryption of the data partition with key sealed to device **[to evaluate: Pi 5 has no TPM; options are a passphrase at provisioning or an external secure element]**. The device holds no personal data in v1, which limits the impact. |
| Kiosk lockdown | Chromium `--kiosk --incognito`, no URL bar, no downloads, keyboard shortcuts disabled, auto-restart. |

## 3. Central security

* Admin authentication: argon2id password hashes; server-side sessions
  (random 32-byte token, stored hashed; `HttpOnly; Secure; SameSite=Strict`).
* RBAC roles: viewer, device_admin, scheme_editor, scheme_reviewer, auditor,
  admin. A reviewer cannot verify their own edit.
* Audit log, scheme versions and published bundles are append-only: database
  triggers block UPDATE/DELETE, and in production the API runs as a separate
  runtime role that owns no tables and has only SELECT/INSERT on them, so it
  cannot disable those triggers (`deployment/server/README.md`,
  `JS_STRICT_DB_ROLE=true`).
* Scheme verification is four-eyes: a reviewer cannot verify a scheme if they
  edited any version since its last VERIFIED version (handler + trigger).
* Admin login: uniform 401 responses and progressive per-IP/per-username
  delays (no hard lockout that could be abused to lock out real admins).
* Device revocation is a single status change; revoked devices cannot sync.
* TLS for all device ↔ server traffic (terminated at the server or a reverse
  proxy) in addition to the application-level signatures.

## 4. DPDP Act 2023 and Rules — design alignment **[legal review]**

| Principle | How the design aligns |
|---|---|
| Purpose limitation & minimisation | Anonymous analytics only by default; no personal data collected in v1. |
| Notice & consent | On-screen privacy notice at each session start (Hindi/English); explicit consent screens before any optional personal data or camera use. |
| Storage limitation | RAM-only transcripts/audio; outbox retention; documented retention table (`OFFLINE_SYNC.md`). |
| Security safeguards | Section 2 & 3 controls; breach-notification runbook TBD. |
| Rights (access, correction, erasure, grievance) | Needed only when consented personal data is introduced; design a separate encrypted `citizen_assistance` schema with per-record consent and deletion. |
| Children's data | Kiosk does not knowingly collect children's data; consent flows would need verifiable parental consent **[legal review]**. |

Other legal items **[legal review]**:

* Representation: the kiosk must not appear to be an official government
  service. It introduces itself as a computer assistant from Janman People's
  Foundation (subject to Janman approval of wording) and links only to
  official channels. Use of any government logos requires authorisation.
* Re-use of government web content: attribution required; several central
  portals require email permission (see `KNOWLEDGE_SOURCES.md`); myScheme
  automated access needs written NeGD permission.
* Model licences: Supertonic-3 OpenRAIL-M use restrictions must be passed on;
  Piper "rohan" voice provenance unresolved (not used).

## 5. Threats considered

| Threat | Mitigation |
|---|---|
| Someone at the kiosk tries to make it chat/act outside scope | Topic guard, deterministic flows, validator; LLM has no capabilities. |
| Prompt injection via spoken/typed text or a scanned document | Inputs treated as data; injection patterns redirected; validator blocks invented facts. |
| Malicious or oversized image | JPEG header check (magic bytes, dimensions ≤ 1600 px) before OCR, 2 MB body cap, OCR in a separate time- and memory-limited process; the OCR text never reaches the LLM or the dialogue engine. |
| Malicious website on the kiosk browser reaching the agent | Loopback bind + Host check + CSRF protection; kiosk browser locked to the agent origin. |
| Stolen device | No personal data stored; device key revocable centrally; disk encryption planned. |
| Forged or replayed sync traffic | Ed25519 signatures, timestamp window, nonce store. |
| Server lies / MITM ack | Device only trusts acks signed by pinned server keys and matching its own hashes. |
| Fake knowledge bundle | Bundles signed centrally; device verifies before import (Phase 8–9). |
