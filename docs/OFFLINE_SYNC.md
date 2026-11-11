# Offline-First Synchronisation

Implementation: `services/device-agent/internal/outbox`, `.../syncer`,
`internal/syncproto` (shared wire contract), `internal/deviceapi` (event
payloads), central ingest in `services/central-api`. Schema and guards:
`services/device-agent/internal/store/migrations/0001_init.sql` and
`0002_outbox_counter_claimed_at.sql`.

## What is synced

Only anonymous operational events (no names, phone numbers, transcripts,
audio or images):

| Event | Payload fields |
|---|---|
| `dialogue.turn` | session id (random UUIDv4), seq, language, input source, intent, category, service id, resolution, used_llm |
| `session.ended` | session id, language, started_at, ended_at, end reason (`citizen_ended`, `inactivity_timeout`, `device_shutdown`, `error`), turns |
| (heartbeat — separate endpoint, see Heartbeat) | health snapshot (fixed reason codes), versions, outbox counts, disk, memory, temperature, throttling, agent uptime |

Session start is recorded only as a local `sessions` row; there is no
`session.started` event (the central API accepts only the two event types
above, and an event it rejects would sit on the device as FAILED). A session
ended because the next citizen pressed the button on an abandoned session
(`session.EndAbandoned`) is sent as `inactivity_timeout`
(`EndReason.Wire()`).

## State machine

```
            claim                 verified durable ack
 PENDING ───────────▶ UPLOADING ──────────────────────▶ ACKNOWLEDGED
  ▲  ▲                  │  │  │                              │ retention (7 days)
  │  │ retry / restart  │  │  │ conflict                     ▼
  │  └──────────────────┘  │  └────────▶ CONFLICT      DELETE_ELIGIBLE ──▶ deleted
  │                        │ rejected / max attempts       (operator requeue only)
  │                        └──────────▶ FAILED
  └──── automatic after 24 h, or operator requeue ──┘
```

Enforced twice: in Go (`outbox`) and in SQLite triggers, which refuse

* `DELETE` of any row not in `DELETE_ELIGIBLE`,
* any transition not drawn above,
* `ACKNOWLEDGED` without `ack_receipt` + `acked_at`,
* any change to an event's id, type, payload, hash or creation time.

## Protocol (v1)

1. **Store first.** Events are written to SQLite (WAL, `synchronous=FULL`)
   in the same transaction as the state change that caused them.
2. **Maintain, then claim.** At the start of every sync pass the worker
   runs `Outbox.Maintain` (see below), then moves ≤ 100 due `PENDING` events
   to `UPLOADING` under a new batch id (UUIDv7), stamping `claimed_at`.
   Claiming does not consume an attempt.
3. **Sign & send.** `POST /v1/sync/events`, body = `BatchRequest`, headers
   `X-JS-Device`, `X-JS-Ts`, `X-JS-Nonce` (128-bit random), `X-JS-Sig` =
   Ed25519 over `JS1\nPOST\n/v1/sync/events\n<ts>\n<nonce>\n<sha256(body)>`.
4. **Server validates** device (active, key on file), timestamp (±5 min),
   nonce (unique per device, stored with TTL), body size, batch schema,
   each event (UUID, type, RFC 3339 time, payload ≤ 16 KB, SHA-256 matches).
5. **Server commits** all events in one transaction with
   `ON CONFLICT (id) DO NOTHING`; per event: `accepted` (new), `duplicate`
   (same id + same hash — returns the original receipt), `conflict` (same id,
   different hash), `rejected` (invalid).
6. **Server returns a signed `BatchAck`** (Ed25519 over a canonical,
   order-independent encoding of batch id, device id, server time, key id and
   each `id|status|payload_sha256|receipt`). No ack is sent if the
   transaction fails.
7. **Device verifies** the ack signature against pinned server keys (key ids
   allow rotation), batch id, device id, that every result refers to an event
   in the batch, and per event that status is accepted/duplicate **and** the
   echoed hash equals the local hash **and** a receipt is present. Only then
   → `ACKNOWLEDGED`.
8. **Transport / HTTP-level failure** (network error, DNS, timeout, non-200
   including redirects — the client never follows 3xx — HTTP 200 without a
   valid ack, bad signature, wrong batch, unknown event ids) → the whole
   batch returns to `PENDING`, due immediately, and **no attempt is
   consumed**. The worker's own backoff (15 s doubling to 30 min, plus up
   to 25 % jitter) paces these retries. A kiosk can be offline for months
   without a single event leaving `PENDING`.
9. **Per-event verdicts** inside a verified ack are the only thing that
   consumes an attempt: a missing result, an unexpected status or an echoed
   hash mismatch returns that event to `PENDING` with per-event backoff
   (30 s doubling to 6 h); `rejected` moves it to `FAILED` directly; after
   `MaxAttempts` (50) verdicts it becomes `FAILED`. `conflict` →
   `CONFLICT`.
10. **FAILED is not final.** `FAILED` rows are kept and automatically
    re-queued (attempts reset to 0) 24 h after they failed
    (`FailedRequeueAfter`), by `Maintain`. `CONFLICT` rows (same id,
    different payload on the server) need a human and are re-queued only by
    the operator command `Outbox.RequeueFailed`, which re-queues both
    `FAILED` and `CONFLICT` immediately.
11. **Housekeeping** (hourly) promotes `ACKNOWLEDGED` rows older than the
    retention window to `DELETE_ELIGIBLE` and deletes those, at most 1,000
    rows per statement, and prunes old local session rows (see Retention).

### Per-pass maintenance (`Outbox.Maintain`)

Runs before each sync pass, when no batch of this worker is in flight:

* `UPLOADING` rows claimed more than 2 × the HTTP timeout ago (or with a
  claim time in the future after a clock jump) return to `PENDING`. This
  covers an `ApplyAck` / `RetryBatch` that failed on a database error,
  which previously left rows `UPLOADING` until the next restart.
* `FAILED` rows whose 24 h wait has elapsed return to `PENDING`.
* Retry times are capped at now + 6 h (`PENDING`) and now + 24 h
  (`FAILED`); housekeeping caps `acked_at` at now. A Pi without an RTC
  battery that boots with a clock far behind (or whose clock is corrected
  backwards) therefore delays retries and deletion by at most one normal
  backoff, not by the size of the jump.

### Timestamps

Scheduling timestamps (`next_attempt_at`, `acked_at`, `claimed_at`,
`created_at` of new events) are written fixed-width: RFC 3339, UTC, exactly
nine fractional digits (`2026-11-09T10:00:00.000000000Z`), so SQLite text
comparison is chronological. Migration 2 normalises rows written by older
versions.

### Crash and power-loss recovery

* At startup every `UPLOADING` row returns to `PENDING`. The upload may or
  may not have reached the server; resending is safe because the server is
  idempotent on event id (tested: lost response after commit → resend →
  `duplicate` → acknowledged once, stored once).
* WAL + `synchronous=FULL` means a committed event survives power loss.
* At boot the database is classified before anything is moved:
  * **Genuine corruption** — `PRAGMA quick_check` returns anything other
    than a single `ok` row, or SQLite reports `SQLITE_CORRUPT` /
    `SQLITE_NOTADB`: the file (with its `-wal`/`-shm`) is **quarantined,
    never deleted**, the newest verified backup is restored (or a fresh
    database created), and then every unacknowledged outbox row
    (`PENDING`, `UPLOADING`→`PENDING`, `FAILED`, `CONFLICT`) that can still
    be read from the quarantined file is **salvaged** into the live
    database (`INSERT OR IGNORE` by event id; payloads must still match
    their SHA-256, damaged rows are skipped and counted). `store.Recovery`
    reports `salvaged_events`, `salvage_skipped` and `salvage_error`; the
    quarantined file is kept for a technician either way.
  * **Anything else** — permission denied (e.g. files left owned by root
    after a manual `sudo sqlite3`), I/O error, lock held, cannot open:
    `store.Open` returns `store.ErrUnreadable`, nothing is moved, and the
    agent exits so systemd retries and the fault is visible.
* Backups (`VACUUM INTO`) are fsynced, then renamed into place and the
  backup directory is fsynced; restores stream the backup (no whole-file
  read into RAM) and fsync both the file and its directory. Quarantine
  renames fsync both directories.

### Storage pressure

* The outbox is capped (`MaxRows`, default 100,000 ≈ years of normal use).
  The row count is kept in a trigger-maintained `outbox_counter` table, so
  the check on every insert is O(1). When full, **new** analytics events
  are refused (`outbox.ErrFull`); existing events are never dropped to make
  room. `Outbox.Full(ctx)` and `Stats.Full` expose the condition for the
  health check, which must report `REQUIRES_ATTENTION`.
* Temporary audio is never written to disk; reply audio lives in a bounded
  in-memory cache cleared at session end. These cleanups are independent of
  event retention.

## Heartbeat (`services/device-agent/internal/heartbeat`)

Signed `POST /v1/devices/heartbeat` (body `deviceapi.Heartbeat`, same
`X-JS-*` signature scheme as uploads). Runs only when `sync.base_url` is set.

* **Cadence:** first heartbeat 30 s (±10 %) after start, then every
  `sync.heartbeat_interval` (default 5 min, allowed 1 min–1 h, ±10 % jitter).
  Failures back off: interval doubling, capped at 30 min. HTTP 429
  `too_frequent` waits `max(Retry-After, interval)` and is not counted as a
  failure. Each request has a deadline (`sync.http_timeout`, default 30 s),
  redirects are never followed, responses are read up to 64 KiB.
* **Never blocks anything:** one goroutine; the snapshot sources (outbox
  stats, disk, `ps`/`vcgencmd`) are bounded by a 10 s deadline. Offline is
  normal: failures are only logged (first three, then every 12th) and
  counted in `GET /api/v1/health` → `connectivity.heartbeat`.
* **Contents:** health overall + components (fixed reason codes only; long
  values are clipped, never sent as free text), agent version, OS/arch,
  loaded bundle version, runtime labels (`stt`/`tts`/`llm` engine or `off`),
  outbox counts by state, sync last success + consecutive failures, disk
  free/total of the data dir, system memory, SoC temperature, throttling
  (any "now" bit of `vcgencmd get_throttled`), and **agent process uptime**.
  Values a platform cannot read (memory/temperature/throttling on macOS) are
  omitted, never invented.
* **Restarts:** the wire format has no restart counter, so a restart shows
  centrally as an uptime reset. The device counts its own starts in
  `device_meta['agent.start_count']` (shown in `GET /api/v1/health` →
  `connectivity.agent_start_count`). Sending the counter needs a
  `deviceapi` + central schema change (not done).
* **Clock skew:** a 401 `clock_skew` records server − device seconds
  (`connectivity.heartbeat.clock_skew_seconds`) for the technician.
* The response's `latest_bundle_version` nudges bundle sync (below).

## Knowledge bundle sync (`services/device-agent/internal/bundlesync`)

Signed `GET /v1/devices/knowledge-bundle?since=<loaded version>` (empty
body; the signature covers path **and** query). Runs only when
`sync.base_url` is set.

* **When:** once 15 s after start, then every `sync.bundle_interval`
  (default 6 h, allowed 15 min–7 days, ±10 %); also when a heartbeat
  reports a `latest_bundle_version` that is neither loaded nor previously
  refused (at most once per 10 min). Failures back off 5 min doubling to
  1 h. **No check starts while a citizen session is open** (re-tried every
  minute), so facts never change mid-conversation.
* **Answers:** `204` → up to date. `404 no_bundle` → nothing published yet.
  `200` → body read up to `deviceapi.MaxBundleBytes` (8 MiB);
  `X-JS-Bundle-SHA256` checked (transport corruption only); then:
  1. decode `scheme.Bundle`;
  2. `deviceapi.VerifyBundle` against the pinned `sync.server_keys`
     (**always**, on every profile — the network is never trusted);
  3. `bundle_version` must be the central `YYYY-MM-DD.N` format;
  4. `knowledge.Store.LoadBundle`: refuses an older version
     (`knowledge.ErrBundleRollback`), validates every record, refuses a
     bundle with no valid record, and replaces the scheme table + FTS index
     + `device_meta['knowledge.bundle_version']` in **one transaction**;
  5. the exact signed bytes are written atomically (temp file, fsync,
     rename, dir fsync; mode 0640) to `<data_dir>/knowledge/bundle.json`.
* **Refusals** are logged at ERROR with a fixed reason
  (`bundle_signature_invalid`, `bundle_rollback_refused`,
  `bundle_no_valid_records`, `bundle_checksum_mismatch`,
  `bundle_version_malformed`, `bundle_too_large`, …); the previous knowledge
  and the cached file stay unchanged.
* **Cached file reuse:** at start, if the cached file's version differs from
  the database's (e.g. the database was rebuilt from an older backup), the
  file is verified like a download and loaded; an older file is refused
  (rollback). While the cache does not hold the loaded version, checks omit
  `?since` so the file is rewritten from the server.
* **Boot-time signed-bundle rule (already enforced, unchanged):**
  non-`dev*` profiles force `knowledge.require_signed_bundle=true`; a
  configured `knowledge.path` must then be a bundle file signed by a pinned
  key. If that configured bundle is older than the loaded one (normal after
  a sync), it is skipped with an INFO log and the newer knowledge stays.
* **Health (`knowledge` component):** data `bundle_version`,
  `bundle_age_days` (from the date in the version, IST), and `bundle_sync`
  (last check/result/error, last loaded/refused version). A central bundle
  older than `knowledge.stale_after` (default 30 days) → `DEGRADED`
  `bundle_stale`. Local dev versions (`local-…`) have no age.

## Dev provisioning (`scripts/dev/provision.sh`)

Idempotent: builds `central-admin` + `device-agent`, migrates the dev
database, creates `var/central/server.key` if missing, reads/creates the
device identity (`device-agent -print-identity`), registers the device public
key (refuses if the id is registered with another key), creates `dev-admin`,
`dev-editor`, `dev-reviewer` (random passwords stored once in
`var/central/dev-users.json`, mode 0600), and writes
`<data_dir>/provisioned.json` = base dev config + `sync.base_url` + the
pinned server public key. `--text-only` turns AI runtimes off;
`--knowledge-from-central` blanks `knowledge.path`.

```
scripts/dev/central.sh &                     # central on 127.0.0.1:8750
JS_DEVICE_DATA_DIR=$PWD/var/device-e2e scripts/dev/provision.sh --text-only --knowledge-from-central
bin/device-agent -config var/device-e2e/provisioned.json
```

## Retention (separate concerns)

| Data | Where | Retention |
|---|---|---|
| Raw audio, images | RAM only | dropped at end of each request/session |
| Transcripts, conversation state | RAM only | dropped at session end |
| Outbox events | device SQLite | until acknowledged + 7 days |
| Local anonymous session rows | device SQLite | 90 days (`SessionRetention`), then pruned hourly once no unacknowledged outbox event refers to the session |
| Synced events | central PostgreSQL | per Janman data policy (TBD) |
| Consented personal data | not collected in v1 | separate encrypted schema when added |

## Tests that prove the guarantees

`go test -race ./services/device-agent/internal/{store,outbox,syncer}/`

* `TestDatabaseRefusesToDeleteUnacknowledgedEvents`,
  `TestIllegalTransitionsAndImmutablePayload` (SQLite triggers).
* `TestUntrustworthyAcksNeverAcknowledge` (missing result, hash mismatch,
  no receipt, unknown status).
* `TestRandomisedNoLossInvariant` — 3,000 random operations including
  forged/partial acks, crashes, clock jumps and raw unsafe `DELETE`s: no
  event disappears without a durable, hash-matching acknowledgement.
* `TestBadServerResponsesNeverAcknowledge` — forged signature, wrong batch,
  HTTP 500, HTTP 200 without valid ack, ack naming unknown events, response
  lost after server commit.
* `TestResendAfterLostResponseIsIdempotent`, `TestRecoverInterruptedAfterPowerLoss`,
  `TestFullOutboxRefusesNewButKeepsOld`, `TestBackupAndCorruptionRecovery`.
* `TestBackoffAndGiveUpKeepsRow` (transport failures consume no attempts;
  verdicts do; FAILED auto-requeued after 24 h),
  `TestRejectedIsAutoRequeuedButConflictIsNot`,
  `TestMaintainRequeuesStaleUploading`, `TestStaleUploadingRequeuedOnNextPass`,
  `TestBackwardsClockJumpIsCapped`, `TestTimestampsAreFixedWidth`,
  `TestHousekeepDeletesInChunks`, `TestPruneSessions`,
  `TestRedirectIsNotFollowed`.
* `TestCorruptionRecoverySalvagesUnackedEvents`,
  `TestUnreadableDatabaseFailsLoudlyAndIsNotMoved`,
  `TestOutboxCounterTracksRows`.
* Heartbeat / bundle sync (`go test -race ./services/device-agent/internal/{heartbeat,bundlesync}/`):
  httptest central fakes that verify the real device signatures —
  `TestHeartbeatAcceptedAndCarriesSnapshot`, `TestHeartbeatHasOnlyContractFields`,
  `TestFailuresAreCountedAndBackOff`, `TestOfflineBackoffIsBounded`,
  `TestHungServerIsBoundedByTimeout`; `TestLoadThenUpToDateThenNewer`,
  `TestRefusals` (rollback, unpinned key, altered record/version, checksum,
  malformed version, no valid record, too large, 5xx, redirect, unregistered
  device), `TestBusyPostponesWithoutNetwork`, `TestRestoreFromDisk`,
  `TestStaleCacheIsRewritten`, `TestNotifyLatestRateLimit`.
* Against a real central API (opt-in): `TestAgainstRealCentral` with
  `JS_CENTRAL_URL`, `JS_CENTRAL_DEV_USERS`, `JS_CENTRAL_SERVER_PUB` (see the
  file header) — upload + ack, bundle load, 204, replayed older genuine
  bundle refused, heartbeat stored.
* Review repros: `go test -race -tags reliabilityrepro ./services/device-agent/internal/{store,syncer}/`
  and `JS_REVIEW=1 go test -race ./services/device-agent/internal/outbox/`.
