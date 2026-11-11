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
| (heartbeat — separate endpoint) | health snapshot, versions, outbox counts, disk, memory, temperature |

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
* Review repros: `go test -race -tags reliabilityrepro ./services/device-agent/internal/{store,syncer}/`
  and `JS_REVIEW=1 go test -race ./services/device-agent/internal/outbox/`.
