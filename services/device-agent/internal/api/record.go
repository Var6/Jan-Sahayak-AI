package api

import (
	"context"
	"errors"
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
		// technician must restore sync or export the outbox.
		s.log.Error("api: outbox full; analytics event not stored", "event", what)
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
