package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/ai"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/dialogue"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/session"
)

func listen(t *testing.T, h *harness, sid string) {
	t.Helper()
	if cur := h.sm.Current(); cur.Activity == session.Speaking {
		if w := h.post("/api/v1/session/"+sid+"/speaking-done", nil); w.Code != 200 {
			t.Fatalf("speaking-done: %d %s", w.Code, w.Body)
		}
	}
	if w := h.post("/api/v1/session/"+sid+"/listen", nil); w.Code != 200 {
		t.Fatalf("listen: %d %s", w.Code, w.Body)
	}
}

func TestTranscriptIsRedactedOnScreen(t *testing.T) {
	h := newHarness(t, hopts{})
	sid := h.start().Session.ID
	listen(t, h, sid)
	h.stt.set("मेरा आधार नंबर 1234 5678 9012 है", nil)
	w := h.req(http.MethodPost, "/api/v1/session/"+sid+"/audio", makeWAV(16000, 1), "audio/wav", nil)
	if w.Code != 200 {
		t.Fatalf("audio: %d %s", w.Code, w.Body)
	}
	res := decode[TurnResult](t, w)
	if res.Transcript == nil || strings.Contains(res.Transcript.Text, "9012") || strings.Contains(w.Body.String(), "5678") {
		t.Fatalf("transcript not redacted: %s", w.Body)
	}
}

func TestGreetingEndpointAfterLanguageChange(t *testing.T) {
	h := newHarness(t, hopts{})
	res := h.start()
	sid := res.Session.ID
	if w := h.post("/api/v1/session/"+sid+"/language", map[string]string{"language": "en"}); w.Code != 200 {
		t.Fatalf("language: %d", w.Code)
	}
	w := h.post("/api/v1/session/"+sid+"/greeting", nil)
	if w.Code != 200 {
		t.Fatalf("greeting: %d %s", w.Code, w.Body)
	}
	g := decode[ReplyResult](t, w)
	if g.Reply.Kind != dialogue.KindGreeting || g.Reply.Text == res.Reply.Text {
		t.Fatalf("want an English greeting, got %+v", g.Reply)
	}
	// English with the Hindi-only voice: no audio, voice_output=false.
	if g.AudioURL != "" || h.srv.Capabilities().VoiceOutput {
		t.Fatalf("English greeting must have no audio (got %q, voice_output=%v)", g.AudioURL, h.srv.Capabilities().VoiceOutput)
	}
	for _, txt := range h.tts.texts {
		if txt == g.Reply.Text || txt == g.Reply.Speech {
			t.Fatal("TTS was asked to speak the English greeting")
		}
	}
	// Back to Hindi: greeting with audio again.
	h.post("/api/v1/session/"+sid+"/language", map[string]string{"language": "hi"})
	g = decode[ReplyResult](t, h.post("/api/v1/session/"+sid+"/greeting", nil))
	if g.AudioURL == "" || !h.srv.Capabilities().VoiceOutput {
		t.Fatalf("Hindi greeting should be spoken: %+v", g)
	}
	if w := h.post("/api/v1/session/wrong/greeting", nil); w.Code != http.StatusConflict {
		t.Fatalf("wrong session: %d", w.Code)
	}
}

func TestTurnAfterSessionEndedIsNotRecorded(t *testing.T) {
	h := newHarness(t, hopts{})
	sid := h.start().Session.ID
	c, e := h.srv.lookup(sid)
	if e != nil {
		t.Fatal(e)
	}
	if _, err := h.sm.End(sid, session.EndCitizen); err != nil {
		t.Fatal(err)
	}
	h.srv.recordTurn(context.Background(), c, dialogue.FromText, dialogue.Reply{}, "hi")
	var turns int
	for _, r := range h.outbox() {
		if r.typ == "dialogue.turn" {
			turns++
		}
		if r.typ == "session.ended" && !strings.Contains(r.payload, `"turns":0`) {
			t.Fatalf("session.ended: %s", r.payload)
		}
	}
	if turns != 0 {
		t.Fatalf("%d dialogue.turn events recorded after session.ended", turns)
	}
}

func TestSessionTimestampsRoundedToMinute(t *testing.T) {
	h := newHarness(t, hopts{})
	h.advance(37*time.Second + 123*time.Millisecond)
	sid := h.start().Session.ID
	h.advance(5 * time.Minute)
	if w := h.post("/api/v1/session/"+sid+"/end", nil); w.Code != 200 {
		t.Fatalf("end: %d", w.Code)
	}
	for _, r := range h.outbox() {
		if r.typ != "session.ended" {
			continue
		}
		var ev struct{ StartedAt, EndedAt string }
		if err := json.Unmarshal([]byte(r.payload), &struct {
			S *string `json:"started_at"`
			E *string `json:"ended_at"`
		}{&ev.StartedAt, &ev.EndedAt}); err != nil {
			t.Fatal(err)
		}
		if ev.StartedAt != "2026-11-09T10:00:00Z" || ev.EndedAt != "2026-11-09T10:05:00Z" {
			t.Fatalf("timestamps not rounded to the minute: %s", r.payload)
		}
		var row string
		if err := h.st.DB.QueryRow(`SELECT started_at FROM sessions WHERE id = ?`, sid).Scan(&row); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(row, "2026-11-09T10:00:00.000000000") {
			t.Fatalf("sessions.started_at %q", row)
		}
		return
	}
	t.Fatal("no session.ended event")
}

func TestOutboxFullReportsRequiresAttention(t *testing.T) {
	h := newHarness(t, hopts{})
	h.mon.Register(health.Component{Name: CompOutbox, Critical: true, Check: func(context.Context) health.Result {
		return health.Result{State: health.Healthy}
	}})
	h.mon.CheckAll(context.Background())
	h.srv.recordResult("turn", errorsJoinFull())
	if s, _ := h.mon.Get(CompOutbox); s.State != health.RequiresAttention || s.Detail != ReasonOutboxFull {
		t.Fatalf("outbox component: %+v", s)
	}
	if h.mon.Snapshot().Overall != health.RequiresAttention {
		t.Fatalf("overall %s", h.mon.Snapshot().Overall)
	}
}

// A full outbox under load logs a bounded number of lines, while every
// refusal is still counted.
func TestOutboxFullLogIsRateLimited(t *testing.T) {
	h := newHarness(t, hopts{})
	var buf bytes.Buffer
	h.srv.log = slog.New(slog.NewJSONHandler(&buf, nil))
	for i := 0; i < 2500; i++ {
		h.srv.recordResult("turn", errorsJoinFull())
	}
	if n := strings.Count(buf.String(), "outbox full"); n != 5 { // 1, 10, 100, 1000, 2000
		t.Fatalf("%d log lines for 2500 refusals, want 5", n)
	}
	if h.srv.outboxRefused.Load() != 2500 || h.srv.recordErrors.Load() != 2500 {
		t.Fatalf("counters: refused %d record_errors %d", h.srv.outboxRefused.Load(), h.srv.recordErrors.Load())
	}
}

func TestRepeatedSTTBusyIsAFault(t *testing.T) {
	h := newHarness(t, hopts{})
	sid := h.start().Session.ID
	h.stt.set("", ai.ErrBusy)
	for i := 1; i <= busyFaultAfter; i++ {
		listen(t, h, sid)
		w := h.req(http.MethodPost, "/api/v1/session/"+sid+"/audio", makeWAV(16000, 1), "audio/wav", nil)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("busy %d: %d", i, w.Code)
		}
		st, _ := h.mon.Get(CompSTT)
		if i < busyFaultAfter && st.State != health.Healthy {
			t.Fatalf("one busy answer (%d) marked STT %s", i, st.State)
		}
		if i == busyFaultAfter && (st.State != health.Unavailable || st.Detail != ReasonSTTBusy) {
			t.Fatalf("after %d busy answers STT is %+v", i, st)
		}
	}
}

// slowTTS honours ctx and takes delay to synthesise.
type slowTTS struct {
	fakeTTS
	delay time.Duration
}

func (s *slowTTS) Synthesize(ctx context.Context, text, lang string) ([]byte, error) {
	select {
	case <-time.After(s.delay):
		return s.fakeTTS.Synthesize(ctx, text, lang)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestTTSGetsItsOwnBudget(t *testing.T) {
	h := newHarness(t, hopts{})
	sid := h.start().Session.ID
	h.srv.d.TTS = &slowTTS{delay: 150 * time.Millisecond}
	h.srv.d.Limits.TTSMinBudget = time.Second
	c, _ := h.srv.lookup(sid)
	// A turn that has (almost) used up its time still gets audio.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	url := h.srv.speak(ctx, c, dialogue.Reply{Text: "नमस्ते"}, "hi")
	if url == "" {
		t.Fatal("no audio: TTS was limited to the turn's remaining time")
	}
	// TTS cut short by its own budget is not a runtime fault.
	h.srv.d.Limits.TTSMinBudget = 20 * time.Millisecond
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel2()
	if url := h.srv.speak(ctx2, c, dialogue.Reply{Text: "नमस्ते"}, "hi"); url != "" {
		t.Fatal("expected no audio")
	}
	if !h.mon.IsUsable(CompTTS) {
		t.Fatal("one slow reply marked TTS unavailable")
	}
	// The citizen leaving still cancels synthesis promptly.
	h.srv.d.Limits.TTSMinBudget = 5 * time.Second
	h.srv.d.TTS = &slowTTS{delay: 3 * time.Second}
	ctx3, cancel3 := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel3() }()
	start := time.Now()
	h.srv.speak(ctx3, c, dialogue.Reply{Text: "नमस्ते"}, "hi")
	if time.Since(start) > time.Second {
		t.Fatalf("cancelled synthesis took %s", time.Since(start))
	}
}

func TestSSEKioskSlotReservedAndEvicts(t *testing.T) {
	h := newHarness(t, hopts{})
	max := h.srv.d.Limits.SSEMaxClients
	var plain []*sseClient
	for i := 0; i < max-1; i++ {
		c, ok := h.srv.sseAcquire(false)
		if !ok {
			t.Fatalf("plain client %d refused", i)
		}
		plain = append(plain, c)
	}
	if _, ok := h.srv.sseAcquire(false); ok {
		t.Fatal("plain client took the kiosk's reserved slot")
	}
	k1, ok := h.srv.sseAcquire(true)
	if !ok {
		t.Fatal("kiosk refused")
	}
	// A reloaded kiosk page evicts its own stale stream first.
	k2, ok := h.srv.sseAcquire(true)
	if !ok {
		t.Fatal("second kiosk stream refused")
	}
	select {
	case <-k1.evicted:
	default:
		t.Fatal("old kiosk stream not evicted")
	}
	for _, c := range plain {
		select {
		case <-c.evicted:
			t.Fatal("plain stream evicted while an old kiosk stream existed")
		default:
		}
	}
	h.srv.sseRelease(k2)
	for _, c := range plain {
		h.srv.sseRelease(c)
	}
	if n := len(h.srv.sseClients); n != 0 {
		t.Fatalf("%d clients left", n)
	}
}

func TestPressButtonIsAsyncAndSingleFlight(t *testing.T) {
	h := newHarness(t, hopts{noSim: true})
	h.srv.PressButton(context.Background())
	h.srv.PressButton(context.Background()) // dropped while the first runs
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.srv.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if h.sm.Current().ID == "" {
		t.Fatal("button press started no session")
	}
	var rows int
	_ = h.st.DB.QueryRow(`SELECT count(*) FROM sessions`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("%d sessions started by two quick presses", rows)
	}
	// After Drain, presses are refused (shutdown).
	h.srv.PressButton(context.Background())
}

func TestButtonOnAbandonedSessionStartsFresh(t *testing.T) {
	h := newHarness(t, hopts{noSim: true})
	h.srv.ButtonPressed(context.Background())
	first := h.sm.Current()
	if first.ID == "" || first.Activity != session.Speaking {
		t.Fatalf("precondition: %+v", first)
	}
	// Soon after: the press counts as interaction.
	h.advance(30 * time.Second)
	h.srv.ButtonPressed(context.Background())
	if h.sm.Current().ID != first.ID {
		t.Fatal("recent session replaced")
	}
	// Left in SPEAKING for > StuckAfter: the next press starts afresh.
	h.advance(3 * time.Minute)
	h.srv.ButtonPressed(context.Background())
	cur := h.sm.Current()
	if cur.ID == "" || cur.ID == first.ID {
		t.Fatalf("abandoned session not replaced: %+v", cur)
	}
	var reason string
	if err := h.st.DB.QueryRow(`SELECT end_reason FROM sessions WHERE id = ?`, first.ID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != string(session.EndAbandoned) {
		t.Fatalf("end_reason %q", reason)
	}
	for _, r := range h.outbox() {
		if r.typ == "session.ended" && !strings.Contains(r.payload, `"end_reason":"inactivity_timeout"`) {
			t.Fatalf("abandoned session must use the wire reason: %s", r.payload)
		}
	}
}

func errorsJoinFull() error { return errors.Join(errors.New("insert"), outbox.ErrFull) }
