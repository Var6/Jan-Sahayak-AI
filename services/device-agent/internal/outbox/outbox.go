// Package outbox stores events that must reach the central server and moves
// them through the acknowledgement state machine. The SQLite triggers in
// store/migrations are the last line of defence; this package is the normal
// path and never issues a DELETE for a row that is not DELETE_ELIGIBLE.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/internal/syncproto"
)

// State values (mirror the CHECK constraint).
const (
	Pending        = "PENDING"
	Uploading      = "UPLOADING"
	Acknowledged   = "ACKNOWLEDGED"
	DeleteEligible = "DELETE_ELIGIBLE"
	Failed         = "FAILED"
	Conflict       = "CONFLICT"
)

// Config bounds the outbox.
type Config struct {
	// MaxRows caps the number of rows not yet deleted. When reached, new
	// events are refused (ErrFull) — existing records are never dropped.
	MaxRows int
	// AckRetention is how long ACKNOWLEDGED rows stay before becoming
	// DELETE_ELIGIBLE (gives operators an audit window).
	AckRetention time.Duration
	// MaxAttempts after which an event goes to FAILED (still kept).
	MaxAttempts int
	// Backoff bounds.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// DefaultConfig is sized for a kiosk: ~200 sessions/day × a few events.
var DefaultConfig = Config{
	MaxRows:      100_000,
	AckRetention: 7 * 24 * time.Hour,
	MaxAttempts:  50,
	BaseBackoff:  30 * time.Second,
	MaxBackoff:   6 * time.Hour,
}

// ErrFull means the outbox reached MaxRows.
var ErrFull = errors.New("outbox: full; refusing new events (existing events are kept)")

// Outbox is safe for concurrent use (SQLite serialises writers).
type Outbox struct {
	db  *sql.DB
	cfg Config
	now func() time.Time
}

// New creates an Outbox. now may be nil.
func New(db *sql.DB, cfg Config, now func() time.Time) *Outbox {
	if now == nil {
		now = time.Now
	}
	if cfg.MaxRows <= 0 {
		cfg = DefaultConfig
	}
	return &Outbox{db: db, cfg: cfg, now: now}
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Execer is satisfied by *sql.DB and *sql.Tx so events can be enqueued in
// the same transaction as the state change that produced them.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Enqueue stores a new event and returns its id. payload is marshalled to
// compact JSON and must be a JSON object under MaxPayloadBytes.
func (o *Outbox) Enqueue(ctx context.Context, x Execer, eventType string, payload any) (string, error) {
	if x == nil {
		x = o.db
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("outbox: marshal: %w", err)
	}
	if len(b) == 0 || b[0] != '{' {
		return "", errors.New("outbox: payload must be a JSON object")
	}
	if len(b) > syncproto.MaxPayloadBytes {
		return "", fmt.Errorf("outbox: payload %d bytes exceeds %d", len(b), syncproto.MaxPayloadBytes)
	}
	if eventType == "" || len(eventType) > syncproto.MaxEventTypeLen || strings.ContainsAny(eventType, "\n|") {
		return "", errors.New("outbox: bad event type")
	}
	var n int
	if err := x.QueryRowContext(ctx, `SELECT count(*) FROM outbox_events`).Scan(&n); err != nil {
		return "", err
	}
	if n >= o.cfg.MaxRows {
		return "", ErrFull
	}
	id := ids.NewV7()
	now := ts(o.now())
	_, err = x.ExecContext(ctx, `INSERT INTO outbox_events
		(id, type, schema_version, payload, payload_sha256, created_at, state, next_attempt_at)
		VALUES (?, ?, 1, ?, ?, ?, 'PENDING', ?)`,
		id, eventType, string(b), syncproto.PayloadHash(b), now, now)
	if err != nil {
		return "", fmt.Errorf("outbox: insert: %w", err)
	}
	return id, nil
}

// Claim moves up to max due PENDING events to UPLOADING under a new batch id
// and returns them as wire events.
func (o *Outbox) Claim(ctx context.Context, max int) (string, []syncproto.Event, error) {
	if max <= 0 || max > syncproto.MaxBatchEvents {
		max = syncproto.MaxBatchEvents
	}
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, type, schema_version, created_at, payload, payload_sha256
		FROM outbox_events WHERE state='PENDING' AND next_attempt_at <= ?
		ORDER BY id LIMIT ?`, ts(o.now()), max)
	if err != nil {
		return "", nil, err
	}
	var evs []syncproto.Event
	for rows.Next() {
		var e syncproto.Event
		var payload string
		if err := rows.Scan(&e.ID, &e.Type, &e.SchemaVersion, &e.CreatedAt, &payload, &e.PayloadSHA256); err != nil {
			rows.Close()
			return "", nil, err
		}
		e.Payload = []byte(payload)
		evs = append(evs, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if len(evs) == 0 {
		return "", nil, nil
	}
	batch := ids.NewV7()
	for _, e := range evs {
		if _, err := tx.ExecContext(ctx, `UPDATE outbox_events
			SET state='UPLOADING', upload_batch_id=?, attempts=attempts+1 WHERE id=? AND state='PENDING'`, batch, e.ID); err != nil {
			return "", nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return batch, evs, nil
}

// Outcome summarises how a verified ack was applied.
type Outcome struct {
	Acknowledged int
	Retried      int
	Conflicts    int
	Failed       int
}

// ApplyAck applies VERIFIED per-event results for a batch. Events of the
// batch without a durable result go back to PENDING with backoff. Call this
// only with results returned by syncproto.VerifyAck.
func (o *Outbox) ApplyAck(ctx context.Context, batchID string, results map[string]syncproto.Result) (Outcome, error) {
	var out Outcome
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, payload_sha256, attempts FROM outbox_events
		WHERE upload_batch_id=? AND state='UPLOADING'`, batchID)
	if err != nil {
		return out, err
	}
	type row struct {
		id, hash string
		attempts int
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.hash, &r.attempts); err != nil {
			rows.Close()
			return out, err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	now := o.now()
	for _, r := range rs {
		res, ok := results[r.id]
		switch {
		case ok && syncproto.IsDurablyStored(res, r.hash):
			_, err = tx.ExecContext(ctx, `UPDATE outbox_events SET state='ACKNOWLEDGED', acked_at=?, ack_receipt=?, last_error=NULL WHERE id=?`,
				ts(now), res.Receipt, r.id)
			out.Acknowledged++
		case ok && res.Status == syncproto.StatusConflict:
			_, err = tx.ExecContext(ctx, `UPDATE outbox_events SET state='CONFLICT', last_error=? WHERE id=?`, trunc("conflict: "+res.Reason), r.id)
			out.Conflicts++
		case ok && res.Status == syncproto.StatusRejected:
			_, err = tx.ExecContext(ctx, `UPDATE outbox_events SET state='FAILED', last_error=? WHERE id=?`, trunc("rejected: "+res.Reason), r.id)
			out.Failed++
		default:
			reason := "no durable acknowledgement"
			if ok {
				reason = fmt.Sprintf("unexpected result status=%q or hash mismatch", res.Status)
			}
			err = o.retryLocked(ctx, tx, r.id, r.attempts, reason, now)
			out.Retried++
		}
		if err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}

// RetryBatch returns every UPLOADING event of a batch to PENDING (network
// error, invalid ack, server 5xx).
func (o *Outbox) RetryBatch(ctx context.Context, batchID, reason string) error {
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, attempts FROM outbox_events WHERE upload_batch_id=? AND state='UPLOADING'`, batchID)
	if err != nil {
		return err
	}
	type row struct {
		id       string
		attempts int
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.attempts); err != nil {
			rows.Close()
			return err
		}
		rs = append(rs, r)
	}
	rows.Close()
	now := o.now()
	for _, r := range rs {
		if err := o.retryLocked(ctx, tx, r.id, r.attempts, reason, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (o *Outbox) retryLocked(ctx context.Context, tx *sql.Tx, id string, attempts int, reason string, now time.Time) error {
	if attempts >= o.cfg.MaxAttempts {
		_, err := tx.ExecContext(ctx, `UPDATE outbox_events SET state='FAILED', last_error=? WHERE id=?`,
			trunc(fmt.Sprintf("gave up after %d attempts: %s", attempts, reason)), id)
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE outbox_events SET state='PENDING', next_attempt_at=?, last_error=? WHERE id=?`,
		ts(now.Add(o.Backoff(attempts))), trunc(reason), id)
	return err
}

// Backoff returns the delay before attempt n+1: exponential with a cap and
// deterministic per-attempt jitter-free schedule (jitter is added by the
// sync worker's own timer to avoid thundering herds across devices).
func (o *Outbox) Backoff(attempts int) time.Duration {
	d := o.cfg.BaseBackoff
	for i := 1; i < attempts && d < o.cfg.MaxBackoff; i++ {
		d *= 2
	}
	if d > o.cfg.MaxBackoff {
		d = o.cfg.MaxBackoff
	}
	return d
}

// RecoverInterrupted returns UPLOADING events to PENDING. Call once at
// startup: an upload interrupted by a crash or power cut may or may not have
// reached the server; resending is safe because the server is idempotent.
func (o *Outbox) RecoverInterrupted(ctx context.Context) (int64, error) {
	res, err := o.db.ExecContext(ctx, `UPDATE outbox_events SET state='PENDING', next_attempt_at=?,
		last_error='upload interrupted (restart)' WHERE state='UPLOADING'`, ts(o.now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RequeueFailed moves FAILED/CONFLICT events back to PENDING (operator action).
func (o *Outbox) RequeueFailed(ctx context.Context) (int64, error) {
	res, err := o.db.ExecContext(ctx, `UPDATE outbox_events SET state='PENDING', attempts=0, next_attempt_at=?
		WHERE state IN ('FAILED','CONFLICT')`, ts(o.now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Housekeep promotes ACKNOWLEDGED rows older than AckRetention to
// DELETE_ELIGIBLE and deletes DELETE_ELIGIBLE rows. Only acknowledged rows
// are ever touched.
func (o *Outbox) Housekeep(ctx context.Context) (promoted, deleted int64, err error) {
	now := o.now()
	cut := ts(now.Add(-o.cfg.AckRetention))
	res, err := o.db.ExecContext(ctx, `UPDATE outbox_events SET state='DELETE_ELIGIBLE', delete_eligible_at=?
		WHERE state='ACKNOWLEDGED' AND acked_at <= ?`, ts(now), cut)
	if err != nil {
		return 0, 0, err
	}
	promoted, _ = res.RowsAffected()
	res, err = o.db.ExecContext(ctx, `DELETE FROM outbox_events WHERE state='DELETE_ELIGIBLE'`)
	if err != nil {
		return promoted, 0, err
	}
	deleted, _ = res.RowsAffected()
	return promoted, deleted, nil
}

// Stats counts events per state plus the age of the oldest pending event.
type Stats struct {
	ByState         map[string]int `json:"by_state"`
	Total           int            `json:"total"`
	OldestPendingAt string         `json:"oldest_pending_at,omitempty"`
	Capacity        int            `json:"capacity"`
}

// Stats returns current counts.
func (o *Outbox) Stats(ctx context.Context) (Stats, error) {
	s := Stats{ByState: map[string]int{}, Capacity: o.cfg.MaxRows}
	rows, err := o.db.QueryContext(ctx, `SELECT state, count(*) FROM outbox_events GROUP BY state`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return s, err
		}
		s.ByState[st] = n
		s.Total += n
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	var oldest sql.NullString
	_ = o.db.QueryRowContext(ctx, `SELECT min(created_at) FROM outbox_events WHERE state IN ('PENDING','UPLOADING','FAILED','CONFLICT')`).Scan(&oldest)
	s.OldestPendingAt = oldest.String
	return s, nil
}

func trunc(s string) string {
	if len(s) > syncproto.MaxReasonLength {
		return s[:syncproto.MaxReasonLength]
	}
	return s
}
