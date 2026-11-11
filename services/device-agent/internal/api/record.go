package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/deviceapi"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/dialogue"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/session"
)

// What the agent records (all anonymous; see internal/deviceapi):
//
//	session start → INSERT sessions row (no outbox event: the central API
//	                accepts only session.ended and dialogue.turn, and an
//	                event it rejects would sit on the device as FAILED forever)
//	each turn     → UPDATE sessions.turns + outbox "dialogue.turn"   (one tx)
//	session end   → UPDATE sessions end   + outbox "session.ended"   (one tx)
//
// No transcript, typed text, option label or audio is ever written.
// Session start/end times are truncated to the minute (minuteTS).

const recordTimeout = 5 * time.Second

func (s *Server) recordStart(ctx context.Context, c *convo) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	s.mu.Lock()
	lang := c.lang
	s.mu.Unlock()
	_, err := s.d.DB.ExecContext(ctx, `INSERT INTO sessions (id, started_at, language) VALUES (?, ?, ?)`,
		c.id, rowTS(c.startedAt), lang)
	return err
}

// turnEvent builds the dialogue.turn payload from the engine's analytics,
// mapping engine-internal values onto the central schema's enumerations.
func turnEvent(sid string, seq int, src dialogue.Source, r dialogue.Reply, lang string) (deviceapi.Turn, bool) {
	a := r.Analytics
	ev := deviceapi.Turn{
		SessionID: sid, Seq: seq, Language: lang, Source: string(src),
		Intent: a.Intent, Category: a.Category, Service: a.Service, Resolution: a.Resolution, UsedLLM: r.UsedLLM,
	}
	if ev.Resolution == "in_progress" {
		ev.Resolution = "" // the turn asked a question; no outcome yet
	}
	if ev.Intent == "" {
		ev.Intent = "unknown"
	}
	if ev.Validate() == nil {
		return ev, true
	}
	// Keep the counter even if an engine value falls outside the schema.
	ev.Intent, ev.Category, ev.Service = "unknown", "", ""
	return ev, ev.Validate() == nil
}

// recordTurn numbers and records one dialogue turn. A turn that finishes
// after its session ended (End pressed mid-turn, timeout, shutdown) is not
// counted or recorded: session.ended already carries the final count.
func (s *Server) recordTurn(ctx context.Context, c *convo, src dialogue.Source, r dialogue.Reply, lang string) {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	s.mu.Lock()
	recorded, ended := c.recorded, c.ended
	s.mu.Unlock()
	if ended {
		return
	}
	seq := int(c.seq.Add(1))
	if seq > deviceapi.MaxTurns || !recorded {
		return
	}
	ev, ok := turnEvent(c.id, seq, src, r, lang)
	if !ok {
		s.log.Warn("api: turn analytics did not validate; not recorded", "intent", r.Analytics.Intent)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	err := s.inTx(ctx, func(x outbox.Execer) error {
		if _, err := x.ExecContext(ctx, `UPDATE sessions SET turns = ? WHERE id = ?`, seq, c.id); err != nil {
			return err
		}
		_, err := s.d.Outbox.Enqueue(ctx, x, deviceapi.TypeTurn, ev)
		return err
	})
	s.recordResult("turn", err)
}

func (s *Server) recordEnd(c *convo, lang string, reason session.EndReason, endedAt time.Time) {
	c.recMu.Lock() // after any turn record already in progress
	defer c.recMu.Unlock()
	turns := int(c.seq.Load())
	if turns > deviceapi.MaxTurns {
		turns = deviceapi.MaxTurns
	}
	ev := deviceapi.SessionEnded{
		SessionID: c.id, Language: lang, StartedAt: minuteTS(c.startedAt), EndedAt: minuteTS(endedAt),
		EndReason: reason.Wire(), Turns: turns, // abandoned → inactivity_timeout on the wire
	}
	if err := ev.Validate(); err != nil {
		s.log.Warn("api: session.ended did not validate; not recorded", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	err := s.inTx(ctx, func(x outbox.Execer) error {
		if _, err := x.ExecContext(ctx, `UPDATE sessions SET ended_at = ?, end_reason = ?, turns = ? WHERE id = ?`,
			rowTS(endedAt), string(reason), turns, c.id); err != nil {
			return err
		}
		_, err := s.d.Outbox.Enqueue(ctx, x, deviceapi.TypeSessionEnded, ev)
		return err
	})
	if errors.Is(err, outbox.ErrFull) {
		// The event is refused, but the session did end: close the local
		// row anyway, or the next start would treat it as left open by a
		// crash (CloseOrphanedSessions) and report it as an error.
		if _, uerr := s.d.DB.ExecContext(ctx, `UPDATE sessions SET ended_at = ?, end_reason = ?, turns = ? WHERE id = ?`,
			rowTS(endedAt), string(reason), turns, c.id); uerr != nil {
			s.log.Error("api: could not close the session row", "err", uerr)
		}
	}
	s.recordResult("session_end", err)
}

func (s *Server) inTx(ctx context.Context, fn func(outbox.Execer) error) error {
	tx, err := s.d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Server) recordResult(what string, err error) {
	if err == nil {
		return
	}
	s.recordErrors.Add(1)
	if errors.Is(err, outbox.ErrFull) {
		// Existing events are never dropped; this one is not stored. A
		// technician must restore sync or export the outbox. Logged on the
		// 1st, 10th, 100th, then every 1000th refusal (Phase 12 soak: 80k
		// identical lines in 4 minutes otherwise).
		if n := s.outboxRefused.Add(1); n == 1 || n == 10 || n == 100 || n%1000 == 0 {
			s.log.Error("api: outbox full; analytics event not stored", "event", what, "refused_total", n)
		}
		s.d.Health.Report(CompOutbox, health.Result{State: health.RequiresAttention, Detail: ReasonOutboxFull})
		return
	}
	s.log.Error("api: could not record analytics", "event", what, "err", err)
	s.d.Health.Report(CompDatabase, health.Result{State: health.RequiresAttention, Detail: ReasonDBWriteFailed})
}

// onSessionEnd is the session.Manager cleanup hook: it drops the
// conversation memory and reply audio, records the anonymous end event and
// kicks the sync worker. It must not call back into the Manager.
func (s *Server) onSessionEnd(id string, reason session.EndReason) {
	s.mu.Lock()
	c := s.convo
	var lang string
	recorded := false
	if c != nil && c.id == id {
		s.convo = nil
		c.ended = true
		lang, recorded = c.lang, c.recorded
	} else {
		c = nil
	}
	s.mu.Unlock()
	s.audio.DropSession(id)
	if c != nil && recorded {
		if !validLang(lang) {
			lang = s.d.DefaultLang
		}
		s.recordEnd(c, lang, reason, s.now())
	}
	if s.d.Kick != nil {
		s.d.Kick()
	}
}

// maxOrphansPerStart bounds the startup sweep (normally there is at most one).
const maxOrphansPerStart = 100

// CloseOrphanedSessions records the end of sessions that a previous agent
// process left open because it was killed (SIGKILL, OOM, watchdog, power
// cut) before it could record them. It must run at startup, before any
// session starts: at that point every row with ended_at NULL is an orphan.
//
// Each orphan gets end_reason "error" and a session.ended event in the same
// transaction as the row update, so it is either fully recorded or left for
// the next start (e.g. while the outbox is full). The real end time is
// unknown, so ended_at is the start minute (a zero-length session with
// reason "error" means "end not observed"); turns is the count already
// recorded, which matches the dialogue.turn events in the outbox.
func CloseOrphanedSessions(ctx context.Context, db *sql.DB, ob *outbox.Outbox) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, started_at, language, turns FROM sessions
		WHERE ended_at IS NULL ORDER BY started_at LIMIT ?`, maxOrphansPerStart)
	if err != nil {
		return 0, err
	}
	type orphan struct {
		id, startedAt, lang string
		turns               int
	}
	var list []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.id, &o.startedAt, &o.lang, &o.turns); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	closed := 0
	for _, o := range list {
		started, err := time.Parse(outbox.TimeLayout, o.startedAt)
		if err != nil {
			if started, err = time.Parse(time.RFC3339Nano, o.startedAt); err != nil {
				return closed, fmt.Errorf("session %s: bad started_at: %w", o.id, err)
			}
		}
		turns := min(max(o.turns, 0), deviceapi.MaxTurns)
		ev := deviceapi.SessionEnded{
			SessionID: o.id, Language: o.lang, StartedAt: minuteTS(started), EndedAt: minuteTS(started),
			EndReason: session.EndError.Wire(), Turns: turns,
		}
		if err := ev.Validate(); err != nil {
			return closed, fmt.Errorf("session %s: %w", o.id, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return closed, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at = ?, end_reason = ?, turns = ? WHERE id = ? AND ended_at IS NULL`,
			rowTS(started), string(session.EndError), turns, o.id); err != nil {
			_ = tx.Rollback()
			return closed, err
		}
		if _, err := ob.Enqueue(ctx, tx, deviceapi.TypeSessionEnded, ev); err != nil {
			_ = tx.Rollback()
			return closed, err
		}
		if err := tx.Commit(); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}
