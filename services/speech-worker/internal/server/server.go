// Package server is the speech worker's loopback HTTP layer. It knows
// nothing about sherpa-onnx: engines are injected through the Transcriber
// and Synthesizer interfaces once they have loaded, so the package is
// testable without native libraries or model files.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/janmanindia/jan-sahayak/services/speech-worker/internal/audio"
)

// STTResult is what a Transcriber returns.
type STTResult struct {
	Text             string
	Confidence       float64
	ConfidenceSource string
	NoSpeech         bool
	SpeechSecs       float64
	SNRDB            float64
}

// Transcriber turns 16 kHz mono samples into text. Implementations need
// not be goroutine-safe: the server calls them one at a time.
type Transcriber interface {
	Transcribe(samples []float32) STTResult
}

// Synthesizer renders text as mono float32 samples at SampleRate().
// Implementations need not be goroutine-safe. They should return promptly
// once ctx is done (between sentences is fine).
type Synthesizer interface {
	Synthesize(ctx context.Context, text, lang string, sid int, speed float32, maxSamples int) ([]float32, int, error)
	SampleRate() int
	NumSpeakers() int
}

// Config bounds every request.
type Config struct {
	MaxAudioBytes int64         // POST /v1/transcribe body cap (default 1 MiB)
	MaxAudioSecs  float64       // default 20
	MaxJSONBytes  int64         // POST /v1/synthesize body cap (default 16 KiB)
	MaxTextRunes  int           // default 600
	MaxTTSSecs    float64       // output audio cap (default 120)
	STTTimeout    time.Duration // default 15 s
	TTSTimeout    time.Duration // default 30 s
	BusyWait      time.Duration // how long a request waits for a busy engine (default 2 s)
	TTSLangs      []string      // accepted synthesis languages (default hi)
	DefaultSID    int           // voice used when a request omits sid
}

func (c *Config) defaults() {
	if c.MaxAudioBytes <= 0 {
		c.MaxAudioBytes = 1 << 20
	}
	if c.MaxAudioSecs <= 0 {
		c.MaxAudioSecs = 20
	}
	if c.MaxJSONBytes <= 0 {
		c.MaxJSONBytes = 16 << 10
	}
	if c.MaxTextRunes <= 0 {
		c.MaxTextRunes = 600
	}
	if c.MaxTTSSecs <= 0 {
		c.MaxTTSSecs = 120
	}
	if c.STTTimeout <= 0 {
		c.STTTimeout = 15 * time.Second
	}
	if c.TTSTimeout <= 0 {
		c.TTSTimeout = 30 * time.Second
	}
	if c.BusyWait <= 0 {
		c.BusyWait = 2 * time.Second
	}
	if len(c.TTSLangs) == 0 {
		c.TTSLangs = []string{"hi"}
	}
}

type engines struct {
	stt  Transcriber
	tts  Synthesizer
	info map[string]any
}

// Server serves the worker API.
type Server struct {
	cfg Config
	log *slog.Logger

	eng atomic.Pointer[engines]

	sttSlot chan struct{} // capacity 1: one transcription at a time
	ttsSlot chan struct{} // capacity 1: one synthesis at a time
	// inflight tracks inference goroutines, which may outlive their HTTP
	// request after a timeout (native calls cannot be interrupted).
	inflight sync.WaitGroup
}

// New creates a server in the "loading" state.
func New(cfg Config, log *slog.Logger) *Server {
	cfg.defaults()
	return &Server{
		cfg:     cfg,
		log:     log,
		sttSlot: make(chan struct{}, 1),
		ttsSlot: make(chan struct{}, 1),
	}
}

// SetEngines marks the server ready. info is reported by GET /health.
func (s *Server) SetEngines(stt Transcriber, tts Synthesizer, info map[string]any) {
	s.eng.Store(&engines{stt: stt, tts: tts, info: info})
}

// Drain waits until no inference is running (or ctx ends). Call it after
// http.Server.Shutdown and before freeing native engines.
func (s *Server) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() { s.inflight.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Handler returns the HTTP handler with routing, Host/Origin checks,
// panic recovery and access logging.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /v1/transcribe", s.transcribe)
	mux.HandleFunc("POST /v1/synthesize", s.synthesize)
	return s.middleware(mux)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				if sw.status == 0 {
					writeErr(sw, http.StatusInternalServerError, "internal", "internal error")
				}
			}
			if r.URL.Path != "/health" || sw.status != http.StatusOK {
				s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
					"ms", time.Since(start).Milliseconds())
			}
		}()
		if !loopbackHost(r.Host) {
			writeErr(sw, http.StatusForbidden, "forbidden", "host not allowed")
			return
		}
		// Browsers always send Origin on cross-origin POSTs; the device
		// agent never does. Rejecting it blocks DNS-rebinding/CSRF from any
		// page the kiosk browser might load.
		if r.Header.Get("Origin") != "" {
			writeErr(sw, http.StatusForbidden, "forbidden", "browser requests not allowed")
			return
		}
		next.ServeHTTP(sw, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	e := s.eng.Load()
	if e == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "loading"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"models":   e.info,
		"stt_busy": len(s.sttSlot) > 0,
		"tts_busy": len(s.ttsSlot) > 0,
	})
}

var (
	errBusy    = errors.New("engine busy")
	errTimeout = errors.New("inference timed out")
	errPanic   = errors.New("inference panicked")
)

// run executes fn while holding slot. It waits at most BusyWait for the
// slot. fn runs in its own goroutine so the request can give up at the
// deadline; the goroutine keeps the slot until fn really returns, so a
// stuck native call blocks later requests (which get 503) instead of
// piling up concurrent native work.
func (s *Server) run(ctx context.Context, slot chan struct{}, timeout time.Duration, fn func(ctx context.Context) error) error {
	wait := time.NewTimer(s.cfg.BusyWait)
	defer wait.Stop()
	select {
	case slot <- struct{}{}:
	case <-wait.C:
		return errBusy
	case <-ctx.Done():
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	done := make(chan error, 1)
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		defer func() { <-slot }()
		defer cancel()
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in inference", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				done <- errPanic
			}
		}()
		done <- fn(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errTimeout
		}
		return ctx.Err()
	}
}

func (s *Server) runErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBusy):
		writeErr(w, http.StatusServiceUnavailable, "busy", "engine busy, retry")
	case errors.Is(err, errTimeout):
		writeErr(w, http.StatusGatewayTimeout, "timeout", "inference timed out")
	case errors.Is(err, context.Canceled):
		// client went away; nothing useful to write
		writeErr(w, 499, "canceled", "request canceled")
	default:
		s.log.Error("inference failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "inference failed")
	}
}

// TranscribeResponse is the JSON body of POST /v1/transcribe.
type TranscribeResponse struct {
	Text             string  `json:"text"`
	Language         string  `json:"language"`
	Confidence       float64 `json:"confidence"`
	ConfidenceSource string  `json:"confidence_source"`
	NoSpeech         bool    `json:"no_speech"`
	AudioSecs        float64 `json:"audio_secs"`
	SpeechSecs       float64 `json:"speech_secs"`
	SNRDB            float64 `json:"snr_db"`
	LatencyMS        int64   `json:"latency_ms"`
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func (s *Server) transcribe(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	e := s.eng.Load()
	if e == nil {
		writeErr(w, http.StatusServiceUnavailable, "loading", "models are loading")
		return
	}
	if lang := r.URL.Query().Get("lang"); lang != "" && lang != "hi" {
		writeErr(w, http.StatusBadRequest, "unsupported_language", "only lang=hi is supported")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, _ := mime.ParseMediaType(ct)
		switch mt {
		case "audio/wav", "audio/x-wav", "audio/wave", "application/octet-stream":
		default:
			writeErr(w, http.StatusUnsupportedMediaType, "bad_audio", "Content-Type must be audio/wav")
			return
		}
	}
	if r.ContentLength > s.cfg.MaxAudioBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "audio body too large")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxAudioBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "audio body too large")
			return
		}
		writeErr(w, http.StatusBadRequest, "bad_request", "could not read body")
		return
	}
	pcm, info, err := audio.DecodeMono16(body, 16000, s.cfg.MaxAudioSecs)
	body = nil // allow GC of the raw bytes during inference
	if err != nil {
		if errors.Is(err, audio.ErrTooLong) {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, "bad_audio", err.Error())
		return
	}
	resp := TranscribeResponse{Language: "hi", AudioSecs: round3(info.Seconds()), ConfidenceSource: "none"}
	if info.Seconds() < 0.25 {
		resp.NoSpeech = true
		resp.LatencyMS = time.Since(start).Milliseconds()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	var res STTResult
	err = s.run(r.Context(), s.sttSlot, s.cfg.STTTimeout, func(context.Context) error {
		res = e.stt.Transcribe(pcm)
		return nil
	})
	if err != nil {
		s.runErr(w, err)
		return
	}
	resp.Text = res.Text
	resp.NoSpeech = res.NoSpeech || res.Text == ""
	resp.SpeechSecs = round3(res.SpeechSecs)
	resp.SNRDB = res.SNRDB
	if !resp.NoSpeech {
		resp.Confidence = round3(res.Confidence)
		resp.ConfidenceSource = res.ConfidenceSource
	}
	resp.LatencyMS = time.Since(start).Milliseconds()
	writeJSON(w, http.StatusOK, resp)
}

// SynthesizeRequest is the JSON body of POST /v1/synthesize.
type SynthesizeRequest struct {
	Text  string   `json:"text"`
	Lang  string   `json:"lang"`
	SID   *int     `json:"sid,omitempty"`
	Speed *float64 `json:"speed,omitempty"`
}

func (s *Server) synthesize(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	e := s.eng.Load()
	if e == nil {
		writeErr(w, http.StatusServiceUnavailable, "loading", "models are loading")
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.MaxJSONBytes))
	dec.DisallowUnknownFields()
	var req SynthesizeRequest
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "body too large")
			return
		}
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	text := strings.TrimSpace(req.Text)
	switch {
	case text == "":
		writeErr(w, http.StatusBadRequest, "bad_request", "text is required")
		return
	case !utf8.ValidString(text):
		writeErr(w, http.StatusBadRequest, "bad_request", "text must be UTF-8")
		return
	case utf8.RuneCountInString(text) > s.cfg.MaxTextRunes:
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("text exceeds %d characters", s.cfg.MaxTextRunes))
		return
	}
	lang := req.Lang
	if lang == "" {
		lang = "hi"
	}
	if !contains(s.cfg.TTSLangs, lang) {
		writeErr(w, http.StatusBadRequest, "unsupported_language", "unsupported lang")
		return
	}
	sid := s.cfg.DefaultSID
	if req.SID != nil {
		sid = *req.SID
	}
	if n := e.tts.NumSpeakers(); sid < 0 || (n > 0 && sid >= n) {
		writeErr(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("sid must be in [0,%d)", n))
		return
	}
	speed := 1.0
	if req.Speed != nil {
		speed = *req.Speed
		if math.IsNaN(speed) || speed < 0.5 || speed > 2 {
			writeErr(w, http.StatusBadRequest, "bad_request", "speed must be in [0.5,2]")
			return
		}
	}
	maxSamples := int(s.cfg.MaxTTSSecs * float64(e.tts.SampleRate()))
	var pcm []float32
	var rate int
	err := s.run(r.Context(), s.ttsSlot, s.cfg.TTSTimeout, func(ctx context.Context) error {
		var err error
		pcm, rate, err = e.tts.Synthesize(ctx, text, lang, sid, float32(speed), maxSamples)
		return err
	})
	if err != nil {
		s.runErr(w, err)
		return
	}
	wav := audio.EncodeMono16(pcm, rate)
	secs := float64(len(pcm)) / float64(rate)
	elapsed := time.Since(start)
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(wav)))
	w.Header().Set("X-Audio-Secs", strconv.FormatFloat(round3(secs), 'f', -1, 64))
	w.Header().Set("X-Sample-Rate", strconv.Itoa(rate))
	w.Header().Set("X-Latency-Ms", strconv.FormatInt(elapsed.Milliseconds(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(wav)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
