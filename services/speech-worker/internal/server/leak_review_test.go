package server

// Reliability review (goroutine leaks): an inference that outlives its
// request (timeout) keeps exactly one goroutine until the native call
// returns; afterwards Drain succeeds and nothing is left running.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// gatedSTT blocks each Transcribe until release is closed.
type gatedSTT struct{ release chan struct{} }

func (g *gatedSTT) Transcribe([]float32) STTResult {
	<-g.release
	return STTResult{Text: "ok", Confidence: 0.9}
}

func TestReliabilityTimedOutInferenceDrainsNoLeak(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	g := &gatedSTT{release: make(chan struct{})}
	s := New(Config{STTTimeout: 100 * time.Millisecond, BusyWait: 50 * time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetEngines(g, &fakeTTS{rate: 44100}, map[string]any{})
	ts := httptest.NewServer(s.Handler())

	resp, b := post(t, ts.URL+"/v1/transcribe", "audio/wav", wav(1))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("want 504, got %d %s", resp.StatusCode, b)
	}
	dctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if err := s.Drain(dctx); err == nil {
		t.Fatal("Drain must report the still-running native call")
	}
	cancel()
	close(g.release)
	dctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Drain(dctx); err != nil {
		t.Fatalf("Drain after the call returned: %v", err)
	}
	// Slot is free again.
	resp, b = post(t, ts.URL+"/v1/transcribe", "audio/wav", wav(1))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 after release, got %d %s", resp.StatusCode, b)
	}
	ts.Close()
	http.DefaultClient.CloseIdleConnections()
	goleak.VerifyNone(t, ignore)
}
