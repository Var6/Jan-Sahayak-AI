package api

import (
	"context"
	"encoding/binary"
	"sync"
	"time"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/session"
)

// inflight counts work that must finish before the database closes: HTTP
// handlers and button-press goroutines. Unlike sync.WaitGroup it may be
// closed while work is still being added (late requests are refused).
type inflight struct {
	mu     sync.Mutex
	n      int
	closed bool
	zero   chan struct{} // closed when n drops to 0 after close
}

// add registers one unit of work; false means the server is shutting down.
func (f *inflight) add() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.n++
	return true
}

func (f *inflight) done() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n--
	if f.n == 0 && f.zero != nil {
		close(f.zero)
		f.zero = nil
	}
}

// wait refuses new work and waits until running work has finished.
func (f *inflight) wait(ctx context.Context) error {
	f.mu.Lock()
	f.closed = true
	if f.n == 0 {
		f.mu.Unlock()
		return nil
	}
	if f.zero == nil {
		f.zero = make(chan struct{})
	}
	z := f.zero
	f.mu.Unlock()
	select {
	case <-z:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Drain refuses new button presses and waits (bounded by ctx) until every
// in-flight handler and button goroutine has returned, so their analytics
// writes finish before main closes the database. Call it after
// http.Server.Shutdown.
func (s *Server) Drain(ctx context.Context) error {
	s.Close()
	return s.work.wait(ctx)
}

// PressButton is the HAL callback for the physical button. It never
// blocks: the press runs on its own goroutine, single-flight (presses
// arriving while one is being handled are dropped, like a bounce).
func (s *Server) PressButton(ctx context.Context) {
	if !s.buttonBusy.CompareAndSwap(false, true) {
		return
	}
	if !s.work.add() {
		s.buttonBusy.Store(false)
		return
	}
	go func() {
		defer s.work.done()
		defer s.buttonBusy.Store(false)
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("api: button press panicked", "panic", p)
			}
		}()
		s.buttonPress(ctx)
	}()
}

// Button presses on an open session normally count as interaction. A
// session that looks abandoned is replaced by a fresh one instead, so the
// next citizen never inherits the previous citizen's conversation.
func (s *Server) abandoned(cur session.Snapshot) bool {
	idle := s.now().Sub(cur.LastActiveAt)
	switch cur.Activity {
	case session.Processing, session.Speaking:
		// Nothing legitimate keeps a session here this long without an
		// activity change: the turn crashed or the UI never released it.
		return idle >= s.d.Limits.StuckAfter
	default:
		return idle >= s.d.Limits.AbandonAfter
	}
}

// ------------------------------------------------------------------ session.Manager bridge

// forceReady returns the session to READY after a turn crashed, whatever
// activity it was left in (without counting as citizen interaction).
func (s *Server) forceReady(id string) {
	_, _ = s.d.Sessions.ForceReady(id)
}

// startFresh ends any open (abandoned) session with EndAbandoned — its
// cleanup hook records session.ended — and starts a new one.
func (s *Server) startFresh(lang string) (session.Snapshot, error) {
	return s.d.Sessions.StartFresh(lang)
}

// wavDuration estimates a PCM WAV's length from its canonical header (the
// speech worker writes a 44-byte header); 0 when unknown. It only sizes the
// SPEAKING cap, which is never below session.Timeouts.MaxSpeaking.
func wavDuration(b []byte) time.Duration {
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0
	}
	ch := int(binary.LittleEndian.Uint16(b[22:24]))
	rate := int(binary.LittleEndian.Uint32(b[24:28]))
	bits := int(binary.LittleEndian.Uint16(b[34:36]))
	bps := rate * ch * bits / 8
	if bps <= 0 {
		return 0
	}
	return time.Duration(float64(len(b)-44) / float64(bps) * float64(time.Second))
}

// ------------------------------------------------------------------ analytics time

// minuteTS formats t truncated to the minute: session timestamps in the
// sessions table and in analytics events carry no finer resolution, so a
// single visit is harder to single out (decision 9, DPDP).
func minuteTS(t time.Time) string { return t.UTC().Truncate(time.Minute).Format(time.RFC3339) }

// rowTS is minuteTS in the outbox's fixed-width layout, for the local
// sessions table (so retention pruning compares timestamps correctly).
func rowTS(t time.Time) string { return t.UTC().Truncate(time.Minute).Format(outbox.TimeLayout) }
