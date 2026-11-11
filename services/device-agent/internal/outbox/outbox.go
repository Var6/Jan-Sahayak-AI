// Package outbox stores events that must reach the central server and moves
// them through the acknowledgement state machine. The SQLite triggers in
// store/migrations are the last line of defence; this package is the normal
// path and never issues a DELETE for a row that is not DELETE_ELIGIBLE.
//
// Retry policy (lead-engineer decision 1, reliability review finding 2):
//   - Transport and HTTP-level failures (no network, DNS, timeout, non-200,
//     invalid or unverifiable ack) never consume an event's attempt budget:
//     RetryBatch returns the batch to PENDING, immediately due, and the sync
//     worker's own backoff (15 s → 30 min with jitter) paces the retries. An
//     offline kiosk therefore keeps every event PENDING however long it is
//     offline.
//   - Only a per-event verdict inside a verified ack consumes an attempt
//     (missing result, unexpected status, hash mismatch, rejected).
//   - After MaxAttempts verdicts, or on "rejected", an event becomes FAILED.
//     FAILED events are kept and automatically re-queued (attempts reset)
//     FailedRequeueAfter (24 h) later by Maintain; RequeueFailed is the
//     operator command that re-queues FAILED and CONFLICT events at once.
//     CONFLICT is never re-queued automatically.
//
// Timestamps used for scheduling are stored fixed-width (RFC 3339 UTC with
// exactly nine fractional digits) so SQLite text comparison is time order.
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
	// Backoff bounds for per-event verdict retries.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// FailedRequeueAfter is how long a FAILED event waits before Maintain
	// automatically re-queues it with a fresh attempt budget.
	FailedRequeueAfter time.Duration
	// SessionRetention is how long local anonymous session rows are kept
	// (PruneSessions); rows are pruned only once no unacknowledged outbox
	// event refers to them.
	SessionRetention time.Duration
}

// DefaultConfig is sized for a kiosk: ~200 sessions/day × a few events.
var DefaultConfig = Config{
	MaxRows:            100_000,
	AckRetention:       7 * 24 * time.Hour,
	MaxAttempts:        50,
	BaseBackoff:        30 * time.Second,
	MaxBackoff:         6 * time.Hour,
	FailedRequeueAfter: 24 * time.Hour,
	SessionRetention:   90 * 24 * time.Hour,
}

// chunk bounds every bulk UPDATE/DELETE issued by housekeeping so no single
// write transaction grows with the table.
const chunk = 1000

// ErrFull means the outbox reached MaxRows.
var ErrFull = errors.New("outbox: full; refusing new events (existing events are kept)")

// Outbox is safe for concurrent use (SQLite serialises writers).
type Outbox struct {
	db  *sql.DB
	cfg Config
	now func() time.Time
}

// New creates an Outbox. now may be nil. Zero Config fields take their
// DefaultConfig value.
func New(db *sql.DB, cfg Config, now func() time.Time) *Outbox {
	if now == nil {
		now = time.Now
	}
	d := DefaultConfig
	setDef := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	if cfg.MaxRows <= 0 {
		cfg.MaxRows = d.MaxRows
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = d.MaxAttempts
	}
	setDef(&cfg.AckRetention, d.AckRetention)
	setDef(&cfg.BaseBackoff, d.BaseBackoff)
	setDef(&cfg.MaxBackoff, d.MaxBackoff)
	setDef(&cfg.FailedRequeueAfter, d.FailedRequeueAfter)
	setDef(&cfg.SessionRetention, d.SessionRetention)
	return &Outbox{db: db, cfg: cfg, now: now}
}

// TimeLayout is the fixed-width timestamp format written by the outbox:
// RFC 3339, UTC, exactly nine fractional digits (30 bytes). Fixed width makes
// lexical order equal chronological order in SQL comparisons, and it is
// still valid RFC 3339 for the wire.
const TimeLayout = "2006-01-02T15:04:05.000000000Z"

func ts(t time.Time) string { return t.UTC().Format(TimeLayout) }

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
	n, err := rowCount(ctx, x)
	if err != nil {
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

// rowCount reads the trigger-maintained row counter (O(1); schema v2).
func rowCount(ctx context.Context, x Execer) (int, error) {
	var n int
	err := x.QueryRowContext(ctx, `SELECT n FROM outbox_counter WHERE id = 1`).Scan(&n)
	return n, err
}

// Full reports whether the outbox has reached MaxRows, i.e. new analytics
// events are being refused (ErrFull). Health checks use it to report
// REQUIRES_ATTENTION. A read error is reported as not full; database
// health is checked separately.
func (o *Outbox) Full(ctx context.Context) bool {
	n, err := rowCount(ctx, o.db)
	return err == nil && n >= o.cfg.MaxRows
}

// Claim moves up to max due PENDING events to UPLOADING under a new batch id
// and returns them as wire events. Claiming does not consume an attempt.
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
	claimedAt := ts(o.now())
	for _, e := range evs {
		if _, err := tx.ExecContext(ctx, `UPDATE outbox_events
			SET state='UPLOADING', upload_batch_id=?, claimed_at=? WHERE id=? AND state='PENDING'`, batch, claimedAt, e.ID); err != nil {
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
// batch without a durable result go back to PENDING with backoff; each such
// per-event verdict consumes one attempt. Call this only with results
// returned by syncproto.VerifyAck.
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
			_, err = tx.ExecContext(ctx, `UPDATE outbox_events SET state='FAILED', attempts=attempts+1, next_attempt_at=?, last_error=? WHERE id=?`,
				ts(now.Add(o.cfg.FailedRequeueAfter)), trunc("rejected: "+res.Reason), r.id)
			out.Failed++
		default:
			reason := "no durable acknowledgement"
			if ok {
				reason = fmt.Sprintf("unexpected result status=%q or hash mismatch", res.Status)
			}
			var failed bool
			failed, err = o.verdictRetryLocked(ctx, tx, r.id, r.attempts+1, reason, now)
			if failed {
				out.Failed++
			} else {
				out.Retried++
			}
		}
		if err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}

// RetryBatch returns every UPLOADING event of a batch to PENDING after a
// transport or HTTP-level failure (network error, timeout, invalid ack,
// server 5xx). It does NOT consume attempts and makes the events due
// immediately: the sync worker's own backoff paces transport retries, so a
// kiosk that is offline for months loses nothing and fails nothing.
func (o *Outbox) RetryBatch(ctx context.Context, batchID, reason string) error {
	_, err := o.db.ExecContext(ctx, `UPDATE outbox_events SET state='PENDING', next_attempt_at=?, last_error=?
		WHERE upload_batch_id=? AND state='UPLOADING'`, ts(o.now()), trunc(reason), batchID)
	return err
}

// verdictRetryLocked records one consumed attempt for a per-event verdict:
// back to PENDING with exponential backoff, or FAILED (kept, auto-requeued
// after FailedRequeueAfter) once MaxAttempts is reached.
func (o *Outbox) verdictRetryLocked(ctx context.Context, tx *sql.Tx, id string, attempts int, reason string, now time.Time) (failed bool, err error) {
	if attempts >= o.cfg.MaxAttempts {
		_, err := tx.ExecContext(ctx, `UPDATE outbox_events SET state='FAILED', attempts=?, next_attempt_at=?, last_error=? WHERE id=?`,
			attempts, ts(now.Add(o.cfg.FailedRequeueAfter)), trunc(fmt.Sprintf("gave up after %d attempts: %s", attempts, reason)), id)
		return true, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE outbox_events SET state='PENDING', attempts=?, next_attempt_at=?, last_error=? WHERE id=?`,
		attempts, ts(now.Add(o.Backoff(attempts))), trunc(reason), id)
	return false, err
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

// RequeueFailed moves FAILED and CONFLICT events back to PENDING with a
// fresh attempt budget. This is the operator command; FAILED (but not
// CONFLICT) events are also re-queued automatically by Maintain.
func (o *Outbox) RequeueFailed(ctx context.Context) (int64, error) {
	res, err := o.db.ExecContext(ctx, `UPDATE outbox_events SET state='PENDING', attempts=0, next_attempt_at=?
		WHERE state IN ('FAILED','CONFLICT')`, ts(o.now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MaintainResult reports what Maintain changed.
type MaintainResult struct {
	// StaleUploading rows were left in UPLOADING by a failed ApplyAck or
	// RetryBatch and went back to PENDING.
	StaleUploading int64
	// FailedRequeued rows were FAILED for FailedRequeueAfter and went back to
	// PENDING with a fresh attempt budget.
	FailedRequeued int64
	// ClockCapped rows had a retry time further in the future than any
	// schedule can produce (the wall clock went backwards, e.g. a Pi without
	// an RTC battery booting before NTP) and were pulled in.
	ClockCapped int64
}

// Maintain runs at the start of every sync pass, when no batch of this
// worker is in flight:
//   - caps next_attempt_at at now+MaxBackoff (PENDING) and
//     now+FailedRequeueAfter (FAILED), so a backwards clock jump cannot
//     delay retries by the size of the jump;
//   - re-queues UPLOADING rows claimed more than staleAfter ago (or "in
//     the future" after a clock jump);
//   - re-queues FAILED rows whose FailedRequeueAfter has elapsed.
func (o *Outbox) Maintain(ctx context.Context, staleAfter time.Duration) (MaintainResult, error) {
	var r MaintainResult
	now := o.now()
	nowS := ts(now)
	exec := func(dst *int64, q string, args ...any) error {
		res, err := o.db.ExecContext(ctx, q, args...)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		*dst += n
		return nil
	}
	pendCap, failCap := ts(now.Add(o.cfg.MaxBackoff)), ts(now.Add(o.cfg.FailedRequeueAfter))
	if err := exec(&r.ClockCapped, `UPDATE outbox_events SET next_attempt_at=? WHERE state='PENDING' AND next_attempt_at > ?`, pendCap, pendCap); err != nil {
		return r, err
	}
	if err := exec(&r.ClockCapped, `UPDATE outbox_events SET next_attempt_at=? WHERE state='FAILED' AND next_attempt_at > ?`, failCap, failCap); err != nil {
		return r, err
	}
	if staleAfter > 0 {
		if err := exec(&r.StaleUploading, `UPDATE outbox_events SET state='PENDING', next_attempt_at=?,
			last_error='upload claim went stale (ack or retry was not applied)'
			WHERE state='UPLOADING' AND (claimed_at IS NULL OR claimed_at <= ? OR claimed_at > ?)`,
			nowS, ts(now.Add(-staleAfter)), nowS); err != nil {
			return r, err
		}
	}
	if err := exec(&r.FailedRequeued, `UPDATE outbox_events SET state='PENDING', attempts=0, next_attempt_at=?
		WHERE state='FAILED' AND next_attempt_at <= ?`, nowS, nowS); err != nil {
		return r, err
	}
	return r, nil
}

// Housekeep promotes ACKNOWLEDGED rows older than AckRetention to
// DELETE_ELIGIBLE and deletes DELETE_ELIGIBLE rows, in chunks of 1000 rows
// per statement. Only acknowledged rows are ever touched.
func (o *Outbox) Housekeep(ctx context.Context) (promoted, deleted int64, err error) {
	now := o.now()
	// A backwards clock jump must not postpone deletion by the jump size.
	if _, err := o.db.ExecContext(ctx, `UPDATE outbox_events SET acked_at=? WHERE state='ACKNOWLEDGED' AND acked_at > ?`, ts(now), ts(now)); err != nil {
		return 0, 0, err
	}
	cut := ts(now.Add(-o.cfg.AckRetention))
	promoted, err = o.chunked(ctx, `UPDATE outbox_events SET state='DELETE_ELIGIBLE', delete_eligible_at=?
		WHERE rowid IN (SELECT rowid FROM outbox_events WHERE state='ACKNOWLEDGED' AND acked_at <= ? LIMIT ?)`, ts(now), cut, chunk)
	if err != nil {
		return promoted, 0, err
	}
	deleted, err = o.chunked(ctx, `DELETE FROM outbox_events
		WHERE rowid IN (SELECT rowid FROM outbox_events WHERE state='DELETE_ELIGIBLE' LIMIT ?)`, chunk)
	return promoted, deleted, err
}

// PruneSessions deletes local session rows that started more than
// SessionRetention ago and that no unacknowledged outbox event (PENDING,
// UPLOADING, FAILED, CONFLICT) refers to — i.e. their session.ended (and
// dialogue.turn) events were acknowledged or have already been deleted after
// acknowledgement, or never existed (a session cut short by a power cut).
// Outbox rows themselves are never touched here.
func (o *Outbox) PruneSessions(ctx context.Context) (int64, error) {
	cut := ts(o.now().Add(-o.cfg.SessionRetention))
	return o.chunked(ctx, `DELETE FROM sessions WHERE rowid IN (
		SELECT rowid FROM sessions
		 WHERE started_at < ?
		   AND id NOT IN (SELECT json_extract(payload, '$.session_id') FROM outbox_events
		                   WHERE state IN ('PENDING','UPLOADING','FAILED','CONFLICT')
		                     AND json_extract(payload, '$.session_id') IS NOT NULL)
		 LIMIT ?)`, cut, chunk)
}

// chunked repeats a bounded statement until it affects fewer rows than one
// chunk or ctx ends. Each repetition is its own short transaction.
func (o *Outbox) chunked(ctx context.Context, q string, args ...any) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := o.db.ExecContext(ctx, q, args...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < chunk {
			return total, nil
		}
	}
}

// Stats counts events per state plus the age of the oldest pending event.
type Stats struct {
	ByState         map[string]int `json:"by_state"`
	Total           int            `json:"total"`
	OldestPendingAt string         `json:"oldest_pending_at,omitempty"`
	Capacity        int            `json:"capacity"`
	// Full is true when Total has reached Capacity and new events are
	// refused; health reports this as REQUIRES_ATTENTION.
	Full bool `json:"full"`
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
	s.Full = s.Total >= s.Capacity
	return s, nil
}

func trunc(s string) string {
	if len(s) > syncproto.MaxReasonLength {
		return s[:syncproto.MaxReasonLength]
	}
	return s
}
