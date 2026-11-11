package health

// Reliability review (goroutine leaks): Run with a slow, ctx-respecting
// check must stop promptly and leave nothing running.

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestReliabilityRunWithSlowCheckDoesNotLeak(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := NewMonitor()
	m.Register(Component{Name: "slow", Interval: time.Millisecond, Timeout: time.Hour, Check: func(ctx context.Context) Result {
		<-ctx.Done()
		return Result{State: Unavailable}
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
