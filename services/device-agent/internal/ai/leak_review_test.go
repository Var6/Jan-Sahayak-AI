package ai

// Reliability review (cancellation / goroutine leaks): a runtime that
// accepts a request and never answers must not hold the caller past its
// context, must release the 1-slot gate, and must leave no goroutines once
// idle connections are closed.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestReliabilityHangingRuntimesRespectContextNoLeak(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	llm := NewLlama(LlamaConfig{BaseURL: srv.URL, Timeout: time.Minute})
	stt := NewSpeechSTT(SpeechConfig{BaseURL: srv.URL, Timeout: time.Minute})
	tts := NewSpeechTTS(SpeechConfig{BaseURL: srv.URL, Timeout: time.Minute})
	wav := relWAV(16000, 1)

	for name, call := range map[string]func(context.Context) error{
		"llm": func(ctx context.Context) error {
			_, err := llm.Complete(ctx, CompletionRequest{Messages: []Message{{Role: "user", Content: "x"}}})
			return err
		},
		"stt": func(ctx context.Context) error { _, err := stt.Transcribe(ctx, wav, "hi"); return err },
		"tts": func(ctx context.Context) error { _, err := tts.Synthesize(ctx, "नमस्ते", "hi"); return err },
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		start := time.Now()
		err := call(ctx)
		cancel()
		if err == nil {
			t.Fatalf("%s: expected an error from a hanging runtime", name)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("%s: call took %v despite a 150 ms context", name, d)
		}
		// The slot must be free again: a second call must not report busy.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
		err = call(ctx2)
		cancel2()
		if errors.Is(err, ErrBusy) {
			t.Fatalf("%s: gate slot leaked (second call got ErrBusy)", name)
		}
	}
	close(release)
	for _, c := range []*http.Client{llm.http, stt.http, tts.http} {
		c.CloseIdleConnections()
	}
	srv.Close()
	goleak.VerifyNone(t, ignore)
}

// relWAV builds a 16-bit mono PCM WAV of the given rate and length.
func relWAV(rate, secs int) []byte {
	n := rate * secs
	data := n * 2
	b := make([]byte, 44+data)
	copy(b[0:], "RIFF")
	le32(b[4:], uint32(36+data))
	copy(b[8:], "WAVEfmt ")
	le32(b[16:], 16)
	le16(b[20:], 1)
	le16(b[22:], 1)
	le32(b[24:], uint32(rate))
	le32(b[28:], uint32(rate*2))
	le16(b[32:], 2)
	le16(b[34:], 16)
	copy(b[36:], "data")
	le32(b[40:], uint32(data))
	for i := 0; i < n; i++ {
		v := int16((i%50)*200 - 5000)
		le16(b[44+2*i:], uint16(v))
	}
	return b
}

func le16(b []byte, v uint16) { b[0], b[1] = byte(v), byte(v>>8) }
func le32(b []byte, v uint32) { b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24) }
