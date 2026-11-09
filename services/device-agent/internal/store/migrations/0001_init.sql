-- Jan Sahayak device database, schema version 1.
-- Applied inside a single transaction by store.migrate().

-- Small key/value table for device identity and runtime metadata.
CREATE TABLE device_meta (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

-- Anonymous session log. No names, phone numbers, audio or transcripts.
CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,               -- random UUIDv4
    started_at  TEXT NOT NULL,
    ended_at    TEXT,
    language    TEXT NOT NULL,
    end_reason  TEXT,
    turns       INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX sessions_started_at ON sessions(started_at);

-- Outbox of events waiting to reach the central server.
--
-- State machine (enforced by triggers below):
--   PENDING -> UPLOADING -> ACKNOWLEDGED -> DELETE_ELIGIBLE -> (deleted)
--   UPLOADING -> PENDING (retry) | FAILED | CONFLICT
--   FAILED -> PENDING (operator/automatic retry)
-- Rows may be deleted ONLY in DELETE_ELIGIBLE state.
CREATE TABLE outbox_events (
    id                 TEXT PRIMARY KEY,         -- UUID, also the idempotency key
    type               TEXT NOT NULL,
    schema_version     INTEGER NOT NULL DEFAULT 1,
    payload            TEXT NOT NULL CHECK (json_valid(payload)),
    payload_sha256     TEXT NOT NULL,
    created_at         TEXT NOT NULL,
    state              TEXT NOT NULL DEFAULT 'PENDING'
                       CHECK (state IN ('PENDING','UPLOADING','ACKNOWLEDGED','DELETE_ELIGIBLE','FAILED','CONFLICT')),
    attempts           INTEGER NOT NULL DEFAULT 0,
    next_attempt_at    TEXT NOT NULL,
    last_error         TEXT,
    upload_batch_id    TEXT,
    acked_at           TEXT,
    ack_receipt        TEXT,                     -- server receipt id for audit
    delete_eligible_at TEXT
) STRICT;
CREATE INDEX outbox_due ON outbox_events(state, next_attempt_at);

-- Guard 1: unacknowledged events can never be deleted, whatever code runs.
CREATE TRIGGER outbox_no_unsafe_delete
BEFORE DELETE ON outbox_events
WHEN OLD.state <> 'DELETE_ELIGIBLE'
BEGIN
    SELECT RAISE(ABORT, 'outbox: refusing to delete event that is not DELETE_ELIGIBLE');
END;

-- Guard 2: only legal state transitions.
CREATE TRIGGER outbox_legal_transition
BEFORE UPDATE OF state ON outbox_events
WHEN NEW.state <> OLD.state AND NOT (
       (OLD.state = 'PENDING'      AND NEW.state = 'UPLOADING')
    OR (OLD.state = 'UPLOADING'    AND NEW.state IN ('PENDING','ACKNOWLEDGED','FAILED','CONFLICT'))
    OR (OLD.state = 'ACKNOWLEDGED' AND NEW.state = 'DELETE_ELIGIBLE')
    OR (OLD.state = 'FAILED'       AND NEW.state = 'PENDING')
    OR (OLD.state = 'CONFLICT'     AND NEW.state = 'PENDING')
)
BEGIN
    SELECT RAISE(ABORT, 'outbox: illegal state transition');
END;

-- Guard 3: ACKNOWLEDGED requires a receipt.
CREATE TRIGGER outbox_ack_requires_receipt
BEFORE UPDATE OF state ON outbox_events
WHEN NEW.state = 'ACKNOWLEDGED' AND (NEW.ack_receipt IS NULL OR NEW.acked_at IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'outbox: ACKNOWLEDGED requires ack_receipt and acked_at');
END;

-- Guard 4: payloads are immutable once written.
CREATE TRIGGER outbox_payload_immutable
BEFORE UPDATE OF id, type, payload, payload_sha256, created_at ON outbox_events
BEGIN
    SELECT RAISE(ABORT, 'outbox: event identity and payload are immutable');
END;

-- Local copy of the scheme knowledge base (from signed bundles).
CREATE TABLE schemes (
    id                  TEXT NOT NULL,
    version             INTEGER NOT NULL,
    status              TEXT NOT NULL CHECK (status IN ('VERIFIED','NEEDS_REVIEW','OUTDATED','UNVERIFIED')),
    category            TEXT NOT NULL,
    state_code          TEXT,                    -- NULL = central scheme
    name_en             TEXT NOT NULL,
    name_hi             TEXT NOT NULL,
    record              TEXT NOT NULL CHECK (json_valid(record)),
    last_verified_at    TEXT,
    bundle_version      TEXT NOT NULL,
    PRIMARY KEY (id)
) STRICT;

-- Full-text index over scheme names, aliases and keywords (Hindi + English).
CREATE VIRTUAL TABLE schemes_fts USING fts5(
    id UNINDEXED,
    text,
    tokenize = 'unicode61 remove_diacritics 0'
);
