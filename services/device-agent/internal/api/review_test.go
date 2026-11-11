package api

// Independent-review tests (correctness & spec conformance). They encode the
// documented behaviour; a failure here is a spec mismatch, not a flaky test.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/session"
)

// Spec (session package doc, SYSTEM_ARCHITECTURE §2): reminder at 2 min,
// final warning at 9 min, close at 10 min of inactivity. The kiosk UI keeps
// the agent in SPEAKING while it shows the language and privacy screens
// (greeting audio is only played after the privacy notice), so a citizen who
// presses the button and walks away leaves the session in SPEAKING.
func TestReviewAbandonedSessionInSpeakingTimesOut(t *testing.T) {
	reviewGate(t)
	h := newHarness(t, hopts{})
	ch, cancel, err := h.sm.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	res := h.start()
	if res.Session.Activity != session.Speaking {
		t.Fatalf("precondition: start with TTS should be SPEAKING, got %s", res.Session.Activity)
	}
	var sawReminder bool
	for i := 0; i < 30; i++ { // 30 minutes, one tick per minute
		h.advance(time.Minute)
		h.sm.Tick()
	drain:
		for {
			select {
			case ev := <-ch:
				if ev.Type == session.EvReminder {
					sawReminder = true
				}
			default:
				break drain
			}
		}
	}
	if !sawReminder {
		t.Errorf("no session.reminder in 30 idle minutes while SPEAKING")
	}
	if cur := h.sm.Current(); cur.ID != "" {
		t.Errorf("session %s still open after 30 idle minutes (activity %s); spec closes at 10 min", cur.ID, cur.Activity)
		// The next citizen's button press then joins the previous citizen's
		// session (and its in-RAM dialogue state) instead of starting a new one.
		h.srv.ButtonPressed(context.Background())
		if h.sm.Current().ID == res.Session.ID {
			t.Errorf("next button press re-used the abandoned session instead of starting a new one")
		}
	}
}

// signalReader signals on its first Read, then blocks until released.
type signalReader struct {
	started chan struct{}
	release chan struct{}
	data    *bytes.Reader
	once    bool
}

func (r *signalReader) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		close(r.started)
		<-r.release
	}
	return r.data.Read(p)
}

// Spec (API_DOCUMENTATION §1): POST /audio is "Rejected unless the session is
// LISTENING". The handler checks LISTENING before reading the body, then
// toProcessing accepts READY/SPEAKING too, so audio whose upload overlapped
// another turn is transcribed anyway.
func TestReviewAudioRejectedIfSessionLeftListeningDuringUpload(t *testing.T) {
	reviewGate(t)
	h := newHarness(t, hopts{})
	res := h.start()
	sid := res.Session.ID
	if w := h.post("/api/v1/session/"+sid+"/speaking-done", nil); w.Code != 200 {
		t.Fatalf("speaking-done: %d", w.Code)
	}
	if h.sm.Current().Activity != session.Listening {
		t.Fatalf("precondition: want LISTENING, got %s", h.sm.Current().Activity)
	}
	h.stt.set("मुझे जाति प्रमाण पत्र बनवाना है", nil)

	body := &signalReader{started: make(chan struct{}), release: make(chan struct{}), data: bytes.NewReader(makeWAV(16000, 1))}
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8730/api/v1/session/"+sid+"/audio", io.NopCloser(body))
	r.Header.Set("Content-Type", "audio/wav")
	r.Header.Set("X-JS-Kiosk", "1")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.h.ServeHTTP(w, r); close(done) }()
	<-body.started // RequireListening already passed; body upload in progress

	// Citizen taps an option meanwhile: the session leaves LISTENING.
	if ow := h.post("/api/v1/session/"+sid+"/option", map[string]string{"option_id": "cmd:repeat"}); ow.Code != 200 {
		t.Fatalf("option: %d %s", ow.Code, ow.Body)
	}
	if a := h.sm.Current().Activity; a == session.Listening {
		t.Fatalf("precondition: option turn should leave LISTENING, got %s", a)
	}
	close(body.release)
	<-done
	if w.Code == http.StatusOK {
		t.Errorf("audio accepted (200) although the session had left LISTENING before the upload completed; stt calls=%d", h.stt.n)
	}
}

// reviewGate keeps these spec-mismatch reproductions out of the default
// suite until the defects are fixed. Run with JS_REVIEW=1.
func reviewGate(t *testing.T) {
	t.Helper()
	if os.Getenv("JS_REVIEW") != "1" {
		t.Skip("review reproduction; set JS_REVIEW=1")
	}
}
