package ai

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Clients for services/speech-worker (sherpa-onnx: Silero VAD +
// IndicConformer-Hindi STT + Supertonic-3 TTS on loopback HTTP).
//
// SpeechSTT and SpeechTTS each own a gate (1-slot semaphore + circuit
// breaker) because the worker runs one request at a time per engine;
// a busy STT engine must not block TTS and vice versa.

// SpeechMaxWAVBytes is the worker's upload cap (1 MiB ≈ 32 s at 16 kHz).
const SpeechMaxWAVBytes = 1 << 20

// ErrUnsupportedLanguage is returned without contacting the worker when a
// language the speech models do not cover is requested. The STT model is
// Hindi-only; TTS languages are whatever the worker was started with.
var ErrUnsupportedLanguage = errors.New("ai: language not supported by speech worker")

// SpeechConfig configures both speech clients.
type SpeechConfig struct {
	BaseURL string        // e.g. http://127.0.0.1:8743
	Timeout time.Duration // per request; default 20 s (STT) / 35 s (TTS)
	// MaxAudioSecs rejects longer clips before upload (default 20).
	MaxAudioSecs float64
	// MaxResponseBytes bounds TTS WAV downloads (default 12 MiB ≈ 140 s
	// at 44.1 kHz 16-bit mono).
	MaxResponseBytes int64
	// Voice selects a Supertonic preset (sid) when UseVoice is true;
	// otherwise the worker's configured default voice is used.
	Voice    int
	UseVoice bool
	// Speed is the speaking rate (0.5–2). 0 uses the worker's default (1).
	Speed float64
}

func (c *SpeechConfig) defaults(timeout time.Duration) {
	if c.Timeout <= 0 {
		c.Timeout = timeout
	}
	if c.MaxAudioSecs <= 0 {
		c.MaxAudioSecs = 20
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = 12 << 20
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
}

// speechError is a non-2xx answer from the worker.
type speechError struct {
	Status  int
	Code    string
	Message string
}

func (e *speechError) Error() string {
	return fmt.Sprintf("ai: speech worker HTTP %d %s: %.200s", e.Status, e.Code, e.Message)
}

// runtimeFault reports whether the error says something about the
// worker's health (counts towards the breaker) rather than our input.
func runtimeFault(err error) bool {
	var se *speechError
	if errors.As(err, &se) {
		return se.Status >= 500 && se.Code != "busy"
	}
	return err != nil
}

// mapSpeechError turns worker error codes into the package's sentinels.
func mapSpeechError(err error) error {
	var se *speechError
	if !errors.As(err, &se) {
		return err
	}
	switch {
	case se.Code == "busy":
		return fmt.Errorf("%w: %v", ErrBusy, se)
	case se.Code == "loading" || se.Status == http.StatusServiceUnavailable:
		return fmt.Errorf("%w: %v", ErrUnavailable, se)
	case se.Code == "unsupported_language":
		return fmt.Errorf("%w: %v", ErrUnsupportedLanguage, se)
	case se.Code == "bad_audio" || (se.Code == "too_large" && se.Status == http.StatusRequestEntityTooLarge):
		return fmt.Errorf("%w: %v", ErrBadAudio, se)
	}
	return err
}

func readSpeechError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	se := &speechError{Status: resp.StatusCode}
	if json.Unmarshal(b, &body) == nil && body.Error.Code != "" {
		se.Code, se.Message = body.Error.Code, body.Error.Message
	} else {
		se.Message = string(b)
	}
	return se
}

// post sends one request and returns the body of a 200 response, read up
// to limit bytes (exceeding the limit is an error).
func speechPost(ctx context.Context, c *http.Client, url, contentType string, body []byte, limit int64) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, readSpeechError(resp)
	}
	rb, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, fmt.Errorf("ai: speech worker read: %w", err)
	}
	if int64(len(rb)) > limit {
		return nil, nil, fmt.Errorf("ai: speech worker response exceeds %d bytes", limit)
	}
	return rb, resp.Header, nil
}

// ------------------------------------------------------------------- STT

// SpeechSTT implements STT against the speech worker.
type SpeechSTT struct {
	cfg  SpeechConfig
	http *http.Client
	g    *gate
}

var _ STT = (*SpeechSTT)(nil)

// NewSpeechSTT creates a client.
func NewSpeechSTT(cfg SpeechConfig) *SpeechSTT {
	cfg.defaults(20 * time.Second)
	return &SpeechSTT{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}, g: newGate()}
}

type speechTranscribeResp struct {
	Text       string   `json:"text"`
	Language   string   `json:"language"`
	Confidence *float64 `json:"confidence"`
	NoSpeech   bool     `json:"no_speech"`
	AudioSecs  float64  `json:"audio_secs"`
	SpeechSecs float64  `json:"speech_secs"`
	LatencyMS  int64    `json:"latency_ms"`
}

// maxTranscriptRunes rejects absurd worker output (20 s of speech is
// ~100 words).
const maxTranscriptRunes = 2000

// Transcribe sends 16 kHz mono 16-bit WAV to the worker. lang must be ""
// or "hi" (the IndicConformer model is Hindi-only).
func (s *SpeechSTT) Transcribe(ctx context.Context, wav []byte, lang string) (Transcript, error) {
	if lang != "" && lang != "hi" {
		return Transcript{}, fmt.Errorf("%w: STT %q", ErrUnsupportedLanguage, lang)
	}
	if len(wav) > SpeechMaxWAVBytes {
		return Transcript{}, fmt.Errorf("%w: too large", ErrBadAudio)
	}
	info, err := ParseWAV(wav)
	if err != nil {
		return Transcript{}, err
	}
	if info.Seconds > s.cfg.MaxAudioSecs {
		return Transcript{}, fmt.Errorf("%w: %.1fs exceeds %.0fs", ErrBadAudio, info.Seconds, s.cfg.MaxAudioSecs)
	}
	if info.Seconds < 0.25 {
		return Transcript{Language: "hi", NoSpeech: true, AudioSecs: info.Seconds}, nil
	}
	release, err := s.g.acquire(ctx)
	if err != nil {
		return Transcript{}, err
	}
	start := time.Now()
	tr, err := s.do(ctx, wav)
	if runtimeFault(err) {
		release(err)
	} else {
		release(nil)
	}
	if err != nil {
		return Transcript{}, mapSpeechError(err)
	}
	tr.AudioSecs = info.Seconds
	tr.LatencyMS = time.Since(start).Milliseconds()
	return tr, nil
}

func (s *SpeechSTT) do(ctx context.Context, wav []byte) (Transcript, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	rb, _, err := speechPost(ctx, s.http, s.cfg.BaseURL+"/v1/transcribe?lang=hi", "audio/wav", wav, 64<<10)
	if err != nil {
		return Transcript{}, err
	}
	var r speechTranscribeResp
	if err := json.Unmarshal(rb, &r); err != nil {
		return Transcript{}, fmt.Errorf("ai: speech worker decode: %w", err)
	}
	if r.Confidence == nil || math.IsNaN(*r.Confidence) || *r.Confidence < 0 || *r.Confidence > 1 {
		return Transcript{}, errors.New("ai: speech worker returned invalid confidence")
	}
	text := strings.Join(strings.Fields(r.Text), " ")
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxTranscriptRunes {
		return Transcript{}, errors.New("ai: speech worker returned invalid transcript")
	}
	tr := Transcript{Text: text, Language: "hi", Confidence: *r.Confidence, NoSpeech: r.NoSpeech || text == ""}
	if tr.NoSpeech {
		tr.Text, tr.Confidence = "", 0
	}
	return tr, nil
}

// Health reports whether the worker has its models loaded.
func (s *SpeechSTT) Health(ctx context.Context) error {
	if s.g.open() {
		return ErrUnavailable
	}
	return getOK(ctx, s.http, s.cfg.BaseURL+"/health")
}

// ------------------------------------------------------------------- TTS

// SpeechTTS implements TTS against the speech worker.
type SpeechTTS struct {
	cfg  SpeechConfig
	http *http.Client
	g    *gate
}

var _ TTS = (*SpeechTTS)(nil)

// NewSpeechTTS creates a client.
func NewSpeechTTS(cfg SpeechConfig) *SpeechTTS {
	cfg.defaults(35 * time.Second)
	return &SpeechTTS{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}, g: newGate()}
}

type speechSynthReq struct {
	Text  string   `json:"text"`
	Lang  string   `json:"lang"`
	SID   *int     `json:"sid,omitempty"`
	Speed *float64 `json:"speed,omitempty"`
}

// ErrEmptyText is returned when nothing speakable remains after
// normalisation.
var ErrEmptyText = errors.New("ai: nothing to synthesise")

// Synthesize returns a 16-bit PCM mono WAV at the model's native rate
// (44.1 kHz for Supertonic-3). Hindi text is passed through PrepareForTTS
// first (digits, amounts, dates, acronyms and URLs → speakable Hindi,
// bounded to MaxTTSRunes); PrepareForTTS is idempotent, so callers that
// already normalised lose nothing.
func (t *SpeechTTS) Synthesize(ctx context.Context, text, lang string) ([]byte, error) {
	if lang == "" {
		lang = "hi"
	}
	if lang == "hi" {
		text = PrepareForTTS(text)
	} else {
		text = truncateSpeech(strings.Join(strings.Fields(strings.ToValidUTF8(text, " ")), " "), MaxTTSRunes)
	}
	if text == "" {
		return nil, ErrEmptyText
	}
	req := speechSynthReq{Text: text, Lang: lang}
	if t.cfg.UseVoice {
		v := t.cfg.Voice
		req.SID = &v
	}
	if t.cfg.Speed > 0 {
		sp := t.cfg.Speed
		req.Speed = &sp
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	release, err := t.g.acquire(ctx)
	if err != nil {
		return nil, err
	}
	wav, err := t.do(ctx, body)
	if runtimeFault(err) {
		release(err)
	} else {
		release(nil)
	}
	if err != nil {
		return nil, mapSpeechError(err)
	}
	return wav, nil
}

func (t *SpeechTTS) do(ctx context.Context, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, t.cfg.Timeout)
	defer cancel()
	wav, hdr, err := speechPost(ctx, t.http, t.cfg.BaseURL+"/v1/synthesize", "application/json", body, t.cfg.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "audio/wav") {
		return nil, fmt.Errorf("ai: speech worker returned %q, want audio/wav", ct)
	}
	if _, err := parsePCMOutputWAV(wav); err != nil {
		return nil, err
	}
	return wav, nil
}

// Health reports whether the worker has its models loaded.
func (t *SpeechTTS) Health(ctx context.Context) error {
	if t.g.open() {
		return ErrUnavailable
	}
	return getOK(ctx, t.http, t.cfg.BaseURL+"/health")
}

// parsePCMOutputWAV validates synthesised audio: RIFF/WAVE, PCM, mono,
// 16-bit, 8–48 kHz, non-empty. (ParseWAV is input-specific: 16 kHz only.)
func parsePCMOutputWAV(b []byte) (WAVInfo, error) {
	bad := errors.New("ai: speech worker returned invalid WAV")
	var info WAVInfo
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return info, bad
	}
	off, gotFmt := 12, false
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		if body+size > len(b) {
			return info, bad
		}
		switch id {
		case "fmt ":
			if size < 16 || binary.LittleEndian.Uint16(b[body:]) != 1 {
				return info, bad
			}
			info.Channels = int(binary.LittleEndian.Uint16(b[body+2:]))
			info.SampleRate = int(binary.LittleEndian.Uint32(b[body+4:]))
			info.Bits = int(binary.LittleEndian.Uint16(b[body+14:]))
			if info.Channels != 1 || info.Bits != 16 || info.SampleRate < 8000 || info.SampleRate > 48000 {
				return info, bad
			}
			gotFmt = true
		case "data":
			if !gotFmt || size == 0 {
				return info, bad
			}
			info.DataBytes = size
			info.Seconds = float64(size) / float64(info.SampleRate*2)
			return info, nil
		}
		off = body + size + size%2
	}
	return info, bad
}
