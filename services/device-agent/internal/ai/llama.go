package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// LlamaConfig configures the llama-server client.
type LlamaConfig struct {
	BaseURL   string // e.g. http://127.0.0.1:8742
	Timeout   time.Duration
	MaxTokens int // hard cap regardless of request
	// APIKey is sent as a bearer token (llama-server --api-key-file). Empty
	// means no Authorization header.
	APIKey string
}

// Llama talks to llama.cpp's llama-server (OpenAI-compatible endpoint).
// The LLM gets no tools, no file system, no network: it only receives the
// messages we construct and returns text, optionally schema-constrained.
type Llama struct {
	cfg  LlamaConfig
	http *http.Client
	g    *gate
}

// NewLlama creates a client.
func NewLlama(cfg LlamaConfig) *Llama {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 256
	}
	return &Llama{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}, g: newGate()}
}

type chatReq struct {
	Messages       []Message      `json:"messages"`
	MaxTokens      int            `json:"max_tokens"`
	Temperature    float64        `json:"temperature"`
	Stream         bool           `json:"stream"`
	ResponseFormat map[string]any `json:"response_format,omitempty"`
	CachePrompt    bool           `json:"cache_prompt"`
}

type chatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// MaxPromptBytes bounds the total prompt we send.
const MaxPromptBytes = 24 << 10

// Complete runs one chat completion.
func (l *Llama) Complete(ctx context.Context, r CompletionRequest) (string, error) {
	mt := r.MaxTokens
	if mt <= 0 || mt > l.cfg.MaxTokens {
		mt = l.cfg.MaxTokens
	}
	n := 0
	for _, m := range r.Messages {
		n += len(m.Content)
	}
	if n > MaxPromptBytes {
		return "", fmt.Errorf("ai: prompt %d bytes exceeds %d", n, MaxPromptBytes)
	}
	body := chatReq{Messages: r.Messages, MaxTokens: mt, Temperature: r.Temperature, CachePrompt: true}
	if r.JSONSchema != nil {
		body.ResponseFormat = map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "out", "strict": true, "schema": r.JSONSchema},
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	release, err := l.g.acquire(ctx)
	if err != nil {
		return "", err
	}
	out, err := l.do(ctx, b)
	release(err)
	return out, err
}

func (l *Llama) do(ctx context.Context, b []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.cfg.BaseURL+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	l.auth(req)
	resp, err := l.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnavailable, err) // %w: keeps context.Canceled visible to the gate
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ai: llama HTTP %d: %.200s", resp.StatusCode, rb)
	}
	var cr chatResp
	if err := json.Unmarshal(rb, &cr); err != nil {
		return "", fmt.Errorf("ai: llama decode: %w", err)
	}
	if cr.Error != nil {
		return "", fmt.Errorf("ai: llama: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("ai: llama returned no choices")
	}
	return cr.Choices[0].Message.Content, nil
}

func (l *Llama) auth(req *http.Request) {
	if l.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+l.cfg.APIKey)
	}
}

// Health checks llama-server's /health (returns 503 while loading). The
// health endpoint is public in llama-server, but the key is sent anyway so a
// key mismatch shows up as a failing completion, not a silent one.
func (l *Llama) Health(ctx context.Context) error {
	if l.g.open() {
		return ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.cfg.BaseURL+"/health", nil)
	if err != nil {
		return err
	}
	l.auth(req)
	resp, err := l.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	return nil
}
