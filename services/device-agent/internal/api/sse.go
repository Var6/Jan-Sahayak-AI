package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/dialogue"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/session"
)

// sessionEventJSON is the SSE `session` payload. Reply/AudioURL are set only
// on session.started for sessions started by the physical button, whose
// greeting no HTTP response carries.
type sessionEventJSON struct {
	Type      session.EventType `json:"type"`
	Session   session.Snapshot  `json:"session"`
	EndReason session.EndReason `json:"end_reason,omitempty"`
	At        time.Time         `json:"at"`
	Reply     *dialogue.Reply   `json:"reply,omitempty"`
	AudioURL  string            `json:"audio_url,omitempty"`
}

type healthEventJSON struct {
	Overall      health.State `json:"overall"`
	Capabilities Capabilities `json:"capabilities"`
}

// sseClient is one open event stream.
type sseClient struct {
	health  chan struct{} // coalesced "send a health event"
	evicted chan struct{} // closed when a newer kiosk stream takes the slot
	kiosk   bool
	since   time.Time
}

// HealthChanged tells every SSE client to send a fresh `health` event.
// Non-blocking; notifications coalesce.
func (s *Server) HealthChanged() {
	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	for c := range s.sseClients {
		select {
		case c.health <- struct{}{}:
		default:
		}
	}
}

// fromKiosk reports whether an event-stream request comes from the kiosk
// page itself: a same-origin browser fetch (Sec-Fetch-Site, set by the
// browser and not settable by page scripts). Local non-browser processes
// can forge it, but then they only compete with the kiosk's reconnects
// instead of locking it out.
func fromKiosk(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// sseAcquire admits a stream. One slot is reserved for the kiosk page:
// other clients may use at most SSEMaxClients-1. When all slots are taken,
// a kiosk stream evicts the oldest stream (preferring old kiosk streams a
// reloaded page left behind), so nothing local can starve the kiosk UI of
// its event stream.
func (s *Server) sseAcquire(kiosk bool) (*sseClient, bool) {
	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	max := s.d.Limits.SSEMaxClients
	if !kiosk {
		others := 0
		for c := range s.sseClients {
			if !c.kiosk {
				others++
			}
		}
		if len(s.sseClients) >= max || others >= max-1 {
			return nil, false
		}
	} else if len(s.sseClients) >= max {
		var victim *sseClient
		for c := range s.sseClients {
			switch {
			case victim == nil,
				c.kiosk && !victim.kiosk,
				c.kiosk == victim.kiosk && c.since.Before(victim.since):
				victim = c
			}
		}
		delete(s.sseClients, victim)
		close(victim.evicted)
	}
	c := &sseClient{health: make(chan struct{}, 1), evicted: make(chan struct{}), kiosk: kiosk, since: time.Now()}
	s.sseClients[c] = struct{}{}
	return c, true
}

func (s *Server) sseRelease(c *sseClient) {
	s.sseMu.Lock()
	defer s.sseMu.Unlock()
	delete(s.sseClients, c)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	client, ok := s.sseAcquire(fromKiosk(r))
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "too many event subscribers")
		return
	}
	defer s.sseRelease(client)
	hk := client.health
	evs, unsubscribe, err := s.d.Sessions.Subscribe()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "too many event subscribers")
		return
	}
	defer unsubscribe()

	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(s.d.Limits.SSEMaxLifetime + time.Minute))
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	write := func(event string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprint(w, ": connected\nretry: 3000\n\n"); err != nil || rc.Flush() != nil {
		return
	}
	if !write("health", healthEventJSON{Overall: s.d.Health.Snapshot().Overall, Capabilities: s.Capabilities()}) {
		return
	}

	ping := time.NewTicker(s.d.Limits.SSEPing)
	defer ping.Stop()
	life := time.NewTimer(s.d.Limits.SSEMaxLifetime)
	defer life.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		case <-client.evicted:
			return // a newer kiosk stream took this slot
		case <-life.C:
			return // the client reconnects and re-reads GET /status
		case ev, ok := <-evs:
			if !ok {
				return
			}
			out := sessionEventJSON{Type: ev.Type, Session: ev.Session, EndReason: ev.EndReason, At: ev.At}
			if ev.Type == session.EvStarted {
				out.Reply, out.AudioURL = s.waitGreeting(r.Context(), ev.Session.ID)
			}
			if !write("session", out) {
				return
			}
		case <-hk:
			if !write("health", healthEventJSON{Overall: s.d.Health.Snapshot().Overall, Capabilities: s.Capabilities()}) {
				return
			}
		case t := <-ping.C:
			if !write("ping", map[string]string{"at": t.UTC().Format(time.RFC3339)}) {
				return
			}
		}
	}
}
