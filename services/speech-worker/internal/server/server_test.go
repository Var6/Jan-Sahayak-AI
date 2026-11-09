package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/services/speech-worker/internal/audio"
)

// Test doubles for the native engines. They exist only in tests; the real
// binary always wires sherpa-onnx engines.

type fakeSTT struct {
	res   STTResult
	delay time.Duration
	panic bool
	calls int
	mu    sync.Mutex
}

func (f *fakeSTT) Transcribe(samples []float32) STTResult {
	f.mu.Lock()
	f.calls++
	d, p := f.delay, f.panic
	f.mu.Unlock()
	if p {
		panic("boom")
	}
	time.Sleep(d)
	return f.res
}

type fakeTTS struct {
	rate  int
	delay time.Duration
	mu    sync.Mutex
	got   struct {
		text, lang string
		sid        int
		speed      float32
	}
}

func (f *fakeTTS) Synthesize(ctx context.Context, text, lang string, sid int, speed float32, maxSamples int) ([]float32, int, error) {
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	f.mu.Lock()
	f.got.text, f.got.lang, f.got.sid, f.got.speed = text, lang, sid, speed
	f.mu.Unlock()
	return make([]float32, f.rate/2), f.rate, nil
}
func (f *fakeTTS) SampleRate() int  { return f.rate }
func (f *fakeTTS) NumSpeakers() int { return 10 }

func newTest(t *testing.T, cfg Config, stt *fakeSTT, tts *fakeTTS) (*Server, *httptest.Server) {
	t.Helper()
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if stt != nil || tts != nil {
		s.SetEngines(stt, tts, map[string]any{"stt": "fake"})
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func wav(secs float64) []byte {
	pcm := make([]float32, int(secs*16000))
	for i := range pcm {
		pcm[i] = 0.1
	}
	return audio.EncodeMono16(pcm, 16000)
}

func post(t *testing.T, url, ct string, body []byte) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(url, ct, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func errCode(t *testing.T, b []byte) string {
	var e struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("not an error body: %s", b)
	}
	return e.Error.Code
}

func TestHealthLoadingThenReady(t *testing.T) {
	s, ts := newTest(t, Config{}, nil, nil)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("loading: %d", resp.StatusCode)
	}
	r2, b := post(t, ts.URL+"/v1/transcribe", "audio/wav", wav(1))
	if r2.StatusCode != 503 || errCode(t, b) != "loading" {
		t.Fatalf("transcribe while loading: %d %s", r2.StatusCode, b)
	}
	s.SetEngines(&fakeSTT{}, &fakeTTS{rate: 44100}, map[string]any{"stt": "x"})
	resp, err = http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&h)
	if resp.StatusCode != 200 || h["status"] != "ok" {
		t.Fatalf("ready: %d %v", resp.StatusCode, h)
	}
}

func TestTranscribeOK(t *testing.T) {
	stt := &fakeSTT{res: STTResult{Text: "मुझे लोन चाहिए", Confidence: 0.8, ConfidenceSource: "heuristic", SpeechSecs: 0.9}}
	_, ts := newTest(t, Config{}, stt, &fakeTTS{rate: 44100})
	resp, b := post(t, ts.URL+"/v1/transcribe?lang=hi", "audio/wav", wav(1.5))
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	var tr TranscribeResponse
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Text != "मुझे लोन चाहिए" || tr.NoSpeech || tr.AudioSecs != 1.5 || tr.Confidence != 0.8 || tr.SpeechSecs != 0.9 {
		t.Fatalf("%+v", tr)
	}
}

func TestTranscribeNoSpeech(t *testing.T) {
	stt := &fakeSTT{res: STTResult{NoSpeech: true, Confidence: 0.9}}
	_, ts := newTest(t, Config{}, stt, &fakeTTS{rate: 44100})
	_, b := post(t, ts.URL+"/v1/transcribe", "audio/wav", wav(1))
	var tr TranscribeResponse
	_ = json.Unmarshal(b, &tr)
	if !tr.NoSpeech || tr.Confidence != 0 {
		t.Fatalf("%+v", tr)
	}
	// very short clips never reach the engine
	_, b = post(t, ts.URL+"/v1/transcribe", "audio/wav", wav(0.1))
	_ = json.Unmarshal(b, &tr)
	stt.mu.Lock()
	calls := stt.calls
	stt.mu.Unlock()
	if !tr.NoSpeech || calls != 1 {
		t.Fatalf("%+v calls=%d", tr, calls)
	}
}

func TestTranscribeRejects(t *testing.T) {
	_, ts := newTest(t, Config{MaxAudioBytes: 1 << 20, MaxAudioSecs: 20}, &fakeSTT{}, &fakeTTS{rate: 44100})
	cases := []struct {
		name, url, ct string
		body          []byte
		status        int
		code          string
	}{
		{"not wav", "/v1/transcribe", "audio/wav", []byte("hello world, definitely not a wav file at all......"), 400, "bad_audio"},
		{"wrong rate", "/v1/transcribe", "audio/wav", audio.EncodeMono16(make([]float32, 8000), 8000), 400, "bad_audio"},
		{"too long", "/v1/transcribe", "audio/wav", wav(21), 413, "too_large"},
		{"too big", "/v1/transcribe", "audio/wav", make([]byte, 1<<20+1), 413, "too_large"},
		{"bad ct", "/v1/transcribe", "text/plain", wav(1), 415, "bad_audio"},
		{"lang", "/v1/transcribe?lang=en", "audio/wav", wav(1), 400, "unsupported_language"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, b := post(t, ts.URL+c.url, c.ct, c.body)
			if resp.StatusCode != c.status || errCode(t, b) != c.code {
				t.Fatalf("got %d %s", resp.StatusCode, b)
			}
		})
	}
	resp, err := http.Get(ts.URL + "/v1/transcribe")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
}

func TestBusyAndTimeout(t *testing.T) {
	stt := &fakeSTT{delay: 300 * time.Millisecond, res: STTResult{Text: "x"}}
	s, ts := newTest(t, Config{BusyWait: 50 * time.Millisecond, STTTimeout: 100 * time.Millisecond}, stt, &fakeTTS{rate: 44100})
	// first request times out (engine keeps running and holds the slot)
	resp, b := post(t, ts.URL+"/v1/transcribe", "audio/wav", wav(1))
	if resp.StatusCode != 504 || errCode(t, b) != "timeout" {
		t.Fatalf("want timeout, got %d %s", resp.StatusCode, b)
	}
	// second arrives while the slot is still held → busy
	resp, b = post(t, ts.URL+"/v1/transcribe", "audio/wav", wav(1))
	if resp.StatusCode != 503 || errCode(t, b) != "busy" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("want busy, got %d %s", resp.StatusCode, b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	// after draining the engine is free again
	stt.mu.Lock()
	stt.delay = 0
	stt.mu.Unlock()
	resp, _ = post(t, ts.URL+"/v1/transcribe", "audio/wav", wav(1))
	if resp.StatusCode != 200 {
		t.Fatalf("after drain: %d", resp.StatusCode)
	}
}

func TestPanicRecovered(t *testing.T) {
	_, ts := newTest(t, Config{}, &fakeSTT{panic: true}, &fakeTTS{rate: 44100})
	resp, b := post(t, ts.URL+"/v1/transcribe", "audio/wav", wav(1))
	if resp.StatusCode != 500 || errCode(t, b) != "internal" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	// the slot was released
	resp, _ = post(t, ts.URL+"/v1/transcribe", "audio/wav", wav(1))
	if resp.StatusCode != 500 {
		t.Fatalf("second: %d", resp.StatusCode)
	}
}

func TestSynthesize(t *testing.T) {
	tts := &fakeTTS{rate: 44100}
	_, ts := newTest(t, Config{}, &fakeSTT{}, tts)
	resp, b := post(t, ts.URL+"/v1/synthesize", "application/json", []byte(`{"text":"नमस्ते","lang":"hi","sid":3,"speed":1.25}`))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	pcm, info, err := audio.DecodeMono16(b, 44100, 0)
	if err != nil || len(pcm) != 22050 || info.SampleRate != 44100 {
		t.Fatalf("len=%d err=%v", len(pcm), err)
	}
	tts.mu.Lock()
	defer tts.mu.Unlock()
	if tts.got.text != "नमस्ते" || tts.got.sid != 3 || tts.got.speed != 1.25 || tts.got.lang != "hi" {
		t.Fatalf("%+v", tts.got)
	}
	if resp.Header.Get("X-Audio-Secs") != "0.5" {
		t.Fatalf("X-Audio-Secs=%q", resp.Header.Get("X-Audio-Secs"))
	}
}

func TestSynthesizeRejects(t *testing.T) {
	_, ts := newTest(t, Config{MaxTextRunes: 600}, &fakeSTT{}, &fakeTTS{rate: 44100})
	long := strings.Repeat("क", 601)
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"empty", `{"text":"  "}`, 400, "bad_request"},
		{"bad json", `{"text":`, 400, "bad_request"},
		{"unknown field", `{"text":"a","voice":"x"}`, 400, "bad_request"},
		{"too long", `{"text":"` + long + `"}`, 413, "too_large"},
		{"lang", `{"text":"a","lang":"fr"}`, 400, "unsupported_language"},
		{"sid", `{"text":"a","sid":10}`, 400, "bad_request"},
		{"neg sid", `{"text":"a","sid":-1}`, 400, "bad_request"},
		{"speed", `{"text":"a","speed":3}`, 400, "bad_request"},
		{"huge body", `{"text":"` + strings.Repeat("a", 20000) + `"}`, 413, "too_large"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, b := post(t, ts.URL+"/v1/synthesize", "application/json", []byte(c.body))
			if resp.StatusCode != c.status || errCode(t, b) != c.code {
				t.Fatalf("got %d %s", resp.StatusCode, b)
			}
		})
	}
}

func TestSynthesizeTimeoutCancelsEngine(t *testing.T) {
	_, ts := newTest(t, Config{TTSTimeout: 50 * time.Millisecond}, &fakeSTT{}, &fakeTTS{rate: 44100, delay: 5 * time.Second})
	start := time.Now()
	resp, b := post(t, ts.URL+"/v1/synthesize", "application/json", []byte(`{"text":"a"}`))
	if resp.StatusCode != 504 || errCode(t, b) != "timeout" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not honoured")
	}
}

func TestHostAndOriginChecks(t *testing.T) {
	_, ts := newTest(t, Config{}, &fakeSTT{}, &fakeTTS{rate: 44100})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/health", nil)
	req.Host = "evil.example:8743"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("host: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/synthesize", strings.NewReader(`{"text":"a"}`))
	req.Header.Set("Origin", "http://evil.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("origin: %d", resp.StatusCode)
	}
	for _, h := range []string{"127.0.0.1:8743", "localhost:8743", "[::1]:8743", "localhost"} {
		if !loopbackHost(h) {
			t.Errorf("%s should be allowed", h)
		}
	}
	if loopbackHost("10.0.0.1:8743") {
		t.Error("non-loopback allowed")
	}
}
