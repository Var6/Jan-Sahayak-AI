//go:build reliabilityrepro

package api

// Reliability review repro (run with -tags reliabilityrepro). Expected to
// FAIL until fixed: a panic anywhere in a turn after toProcessing (engine,
// TTS client, analytics) is recovered by the HTTP middleware, but the
// session is left in PROCESSING. Tick never times out a PROCESSING session,
// the physical button only "touches" an open session, and further turns get
// 409, so the kiosk is wedged until someone presses the on-screen End.

import (
	"context"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/dialogue"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/session"
)

type panicEngine struct{ dialogue.Engine }

func (p panicEngine) Handle(ctx context.Context, st *dialogue.State, in dialogue.Input) (dialogue.Reply, error) {
	if in.Source == dialogue.FromText {
		panic("bug in a dialogue flow")
	}
	return p.Engine.Handle(ctx, st, in)
}

func TestReproPanicInTurnWedgesSessionForever(t *testing.T) {
	h := newHarness(t, hopts{})
	real := h.srv.d.Engine
	srv, err := New(Deps{
		Sessions: session.NewManager(session.DefaultTimeouts, h.now), Engine: panicEngine{real}, STT: h.stt, Health: h.mon, DB: h.st.DB,
		Outbox: outbox.New(h.st.DB, outbox.DefaultConfig, nil), SimButton: true, DeviceID: "dev-1", Version: "test",
		Listen: "127.0.0.1:8730", Limits: Limits{StartTimeout: 3 * time.Second}, Now: h.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv, h.h, h.sm = srv, srv.Handler(), srv.d.Sessions
	res := h.start()
	id := res.Session.ID
	if w := h.post("/api/v1/session/"+id+"/text", map[string]string{"text": "मुझे पेंशन चाहिए"}); w.Code != 500 {
		t.Fatalf("expected recovered panic → 500, got %d", w.Code)
	}
	for i := 0; i < 60; i++ { // one idle hour
		h.advance(time.Minute)
		h.sm.Tick()
	}
	cur := h.sm.Current()
	h.srv.ButtonPressed(context.Background())
	if cur.ID == id {
		t.Fatalf("session still open after 60 idle minutes in %s; button press did not start a new one (same session: %v)", cur.Activity, h.sm.Current().ID == id)
	}
}
