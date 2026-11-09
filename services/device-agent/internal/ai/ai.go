// Package ai contains clients for the native AI runtimes the device agent
// talks to over loopback HTTP: whisper-server (STT), llama-server (LLM) and
// the TTS runtime.
//
// Every client:
//   - limits concurrency to one in-flight request (a kiosk serves one person),
//   - applies a hard timeout per call,
//   - bounds request and response sizes,
//   - trips a circuit breaker after repeated failures so a dead runtime
//     costs the citizen no waiting time (the dialogue engine falls back to
//     deterministic text or on-screen text).
package ai

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrUnavailable is returned while the breaker is open or the runtime is down.
var ErrUnavailable = errors.New("ai: runtime unavailable")

// ErrBusy is returned when another request holds the runtime and the caller's
// context expires while waiting.
var ErrBusy = errors.New("ai: runtime busy")

// Transcript is an STT result.
type Transcript struct {
	Text       string  `json:"text"`
	Language   string  `json:"language"`
	Confidence float64 `json:"confidence"` // 0..1, exp(duration-weighted avg_logprob)
	NoSpeech   bool    `json:"no_speech"`
	AudioSecs  float64 `json:"audio_secs"`
	LatencyMS  int64   `json:"latency_ms"`
}

// STT transcribes 16 kHz mono 16-bit PCM WAV audio.
type STT interface {
	Transcribe(ctx context.Context, wav []byte, lang string) (Transcript, error)
	Health(ctx context.Context) error
}

// TTS synthesises speech and returns a WAV file.
type TTS interface {
	Synthesize(ctx context.Context, text, lang string) ([]byte, error)
	Health(ctx context.Context) error
}

// Message is a chat message for the LLM.
type Message struct {
	Role    string `json:"role"` // system | user | assistant
	Content string `json:"content"`
}

// CompletionRequest asks the LLM for a completion. If JSONSchema is set the
// runtime constrains output to that schema (llama.cpp grammar).
type CompletionRequest struct {
	Messages    []Message
	JSONSchema  map[string]any
	MaxTokens   int
	Temperature float64
}

// LLM is a constrained text generator. It is never a source of facts.
type LLM interface {
	Complete(ctx context.Context, req CompletionRequest) (string, error)
	Health(ctx context.Context) error
}

// gate combines a 1-slot semaphore and a circuit breaker.
type gate struct {
	sem chan struct{}

	mu        sync.Mutex
	failures  int
	openUntil time.Time
	threshold int
	cooldown  time.Duration
	now       func() time.Time
}

func newGate() *gate {
	return &gate{sem: make(chan struct{}, 1), threshold: 3, cooldown: 30 * time.Second, now: time.Now}
}

// acquire waits for the slot or fails fast when the breaker is open.
func (g *gate) acquire(ctx context.Context) (func(err error), error) {
	g.mu.Lock()
	if g.now().Before(g.openUntil) {
		g.mu.Unlock()
		return nil, ErrUnavailable
	}
	g.mu.Unlock()
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ErrBusy
	}
	return func(err error) {
		<-g.sem
		g.mu.Lock()
		defer g.mu.Unlock()
		if err == nil || errors.Is(err, context.Canceled) {
			g.failures = 0
			return
		}
		g.failures++
		if g.failures >= g.threshold {
			g.openUntil = g.now().Add(g.cooldown)
			g.failures = 0
		}
	}, nil
}

// open reports whether the breaker is currently open.
func (g *gate) open() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.now().Before(g.openUntil)
}
