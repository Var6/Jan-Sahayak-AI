package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTest() (*Manager, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)}
	return NewManager(DefaultTimeouts, c.Now), c
}

func drain(ch <-chan Event) []EventType {
	var out []EventType
	for {
		select {
		case ev := <-ch:
			out = append(out, ev.Type)
		default:
			return out
		}
	}
}

func TestInactivityTimeline(t *testing.T) {
	m, clk := newTest()
	ch, cancel, _ := m.Subscribe()
	defer cancel()

	var cleaned []string
	m.OnEnd(func(id string, r EndReason) { cleaned = append(cleaned, id+":"+string(r)) })

	s, err := m.Start("hi")
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(119 * time.Second)
	m.Tick()
	clk.Advance(2 * time.Second) // 2:01
	m.Tick()
	m.Tick()                     // reminder must fire once only
	clk.Advance(7 * time.Minute) // 9:01
	m.Tick()
	clk.Advance(59 * time.Second) // 10:00
	m.Tick()

	got := drain(ch)
	want := []EventType{EvStarted, EvReminder, EvFinalWarning, EvEnded}
	if len(got) != len(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events %v, want %v", got, want)
		}
	}
	if m.Current().Activity != Idle {
		t.Fatal("session should be idle after timeout")
	}
	if len(cleaned) != 1 || cleaned[0] != s.ID+":"+string(EndTimeout) {
		t.Fatalf("cleanup not run correctly: %v", cleaned)
	}
}

func TestInteractionResetsClock(t *testing.T) {
	m, clk := newTest()
	s, _ := m.Start("hi")
	clk.Advance(9*time.Minute + 30*time.Second)
	m.Tick()
	if err := m.Touch(s.ID); err != nil {
		t.Fatal(err)
	}
	clk.Advance(9 * time.Minute)
	m.Tick()
	if m.Current().Activity == Idle {
		t.Fatal("session should still be open after touch reset")
	}
}

// Busy states are exempt from the idle rules only up to their cap. (This
// test previously asserted that PROCESSING never times out, even after 30
// minutes; that wedged the kiosk after a panicking turn — reliability
// finding 5.)
func TestNoTimeoutWhileDeviceBusy(t *testing.T) {
	m, clk := newTest()
	s, _ := m.Start("hi")
	_, _ = m.SetActivity(s.ID, Processing)
	clk.Advance(DefaultMaxProcessing - time.Second)
	m.Tick()
	if m.Current().Activity != Processing {
		t.Fatal("must not time out while PROCESSING within its cap")
	}
}

func TestStuckProcessingBecomesReadyThenTimesOut(t *testing.T) {
	m, clk := newTest()
	ch, cancel, _ := m.Subscribe()
	defer cancel()
	s, _ := m.Start("hi")
	_, _ = m.SetActivity(s.ID, Processing)
	drain(ch)
	clk.Advance(DefaultMaxProcessing)
	m.Tick()
	if cur := m.Current(); cur.Activity != Ready || cur.ID != s.ID {
		t.Fatalf("stuck PROCESSING should drop to READY, got %+v", cur)
	}
	if evs := drain(ch); len(evs) == 0 || evs[0] != EvActivity {
		t.Fatalf("expected session.activity, got %v", evs)
	}
	var sawReminder, ended bool
	for i := 0; i < 12; i++ {
		clk.Advance(time.Minute)
		m.Tick()
		for _, e := range drain(ch) {
			sawReminder = sawReminder || e == EvReminder
			ended = ended || e == EvEnded
		}
	}
	if !sawReminder || !ended || m.Current().Activity != Idle {
		t.Fatalf("idle rules must apply after the cap: reminder=%v ended=%v cur=%+v", sawReminder, ended, m.Current())
	}
}

func TestSpeakingCapUsesClipHint(t *testing.T) {
	m, clk := newTest()
	s, _ := m.Start("hi")
	// Without a hint: capped at MaxSpeaking.
	_, _ = m.SetActivity(s.ID, Speaking)
	clk.Advance(DefaultMaxSpeaking - time.Second)
	m.Tick()
	if m.Current().Activity != Speaking {
		t.Fatal("SPEAKING must hold within MaxSpeaking")
	}
	clk.Advance(time.Second)
	m.Tick()
	if m.Current().Activity != Ready {
		t.Fatalf("SPEAKING past MaxSpeaking should be READY, got %s", m.Current().Activity)
	}
	// A long clip extends the cap to the clip length.
	_, _ = m.SetSpeaking(s.ID, 3*time.Minute)
	clk.Advance(150 * time.Second)
	m.Tick()
	if m.Current().Activity != Speaking {
		t.Fatal("SPEAKING must hold for the hinted clip length")
	}
	clk.Advance(31 * time.Second)
	m.Tick()
	if m.Current().Activity != Ready {
		t.Fatalf("SPEAKING past the clip hint should be READY, got %s", m.Current().Activity)
	}
	// A short clip hint never lowers the cap below MaxSpeaking.
	_, _ = m.SetSpeaking(s.ID, 5*time.Second)
	clk.Advance(time.Minute)
	m.Tick()
	if m.Current().Activity != Speaking {
		t.Fatal("short clip hint must not lower the cap below MaxSpeaking")
	}
}

func TestForceReady(t *testing.T) {
	m, clk := newTest()
	s, _ := m.Start("hi")
	_, _ = m.SetActivity(s.ID, Processing)
	before := m.Current().LastActiveAt
	clk.Advance(5 * time.Second)
	if _, err := m.ForceReady("other"); !errors.Is(err, ErrWrongSession) {
		t.Fatalf("want ErrWrongSession, got %v", err)
	}
	cur, err := m.ForceReady(s.ID)
	if err != nil || cur.Activity != Ready {
		t.Fatalf("ForceReady: %+v %v", cur, err)
	}
	if !cur.LastActiveAt.Equal(before) {
		t.Fatal("ForceReady must not reset the idle clock")
	}
}

func TestStartFreshEndsAbandonedSession(t *testing.T) {
	m, _ := newTest()
	var ended []EndReason
	m.OnEnd(func(_ string, r EndReason) { ended = append(ended, r) })
	old, _ := m.Start("hi")
	_, _ = m.SetActivity(old.ID, Speaking)
	cur, err := m.StartFresh("hi")
	if err != nil || cur.ID == "" || cur.ID == old.ID || cur.Activity != Ready {
		t.Fatalf("StartFresh: %+v %v", cur, err)
	}
	if len(ended) != 1 || ended[0] != EndAbandoned {
		t.Fatalf("old session must end with EndAbandoned, got %v", ended)
	}
	if EndAbandoned.Wire() != "inactivity_timeout" || EndCitizen.Wire() != "citizen_ended" {
		t.Fatal("Wire mapping")
	}
	// With no open session it just starts one.
	m2, _ := newTest()
	if s, err := m2.StartFresh("hi"); err != nil || s.ID == "" {
		t.Fatalf("StartFresh on idle: %+v %v", s, err)
	}
}

func TestMicrophoneGate(t *testing.T) {
	m, _ := newTest()
	if err := m.RequireListening("x"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
	s, _ := m.Start("hi")
	if err := m.RequireListening(s.ID); err == nil {
		t.Fatal("mic must be refused while READY")
	}
	if _, err := m.SetActivity(s.ID, Listening); err != nil {
		t.Fatal(err)
	}
	if err := m.RequireListening(s.ID); err != nil {
		t.Fatalf("mic should be allowed: %v", err)
	}
	if err := m.RequireListening("other"); !errors.Is(err, ErrWrongSession) {
		t.Fatalf("want ErrWrongSession, got %v", err)
	}
}

func TestCameraGate(t *testing.T) {
	m, _ := newTest()
	s, _ := m.Start("hi")
	if err := m.RequireCameraConsent(s.ID); !errors.Is(err, ErrNoCameraConsent) {
		t.Fatal("camera must require consent")
	}
	_ = m.SetCameraConsent(s.ID, true)
	if err := m.RequireCameraConsent(s.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = m.End(s.ID, EndCitizen)
	s2, _ := m.Start("hi")
	if err := m.RequireCameraConsent(s2.ID); err == nil {
		t.Fatal("consent must not carry over to a new session")
	}
}

func TestSingleSessionAndBadTransitions(t *testing.T) {
	m, _ := newTest()
	s, _ := m.Start("hi")
	if _, err := m.Start("hi"); !errors.Is(err, ErrAlreadyActive) {
		t.Fatal("second start must fail")
	}
	if _, err := m.SetActivity(s.ID, Listening); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetActivity(s.ID, Speaking); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("LISTENING->SPEAKING must be rejected, got %v", err)
	}
}

func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	m, _ := newTest()
	_, cancel, _ := m.Subscribe()
	defer cancel()
	s, _ := m.Start("hi")
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			_ = m.Touch(s.ID)
			_ = m.SetLanguage(s.ID, "hi")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publishing blocked on a slow subscriber")
	}
}

func TestSubscriberLimit(t *testing.T) {
	m, _ := newTest()
	var cancels []func()
	for i := 0; i < maxSubscribers; i++ {
		_, c, err := m.Subscribe()
		if err != nil {
			t.Fatal(err)
		}
		cancels = append(cancels, c)
	}
	if _, _, err := m.Subscribe(); !errors.Is(err, ErrTooManySubscribers) {
		t.Fatal("expected subscriber limit")
	}
	for _, c := range cancels {
		c()
		c() // idempotent
	}
}

func TestRunEndsSessionOnShutdown(t *testing.T) {
	m := NewManager(DefaultTimeouts, nil)
	ended := make(chan EndReason, 1)
	m.OnEnd(func(_ string, r EndReason) { ended <- r })
	_, _ = m.Start("hi")
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	cancel()
	select {
	case r := <-ended:
		if r != EndShutdown {
			t.Fatalf("want shutdown, got %s", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session not ended on shutdown")
	}
}

func TestIDsAreUUIDv4(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newID()
		if len(id) != 36 || id[14] != '4' {
			t.Fatalf("bad uuid %q", id)
		}
		if seen[id] {
			t.Fatal("duplicate id")
		}
		seen[id] = true
	}
}
