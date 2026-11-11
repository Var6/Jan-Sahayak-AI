package ai

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Reliability finding 10: caller cancellations never trip the breaker and
// never hide a run of real failures either.
func TestGateIgnoresCancellations(t *testing.T) {
	g := newGate()
	ctx := context.Background()
	fail := func(err error) {
		release, aerr := g.acquire(ctx)
		if aerr != nil {
			t.Fatal(aerr)
		}
		release(err)
	}
	cancelled := fmt.Errorf("%w: %w", ErrUnavailable, context.Canceled)
	for i := 0; i < 5; i++ {
		fail(cancelled)
	}
	if g.open() {
		t.Fatal("breaker opened on cancellations")
	}
	boom := errors.New("connection refused")
	fail(boom)
	fail(cancelled)
	fail(boom)
	fail(cancelled)
	fail(boom)
	if !g.open() {
		t.Fatal("interleaved cancellations must not reset the failure count")
	}
}
