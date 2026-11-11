package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/services/speech-worker/internal/audio"
)

func TestWedgedEngineReportedAndWatched(t *testing.T) {
	g := &gatedSTT{release: make(chan struct{})}
	srv, ts := newTest(t, Config{STTTimeout: 40 * time.Millisecond, BusyWait: 10 * time.Millisecond}, nil, nil)
	srv.SetEngines(g, &fakeTTS{rate: 44100}, map[string]any{})
	if resp, _ := post(t, ts.URL+"/v1/transcribe", "audio/wav", wav(1)); resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("want 504, got %d", resp.StatusCode)
	}
	if _, _, ok := srv.Wedged(); ok {
		t.Fatal("wedged right after one timeout (limit is 3× timeout)")
	}
	fired := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go srv.WatchWedged(ctx, 10*time.Millisecond, func(engine string, held time.Duration) { fired <- engine })
	select {
	case eng := <-fired:
		if eng != "stt" {
			t.Fatalf("engine %q", eng)
		}
	case <-ctx.Done():
		t.Fatal("WatchWedged never fired")
	}
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var h map[string]any
	_ = json.Unmarshal(b, &h)
	if resp.StatusCode != http.StatusServiceUnavailable || h["status"] != "wedged" {
		t.Fatalf("health %d %s", resp.StatusCode, b)
	}
	close(g.release)
	dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dcancel()
	if err := srv.Drain(dctx); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := srv.Wedged(); ok {
		t.Fatal("still wedged after the call returned")
	}
}

func TestTTSMaxRateDownsamples(t *testing.T) {
	_, ts := newTest(t, Config{TTSMaxRate: 22050}, &fakeSTT{}, &fakeTTS{rate: 44100})
	resp, b := post(t, ts.URL+"/v1/synthesize", "application/json", []byte(`{"text":"नमस्ते","lang":"hi"}`))
	if resp.StatusCode != 200 || resp.Header.Get("X-Sample-Rate") != "22050" {
		t.Fatalf("%d rate=%s", resp.StatusCode, resp.Header.Get("X-Sample-Rate"))
	}
	pcm, info, err := audio.DecodeMono16(b, 22050, 0)
	if err != nil || info.SampleRate != 22050 || len(pcm) != 11025 {
		t.Fatalf("pcm=%d info=%+v err=%v", len(pcm), info, err)
	}
}
