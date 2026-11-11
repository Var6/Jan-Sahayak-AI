-- Jan Sahayak device database, schema version 2.
-- Applied inside a single transaction by store.migrate().

-- Exact row count of outbox_events maintained by triggers, so the MaxRows
-- check on every insert is O(1) instead of a full count(*) scan inside the
-- write transaction (reliability review finding 13).
CREATE TABLE outbox_counter (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    n  INTEGER NOT NULL
) STRICT;
INSERT INTO outbox_counter (id, n) SELECT 1, count(*) FROM outbox_events;

CREATE TRIGGER outbox_count_insert
AFTER INSERT ON outbox_events
BEGIN
    UPDATE outbox_counter SET n = n + 1 WHERE id = 1;
END;

CREATE TRIGGER outbox_count_delete
AFTER DELETE ON outbox_events
BEGIN
    UPDATE outbox_counter SET n = n - 1 WHERE id = 1;
END;

-- When the current upload claim was made, so rows left in UPLOADING by a
-- failed ApplyAck/RetryBatch can be re-queued on the next sync pass instead
-- of waiting for a restart (reliability review finding 7).
ALTER TABLE outbox_events ADD COLUMN claimed_at TEXT;

-- Scheduling timestamps are now written fixed-width (RFC 3339, UTC, exactly
-- nine fractional digits) so text comparison equals time comparison
-- (reliability review finding 14). Normalise rows written by v1, which used
-- a variable number of fractional digits. Rounding down to the whole second
-- only makes a retry due slightly earlier. created_at is immutable (trigger)
-- and is not used for scheduling, so it is left untouched.
UPDATE outbox_events
   SET next_attempt_at = strftime('%Y-%m-%dT%H:%M:%S', next_attempt_at) || '.000000000Z'
 WHERE length(next_attempt_at) <> 30 AND strftime('%s', next_attempt_at) IS NOT NULL;
UPDATE outbox_events
   SET acked_at = strftime('%Y-%m-%dT%H:%M:%S', acked_at) || '.000000000Z'
 WHERE acked_at IS NOT NULL AND length(acked_at) <> 30 AND strftime('%s', acked_at) IS NOT NULL;
