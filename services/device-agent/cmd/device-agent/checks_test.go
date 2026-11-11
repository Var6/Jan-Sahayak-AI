package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/config"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
)

func TestSpeechWorkerHealthCodes(t *testing.T) {
	cases := []struct {
		status int
		body   string
		state  health.State
		code   string
	}{
		{200, `{"status":"ok"}`, health.Healthy, ""},
		{503, `{"status":"loading"}`, health.Recovering, "speech_loading"},
		{503, `{"status":"wedged","engine":"stt"}`, health.Unavailable, "speech_engine_hung"},
		{500, `oops`, health.Unavailable, "speech_http_500"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		st, code := speechWorkerHealth(context.Background(), srv.Client(), srv.URL+"/health")
		srv.Close()
		if st != c.state || code != c.code {
			t.Errorf("%d %s → %s %q, want %s %q", c.status, c.body, st, code, c.state, c.code)
		}
	}
	if st, code := speechWorkerHealth(context.Background(), http.DefaultClient, "http://127.0.0.1:1/health"); st != health.Unavailable || code != "speech_unreachable" {
		t.Errorf("unreachable → %s %q", st, code)
	}
}

func TestStaleLoopDetection(t *testing.T) {
	a := &agent{cfg: config.Defaults()}
	a.startedAt = time.Now()
	if l := a.staleLoop(); l != "" {
		t.Fatalf("inside the grace period: %q", l)
	}
	a.startedAt = time.Now().Add(-time.Hour)
	if l := a.staleLoop(); l != "health" {
		t.Fatalf("a database check that never completed must count as stalled, got %q", l)
	}
	stamp(&a.hb.health)
	if l := a.staleLoop(); l != "session" {
		t.Fatalf("missing session heartbeat: got %q", l)
	}
	stamp(&a.hb.session)
	if l := a.staleLoop(); l != "" { // sync not configured in defaults
		t.Fatalf("all alive: got %q", l)
	}
	a.hb.session.Store(time.Now().Add(-time.Minute).UnixNano())
	if l := a.staleLoop(); l != "session" {
		t.Fatalf("old session heartbeat: got %q", l)
	}
}
