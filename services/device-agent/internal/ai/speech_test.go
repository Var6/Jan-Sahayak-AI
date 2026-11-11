package ai

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testWAV builds a 16-bit mono PCM WAV of n samples at rate.
func testWAV(rate, n int) []byte {
	b := make([]byte, 44+2*n)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+2*n))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(2*n))
	return b
}

func writeWorkerErr(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": code}})
}

func TestSpeechSTTTranscribe(t *testing.T) {
	var gotQuery, gotCT string
	var gotLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/transcribe" {
			http.NotFound(w, r)
			return
		}
		gotQuery, gotCT = r.URL.RawQuery, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotLen = len(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"text": "  मुझे   लोन चाहिए ", "language": "hi", "confidence": 0.8,
			"confidence_source": "heuristic", "no_speech": false, "audio_secs": 1, "speech_secs": 0.9, "latency_ms": 120,
		})
	}))
	defer srv.Close()
	c := NewSpeechSTT(SpeechConfig{BaseURL: srv.URL + "/"})
	wav := testWAV(16000, 16000)
	tr, err := c.Transcribe(context.Background(), wav, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Text != "मुझे लोन चाहिए" || tr.Confidence != 0.8 || tr.NoSpeech || tr.Language != "hi" || tr.AudioSecs != 1 {
		t.Fatalf("%+v", tr)
	}
	if gotQuery != "lang=hi" || gotCT != "audio/wav" || gotLen != len(wav) {
		t.Fatalf("query=%q ct=%q len=%d", gotQuery, gotCT, gotLen)
	}
}

func TestSpeechSTTLocalValidation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()
	c := NewSpeechSTT(SpeechConfig{BaseURL: srv.URL, MaxAudioSecs: 20})
	ctx := context.Background()

	if _, err := c.Transcribe(ctx, testWAV(16000, 16000), "en"); !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("lang: %v", err)
	}
	if _, err := c.Transcribe(ctx, testWAV(44100, 1000), "hi"); !errors.Is(err, ErrBadAudio) {
		t.Fatalf("rate: %v", err)
	}
	if _, err := c.Transcribe(ctx, testWAV(16000, 21*16000), "hi"); !errors.Is(err, ErrBadAudio) {
		t.Fatalf("too long: %v", err)
	}
	if _, err := c.Transcribe(ctx, make([]byte, SpeechMaxWAVBytes+1), "hi"); !errors.Is(err, ErrBadAudio) {
		t.Fatalf("too big: %v", err)
	}
	tr, err := c.Transcribe(ctx, testWAV(16000, 1600), "")
	if err != nil || !tr.NoSpeech {
		t.Fatalf("short clip: %+v %v", tr, err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("worker contacted %d times", n)
	}
}

func TestSpeechSTTNoSpeechAndBadResponses(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body.Load().(string))
	}))
	defer srv.Close()
	c := NewSpeechSTT(SpeechConfig{BaseURL: srv.URL})
	wav := testWAV(16000, 16000)

	body.Store(`{"text":"","confidence":0,"no_speech":true}`)
	tr, err := c.Transcribe(context.Background(), wav, "hi")
	if err != nil || !tr.NoSpeech || tr.Text != "" {
		t.Fatalf("%+v %v", tr, err)
	}
	for _, bad := range []string{`not json`, `{"text":"x"}`, `{"text":"x","confidence":1.5}`, `{"text":"` + strings.Repeat("क", 2001) + `","confidence":0.5}`} {
		body.Store(bad)
		if _, err := c.Transcribe(context.Background(), wav, "hi"); err == nil {
			t.Errorf("accepted %.40q", bad)
		}
	}
}

func TestSpeechErrorsAndBreaker(t *testing.T) {
	var status atomic.Int32
	var code atomic.Value
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeWorkerErr(w, int(status.Load()), code.Load().(string))
	}))
	defer srv.Close()
	c := NewSpeechSTT(SpeechConfig{BaseURL: srv.URL})
	wav := testWAV(16000, 16000)
	ctx := context.Background()

	// input errors and busy never trip the breaker
	for _, tc := range []struct {
		status int
		code   string
		want   error
	}{
		{400, "bad_audio", ErrBadAudio},
		{413, "too_large", ErrBadAudio},
		{400, "unsupported_language", ErrUnsupportedLanguage},
		{503, "busy", ErrBusy},
	} {
		status.Store(int32(tc.status))
		code.Store(tc.code)
		for range 4 {
			if _, err := c.Transcribe(ctx, wav, "hi"); !errors.Is(err, tc.want) {
				t.Fatalf("%s: %v", tc.code, err)
			}
		}
	}
	if c.g.open() {
		t.Fatal("breaker opened on non-runtime errors")
	}

	// loading → unavailable, counts as a fault
	status.Store(503)
	code.Store("loading")
	if _, err := c.Transcribe(ctx, wav, "hi"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("loading: %v", err)
	}
	// 3 consecutive runtime faults open the breaker
	status.Store(500)
	code.Store("internal")
	for range 2 {
		_, _ = c.Transcribe(ctx, wav, "hi")
	}
	if !c.g.open() {
		t.Fatal("breaker should be open")
	}
	before := calls.Load()
	if _, err := c.Transcribe(ctx, wav, "hi"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("open breaker: %v", err)
	}
	if calls.Load() != before {
		t.Fatal("open breaker still contacted the worker")
	}
	if err := c.Health(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("health with open breaker: %v", err)
	}
}

func TestSpeechTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	c := NewSpeechSTT(SpeechConfig{BaseURL: srv.URL, Timeout: 100 * time.Millisecond})
	start := time.Now()
	_, err := c.Transcribe(context.Background(), testWAV(16000, 16000), "hi")
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestSpeechConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := NewSpeechTTS(SpeechConfig{BaseURL: url})
	if _, err := c.Synthesize(context.Background(), "नमस्ते", "hi"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	if err := c.Health(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("health: %v", err)
	}
}

func TestSpeechTTSSynthesize(t *testing.T) {
	var mu sync.Mutex
	var got speechSynthReq
	last := func() speechSynthReq { mu.Lock(); defer mu.Unlock(); return got }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = io.WriteString(w, `{"status":"ok"}`)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			writeWorkerErr(w, 400, "bad_request")
			return
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var req speechSynthReq
		if err := dec.Decode(&req); err != nil {
			writeWorkerErr(w, 400, "bad_request")
			return
		}
		mu.Lock()
		got = req
		mu.Unlock()
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(testWAV(44100, 4410))
	}))
	defer srv.Close()
	c := NewSpeechTTS(SpeechConfig{BaseURL: srv.URL, UseVoice: true, Voice: 3, Speed: 1.1})
	wav, err := c.Synthesize(context.Background(), "PM Kisan की किस्त ₹2,000 है।", "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := parsePCMOutputWAV(wav)
	if err != nil || info.SampleRate != 44100 {
		t.Fatalf("%+v %v", info, err)
	}
	want := PrepareForTTS("PM Kisan की किस्त ₹2,000 है।")
	g := last()
	if g.Text != want || g.Lang != "hi" || g.SID == nil || *g.SID != 3 || g.Speed == nil || *g.Speed != 1.1 {
		t.Fatalf("request %+v (want text %q)", g, want)
	}
	if strings.ContainsAny(g.Text, "0123456789₹") {
		t.Fatalf("digits reached the worker: %q", g.Text)
	}
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	// default voice/speed are omitted
	c2 := NewSpeechTTS(SpeechConfig{BaseURL: srv.URL})
	if _, err := c2.Synthesize(context.Background(), "नमस्ते", "hi"); err != nil {
		t.Fatal(err)
	}
	if got := last(); got.SID != nil || got.Speed != nil {
		t.Fatalf("defaults sent: %+v", got)
	}
}

func TestSpeechTTSRejectsBadOutput(t *testing.T) {
	var mode atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 0: // wrong content type
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(testWAV(44100, 10))
		case 1: // not a WAV
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = io.WriteString(w, strings.Repeat("x", 100))
		case 2: // stereo
			b := testWAV(44100, 10)
			binary.LittleEndian.PutUint16(b[22:], 2)
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write(b)
		case 3: // too big
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write(testWAV(44100, 4096))
		}
	}))
	defer srv.Close()
	c := NewSpeechTTS(SpeechConfig{BaseURL: srv.URL, MaxResponseBytes: 4096})
	for m := range int32(4) {
		mode.Store(m)
		if _, err := c.Synthesize(context.Background(), "नमस्ते", "hi"); err == nil {
			t.Errorf("mode %d accepted", m)
		}
	}
}

func TestSpeechTTSEmptyText(t *testing.T) {
	c := NewSpeechTTS(SpeechConfig{BaseURL: "http://127.0.0.1:1"})
	if _, err := c.Synthesize(context.Background(), " ** ", "hi"); !errors.Is(err, ErrEmptyText) {
		t.Fatalf("%v", err)
	}
}

func TestSpeechGateSerialises(t *testing.T) {
	var inFlight, maxInFlight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(testWAV(44100, 100))
	}))
	defer srv.Close()
	c := NewSpeechTTS(SpeechConfig{BaseURL: srv.URL})
	done := make(chan error, 4)
	for range 4 {
		go func() {
			_, err := c.Synthesize(context.Background(), "नमस्ते", "hi")
			done <- err
		}()
	}
	for range 4 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if maxInFlight.Load() != 1 {
		t.Fatalf("max in flight %d", maxInFlight.Load())
	}
}
