package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/ai"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/dialogue"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/session"
)

// optEnd is the dialogue engine's global "end conversation" option id
// (dialogue/flows.go). /end asks the engine for its goodbye with it so the
// wording stays in one place.
const optEnd = "cmd:end"

// DeviceStatus is GET /status.
type DeviceStatus struct {
	DeviceID     string           `json:"device_id"`
	Version      string           `json:"version"`
	Overall      health.State     `json:"overall"`
	Capabilities Capabilities     `json:"capabilities"`
	Session      session.Snapshot `json:"session"`
}

// ReplyResult is the body of /session/start and /session/{id}/end.
type ReplyResult struct {
	Session  session.Snapshot `json:"session"`
	Reply    dialogue.Reply   `json:"reply"`
	AudioURL string           `json:"audio_url,omitempty"`
}

// TranscriptJSON is shown on screen so the citizen can see what was heard.
type TranscriptJSON struct {
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	NoSpeech   bool    `json:"no_speech"`
}

// TurnResult is the body of /audio, /text and /option.
type TurnResult struct {
	Session    session.Snapshot `json:"session"`
	Transcript *TranscriptJSON  `json:"transcript,omitempty"`
	Reply      dialogue.Reply   `json:"reply"`
	AudioURL   string           `json:"audio_url,omitempty"`
}

type sessionResult struct {
	Session session.Snapshot `json:"session"`
}

func (s *Server) status() DeviceStatus {
	return DeviceStatus{
		DeviceID: s.d.DeviceID, Version: s.d.Version,
		Overall: s.d.Health.Snapshot().Overall, Capabilities: s.Capabilities(),
		Session: s.d.Sessions.Current(),
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.status())
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.d.Limits.ControlTimeout)
	defer cancel()
	snap := s.d.Health.Snapshot()
	clips, bytes := s.audio.Stats()
	out := map[string]any{
		"overall": snap.Overall, "components": snap.Components, "taken_at": snap.TakenAt,
		"device_id": s.d.DeviceID, "version": s.d.Version, "capabilities": s.Capabilities(),
		"api": map[string]any{
			"turns": s.turns.Load(), "stt_errors": s.sttErrors.Load(), "tts_errors": s.ttsErrors.Load(),
			"record_errors": s.recordErrors.Load(), "audio_cache_clips": clips, "audio_cache_bytes": bytes,
		},
	}
	if st, err := s.d.Outbox.Stats(ctx); err == nil {
		out["outbox"] = st
	} else {
		out["outbox_error"] = err.Error()
	}
	if s.d.HealthExtra != nil {
		for k, v := range s.d.HealthExtra(ctx) {
			if _, taken := out[k]; !taken {
				out[k] = v
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ----------------------------------------------------------------- session start / button

func validLang(l string) bool { return l == "hi" || l == "en" }

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if !s.d.SimButton {
		writeErr(w, http.StatusForbidden, "button_disabled", "sessions start from the physical button on this device")
		return
	}
	var body struct {
		Language string `json:"language"`
	}
	if e := s.decodeJSON(w, r, &body, true); e != nil {
		writeAPIErr(w, e)
		return
	}
	lang := body.Language
	if lang == "" {
		lang = s.d.DefaultLang
	}
	if !validLang(lang) {
		writeErr(w, http.StatusBadRequest, "bad_request", "language must be hi or en")
		return
	}
	extendDeadlines(w, s.d.Limits.StartTimeout+5*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), s.d.Limits.StartTimeout)
	defer cancel()
	res, e := s.startSession(ctx, lang, false, false)
	if e != nil {
		writeAPIErr(w, e)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ButtonPressed handles a physical button press synchronously: start a
// session (with greeting, delivered on the SSE session.started event) or,
// during a session, count as interaction — unless the open session looks
// abandoned, in which case it is replaced by a fresh one. main uses
// PressButton, which runs this on its own goroutine.
func (s *Server) ButtonPressed(ctx context.Context) {
	if !s.buttonBusy.CompareAndSwap(false, true) {
		return
	}
	defer s.buttonBusy.Store(false)
	s.buttonPress(ctx)
}

func (s *Server) buttonPress(ctx context.Context) {
	fresh := false
	if cur := s.d.Sessions.Current(); cur.ID != "" {
		if !s.abandoned(cur) {
			_ = s.d.Sessions.Touch(cur.ID)
			return
		}
		s.log.Info("api: button pressed on an abandoned session; starting a fresh one", "activity", string(cur.Activity))
		fresh = true
	}
	ctx, cancel := context.WithTimeout(ctx, s.d.Limits.StartTimeout)
	defer cancel()
	if _, e := s.startSession(ctx, s.d.DefaultLang, true, fresh); e != nil {
		s.log.Warn("api: button press did not start a session", "code", e.code, "reason", e.msg)
	}
}

// startSession opens a session, writes its row, greets and (if possible)
// speaks the greeting. fromButton marks physical-button starts, whose
// greeting is delivered on the SSE session.started event. fresh replaces
// an abandoned open session.
func (s *Server) startSession(ctx context.Context, lang string, fromButton, fresh bool) (ReplyResult, *apiError) {
	if !s.dbUsable() {
		return ReplyResult{}, errUnavailableDB
	}
	s.startMu.Lock()
	var snap session.Snapshot
	var err error
	if fresh {
		snap, err = s.startFresh(lang)
	} else {
		snap, err = s.d.Sessions.Start(lang)
	}
	if err != nil {
		s.startMu.Unlock()
		return ReplyResult{}, sessionErr(err)
	}
	now := s.now()
	c := &convo{id: snap.ID, startedAt: snap.StartedAt, lang: lang, st: dialogue.NewState(dialogue.Lang(lang), now)}
	c.turnMu.Lock() // no turn may run until the greeting is in place
	defer c.turnMu.Unlock()
	s.mu.Lock()
	s.convo = c
	s.mu.Unlock()
	g := &greeting{sid: snap.ID, fromButton: fromButton, done: make(chan struct{})}
	s.greet = g
	s.startMu.Unlock()
	defer close(g.done)
	s.audio.ClearGrace()

	if err := s.recordStart(ctx, c); err != nil {
		s.recordErrors.Add(1)
		s.log.Error("api: could not record session start; refusing session", "err", err)
		s.d.Health.Report(CompDatabase, health.Result{State: health.RequiresAttention, Detail: ReasonDBWriteFailed})
		_, _ = s.d.Sessions.End(snap.ID, session.EndError)
		return ReplyResult{}, errUnavailableDB
	}
	s.mu.Lock()
	c.recorded = true
	s.mu.Unlock()

	reply := s.d.Engine.Greet(dialogue.Lang(lang))
	last := reply
	c.st.Last = &last
	audioURL := s.speak(ctx, c, reply, lang)
	snap, aerr := s.afterReply(c, reply, audioURL)
	if aerr != nil {
		return ReplyResult{}, aerr
	}
	g.reply, g.audioURL = &reply, audioURL
	return ReplyResult{Session: snap, Reply: reply, AudioURL: audioURL}, nil
}

// waitGreeting returns the greeting for a just-started session (for the
// SSE session.started event of button-started sessions).
func (s *Server) waitGreeting(ctx context.Context, sid string) (*dialogue.Reply, string) {
	s.startMu.Lock()
	g := s.greet
	s.startMu.Unlock()
	if g == nil || g.sid != sid || !g.fromButton {
		return nil, ""
	}
	t := time.NewTimer(s.d.Limits.StartTimeout)
	defer t.Stop()
	select {
	case <-g.done:
		return g.reply, g.audioURL
	case <-ctx.Done():
	case <-t.C:
	case <-s.closing:
	}
	return nil, ""
}

// ----------------------------------------------------------------- lookup helpers

// lookup returns the conversation for id or the matching API error.
func (s *Server) lookup(id string) (*convo, *apiError) {
	s.mu.Lock()
	c := s.convo
	s.mu.Unlock()
	if c != nil && c.id == id {
		return c, nil
	}
	cur := s.d.Sessions.Current()
	if cur.ID == "" {
		return nil, sessionErr(session.ErrNoSession)
	}
	if cur.ID != id {
		return nil, sessionErr(session.ErrWrongSession)
	}
	// The manager knows the session but the conversation is not registered
	// yet (start in progress) or already dropped (ending).
	return nil, errf(http.StatusConflict, "bad_state", "session is starting or ending")
}

func (s *Server) convoLang(c *convo) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return c.lang
}

// ----------------------------------------------------------------- control endpoints

func (s *Server) handleListen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, e := s.lookup(id)
	if e != nil {
		writeAPIErr(w, e)
		return
	}
	// Not while the greeting or a turn is being prepared.
	if !c.turnMu.TryLock() {
		writeErr(w, http.StatusConflict, "bad_state", "busy; try again after the reply")
		return
	}
	defer c.turnMu.Unlock()
	if !s.voiceInput() {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "voice input is unavailable; please use the keyboard or options")
		return
	}
	cur := s.d.Sessions.Current()
	if cur.Activity != session.Ready && cur.Activity != session.Listening {
		writeErr(w, http.StatusConflict, "bad_state", "cannot listen while "+string(cur.Activity))
		return
	}
	snap, err := s.d.Sessions.SetActivity(id, session.Listening)
	if e := sessionErr(err); e != nil {
		writeAPIErr(w, e)
		return
	}
	writeJSON(w, http.StatusOK, sessionResult{Session: snap})
}

func (s *Server) handleSpeakingDone(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, e := s.lookup(id)
	if e != nil {
		writeAPIErr(w, e)
		return
	}
	if cur := s.d.Sessions.Current(); cur.Activity != session.Speaking {
		writeErr(w, http.StatusConflict, "bad_state", "not speaking ("+string(cur.Activity)+")")
		return
	}
	s.mu.Lock()
	listen, end := c.pendingListen, c.pendingEnd
	c.pendingListen, c.pendingEnd = false, false
	s.mu.Unlock()
	if end {
		if _, err := s.d.Sessions.End(id, session.EndCitizen); err != nil {
			writeAPIErr(w, sessionErr(err))
			return
		}
		writeJSON(w, http.StatusOK, sessionResult{Session: s.d.Sessions.Current()})
		return
	}
	next := session.Ready
	if listen && s.voiceInput() {
		next = session.Listening
	}
	snap, err := s.d.Sessions.SetActivity(id, next)
	if e := sessionErr(err); e != nil {
		writeAPIErr(w, e)
		return
	}
	writeJSON(w, http.StatusOK, sessionResult{Session: snap})
}

func (s *Server) handleLanguage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, e := s.lookup(id)
	if e != nil {
		writeAPIErr(w, e)
		return
	}
	var body struct {
		Language string `json:"language"`
	}
	if e := s.decodeJSON(w, r, &body, false); e != nil {
		writeAPIErr(w, e)
		return
	}
	if !validLang(body.Language) {
		writeErr(w, http.StatusBadRequest, "bad_request", "language must be hi or en")
		return
	}
	if err := s.d.Sessions.SetLanguage(id, body.Language); err != nil {
		writeAPIErr(w, sessionErr(err))
		return
	}
	s.mu.Lock()
	c.lang = body.Language // the dialogue state picks it up at the next turn
	s.mu.Unlock()
	s.HealthChanged() // voice_output depends on the session language
	writeJSON(w, http.StatusOK, sessionResult{Session: s.d.Sessions.Current()})
}

// handleGreeting composes the greeting again in the session's current
// language (after the citizen changed language on the welcome screens) and
// speaks it. Body: none. Response: ReplyResult (session, reply, audio_url).
func (s *Server) handleGreeting(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, e := s.lookup(id)
	if e != nil {
		writeAPIErr(w, e)
		return
	}
	if !c.turnMu.TryLock() {
		writeErr(w, http.StatusConflict, "bad_state", "busy; try again after the reply")
		return
	}
	defer c.turnMu.Unlock()
	switch cur := s.d.Sessions.Current(); cur.Activity {
	case session.Processing:
		writeErr(w, http.StatusConflict, "bad_state", "cannot greet while "+string(cur.Activity))
		return
	case session.Listening:
		// The UI discards its recording; the greeting is spoken instead.
		if _, err := s.d.Sessions.SetActivity(id, session.Ready); err != nil {
			writeAPIErr(w, sessionErr(err))
			return
		}
	}
	extendDeadlines(w, s.d.Limits.StartTimeout+5*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), s.d.Limits.StartTimeout)
	defer cancel()
	lang := s.convoLang(c)
	reply := s.d.Engine.Greet(dialogue.Lang(lang))
	c.st.Lang = dialogue.Lang(lang)
	last := reply
	c.st.Last = &last
	audioURL := s.speak(ctx, c, reply, lang)
	snap, aerr := s.afterReply(c, reply, audioURL)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	writeJSON(w, http.StatusOK, ReplyResult{Session: snap, Reply: reply, AudioURL: audioURL})
}

func (s *Server) handleTouch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.d.Sessions.Touch(id); err != nil {
		writeAPIErr(w, sessionErr(err))
		return
	}
	writeJSON(w, http.StatusOK, sessionResult{Session: s.d.Sessions.Current()})
}

func (s *Server) handleEnd(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur := s.d.Sessions.Current()
	if cur.ID == "" {
		writeAPIErr(w, sessionErr(session.ErrNoSession))
		return
	}
	if cur.ID != id {
		writeAPIErr(w, sessionErr(session.ErrWrongSession))
		return
	}
	lang := cur.Language
	var clipN int
	if c, e := s.lookup(id); e == nil {
		lang = s.convoLang(c)
		clipN = int(c.clipN.Add(1) - 1)
	}
	if !validLang(lang) {
		lang = s.d.DefaultLang
	}
	extendDeadlines(w, s.d.Limits.StartTimeout+5*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), s.d.Limits.StartTimeout)
	defer cancel()
	// The goodbye comes from the engine on a throwaway state so it never
	// races with a turn that may still be running on the real one.
	tmp := dialogue.NewState(dialogue.Lang(lang), s.now())
	reply, err := s.d.Engine.Handle(ctx, tmp, dialogue.Input{Source: dialogue.FromOption, OptionID: optEnd, Lang: dialogue.Lang(lang)})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not compose goodbye")
		return
	}
	// End first (privacy: drops audio and conversation memory), then speak.
	if _, err := s.d.Sessions.End(id, session.EndCitizen); err != nil {
		writeAPIErr(w, sessionErr(err))
		return
	}
	res := ReplyResult{Session: s.d.Sessions.Current(), Reply: reply}
	if s.voiceOutputFor(lang) && s.d.Limits.GoodbyeGrace > 0 {
		if wav, ok := s.synthesize(ctx, reply, lang); ok && s.audio.PutGrace(id, clipN, wav, s.d.Limits.GoodbyeGrace) {
			res.AudioURL = clipURL(id, clipN)
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func clipURL(sid string, n int) string { return fmt.Sprintf("/api/v1/session/%s/audio/%d.wav", sid, n) }

func (s *Server) handleClip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	file := r.PathValue("file")
	num, ok := strings.CutSuffix(file, ".wav")
	n, err := strconv.Atoi(num)
	if !ok || err != nil || n < 0 || n > 100000 {
		writeErr(w, http.StatusNotFound, "bad_request", "clip not found")
		return
	}
	data, found := s.audio.Get(id, n)
	if !found {
		if cur := s.d.Sessions.Current(); cur.ID != id {
			writeErr(w, http.StatusNotFound, "no_session", "clip belongs to no active session")
			return
		}
		writeErr(w, http.StatusNotFound, "bad_request", "clip not found")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "audio/wav")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

// ----------------------------------------------------------------- turns

func (s *Server) handleText(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Text string `json:"text"`
	}
	if e := s.decodeJSON(w, r, &body, false); e != nil {
		writeAPIErr(w, e)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" || !utf8.ValidString(text) {
		writeErr(w, http.StatusBadRequest, "bad_request", "text is required")
		return
	}
	if utf8.RuneCountInString(text) > s.d.Limits.MaxTextRunes {
		writeErr(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("text exceeds %d characters", s.d.Limits.MaxTextRunes))
		return
	}
	s.turn(w, r, id, false, func(context.Context, *convo) (dialogue.Input, *TranscriptJSON, *apiError) {
		return dialogue.Input{Source: dialogue.FromText, Text: text}, nil, nil
	})
}

func (s *Server) handleOption(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		OptionID string `json:"option_id"`
	}
	if e := s.decodeJSON(w, r, &body, false); e != nil {
		writeAPIErr(w, e)
		return
	}
	opt := strings.TrimSpace(body.OptionID)
	if opt == "" || len(opt) > 200 {
		writeErr(w, http.StatusBadRequest, "bad_request", "option_id is required (≤ 200 bytes)")
		return
	}
	s.turn(w, r, id, false, func(context.Context, *convo) (dialogue.Input, *TranscriptJSON, *apiError) {
		return dialogue.Input{Source: dialogue.FromOption, OptionID: opt}, nil, nil
	})
}

func (s *Server) handleAudio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, e := s.lookup(id); e != nil {
		writeAPIErr(w, e)
		return
	}
	if !s.voiceInput() {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "voice input is unavailable; please use the keyboard or options")
		return
	}
	// The microphone is legitimate only while the agent says LISTENING.
	if err := s.d.Sessions.RequireListening(id); err != nil {
		writeAPIErr(w, sessionErr(err))
		return
	}
	switch ct := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); ct {
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
	default:
		writeErr(w, http.StatusUnsupportedMediaType, "bad_audio", "Content-Type must be audio/wav")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.d.Limits.MaxAudioBytes)
	wav, err := io.ReadAll(r.Body) // RAM only; never written to disk
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("audio exceeds %d bytes", s.d.Limits.MaxAudioBytes))
			return
		}
		writeErr(w, http.StatusBadRequest, "bad_audio", "could not read audio")
		return
	}
	info, err := ai.ParseWAV(wav)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_audio", "audio must be 16 kHz mono 16-bit PCM WAV")
		return
	}
	if info.Seconds > s.d.Limits.MaxAudioSecs {
		writeErr(w, http.StatusBadRequest, "bad_audio", fmt.Sprintf("audio longer than %.0f s", s.d.Limits.MaxAudioSecs))
		return
	}
	// requireListening: the session must still be LISTENING when the turn
	// starts (checked under the turn lock), not only before the upload.
	s.turn(w, r, id, true, func(ctx context.Context, c *convo) (dialogue.Input, *TranscriptJSON, *apiError) {
		tr, err := s.d.STT.Transcribe(ctx, wav, "hi") // IndicConformer is Hindi(+Hinglish)-only
		if err != nil {
			s.sttErrors.Add(1)
			if errors.Is(err, ai.ErrBadAudio) {
				s.sttBusyRun.Store(0)
				return dialogue.Input{}, nil, errf(http.StatusBadRequest, "bad_audio", "audio rejected by the recogniser")
			}
			switch {
			case errors.Is(err, ai.ErrBusy):
				// One busy answer is transient; a run of them means the
				// engine is stuck (the worker restarts itself at 3× its
				// timeout), so stop offering voice input meanwhile.
				if s.sttBusyRun.Add(1) >= busyFaultAfter {
					s.d.Health.Report(CompSTT, health.Result{State: health.Unavailable, Detail: ReasonSTTBusy})
				}
			case errors.Is(err, context.Canceled):
			default:
				s.d.Health.Report(CompSTT, health.Result{State: health.Unavailable, Detail: ReasonSTTFailed})
			}
			s.log.Warn("api: speech recognition failed", "err", trimErr(err))
			return dialogue.Input{}, nil, errf(http.StatusServiceUnavailable, "unavailable", "speech recognition is unavailable; please use the keyboard or options")
		}
		s.sttBusyRun.Store(0)
		// The screen shows what was heard with sensitive values masked
		// (a spoken Aadhaar number or OTP must not appear on the display
		// or in the HTTP response).
		return dialogue.Input{Source: dialogue.FromVoice, Text: tr.Text, STTConfidence: tr.Confidence, NoSpeech: tr.NoSpeech},
			&TranscriptJSON{Text: redactTranscript(tr.Text), Confidence: tr.Confidence, NoSpeech: tr.NoSpeech}, nil
	})
}

func trimErr(err error) string {
	m := err.Error()
	if len(m) > 200 {
		m = m[:200]
	}
	return m
}

type prepareFunc func(ctx context.Context, c *convo) (dialogue.Input, *TranscriptJSON, *apiError)

// turn runs one citizen turn: PROCESSING → (STT) → dialogue → (TTS) →
// record analytics → SPEAKING / LISTENING / READY. requireListening (audio
// turns) refuses the turn unless the session is LISTENING at that moment.
func (s *Server) turn(w http.ResponseWriter, r *http.Request, id string, requireListening bool, prepare prepareFunc) {
	c, e := s.lookup(id)
	if e != nil {
		writeAPIErr(w, e)
		return
	}
	if !c.turnMu.TryLock() {
		writeErr(w, http.StatusConflict, "bad_state", "a turn is already being processed")
		return
	}
	defer c.turnMu.Unlock()
	if e := s.toProcessing(id, requireListening); e != nil {
		writeAPIErr(w, e)
		return
	}
	// A panic anywhere below (engine, TTS client, analytics) must not leave
	// the session wedged in PROCESSING: return it to READY, then let the
	// recoverer middleware answer 500.
	defer func() {
		if p := recover(); p != nil {
			s.forceReady(id)
			panic(p)
		}
	}()
	extendDeadlines(w, s.d.Limits.TurnTimeout+s.d.Limits.TTSMinBudget+5*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), s.d.Limits.TurnTimeout)
	defer cancel()
	start := time.Now()

	in, transcript, perr := prepare(ctx, c)
	if perr != nil {
		_, _ = s.d.Sessions.SetActivity(id, session.Ready)
		writeAPIErr(w, perr)
		return
	}
	lang := s.convoLang(c)
	in.Lang = dialogue.Lang(lang)
	c.st.Lang = dialogue.Lang(lang) // picks up a /language change made between turns
	reply, err := s.d.Engine.Handle(ctx, c.st, in)
	if err != nil {
		s.log.Error("api: dialogue failed", "err", err)
		_, _ = s.d.Sessions.SetActivity(id, session.Ready)
		writeErr(w, http.StatusInternalServerError, "internal", "could not process the request")
		return
	}
	s.turns.Add(1)
	if nl := string(c.st.Lang); nl != lang && validLang(nl) {
		// The citizen asked to switch language inside the conversation.
		if err := s.d.Sessions.SetLanguage(id, nl); err == nil {
			s.mu.Lock()
			c.lang = nl
			s.mu.Unlock()
			lang = nl
		}
	}
	audioURL := s.speak(ctx, c, reply, lang)
	s.recordTurn(ctx, c, in.Source, reply, lang)
	snap, aerr := s.afterReply(c, reply, audioURL)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	s.log.Debug("api: turn", "source", string(in.Source), "kind", string(reply.Kind),
		"intent", reply.Analytics.Intent, "used_llm", reply.UsedLLM, "audio", audioURL != "",
		"ms", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, TurnResult{Session: snap, Transcript: transcript, Reply: reply, AudioURL: audioURL})
}

// toProcessing moves the session to PROCESSING. Text/option input is also
// accepted while LISTENING (the UI discards its recording) or SPEAKING
// (the citizen interrupted the reply); audio only while LISTENING. Called
// with the conversation's turn lock held, so no other turn, /listen or
// greeting can change the activity in between.
func (s *Server) toProcessing(id string, requireListening bool) *apiError {
	cur := s.d.Sessions.Current()
	if cur.Activity == session.Processing {
		return errf(http.StatusConflict, "bad_state", "a turn is already being processed")
	}
	if requireListening && cur.Activity != session.Listening {
		return errf(http.StatusConflict, "bad_state", "not listening (%s); audio discarded", cur.Activity)
	}
	if cur.Activity == session.Speaking {
		if _, err := s.d.Sessions.SetActivity(id, session.Ready); err != nil {
			return sessionErr(err)
		}
	}
	_, err := s.d.Sessions.SetActivity(id, session.Processing)
	return sessionErr(err)
}

// afterReply sets the activity that follows a reply and returns the
// snapshot to send back.
func (s *Server) afterReply(c *convo, reply dialogue.Reply, audioURL string) (session.Snapshot, *apiError) {
	var err error
	var snap session.Snapshot
	switch {
	case audioURL != "":
		s.mu.Lock()
		c.pendingListen, c.pendingEnd = reply.ListenNext, reply.EndSession
		s.mu.Unlock()
		// SPEAKING is capped by the session manager at max(clip, 2 min), so
		// a UI that never sends speaking-done cannot wedge the session.
		snap, err = s.d.Sessions.SetSpeaking(c.id, c.lastClip+5*time.Second)
	case reply.EndSession:
		_, err = s.d.Sessions.End(c.id, session.EndCitizen)
		snap = s.d.Sessions.Current()
	case reply.ListenNext && s.voiceInput():
		snap, err = s.d.Sessions.SetActivity(c.id, session.Listening)
	default:
		snap, err = s.d.Sessions.SetActivity(c.id, session.Ready)
	}
	if err != nil {
		// The session ended while we worked (citizen pressed End, or
		// shutdown): make sure nothing of it stays in memory.
		s.audio.DropSession(c.id)
		return snap, sessionErr(err)
	}
	return snap, nil
}

// synthesize runs TTS for a reply; ok=false means "no audio" (the reply is
// still shown on screen).
func (s *Server) synthesize(ctx context.Context, reply dialogue.Reply, lang string) ([]byte, bool) {
	if s.d.TTS == nil {
		return nil, false
	}
	text := reply.Speech
	if text == "" {
		text = reply.Text
	}
	// TTS gets its own budget: at least TTSMinBudget, even when the turn
	// already used most of its time, so one slow turn neither loses its
	// audio nor marks TTS UNAVAILABLE. The citizen leaving (End, reload)
	// still cancels it.
	tctx, cancel := ttsContext(ctx, s.d.Limits.TTSMinBudget)
	defer cancel()
	wav, err := s.d.TTS.Synthesize(tctx, text, lang)
	if err != nil {
		switch {
		case errors.Is(err, ai.ErrUnsupportedLanguage):
			// e.g. English with a Hindi-only voice: stop offering voice
			// output for this language (capabilities.voice_output=false).
			s.ttsNoLangMu.Lock()
			first := !s.ttsNoLang[lang]
			s.ttsNoLang[lang] = true
			s.ttsNoLangMu.Unlock()
			if first {
				s.HealthChanged()
			}
		case errors.Is(err, ai.ErrEmptyText):
		case errors.Is(err, ai.ErrBusy):
			s.ttsErrors.Add(1)
			if s.ttsBusyRun.Add(1) >= busyFaultAfter {
				s.d.Health.Report(CompTTS, health.Result{State: health.Unavailable, Detail: ReasonTTSBusy})
			}
		case errors.Is(err, context.Canceled), tctx.Err() != nil:
			// The citizen left, or this reply ran out of time: not a
			// runtime fault (the periodic health check judges the worker).
			s.ttsErrors.Add(1)
			s.log.Info("api: speech synthesis cut short", "err", trimErr(err))
		default:
			s.ttsErrors.Add(1)
			s.d.Health.Report(CompTTS, health.Result{State: health.Unavailable, Detail: ReasonTTSFailed})
			s.log.Warn("api: speech synthesis failed", "err", trimErr(err))
		}
		return nil, false
	}
	s.ttsBusyRun.Store(0)
	return wav, true
}

// ttsContext derives the synthesis context from a turn context: it is
// cancelled when the turn is cancelled (client gone), but its deadline is
// the later of the turn's deadline and now+min.
func ttsContext(parent context.Context, min time.Duration) (context.Context, context.CancelFunc) {
	d := min
	if dl, ok := parent.Deadline(); ok && time.Until(dl) > d {
		d = time.Until(dl)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), d)
	stop := context.AfterFunc(parent, func() {
		if errors.Is(parent.Err(), context.Canceled) {
			cancel()
		}
	})
	return ctx, func() { stop(); cancel() }
}

// speak synthesises a reply into the session's audio cache and returns its
// URL ("" when there is no audio).
func (s *Server) speak(ctx context.Context, c *convo, reply dialogue.Reply, lang string) string {
	if !s.voiceOutputFor(lang) {
		return ""
	}
	wav, ok := s.synthesize(ctx, reply, lang)
	if !ok {
		return ""
	}
	n := int(c.clipN.Add(1) - 1)
	if !s.audio.Put(c.id, n, wav) {
		return ""
	}
	c.lastClip = wavDuration(wav)
	return clipURL(c.id, n)
}
