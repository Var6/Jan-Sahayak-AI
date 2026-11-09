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

func TestNoTimeoutWhileDeviceBusy(t *testing.T) {
	m, clk := newTest()
	s, _ := m.Start("hi")
	_, _ = m.SetActivity(s.ID, Processing)
	clk.Advance(30 * time.Minute)
	m.Tick()
	if m.Current().Activity != Processing {
		t.Fatal("must not time out while PROCESSING")
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
