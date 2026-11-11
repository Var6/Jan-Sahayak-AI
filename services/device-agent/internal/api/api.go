// Package api is the device agent's loopback HTTP API for the citizen kiosk
// UI (docs/API_DOCUMENTATION.md §1) plus the static UI itself.
//
// Security model: the server listens on 127.0.0.1 only; every request must
// carry a loopback Host header (DNS-rebinding protection); mutating requests
// need `X-JS-Kiosk: 1` and pass net/http's CrossOriginProtection (CSRF);
// bodies are capped (16 KiB JSON, 1 MiB audio); every handler has a timeout.
//
// Privacy: audio is processed in memory and never written to disk;
// synthesised replies live in a bounded RAM LRU dropped at session end;
// request logs carry the route pattern, status and latency — never
// transcripts, typed text or audio. Analytics events written to the outbox
// carry only enumerated fields (internal/deviceapi), in the same SQLite
// transaction as the session row update they describe.
//
// Degraded operation (each capability is reported in GET /status and the
// SSE `health` event so the UI can adapt):
//
//   - STT down: capabilities.voice_input=false, POST /listen and /audio
//     return 503 `unavailable`; text and option input keep working.
//   - TTS down: replies are returned without audio_url (text on screen).
//     The same holds per session for a language the TTS voice does not
//     speak (English with the Hindi-only voice): voice_output=false.
//   - LLM down: the dialogue engine answers deterministically (the LLM is
//     only ever used for classification hints and validated rephrasing).
//   - Database down: the agent cannot record sessions or analytics safely,
//     so the device goes REQUIRES_ATTENTION and POST /session/start (and
//     the physical button) are refused with 503 `unavailable`. A session
//     already in progress may finish; its analytics writes then fail and
//     are logged (the outbox itself lives in that database).
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/ai"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/dialogue"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/session"
)

// Health component names the API consults for capabilities.
const (
	CompDatabase = "database"
	CompSTT      = "stt"
	CompTTS      = "tts"
	CompLLM      = "llm"
	// CompOutbox is critical: a full outbox means new analytics events are
	// refused, which needs a technician (REQUIRES_ATTENTION).
	CompOutbox = "outbox"
)

// Fixed health reason codes. Health details may leave the device (central
// heartbeat), so they are never free text: errors go to the local log.
const (
	ReasonSTTFailed     = "stt_request_failed"
	ReasonSTTBusy       = "stt_busy_repeated"
	ReasonTTSFailed     = "tts_request_failed"
	ReasonTTSBusy       = "tts_busy_repeated"
	ReasonDBWriteFailed = "db_write_failed"
	ReasonOutboxFull    = "outbox_full"
)

// DefaultTTSMinBudget is Limits.TTSMinBudget's default. A turn can run for
// TurnTimeout + TTSMinBudget, so session.Timeouts.MaxProcessing must exceed
// that (main sets it).
const DefaultTTSMinBudget = 10 * time.Second

// busyFaultAfter: this many consecutive "engine busy" answers from a speech
// engine count as a fault (a hung engine answers busy forever).
const busyFaultAfter = 3

// Limits bound the API. See internal/config for defaults and ranges.
type Limits struct {
	MaxJSONBytes    int64
	MaxAudioBytes   int64
	MaxTextRunes    int
	MaxAudioSecs    float64
	TurnTimeout     time.Duration
	ControlTimeout  time.Duration
	StartTimeout    time.Duration
	ReadTimeout     time.Duration
	SSEMaxClients   int
	SSEPing         time.Duration
	SSEMaxLifetime  time.Duration
	AudioCacheClips int
	AudioCacheBytes int64
	GoodbyeGrace    time.Duration
	// TTSMinBudget is the least time speech synthesis gets for a reply,
	// even when the turn used most of TurnTimeout (default 10 s).
	TTSMinBudget time.Duration
	// StuckAfter: a session in PROCESSING/SPEAKING without activity for
	// this long is abandoned; a button press starts a fresh one (2 min).
	StuckAfter time.Duration
	// AbandonAfter: same for READY/LISTENING (default 9 min, the final
	// warning; the session closes at 10 min anyway).
	AbandonAfter time.Duration
}

// Deps are the collaborators wired by main.
type Deps struct {
	Sessions *session.Manager
	Engine   dialogue.Engine
	STT      ai.STT // nil: no voice input on this device
	TTS      ai.TTS // nil: no voice output on this device
	// TTSLangs are the languages the TTS voice speaks (default hi).
	TTSLangs []string
	Health   *health.Monitor
	DB       *sql.DB
	Outbox   *outbox.Outbox
	// Kick asks the sync worker to upload now (called after session end).
	Kick func()
	// SimButton enables POST /session/start (PC only).
	SimButton bool
	DeviceID  string
	Version   string
	// DefaultLang is used when a session starts without a language.
	DefaultLang string
	// Listen is the host:port the server is bound to (for the Host check).
	Listen string
	// UI serves the kiosk's static files at "/" (nil: 404).
	UI http.Handler
	// HealthExtra adds technician data to GET /health (no citizen data).
	HealthExtra func(ctx context.Context) map[string]any
	Log         *slog.Logger
	Limits      Limits
	Now         func() time.Time
	// Documents enables document assistance (documents.go); nil = off.
	Documents *DocumentDeps
}

// Server implements the local API.
type Server struct {
	d     Deps
	log   *slog.Logger
	audio *audioCache
	now   func() time.Time

	mu    sync.Mutex
	convo *convo // the active session's conversation (nil when idle)

	// startMu serialises session start with the SSE stream's lookup of the
	// greeting for a session.started event.
	startMu sync.Mutex
	greet   *greeting

	sseMu      sync.Mutex
	sseClients map[*sseClient]struct{}

	closing   chan struct{}
	closeOnce sync.Once

	buttonBusy atomic.Bool
	work       inflight

	// consecutive "busy" answers per speech engine (reset on success)
	sttBusyRun, ttsBusyRun atomic.Int32
	// ttsNoLang records languages the TTS worker refused at runtime.
	ttsNoLangMu sync.Mutex
	ttsNoLang   map[string]bool

	// counters for GET /health
	turns, sttErrors, ttsErrors, recordErrors atomic.Int64
	// outboxRefused counts events not stored because the outbox was full;
	// it also rate-limits that log line (a busy kiosk would otherwise write
	// one ERROR per turn and push useful history out of journald).
	outboxRefused atomic.Int64
}

// convo is the per-session conversation memory. It lives only in RAM and
// is dropped when the session ends.
type convo struct {
	id        string
	startedAt time.Time

	// turnMu admits one turn at a time; st is guarded by it.
	turnMu sync.Mutex
	st     *dialogue.State

	seq   atomic.Int32 // dialogue turns handled
	clipN atomic.Int32 // next reply-audio clip number
	// lastClip is the length of the reply audio speak() just cached
	// (guarded by turnMu); it sizes the SPEAKING cap.
	lastClip time.Duration

	// recMu orders the analytics writes of a session (turns, end).
	recMu sync.Mutex

	// guarded by Server.mu
	lang          string
	recorded      bool     // the sessions row was written
	ended         bool     // the session ended (no more turn records)
	pendingListen bool     // after SPEAKING → LISTENING
	pendingEnd    bool     // after SPEAKING → end session
	doc           docState // document check results (documents.go)
}

type greeting struct {
	sid        string
	fromButton bool
	done       chan struct{}
	reply      *dialogue.Reply
	audioURL   string
}

// New builds the server and registers its session-end hook.
func New(d Deps) (*Server, error) {
	if d.Sessions == nil || d.Engine == nil || d.Health == nil || d.DB == nil || d.Outbox == nil {
		return nil, errors.New("api: Sessions, Engine, Health, DB and Outbox are required")
	}
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.DefaultLang != "en" {
		d.DefaultLang = "hi"
	}
	l := &d.Limits
	setDef := func(p *time.Duration, v time.Duration) {
		if *p <= 0 {
			*p = v
		}
	}
	if l.MaxJSONBytes <= 0 {
		l.MaxJSONBytes = 16 << 10
	}
	if l.MaxAudioBytes <= 0 {
		l.MaxAudioBytes = 1 << 20
	}
	if l.MaxTextRunes <= 0 || l.MaxTextRunes > dialogue.MaxInputRunes {
		l.MaxTextRunes = dialogue.MaxInputRunes
	}
	if l.MaxAudioSecs <= 0 {
		l.MaxAudioSecs = 20
	}
	setDef(&l.TurnTimeout, 55*time.Second)
	setDef(&l.ControlTimeout, 5*time.Second)
	setDef(&l.StartTimeout, 15*time.Second)
	setDef(&l.ReadTimeout, 30*time.Second)
	setDef(&l.SSEPing, 15*time.Second)
	setDef(&l.SSEMaxLifetime, 30*time.Minute)
	if l.SSEMaxClients <= 0 || l.SSEMaxClients > 8 {
		l.SSEMaxClients = 4
	}
	if l.AudioCacheClips <= 0 || l.AudioCacheClips > 8 {
		l.AudioCacheClips = 8
	}
	if l.AudioCacheBytes <= 0 {
		l.AudioCacheBytes = 24 << 20
	}
	setDef(&l.TTSMinBudget, DefaultTTSMinBudget)
	setDef(&l.StuckAfter, 2*time.Minute)
	setDef(&l.AbandonAfter, 9*time.Minute)
	if len(d.TTSLangs) == 0 {
		d.TTSLangs = []string{"hi"}
	}
	s := &Server{
		d: d, log: d.Log, now: d.Now,
		audio:      newAudioCache(l.AudioCacheClips, l.AudioCacheBytes, d.Now),
		sseClients: map[*sseClient]struct{}{},
		closing:    make(chan struct{}),
		ttsNoLang:  map[string]bool{},
	}
	d.Sessions.OnEnd(s.onSessionEnd)
	return s, nil
}

// Close ends all SSE streams (call from http.Server.RegisterOnShutdown).
func (s *Server) Close() { s.closeOnce.Do(func() { close(s.closing) }) }

// ----------------------------------------------------------------- routing

// Handler returns the full HTTP handler (API + UI) with all middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	p := "/api/v1"
	mux.HandleFunc("GET "+p+"/status", s.handleStatus)
	mux.HandleFunc("GET "+p+"/health", s.handleHealth)
	mux.HandleFunc("GET "+p+"/events", s.handleEvents)
	mux.HandleFunc("POST "+p+"/session/start", s.handleStart)
	mux.HandleFunc("POST "+p+"/session/{id}/listen", s.handleListen)
	mux.HandleFunc("POST "+p+"/session/{id}/audio", s.handleAudio)
	mux.HandleFunc("POST "+p+"/session/{id}/text", s.handleText)
	mux.HandleFunc("POST "+p+"/session/{id}/option", s.handleOption)
	mux.HandleFunc("POST "+p+"/session/{id}/speaking-done", s.handleSpeakingDone)
	mux.HandleFunc("POST "+p+"/session/{id}/language", s.handleLanguage)
	mux.HandleFunc("POST "+p+"/session/{id}/greeting", s.handleGreeting)
	mux.HandleFunc("POST "+p+"/session/{id}/touch", s.handleTouch)
	mux.HandleFunc("POST "+p+"/session/{id}/end", s.handleEnd)
	mux.HandleFunc("GET "+p+"/session/{id}/audio/{file}", s.handleClip)
	s.registerDocumentRoutes(mux, p)
	mux.HandleFunc(p+"/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "bad_request", "unknown endpoint")
	})
	if s.d.UI != nil {
		mux.Handle("/", s.d.UI)
	}

	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusForbidden, "bad_request", "cross-origin request refused")
	}))
	var h http.Handler = mux
	h = s.kioskHeader(h)
	h = cop.Handler(h)
	h = s.hostCheck(h)
	h = s.recoverer(h)
	h = s.logRequests(h)
	h = s.track(h)
	return h
}

// track counts in-flight requests for Drain.
func (s *Server) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.work.add() {
			writeErr(w, http.StatusServiceUnavailable, "unavailable", "device agent is shutting down")
			return
		}
		defer s.work.done()
		next.ServeHTTP(w, r)
	})
}

// hostCheck allows only loopback Host headers for our port (DNS rebinding).
func (s *Server) hostCheck(next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(s.d.Listen)
	allowed := map[string]bool{
		strings.ToLower(s.d.Listen): true,
		"127.0.0.1:" + port:         true,
		"localhost:" + port:         true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			writeErr(w, http.StatusForbidden, "bad_request", "host not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// kioskHeader requires X-JS-Kiosk: 1 on mutating API requests.
func (s *Server) kioskHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if strings.HasPrefix(r.URL.Path, "/api/") && r.Header.Get("X-JS-Kiosk") != "1" {
				writeErr(w, http.StatusForbidden, "bad_request", "missing X-JS-Kiosk header")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				s.log.Error("api: handler panic", "pattern", r.Pattern, "panic", fmt.Sprint(p))
				writeErr(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusWriter records the status for logging; Unwrap keeps
// http.ResponseController (Flush, deadlines) working.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
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
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests logs the matched route pattern (never the raw path, which
// would contain the session id, and never bodies).
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now() // wall clock: deadlines are real time
		sw := &statusWriter{ResponseWriter: w}
		// Bound reading of headers+body for every request; long handlers
		// extend this after the body is read (see extendDeadlines).
		_ = http.NewResponseController(sw).SetReadDeadline(start.Add(s.d.Limits.ReadTimeout))
		next.ServeHTTP(sw, r)
		route := r.Pattern
		if route == "" {
			route = "(unmatched)"
		}
		lvl := slog.LevelDebug
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			lvl = slog.LevelInfo
		}
		if sw.status >= 500 {
			lvl = slog.LevelWarn
		}
		s.log.Log(r.Context(), lvl, "http", "method", r.Method, "route", route, "status", sw.status,
			"ms", time.Since(start).Milliseconds(), "bytes", sw.bytes)
	})
}

// extendDeadlines lets a long handler (turns, SSE) outlive the default read
// deadline. Without this, net/http's background read would hit the read
// deadline and cancel the request context mid-turn.
func extendDeadlines(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	t := time.Now().Add(d)
	_ = rc.SetReadDeadline(t)
	_ = rc.SetWriteDeadline(t)
}

// ----------------------------------------------------------------- errors

type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.code + ": " + e.msg }

func errf(status int, code, format string, a ...any) *apiError {
	return &apiError{status: status, code: code, msg: fmt.Sprintf(format, a...)}
}

var errUnavailableDB = errf(http.StatusServiceUnavailable, "unavailable",
	"the device cannot record sessions safely right now (database unavailable); please inform the operator")

// sessionErr maps session.Manager errors to API errors.
func sessionErr(err error) *apiError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, session.ErrNoSession):
		return errf(http.StatusNotFound, "no_session", "no active session")
	case errors.Is(err, session.ErrWrongSession):
		return errf(http.StatusConflict, "wrong_session", "session id does not match the active session")
	case errors.Is(err, session.ErrAlreadyActive):
		return errf(http.StatusConflict, "bad_state", "a session is already active")
	case errors.Is(err, session.ErrBadTransition):
		return errf(http.StatusConflict, "bad_state", "%s", strings.TrimPrefix(err.Error(), "session: "))
	default:
		return errf(http.StatusInternalServerError, "internal", "session error")
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func writeAPIErr(w http.ResponseWriter, e *apiError) { writeErr(w, e.status, e.code, e.msg) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		b = []byte(`{"error":{"code":"internal","message":"encode failed"}}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// decodeJSON reads a bounded JSON body into v, rejecting unknown fields.
// An empty body is accepted when allowEmpty.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) *apiError {
	r.Body = http.MaxBytesReader(w, r.Body, s.d.Limits.MaxJSONBytes)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errf(http.StatusRequestEntityTooLarge, "too_large", "JSON body exceeds %d bytes", s.d.Limits.MaxJSONBytes)
		}
		return errf(http.StatusBadRequest, "bad_request", "could not read body")
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		if allowEmpty {
			return nil
		}
		return errf(http.StatusBadRequest, "bad_request", "JSON body required")
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return errf(http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errf(http.StatusBadRequest, "bad_request", "invalid JSON body")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errf(http.StatusBadRequest, "bad_request", "trailing data after JSON body")
	}
	return nil
}

// ----------------------------------------------------------------- capabilities

// Capabilities is what the UI may offer right now.
type Capabilities struct {
	VoiceInput  bool `json:"voice_input"`
	VoiceOutput bool `json:"voice_output"`
	LLM         bool `json:"llm"`
	SimButton   bool `json:"sim_button"`
	Camera      bool `json:"camera"`
}

func (s *Server) voiceInput() bool  { return s.d.STT != nil && s.d.Health.IsUsable(CompSTT) }
func (s *Server) voiceOutput() bool { return s.d.TTS != nil && s.d.Health.IsUsable(CompTTS) }
func (s *Server) dbUsable() bool    { return s.d.Health.IsUsable(CompDatabase) }

// ttsSpeaks reports whether the TTS voice speaks lang ("" = any).
func (s *Server) ttsSpeaks(lang string) bool {
	if lang == "" {
		return true
	}
	s.ttsNoLangMu.Lock()
	refused := s.ttsNoLang[lang]
	s.ttsNoLangMu.Unlock()
	if refused {
		return false
	}
	for _, l := range s.d.TTSLangs {
		if l == lang {
			return true
		}
	}
	return false
}

// voiceOutputFor: voice output is usable and speaks lang.
func (s *Server) voiceOutputFor(lang string) bool { return s.voiceOutput() && s.ttsSpeaks(lang) }

// Capabilities returns the current capabilities. voice_output follows the
// active session's language (an English session with a Hindi-only voice
// has no voice output).
func (s *Server) Capabilities() Capabilities {
	return Capabilities{
		VoiceInput: s.voiceInput(), VoiceOutput: s.voiceOutputFor(s.d.Sessions.Current().Language),
		LLM: s.d.Health.IsUsable(CompLLM), SimButton: s.d.SimButton, Camera: s.camera(),
	}
}
