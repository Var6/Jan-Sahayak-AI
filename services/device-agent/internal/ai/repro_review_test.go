//go:build reliabilityrepro

package ai

// Reliability review repro (run with -tags reliabilityrepro). Expected to
// FAIL until fixed: Llama.do wraps transport errors with %v, so a request
// cancelled by the caller (citizen pressed End, kiosk page reloaded) is not
// recognised as context.Canceled by the gate and counts as a runtime fault;
// three such cancellations open the breaker for 30 s.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReproCancelledLLMCallsOpenBreaker(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	l := NewLlama(LlamaConfig{BaseURL: srv.URL, Timeout: time.Minute})
	req := CompletionRequest{Messages: []Message{{Role: "user", Content: "x"}}}
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		_, err := l.Complete(ctx, req)
		if !errors.Is(err, context.Canceled) {
			t.Logf("call %d: cancelled request returned %v (errors.Is Canceled = false)", i, err)
		}
	}
	if l.g.open() {
		t.Fatal("breaker opened after 3 caller cancellations; the healthy LLM is now skipped for 30 s")
	}
}
