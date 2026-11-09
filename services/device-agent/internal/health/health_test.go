package health

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestOverallStateRules(t *testing.T) {
	m := NewMonitor()
	m.Register(Component{Name: "db", Critical: true, Check: func(context.Context) Result { return Result{State: Healthy} }})
	m.Register(Component{Name: "llm", Check: func(context.Context) Result { return Result{State: Unavailable, Detail: "down"} }})
	m.CheckAll(context.Background())

	snap := m.Snapshot()
	if snap.Overall != Degraded {
		t.Fatalf("optional failure should degrade, got %s", snap.Overall)
	}
	if m.IsUsable("llm") {
		t.Fatal("llm should not be usable")
	}
	if !m.IsUsable("db") {
		t.Fatal("db should be usable")
	}

	m.Report("db", Result{State: Unavailable})
	if got := m.Snapshot().Overall; got != Unavailable {
		t.Fatalf("critical failure should make device UNAVAILABLE, got %s", got)
	}
}

func TestEscalatesToRequiresAttention(t *testing.T) {
	m := NewMonitor()
	m.Register(Component{Name: "db", Critical: true, Check: func(context.Context) Result { return Result{State: Unavailable} }})
	for i := 0; i < attentionAfter; i++ {
		m.CheckAll(context.Background())
	}
	s, _ := m.Get("db")
	if s.State != RequiresAttention {
		t.Fatalf("want REQUIRES_ATTENTION after %d failures, got %s", attentionAfter, s.State)
	}
	m.Report("db", Result{State: Healthy})
	s, _ = m.Get("db")
	if s.State != Healthy || s.ConsecutiveFailures != 0 {
		t.Fatalf("recovery should reset, got %+v", s)
	}
}

func TestPanickingAndHangingChecksAreContained(t *testing.T) {
	m := NewMonitor()
	m.Register(Component{Name: "panics", Check: func(context.Context) Result { panic("boom") }})
	m.Register(Component{Name: "hangs", Timeout: 20 * time.Millisecond, Check: func(ctx context.Context) Result {
		<-ctx.Done()
		return Result{}
	}})
	m.CheckAll(context.Background())
	for _, n := range []string{"panics", "hangs"} {
		s, _ := m.Get(n)
		if s.State != Unavailable {
			t.Fatalf("%s: want UNAVAILABLE, got %s (%s)", n, s.State, s.Detail)
		}
	}
}

func TestChangeListenerFiresOnlyOnTransitions(t *testing.T) {
	m := NewMonitor()
	var calls atomic.Int32
	m.OnChange(func(prev, cur Status) { calls.Add(1) })
	m.Register(Component{Name: "x", Check: func(context.Context) Result { return Result{State: Healthy} }})
	m.CheckAll(context.Background()) // RECOVERING -> HEALTHY
	m.CheckAll(context.Background()) // no change
	if calls.Load() != 1 {
		t.Fatalf("want 1 transition, got %d", calls.Load())
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	m := NewMonitor()
	m.Register(Component{Name: "x", Interval: time.Millisecond, Check: func(context.Context) Result { return Result{State: Healthy} }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
