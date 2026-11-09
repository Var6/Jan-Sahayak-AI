# Offline-First Synchronisation

Implementation: `services/device-agent/internal/outbox`, `.../syncer`,
`internal/syncproto` (shared wire contract), central ingest in
`services/central-api`. Schema and guards:
`services/device-agent/internal/store/migrations/0001_init.sql`.

## What is synced

Only anonymous operational events (no names, phone numbers, transcripts,
audio or images):

| Event | Payload fields |
|---|---|
| `session.started` | session id (random UUIDv4), language, started_at |
| `turn` | session id, intent, category, service id, resolution, language, input source, used_llm |
| `session.ended` | session id, end reason, turns, duration |
| (heartbeat — separate endpoint) | health snapshot, versions, outbox counts, disk, memory, temperature |

## State machine

```
            claim                 verified durable ack
 PENDING ───────────▶ UPLOADING ──────────────────────▶ ACKNOWLEDGED
    ▲                   │  │  │                              │ retention (7 days)
    │ backoff / restart │  │  │ conflict                     ▼
    └───────────────────┘  │  └────────▶ CONFLICT      DELETE_ELIGIBLE ──▶ deleted
                           │ rejected / max attempts
                           └──────────▶ FAILED  (operator: requeue)
```

Enforced twice: in Go (`outbox`) and in SQLite triggers, which refuse

* `DELETE` of any row not in `DELETE_ELIGIBLE`,
* any transition not drawn above,
* `ACKNOWLEDGED` without `ack_receipt` + `acked_at`,
* any change to an event's id, type, payload, hash or creation time.

## Protocol (v1)

1. **Store first.** Events are written to SQLite (WAL, `synchronous=FULL`)
   in the same transaction as the state change that caused them.
2. **Claim.** The worker moves ≤ 100 due `PENDING` events to `UPLOADING`
   under a new batch id (UUIDv7).
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
8. **Anything else** (network error, timeout, non-200, HTTP 200 without a
   valid ack, bad signature, wrong batch, unknown event ids) → the whole
   batch returns to `PENDING` with exponential backoff (30 s doubling to
   6 h per event; worker-level backoff 15 s → 30 min with jitter).
9. **Housekeeping** promotes `ACKNOWLEDGED` rows older than the retention
   window to `DELETE_ELIGIBLE` and deletes those.

### Crash and power-loss recovery

* At startup every `UPLOADING` row returns to `PENDING`. The upload may or
  may not have reached the server; resending is safe because the server is
  idempotent on event id (tested: lost response after commit → resend →
  `duplicate` → acknowledged once, stored once).
* WAL + `synchronous=FULL` means a committed event survives power loss.
* If the database fails `PRAGMA quick_check` at boot it is **quarantined,
  never deleted**, and the newest verified backup is restored. Events
  written after that backup are recoverable from the quarantined file by a
  technician.

### Storage pressure

* The outbox is capped (`MaxRows`, default 100,000 ≈ years of normal use).
  When full, **new** analytics events are refused and the device reports
  `REQUIRES_ATTENTION`; existing events are never dropped to make room.
* Temporary audio is never written to disk; reply audio lives in a bounded
  in-memory cache cleared at session end. These cleanups are independent of
  event retention.

## Retention (separate concerns)

| Data | Where | Retention |
|---|---|---|
| Raw audio, images | RAM only | dropped at end of each request/session |
| Transcripts, conversation state | RAM only | dropped at session end |
| Outbox events | device SQLite | until acknowledged + 7 days |
| Local anonymous session rows | device SQLite | 90 days (configurable) |
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
