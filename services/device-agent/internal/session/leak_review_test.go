package session

// Reliability review (goroutine leaks): Run and subscriptions must leave no
// goroutines behind once the context is cancelled.

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestReliabilityRunAndSubscribersDoNotLeak(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := NewManager(Timeouts{Reminder: 10 * time.Millisecond, FinalWarning: 20 * time.Millisecond, Close: 30 * time.Millisecond}, nil)
	var cancels []func()
	for i := 0; i < maxSubscribers; i++ {
		_, c, err := m.Subscribe()
		if err != nil {
			t.Fatal(err)
		}
		cancels = append(cancels, c)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	if _, err := m.Start("hi"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	for _, c := range cancels {
		c()
		c() // idempotent
	}
	if _, c, err := m.Subscribe(); err != nil {
		t.Fatalf("subscriber slots not released: %v", err)
	} else {
		c()
	}
}
