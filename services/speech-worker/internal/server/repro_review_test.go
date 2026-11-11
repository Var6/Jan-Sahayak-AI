//go:build reliabilityrepro

package server

// Reliability review repro (run with -tags reliabilityrepro). Expected to
// FAIL until fixed: a native inference call that never returns (onnxruntime
// deadlock, pathological input) holds the engine slot forever. Every later
// request gets 503 "busy" — which the device agent treats as transient
// (ErrBusy: no breaker count, no health report) — while GET /health keeps
// answering 200 "ok". The unit has no WatchdogSec, so nothing ever restarts
// the worker: voice input is advertised but every turn fails, indefinitely.

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestReproHungEngineStaysHealthyForever(t *testing.T) {
	g := &gatedSTT{release: make(chan struct{})}
	defer close(g.release)
	srv, ts2 := newTest(t, Config{STTTimeout: 100 * time.Millisecond, BusyWait: 50 * time.Millisecond}, nil, nil)
	srv.SetEngines(g, &fakeTTS{rate: 44100}, map[string]any{})

	if resp, _ := post(t, ts2.URL+"/v1/transcribe", "audio/wav", wav(1)); resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("first call should time out, got %d", resp.StatusCode)
	}
	busy := 0
	for i := 0; i < 5; i++ {
		resp, b := post(t, ts2.URL+"/v1/transcribe", "audio/wav", wav(1))
		if resp.StatusCode == http.StatusServiceUnavailable && errCode(t, b) == "busy" {
			busy++
		}
	}
	resp, err := http.Get(ts2.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var h map[string]any
	_ = json.Unmarshal(body, &h)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("engine wedged (%d/5 later requests got 503 busy) but /health = 200 %v; agent sees HEALTHY and never restarts the worker", busy, h)
	}
}
