// Command faultsoak runs a fault-injection soak against the dev stack: it
// owns the processes (central-api, llama-server, speech worker, device
// agent), drives continuous kiosk sessions through the agent's local API,
// injects faults on a schedule, samples resource use every -sample, and at
// the end waits for the outbox to drain and compares device-side and
// central-side event sets. Stdlib only; shells out to ps, lsof, sqlite3 and
// psql (all present on the dev Mac and on Pi OS).
//
//	go run ./scripts/dev/faultsoak -duration 60m \
//	  -agent-cmd   'bin/device-agent -config var/soak/device/soak.json' \
//	  -speech-cmd  'services/speech-worker/bin/speech-worker -models-dir models/weights' \
//	  -llama-cmd   'llama-server -m … --port 8742' \
//	  -central-cmd 'var/central/bin/central-api -addr 127.0.0.1:8750 -server-key var/central/server.key -cookie-secure=false' \
//	  -data-dir var/soak/device -device-id <uuid>
//
// Fault schedule (-faults): comma-separated kind@offset, kinds:
//
//	speech   SIGKILL the speech worker, restart after -speech-down
//	llama    SIGKILL llama-server, restart after -llama-down
//	central  SIGTERM central-api, restart after -central-down
//	agent    SIGKILL the agent between two turns of an open session,
//	         restart after -agent-down
//
// A fault whose process has no command is skipped (and reported). Output
// goes to <-out>/<run id>/: samples.csv, turns.csv, events.csv,
// summary.json, summary.txt and one log per owned process.
//
// The run exits 1 if events are missing centrally, the outbox did not drain,
// or the per-segment RSS/goroutine/FD trend exceeds -max-*-slope.
package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------------------------------------------------------------- processes

type proc struct {
	name, cmd, health, logPath string

	mu   sync.Mutex
	c    *exec.Cmd
	done chan struct{}
}

func (p *proc) owned() bool { return p.cmd != "" }

func (p *proc) pid() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.c == nil || p.c.Process == nil {
		return 0
	}
	select {
	case <-p.done:
		return 0
	default:
	}
	return p.c.Process.Pid
}

func (p *proc) start() error {
	if !p.owned() {
		return nil
	}
	lf, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	fmt.Fprintf(lf, "\n==== faultsoak start %s %s\n", p.name, time.Now().Format(time.RFC3339))
	c := exec.Command("/bin/sh", "-c", "exec "+p.cmd)
	c.Stdout, c.Stderr = lf, lf
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		lf.Close()
		return err
	}
	done := make(chan struct{})
	go func() { _ = c.Wait(); lf.Close(); close(done) }()
	p.mu.Lock()
	p.c, p.done = c, done
	p.mu.Unlock()
	return nil
}

// signal sends sig to the process group and waits up to wait for exit,
// escalating to SIGKILL.
func (p *proc) stop(sig syscall.Signal, wait time.Duration) {
	p.mu.Lock()
	c, done := p.c, p.done
	p.mu.Unlock()
	if c == nil || c.Process == nil {
		return
	}
	_ = syscall.Kill(-c.Process.Pid, sig)
	select {
	case <-done:
		return
	case <-time.After(wait):
	}
	_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

func waitHTTP(ctx context.Context, url string, max time.Duration) (time.Duration, bool) {
	t0 := time.Now()
	hc := &http.Client{Timeout: 2 * time.Second}
	for time.Since(t0) < max && ctx.Err() == nil {
		resp, err := hc.Get(url)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return time.Since(t0), true
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return time.Since(t0), false
}

// ---------------------------------------------------------------- agent API

type apiClient struct {
	base string
	hc   *http.Client
}

type apiResp struct {
	code int
	body map[string]any
	el   time.Duration
	err  error
}

func (r apiResp) errKey(op string) string {
	if r.err != nil {
		var ne net.Error
		switch {
		case errors.As(r.err, &ne) && ne.Timeout():
			return op + " timeout"
		case strings.Contains(r.err.Error(), "connection refused"):
			return op + " conn_refused"
		default:
			return op + " net_error"
		}
	}
	code := ""
	if e, ok := r.body["error"].(map[string]any); ok {
		code, _ = e["code"].(string)
	}
	return fmt.Sprintf("%s %d %s", op, r.code, code)
}

func (c *apiClient) do(method, path, ctype string, body []byte) apiResp {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if method == http.MethodPost {
		req.Header.Set("X-JS-Kiosk", "1")
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	t0 := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return apiResp{err: err, el: time.Since(t0)}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	r := apiResp{code: resp.StatusCode, el: time.Since(t0)}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(b, &r.body)
	}
	return r
}

func (c *apiClient) postJSON(path string, v any) apiResp {
	var b []byte
	if v != nil {
		b, _ = json.Marshal(v)
		return c.do(http.MethodPost, path, "application/json", b)
	}
	return c.do(http.MethodPost, path, "", nil)
}

func activity(m map[string]any) string {
	s, _ := m["session"].(map[string]any)
	a, _ := s["activity"].(string)
	return a
}

func sessionID(m map[string]any) string {
	s, _ := m["session"].(map[string]any)
	id, _ := s["id"].(string)
	return id
}

// ---------------------------------------------------------------- state

type soak struct {
	cfg     config
	api     *apiClient
	procs   map[string]*proc
	runDir  string
	start   time.Time
	stopRun atomic.Bool

	mu          sync.Mutex
	errs        map[string]int
	textLat     []time.Duration // all text turns
	textLatCalm []time.Duration // text turns outside any fault window
	voiceLat    []time.Duration
	turnsOK     int // 200 turn responses whose session was still open (expected dialogue.turn events)
	endsOK      int // 200 /end responses (expected session.ended events)
	sessions    int
	lastTurnMS  int64
	activeFault string
	faultLog    []faultResult
	agentKill   *agentKillResult

	wantAgentKill atomic.Bool
	killPoint     chan string
	resume        chan struct{}

	turnsW, eventsW *csvFile
	baseline        int // outbox rows present before the run (earlier runs on the same data dir)
}

type faultResult struct {
	Kind       string  `json:"kind"`
	At         string  `json:"at"`
	Skipped    string  `json:"skipped,omitempty"`
	DownS      float64 `json:"down_s"`
	RecoveredS float64 `json:"recovered_after_restart_s"` // restart → recovered signal
	TotalS     float64 `json:"total_outage_s"`            // fault → recovered
	Recovered  bool    `json:"recovered"`
	Signal     string  `json:"signal"`
	Note       string  `json:"note,omitempty"`
}

type agentKillResult struct {
	SessionID          string `json:"session_id"`
	TurnsBeforeKill    int    `json:"turns_before_kill"`
	RowEndedAt         string `json:"row_ended_at"`
	RowEndReason       string `json:"row_end_reason"`
	RowTurns           int    `json:"row_turns"`
	TurnEventsInOutbox int    `json:"turn_events_in_outbox"`
	EndedEventInOutbox int    `json:"ended_event_in_outbox"`
	Verdict            string `json:"verdict"`
}

type csvFile struct {
	mu sync.Mutex
	f  *os.File
	w  *csv.Writer
}

func newCSV(path string, header []string) (*csvFile, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	c := &csvFile{f: f, w: csv.NewWriter(f)}
	c.write(header...)
	return c, nil
}

func (c *csvFile) write(rec ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.w.Write(rec)
	c.w.Flush()
}

func (c *csvFile) close() { c.mu.Lock(); c.w.Flush(); c.f.Close(); c.mu.Unlock() }

func (s *soak) elapsed() string {
	return strconv.FormatFloat(time.Since(s.start).Seconds(), 'f', 1, 64)
}

func (s *soak) event(kind, detail string) {
	s.eventsW.write(time.Now().UTC().Format(time.RFC3339), s.elapsed(), kind, detail)
	fmt.Printf("[%7ss] %-9s %s\n", s.elapsed(), kind, detail)
}

func (s *soak) countErr(key string) {
	s.mu.Lock()
	s.errs[key]++
	fault := s.activeFault
	s.mu.Unlock()
	s.eventsW.write(time.Now().UTC().Format(time.RFC3339), s.elapsed(), "error", key+" fault="+fault)
}

func (s *soak) setFault(f string) { s.mu.Lock(); s.activeFault = f; s.mu.Unlock() }

func (s *soak) fault() string { s.mu.Lock(); defer s.mu.Unlock(); return s.activeFault }

// ---------------------------------------------------------------- sessions

var texts = []string{
	"मुझे लोन चाहिए",
	"बुढ़ापा पेंशन के बारे में बताओ",
	"जाति प्रमाण पत्र कैसे बनेगा",
	"cricket score batao",
	"मेरी माँ की उम्र सत्तर साल है",
}

func (s *soak) recordTurn(kind string, r apiResp, fault string) {
	ms := r.el.Milliseconds()
	status := strconv.Itoa(r.code)
	if r.err != nil {
		status = "neterr"
	}
	s.turnsW.write(time.Now().UTC().Format(time.RFC3339), s.elapsed(), kind, status, strconv.FormatInt(ms, 10), fault)
	if r.err != nil || r.code != 200 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnsOK++
	s.lastTurnMS = ms
	if kind == "voice" {
		s.voiceLat = append(s.voiceLat, r.el)
		return
	}
	s.textLat = append(s.textLat, r.el)
	if fault == "" {
		s.textLatCalm = append(s.textLatCalm, r.el)
	}
}

// afterReply plays the reply clip (fetches it) and releases SPEAKING.
func (s *soak) afterReply(sid string, m map[string]any) {
	if au, _ := m["audio_url"].(string); au != "" {
		if r := s.api.do(http.MethodGet, au, "", nil); r.err != nil || r.code != 200 {
			s.countErr(r.errKey("clip"))
		}
	}
	if activity(m) == "SPEAKING" {
		if r := s.api.postJSON("/api/v1/session/"+sid+"/speaking-done", nil); r.err != nil || r.code != 200 {
			s.countErr(r.errKey("speaking-done"))
		}
	}
}

// endStale ends a session left open (e.g. after an error), so the next start works.
func (s *soak) endStale() {
	r := s.api.do(http.MethodGet, "/api/v1/status", "", nil)
	if r.err != nil || r.code != 200 {
		return
	}
	if sid := sessionID(r.body); sid != "" {
		if e := s.api.postJSON("/api/v1/session/"+sid+"/end", nil); e.code == 200 {
			s.mu.Lock()
			s.endsOK++
			s.mu.Unlock()
		}
	}
}

func (s *soak) loadClips() [][]byte {
	var clips [][]byte
	ents, _ := filepath.Glob(filepath.Join(s.cfg.audioDir, "*.wav"))
	sort.Strings(ents)
	for _, p := range ents {
		if b, err := os.ReadFile(p); err == nil && len(b) > 44 && len(b) <= 1<<20 {
			clips = append(clips, b)
		}
	}
	return clips
}

func (s *soak) sessionLoop(ctx context.Context) {
	clips := s.loadClips()
	if len(clips) == 0 && s.cfg.voiceEvery > 0 {
		s.event("warn", "no WAV clips in "+s.cfg.audioDir+"; voice turns disabled")
	}
	lastVoice := time.Now()
	ti, ci := 0, 0
	for ctx.Err() == nil && !s.stopRun.Load() {
		fault := s.fault()
		r := s.api.postJSON("/api/v1/session/start", map[string]string{"language": "hi"})
		if r.err != nil || r.code != 200 {
			s.countErr(r.errKey("start"))
			if r.code == 409 {
				s.endStale()
			}
			sleepCtx(ctx, 2*time.Second)
			continue
		}
		sid := sessionID(r.body)
		s.afterReply(sid, r.body)
		alive := true
		turnsThis := 0

		if len(clips) > 0 && s.cfg.voiceEvery > 0 && time.Since(lastVoice) >= s.cfg.voiceEvery {
			lastVoice = time.Now()
			l := s.api.postJSON("/api/v1/session/"+sid+"/listen", nil)
			if l.err != nil || l.code != 200 {
				s.countErr(l.errKey("listen"))
			} else {
				fault = s.fault()
				v := s.api.do(http.MethodPost, "/api/v1/session/"+sid+"/audio", "audio/wav", clips[ci%len(clips)])
				ci++
				s.recordTurn("voice", v, fault)
				if v.err != nil || v.code != 200 {
					s.countErr(v.errKey("audio"))
				} else {
					turnsThis++
					s.afterReply(sid, v.body)
				}
			}
		}

		for k := 0; k < s.cfg.turnsPerSession && alive && ctx.Err() == nil; k++ {
			sleepCtx(ctx, s.cfg.think)
			fault = s.fault()
			t := s.api.postJSON("/api/v1/session/"+sid+"/text", map[string]string{"text": texts[ti%len(texts)]})
			ti++
			s.recordTurn("text", t, fault)
			if t.err != nil || t.code != 200 {
				s.countErr(t.errKey("text"))
				if t.err != nil || t.code == 404 {
					alive = false
				}
				continue
			}
			turnsThis++
			s.afterReply(sid, t.body)
			if rep, _ := t.body["reply"].(map[string]any); rep != nil {
				if end, _ := rep["end_session"].(bool); end {
					alive = false
				}
			}
			// Agent-kill fault: hand the open session to the fault goroutine
			// between two turns, then abandon it (the agent is gone).
			if s.wantAgentKill.CompareAndSwap(true, false) {
				s.mu.Lock()
				s.agentKill = &agentKillResult{SessionID: sid, TurnsBeforeKill: turnsThis}
				s.mu.Unlock()
				select {
				case s.killPoint <- sid:
					<-s.resume // the fault always resumes us
					alive = false
					sid = ""
				case <-ctx.Done():
					s.mu.Lock()
					s.agentKill = nil
					s.mu.Unlock()
				}
			}
		}
		if sid != "" {
			e := s.api.postJSON("/api/v1/session/"+sid+"/end", nil)
			if e.err != nil || e.code != 200 {
				s.countErr(e.errKey("end"))
			} else {
				s.mu.Lock()
				s.endsOK++
				s.mu.Unlock()
			}
		}
		s.mu.Lock()
		s.sessions++
		s.mu.Unlock()
		sleepCtx(ctx, s.cfg.gap)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// ---------------------------------------------------------------- faults

type plannedFault struct {
	kind string
	at   time.Duration
}

func parseFaults(spec string) ([]plannedFault, error) {
	var out []plannedFault
	for _, f := range strings.Split(spec, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		k, at, ok := strings.Cut(f, "@")
		if !ok {
			return nil, fmt.Errorf("fault %q: want kind@offset", f)
		}
		d, err := time.ParseDuration(at)
		if err != nil {
			return nil, fmt.Errorf("fault %q: %v", f, err)
		}
		switch k {
		case "speech", "llama", "central", "agent":
		default:
			return nil, fmt.Errorf("fault %q: unknown kind", f)
		}
		out = append(out, plannedFault{k, d})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at < out[j].at })
	return out, nil
}

func (s *soak) status() (map[string]any, bool) {
	r := s.api.do(http.MethodGet, "/api/v1/status", "", nil)
	return r.body, r.err == nil && r.code == 200
}

func capability(st map[string]any, name string) bool {
	c, _ := st["capabilities"].(map[string]any)
	v, _ := c[name].(bool)
	return v
}

// waitCap polls the agent status until capability name is true.
func (s *soak) waitCap(ctx context.Context, name string, max time.Duration) (time.Duration, bool) {
	t0 := time.Now()
	for time.Since(t0) < max && ctx.Err() == nil {
		if st, ok := s.status(); ok && capability(st, name) {
			return time.Since(t0), true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return time.Since(t0), false
}

// faultLoop schedules faults on runCtx (no new fault after the run ends)
// but runs each one on ctx, so a fault in progress always restarts what it
// stopped.
func (s *soak) faultLoop(runCtx, ctx context.Context, plan []plannedFault) {
	for _, f := range plan {
		if d := time.Until(s.start.Add(f.at)); d > 0 {
			sleepCtx(runCtx, d)
		}
		if runCtx.Err() != nil || s.stopRun.Load() {
			return
		}
		res := faultResult{Kind: f.kind, At: time.Since(s.start).Round(time.Second).String()}
		if f.kind == "agent" {
			s.agentFault(runCtx, ctx, &res)
			s.mu.Lock()
			s.faultLog = append(s.faultLog, res)
			s.mu.Unlock()
			b, _ := json.Marshal(res)
			s.event("fault", string(b))
			continue
		}
		switch f.kind {
		case "speech":
			s.runtimeFault(ctx, &res, s.procs["speech"], s.cfg.speechDown, "voice_input")
		case "llama":
			s.runtimeFault(ctx, &res, s.procs["llama"], s.cfg.llamaDown, "llm")
		case "central":
			s.centralFault(ctx, &res)
		}
		s.mu.Lock()
		s.faultLog = append(s.faultLog, res)
		s.mu.Unlock()
		b, _ := json.Marshal(res)
		s.event("fault", string(b))
	}
}

func (s *soak) runtimeFault(ctx context.Context, res *faultResult, p *proc, down time.Duration, capName string) {
	if !p.owned() {
		res.Skipped = "no -" + p.name + "-cmd"
		return
	}
	s.setFault(res.Kind)
	defer s.setFault("")
	t0 := time.Now()
	s.event("inject", fmt.Sprintf("SIGKILL %s pid %d", p.name, p.pid()))
	p.stop(syscall.SIGKILL, 5*time.Second)
	if st, ok := s.status(); ok {
		res.Note = fmt.Sprintf("agent overall right after kill: %v", st["overall"])
	}
	sleepCtx(ctx, down)
	res.DownS = time.Since(t0).Seconds()
	if err := p.start(); err != nil {
		res.Note += "; restart failed: " + err.Error()
		return
	}
	t1 := time.Now()
	_, ok := waitHTTP(ctx, p.health, 5*time.Minute)
	if !ok {
		res.Note += "; runtime never became healthy"
		return
	}
	_, ok = s.waitCap(ctx, capName, 3*time.Minute)
	res.Recovered = ok
	res.RecoveredS = time.Since(t1).Seconds()
	res.TotalS = time.Since(t0).Seconds()
	res.Signal = "agent capabilities." + capName + "=true"
}

func (s *soak) notAcked(ctx context.Context) ([]string, error) {
	out, err := s.sqlite(ctx, "SELECT id FROM outbox_events WHERE state NOT IN ('ACKNOWLEDGED','DELETE_ELIGIBLE')")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func (s *soak) centralFault(ctx context.Context, res *faultResult) {
	p := s.procs["central"]
	if !p.owned() {
		res.Skipped = "no -central-cmd"
		return
	}
	s.setFault("central")
	t0 := time.Now()
	s.event("inject", fmt.Sprintf("SIGTERM central-api pid %d for %s", p.pid(), s.cfg.centralDown))
	p.stop(syscall.SIGTERM, 30*time.Second)
	sleepCtx(ctx, s.cfg.centralDown)
	res.DownS = time.Since(t0).Seconds()
	backlog, err := s.notAcked(ctx)
	if err != nil {
		res.Note = "read backlog: " + err.Error()
	}
	if err := p.start(); err != nil {
		res.Note += "; restart failed: " + err.Error()
		s.setFault("")
		return
	}
	t1 := time.Now()
	waitHTTP(ctx, p.health, 2*time.Minute)
	s.setFault("")
	res.Note = fmt.Sprintf("backlog at restart: %d events", len(backlog))
	// Catch-up: every event that was waiting at restart is acknowledged.
	want := map[string]bool{}
	for _, id := range backlog {
		want[id] = true
	}
	for time.Since(t1) < s.cfg.catchupMax && ctx.Err() == nil {
		cur, err := s.notAcked(ctx)
		if err == nil {
			left := 0
			for _, id := range cur {
				if want[id] {
					left++
				}
			}
			if left == 0 {
				res.Recovered = true
				break
			}
		}
		sleepCtx(ctx, 2*time.Second)
	}
	res.RecoveredS = time.Since(t1).Seconds()
	res.TotalS = time.Since(t0).Seconds()
	res.Signal = "all events pending at restart ACKNOWLEDGED"
}

func (s *soak) agentFault(runCtx, ctx context.Context, res *faultResult) {
	p := s.procs["agent"]
	if !p.owned() {
		res.Skipped = "no -agent-cmd"
		return
	}
	s.wantAgentKill.Store(true)
	var sid string
	select {
	case sid = <-s.killPoint:
	case <-time.After(5 * time.Minute):
		s.wantAgentKill.Store(false)
		res.Skipped = "no open session reached a kill point within 5 min"
		return
	case <-runCtx.Done():
		s.wantAgentKill.Store(false)
		res.Skipped = "run ended before a kill point"
		return
	}
	s.setFault("agent")
	t0 := time.Now()
	s.event("inject", fmt.Sprintf("SIGKILL agent pid %d mid-session %s", p.pid(), sid))
	p.stop(syscall.SIGKILL, 5*time.Second)
	sleepCtx(ctx, s.cfg.agentDown)
	res.DownS = time.Since(t0).Seconds()
	if err := p.start(); err != nil {
		res.Note = "restart failed: " + err.Error()
	}
	t1 := time.Now()
	_, ok := waitHTTP(ctx, s.cfg.agentURL+"/api/v1/status", 2*time.Minute)
	res.Recovered = ok
	res.RecoveredS = time.Since(t1).Seconds()
	res.TotalS = time.Since(t0).Seconds()
	res.Signal = "agent /api/v1/status 200"
	if st, ok := s.status(); ok {
		res.Note = fmt.Sprintf("overall after restart: %v", st["overall"])
	}
	s.inspectKilledSession(ctx, sid)
	s.setFault("")
	s.resume <- struct{}{}
}

func (s *soak) inspectKilledSession(ctx context.Context, sid string) {
	s.mu.Lock()
	ak := s.agentKill
	s.mu.Unlock()
	if ak == nil {
		return
	}
	q := fmt.Sprintf(`SELECT coalesce(ended_at,'NULL'), coalesce(end_reason,'NULL'), turns FROM sessions WHERE id='%s'`, sqlQuote(sid))
	if out, err := s.sqlite(ctx, q); err == nil {
		f := strings.Split(strings.TrimSpace(out), "|")
		if len(f) == 3 {
			ak.RowEndedAt, ak.RowEndReason = f[0], f[1]
			ak.RowTurns, _ = strconv.Atoi(f[2])
		} else {
			ak.RowEndedAt = "ABSENT"
		}
	}
	n := func(typ string) int {
		out, err := s.sqlite(ctx, fmt.Sprintf(`SELECT count(*) FROM outbox_events WHERE type='%s' AND json_extract(payload,'$.session_id')='%s'`, typ, sqlQuote(sid)))
		if err != nil {
			return -1
		}
		v, _ := strconv.Atoi(strings.TrimSpace(out))
		return v
	}
	ak.TurnEventsInOutbox = n("dialogue.turn")
	ak.EndedEventInOutbox = n("session.ended")
	switch {
	case ak.TurnEventsInOutbox != ak.TurnsBeforeKill:
		ak.Verdict = fmt.Sprintf("FAIL: %d turns answered before the kill, %d recorded", ak.TurnsBeforeKill, ak.TurnEventsInOutbox)
	case ak.RowEndedAt == "NULL":
		ak.Verdict = "GAP: session row left open (ended_at NULL), no session.ended event"
	case ak.EndedEventInOutbox == 1:
		ak.Verdict = "PASS: turns recorded, session closed with session.ended (" + ak.RowEndReason + ")"
	default:
		ak.Verdict = "CHECK: unexpected state"
	}
	b, _ := json.Marshal(ak)
	s.event("killcheck", string(b))
}

func sqlQuote(v string) string { return strings.ReplaceAll(v, "'", "''") }

// ---------------------------------------------------------------- sampling

func (s *soak) sqlite(ctx context.Context, q string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sqlite3", "-readonly", "-cmd", ".timeout 3000", s.cfg.dbPath(), q).Output()
	return string(out), err
}

func (s *soak) psql(ctx context.Context, q string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "psql", s.cfg.pgURL, "-At", "-v", "ON_ERROR_STOP=1", "-c", q).Output()
	return string(out), err
}

func rssMB(pid int) float64 {
	if pid <= 0 {
		return -1
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return -1
	}
	kb, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return kb / 1024
}

func openFDs(pid int) int {
	if pid <= 0 {
		return -1
	}
	if ents, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid)); err == nil {
		return len(ents)
	}
	out, err := exec.Command("lsof", "-n", "-P", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return -1
	}
	n := bytes.Count(out, []byte("\n")) - 1 // header line
	return n
}

var goroutineTotal = regexp.MustCompile(`goroutine profile: total (\d+)`)

func (s *soak) goroutines() int {
	if s.cfg.pprofURL != "" {
		hc := &http.Client{Timeout: 3 * time.Second}
		if resp, err := hc.Get(s.cfg.pprofURL + "/debug/pprof/goroutine?debug=1"); err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
			resp.Body.Close()
			if m := goroutineTotal.FindSubmatch(b); m != nil {
				v, _ := strconv.Atoi(string(m[1]))
				return v
			}
		}
	}
	return -1
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

var states = []string{"PENDING", "UPLOADING", "ACKNOWLEDGED", "DELETE_ELIGIBLE", "FAILED", "CONFLICT"}

func (s *soak) outboxCounts(ctx context.Context) map[string]int {
	m := map[string]int{}
	out, err := s.sqlite(ctx, "SELECT state, count(*) FROM outbox_events GROUP BY state")
	if err != nil {
		m["_err"] = 1
		return m
	}
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(ln, "|")
		if ok {
			n, _ := strconv.Atoi(v)
			m[k] = n
		}
	}
	return m
}

type sample struct {
	t        float64 // seconds since start
	pid      int
	rss      float64
	gor, fds int
}

func (s *soak) sampleLoop(ctx context.Context, w *csvFile, out *[]sample, mu *sync.Mutex) {
	t := time.NewTicker(s.cfg.sample)
	defer t.Stop()
	for {
		apid := s.procs["agent"].pid()
		if apid == 0 {
			apid = s.cfg.agentPID
		}
		rss, fds, gor := rssMB(apid), openFDs(apid), s.goroutines()
		ob := s.outboxCounts(ctx)
		total := 0
		for _, st := range states {
			total += ob[st]
		}
		st, ok := s.status()
		overall := "DOWN"
		if ok {
			overall, _ = st["overall"].(string)
		}
		centralUp := false
		if s.procs["central"].health != "" {
			hc := &http.Client{Timeout: 2 * time.Second}
			if resp, err := hc.Get(s.procs["central"].health); err == nil {
				resp.Body.Close()
				centralUp = resp.StatusCode == 200
			}
		}
		s.mu.Lock()
		sess, turns, last, fault := s.sessions, s.turnsOK, s.lastTurnMS, s.activeFault
		s.mu.Unlock()
		el := time.Since(s.start).Seconds()
		w.write(time.Now().UTC().Format(time.RFC3339), strconv.FormatFloat(el, 'f', 1, 64),
			strconv.Itoa(apid), fmt.Sprintf("%.1f", rss), strconv.Itoa(gor), strconv.Itoa(fds),
			strconv.FormatInt(fileSize(s.cfg.dbPath()), 10), strconv.FormatInt(fileSize(s.cfg.dbPath()+"-wal"), 10),
			strconv.Itoa(ob["PENDING"]), strconv.Itoa(ob["UPLOADING"]), strconv.Itoa(ob["ACKNOWLEDGED"]),
			strconv.Itoa(ob["DELETE_ELIGIBLE"]), strconv.Itoa(ob["FAILED"]), strconv.Itoa(ob["CONFLICT"]), strconv.Itoa(total),
			overall, strconv.FormatBool(capability(st, "voice_input")), strconv.FormatBool(capability(st, "llm")),
			strconv.Itoa(sess), strconv.Itoa(turns), strconv.FormatInt(last, 10),
			fmt.Sprintf("%.1f", rssMB(s.procs["speech"].pid())), fmt.Sprintf("%.1f", rssMB(s.procs["llama"].pid())),
			fmt.Sprintf("%.1f", rssMB(s.procs["central"].pid())), strconv.FormatBool(centralUp), fault)
		mu.Lock()
		*out = append(*out, sample{t: el, pid: apid, rss: rss, gor: gor, fds: fds})
		mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------- analysis

// slopePerHour is the least-squares slope of y over t (seconds), per hour.
func slopePerHour(t, y []float64) float64 {
	n := float64(len(t))
	if n < 3 {
		return math.NaN()
	}
	var st, sy, stt, sty float64
	for i := range t {
		st += t[i]
		sy += y[i]
		stt += t[i] * t[i]
		sty += t[i] * y[i]
	}
	den := n*stt - st*st
	if den == 0 {
		return math.NaN()
	}
	return (n*sty - st*sy) / den * 3600
}

type trend struct {
	PID         int     `json:"pid"`
	FromS       float64 `json:"from_s"`
	ToS         float64 `json:"to_s"`
	Samples     int     `json:"samples"`
	RSSFirst    float64 `json:"rss_first_mb"`
	RSSLast     float64 `json:"rss_last_mb"`
	RSSMax      float64 `json:"rss_max_mb"`
	RSSSlope    float64 `json:"rss_slope_mb_per_h"`
	GorMin      int     `json:"goroutines_min"`
	GorMax      int     `json:"goroutines_max"`
	GorSlope    float64 `json:"goroutines_slope_per_h"`
	FDMin       int     `json:"fds_min"`
	FDMax       int     `json:"fds_max"`
	FDSlope     float64 `json:"fds_slope_per_h"`
	WarmupSkipS float64 `json:"warmup_skipped_s"`
}

// trends computes one trend per agent process (pid segment), skipping the
// first warmup of each segment.
func trends(samples []sample, warmup time.Duration) []trend {
	var out []trend
	i := 0
	for i < len(samples) {
		j := i
		for j < len(samples) && samples[j].pid == samples[i].pid {
			j++
		}
		seg := samples[i:j]
		i = j
		if seg[0].pid <= 0 {
			continue
		}
		t0 := seg[0].t
		var ts, rs, gs, fs []float64
		tr := trend{PID: seg[0].pid, WarmupSkipS: warmup.Seconds(), GorMin: math.MaxInt, FDMin: math.MaxInt}
		var gts, fts []float64 // goroutines/FDs may be unknown (no -pprof, lsof failed)
		for _, x := range seg {
			if x.t-t0 < warmup.Seconds() || x.rss < 0 {
				continue
			}
			ts = append(ts, x.t)
			rs = append(rs, x.rss)
			if x.gor >= 0 {
				gts = append(gts, x.t)
				gs = append(gs, float64(x.gor))
			}
			if x.fds >= 0 {
				fts = append(fts, x.t)
				fs = append(fs, float64(x.fds))
			}
			if tr.Samples == 0 {
				tr.FromS, tr.RSSFirst = x.t, x.rss
			}
			tr.Samples++
			tr.ToS, tr.RSSLast = x.t, x.rss
			tr.RSSMax = math.Max(tr.RSSMax, x.rss)
			if x.gor >= 0 {
				tr.GorMin, tr.GorMax = min(tr.GorMin, x.gor), max(tr.GorMax, x.gor)
			}
			if x.fds >= 0 {
				tr.FDMin, tr.FDMax = min(tr.FDMin, x.fds), max(tr.FDMax, x.fds)
			}
		}
		if tr.Samples < 3 {
			continue
		}
		tr.RSSSlope = round2(slopePerHour(ts, rs))
		tr.GorSlope = round2(slopePerHour(gts, gs))
		tr.FDSlope = round2(slopePerHour(fts, fs))
		if tr.GorMin == math.MaxInt {
			tr.GorMin, tr.GorMax = -1, -1
		}
		if tr.FDMin == math.MaxInt {
			tr.FDMin, tr.FDMax = -1, -1
		}
		out = append(out, tr)
	}
	return out
}

// round2 rounds to 2 decimals; NaN (too few points) becomes 0 so the
// summary stays valid JSON.
func round2(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Round(v*100) / 100
}

func pct(d []time.Duration, p float64) float64 {
	if len(d) == 0 {
		return 0
	}
	c := append([]time.Duration(nil), d...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return float64(c[int(math.Round(p*float64(len(c)-1)))].Milliseconds())
}

type latency struct {
	N     int     `json:"n"`
	P50ms float64 `json:"p50_ms"`
	P95ms float64 `json:"p95_ms"`
	Maxms float64 `json:"max_ms"`
}

func lat(d []time.Duration) latency {
	return latency{N: len(d), P50ms: pct(d, 0.5), P95ms: pct(d, 0.95), Maxms: pct(d, 1)}
}

type summary struct {
	RunDir            string            `json:"run_dir"`
	Started           string            `json:"started"`
	DurationS         float64           `json:"duration_s"`
	Sessions          int               `json:"sessions"`
	TurnsOK           int               `json:"turns_ok"`
	EndsOK            int               `json:"ends_ok"`
	ExpectedEvents    int               `json:"expected_events_min"`
	DeviceEvents      int               `json:"device_events"`
	BaselineEvents    int               `json:"baseline_events"`
	NewDeviceEvents   int               `json:"new_device_events"`
	DeviceNotAcked    int               `json:"device_not_acked"`
	CentralEvents     int               `json:"central_events"`
	MissingCentrally  int               `json:"missing_centrally"`
	ExtraCentrally    int               `json:"extra_centrally"`
	DrainS            float64           `json:"drain_s"`
	TextLatency       latency           `json:"text_latency"`
	TextLatencyCalm   latency           `json:"text_latency_no_fault"`
	VoiceLatency      latency           `json:"voice_latency"`
	Errors            map[string]int    `json:"errors"`
	Faults            []faultResult     `json:"faults"`
	AgentKill         *agentKillResult  `json:"agent_kill,omitempty"`
	Trends            []trend           `json:"trends"`
	DBBytes           int64             `json:"db_bytes"`
	WALBytes          int64             `json:"wal_bytes"`
	Verdicts          map[string]string `json:"verdicts"`
	OutboxFinalStates map[string]int    `json:"outbox_final_states"`
}

// ---------------------------------------------------------------- main

type config struct {
	agentURL, pprofURL, dataDir, deviceID, pgURL, audioDir, out, faults string
	duration, sample, gap, think, voiceEvery, warmup                    time.Duration
	speechDown, llamaDown, centralDown, agentDown, catchupMax, drain    time.Duration
	turnsPerSession, agentPID                                           int
	maxRSSSlope, maxGorSlope, maxFDSlope                                float64
}

func (c config) dbPath() string { return filepath.Join(c.dataDir, "device.db") }

func main() { os.Exit(run()) }

func run() int {
	var c config
	var agentCmd, speechCmd, llamaCmd, centralCmd string
	flag.StringVar(&c.agentURL, "url", "http://127.0.0.1:8730", "agent base URL")
	flag.StringVar(&c.pprofURL, "pprof", "", "agent pprof base URL for goroutine counts (e.g. http://127.0.0.1:8739)")
	flag.StringVar(&c.dataDir, "data-dir", "var/soak/device", "agent data_dir (device.db lives here)")
	flag.StringVar(&c.deviceID, "device-id", "", "device id (for the central-side event count)")
	flag.StringVar(&c.pgURL, "pg", "postgres://jansahayak@127.0.0.1:54329/jansahayak?sslmode=disable", "central database URL (read-only queries)")
	flag.StringVar(&c.audioDir, "audio-dir", "var/testaudio", "WAV clips (16 kHz mono 16-bit) for voice turns")
	flag.StringVar(&c.out, "out", "var/soak", "output directory")
	flag.StringVar(&c.faults, "faults", "speech@5m,llama@10m,agent@15m,central@19m,agent@25m,speech@32m,llama@38m,central@45m,speech@55m", "fault schedule kind@offset,…")
	flag.DurationVar(&c.duration, "duration", 60*time.Minute, "how long to drive sessions")
	flag.DurationVar(&c.sample, "sample", 10*time.Second, "sampling interval")
	flag.DurationVar(&c.gap, "gap", 2*time.Second, "pause between sessions")
	flag.DurationVar(&c.think, "think", 500*time.Millisecond, "pause before each text turn")
	flag.DurationVar(&c.voiceEvery, "voice-every", 2*time.Minute, "a voice turn at most this often (0 = never)")
	flag.DurationVar(&c.warmup, "warmup", 5*time.Minute, "samples skipped at the start of each agent process for trend slopes")
	flag.DurationVar(&c.speechDown, "speech-down", 20*time.Second, "speech worker down time")
	flag.DurationVar(&c.llamaDown, "llama-down", 20*time.Second, "llama-server down time")
	flag.DurationVar(&c.centralDown, "central-down", 3*time.Minute, "central-api down time")
	flag.DurationVar(&c.agentDown, "agent-down", 5*time.Second, "agent down time after SIGKILL")
	flag.DurationVar(&c.catchupMax, "catchup-max", 15*time.Minute, "max wait for sync catch-up after central restarts")
	flag.DurationVar(&c.drain, "drain", 15*time.Minute, "max wait at the end for every event to be acknowledged")
	flag.IntVar(&c.turnsPerSession, "turns", 3, "text turns per session")
	flag.IntVar(&c.agentPID, "agent-pid", 0, "agent pid when the agent is not owned (-agent-cmd empty)")
	flag.Float64Var(&c.maxRSSSlope, "max-rss-slope", 5, "fail if a segment's RSS slope exceeds this (MB/h)")
	flag.Float64Var(&c.maxGorSlope, "max-goroutine-slope", 10, "fail if a segment's goroutine slope exceeds this (/h)")
	flag.Float64Var(&c.maxFDSlope, "max-fd-slope", 10, "fail if a segment's FD slope exceeds this (/h)")
	flag.StringVar(&agentCmd, "agent-cmd", "", "command that runs the device agent (owned; needed for agent faults)")
	flag.StringVar(&speechCmd, "speech-cmd", "", "command that runs the speech worker")
	flag.StringVar(&llamaCmd, "llama-cmd", "", "command that runs llama-server")
	flag.StringVar(&centralCmd, "central-cmd", "", "command that runs central-api")
	speechHealth := flag.String("speech-health", "http://127.0.0.1:8743/health", "speech worker health URL")
	llamaHealth := flag.String("llama-health", "http://127.0.0.1:8742/health", "llama-server health URL")
	centralHealth := flag.String("central-health", "http://127.0.0.1:8750/healthz", "central-api health URL")
	flag.Parse()

	plan, err := parseFaults(c.faults)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	runID := time.Now().UTC().Format("20060102T150405Z")
	runDir := filepath.Join(c.out, runID)
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	s := &soak{
		cfg: c, api: &apiClient{base: c.agentURL, hc: &http.Client{Timeout: 90 * time.Second}},
		runDir: runDir, errs: map[string]int{}, killPoint: make(chan string), resume: make(chan struct{}),
		procs: map[string]*proc{
			"central": {name: "central", cmd: centralCmd, health: *centralHealth, logPath: filepath.Join(runDir, "central.log")},
			"llama":   {name: "llama", cmd: llamaCmd, health: *llamaHealth, logPath: filepath.Join(runDir, "llama.log")},
			"speech":  {name: "speech", cmd: speechCmd, health: *speechHealth, logPath: filepath.Join(runDir, "speech.log")},
			"agent":   {name: "agent", cmd: agentCmd, health: c.agentURL + "/api/v1/status", logPath: filepath.Join(runDir, "agent.log")},
		},
	}
	if s.turnsW, err = newCSV(filepath.Join(runDir, "turns.csv"), []string{"time", "elapsed_s", "kind", "status", "latency_ms", "active_fault"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if s.eventsW, err = newCSV(filepath.Join(runDir, "events.csv"), []string{"time", "elapsed_s", "kind", "detail"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sw, err := newCSV(filepath.Join(runDir, "samples.csv"), []string{"time", "elapsed_s", "agent_pid", "agent_rss_mb", "goroutines", "open_fds",
		"db_bytes", "wal_bytes", "ob_pending", "ob_uploading", "ob_acknowledged", "ob_delete_eligible", "ob_failed", "ob_conflict", "ob_total",
		"overall", "voice_input", "llm", "sessions", "turns_ok", "last_turn_ms", "speech_rss_mb", "llama_rss_mb", "central_rss_mb", "central_up", "active_fault"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	defer func() { // stop owned processes: agent first so it records the open session
		for _, n := range []string{"agent", "speech", "llama", "central"} {
			s.procs[n].stop(syscall.SIGTERM, 30*time.Second)
		}
	}()

	// Start owned processes one at a time (little free RAM on the dev Mac).
	for _, n := range []string{"central", "llama", "speech", "agent"} {
		p := s.procs[n]
		if !p.owned() {
			continue
		}
		if err := p.start(); err != nil {
			fmt.Fprintf(os.Stderr, "start %s: %v\n", n, err)
			return 1
		}
		d, ok := waitHTTP(ctx, p.health, 5*time.Minute)
		if !ok {
			fmt.Fprintf(os.Stderr, "%s did not become healthy; see %s\n", n, p.logPath)
			return 1
		}
		fmt.Printf("%s up in %s (pid %d)\n", n, d.Round(time.Millisecond), p.pid())
	}
	if !s.procs["agent"].owned() && c.agentPID == 0 {
		fmt.Fprintln(os.Stderr, "warning: no -agent-cmd and no -agent-pid: RSS/FDs not sampled")
	}
	// Let the agent's health checks see the runtimes before driving load.
	if s.procs["speech"].owned() {
		s.waitCap(ctx, "voice_input", 2*time.Minute)
	}

	if out, err := s.sqlite(ctx, "SELECT count(*) FROM outbox_events"); err == nil {
		s.baseline, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	s.start = time.Now()
	s.event("start", fmt.Sprintf("run %s duration %s faults %q", runID, c.duration, c.faults))
	var samples []sample
	var smu sync.Mutex
	sctx, scancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); s.sampleLoop(sctx, sw, &samples, &smu) }()
	runCtx, runCancel := context.WithTimeout(ctx, c.duration)
	go func() { defer wg.Done(); s.faultLoop(runCtx, ctx, plan) }()
	go func() { defer wg.Done(); s.sessionLoop(runCtx) }()
	<-runCtx.Done()
	s.stopRun.Store(true)
	runCancel()

	// Drain: wait until every device event is acknowledged and central holds them.
	s.event("drain", "sessions stopped; waiting for the outbox to drain")
	t0 := time.Now()
	drained := false
	for time.Since(t0) < c.drain && ctx.Err() == nil {
		if left, err := s.notAcked(ctx); err == nil && len(left) == 0 {
			drained = true
			break
		}
		sleepCtx(ctx, 5*time.Second)
	}
	drainS := time.Since(t0).Seconds()
	// A fault still in progress (it runs on ctx, not runCtx) finishes first,
	// so no owned process is left stopped.
	scancel()
	wg.Wait()
	sw.close()

	sum := s.summarize(ctx, samples, drained, drainS, runID)
	b, _ := json.MarshalIndent(sum, "", "  ")
	_ = os.WriteFile(filepath.Join(runDir, "summary.json"), b, 0o640)
	txt := renderSummary(sum)
	_ = os.WriteFile(filepath.Join(runDir, "summary.txt"), []byte(txt), 0o640)
	fmt.Println(txt)
	s.turnsW.close()
	s.eventsW.close()
	for _, v := range sum.Verdicts {
		if strings.HasPrefix(v, "FAIL") {
			return 1
		}
	}
	return 0
}

func (s *soak) summarize(ctx context.Context, samples []sample, drained bool, drainS float64, runID string) summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := summary{
		RunDir: s.runDir, Started: s.start.UTC().Format(time.RFC3339), DurationS: round2(time.Since(s.start).Seconds()),
		Sessions: s.sessions, TurnsOK: s.turnsOK, EndsOK: s.endsOK, ExpectedEvents: s.turnsOK + s.endsOK,
		DrainS: round2(drainS), TextLatency: lat(s.textLat), TextLatencyCalm: lat(s.textLatCalm), VoiceLatency: lat(s.voiceLat),
		Errors: s.errs, Faults: s.faultLog, AgentKill: s.agentKill, Trends: trends(samples, s.cfg.warmup),
		DBBytes: fileSize(s.cfg.dbPath()), WALBytes: fileSize(s.cfg.dbPath() + "-wal"), Verdicts: map[string]string{},
	}
	sum.OutboxFinalStates = s.outboxCounts(ctx)
	devIDs := map[string]bool{}
	if out, err := s.sqlite(ctx, "SELECT id FROM outbox_events"); err == nil {
		for _, id := range strings.Fields(out) {
			devIDs[id] = true
		}
	}
	sum.DeviceEvents = len(devIDs)
	sum.BaselineEvents = s.baseline
	sum.NewDeviceEvents = sum.DeviceEvents - s.baseline
	if left, err := s.notAcked(ctx); err == nil {
		sum.DeviceNotAcked = len(left)
	}
	if s.cfg.deviceID != "" {
		cen := map[string]bool{}
		if out, err := s.psql(ctx, fmt.Sprintf("SELECT id FROM sync_events WHERE device_id = '%s'", sqlQuote(s.cfg.deviceID))); err == nil {
			for _, id := range strings.Fields(out) {
				cen[id] = true
			}
			sum.CentralEvents = len(cen)
			for id := range devIDs {
				if !cen[id] {
					sum.MissingCentrally++
				}
			}
			for id := range cen {
				if !devIDs[id] {
					sum.ExtraCentrally++
				}
			}
		} else {
			sum.Verdicts["central_count"] = "FAIL: psql: " + err.Error()
		}
	}
	v := sum.Verdicts
	switch {
	case !drained:
		v["drain"] = fmt.Sprintf("FAIL: %d events not acknowledged after %.0fs", sum.DeviceNotAcked, drainS)
	default:
		v["drain"] = fmt.Sprintf("PASS: all %d device events acknowledged (drain %.0fs)", sum.DeviceEvents, drainS)
	}
	if s.cfg.deviceID != "" {
		if sum.MissingCentrally == 0 && sum.ExtraCentrally == 0 && sum.CentralEvents == sum.DeviceEvents {
			v["central_match"] = fmt.Sprintf("PASS: central holds exactly the %d device events", sum.CentralEvents)
		} else {
			v["central_match"] = fmt.Sprintf("FAIL: device %d, central %d, missing %d, extra %d", sum.DeviceEvents, sum.CentralEvents, sum.MissingCentrally, sum.ExtraCentrally)
		}
	}
	// Every answered turn and every ended session must be in the outbox; an
	// agent-kill session may add one session.ended when the agent closes it at restart.
	agentKills := 0
	for _, f := range s.faultLog {
		if f.Kind == "agent" && f.Skipped == "" {
			agentKills++
		}
	}
	if n := sum.NewDeviceEvents; n >= sum.ExpectedEvents && n <= sum.ExpectedEvents+agentKills {
		v["no_lost_events"] = fmt.Sprintf("PASS: %d answered turns + %d ended sessions = %d expected; device recorded %d new", s.turnsOK, s.endsOK, sum.ExpectedEvents, n)
	} else {
		v["no_lost_events"] = fmt.Sprintf("FAIL: expected %d (+ ≤%d restart closures), device recorded %d new", sum.ExpectedEvents, agentKills, n)
	}
	for _, tr := range sum.Trends {
		if tr.ToS-tr.FromS < 20*60 {
			continue // too short a segment to judge a trend
		}
		key := fmt.Sprintf("trend_pid_%d", tr.PID)
		if tr.RSSSlope > s.cfg.maxRSSSlope || tr.GorSlope > s.cfg.maxGorSlope || tr.FDSlope > s.cfg.maxFDSlope {
			v[key] = fmt.Sprintf("FAIL: rss %.2f MB/h, goroutines %.2f/h, fds %.2f/h over %.0f min", tr.RSSSlope, tr.GorSlope, tr.FDSlope, (tr.ToS-tr.FromS)/60)
		} else {
			v[key] = fmt.Sprintf("PASS: rss %.2f MB/h, goroutines %.2f/h, fds %.2f/h over %.0f min", tr.RSSSlope, tr.GorSlope, tr.FDSlope, (tr.ToS-tr.FromS)/60)
		}
	}
	for i, f := range s.faultLog {
		key := fmt.Sprintf("fault_%02d_%s", i+1, f.Kind)
		switch {
		case f.Skipped != "":
			v[key] = "SKIPPED: " + f.Skipped
		case f.Recovered:
			v[key] = fmt.Sprintf("PASS: recovered %.1fs after restart (outage %.1fs)", f.RecoveredS, f.TotalS)
		default:
			v[key] = "FAIL: not recovered: " + f.Note
		}
	}
	if s.agentKill != nil {
		v["agent_kill_session"] = s.agentKill.Verdict
	}
	return sum
}

func renderSummary(s summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FAULTSOAK SUMMARY  run %s\n", s.RunDir)
	fmt.Fprintf(&b, "duration %.0fs  sessions %d  turns ok %d  ends ok %d\n", s.DurationS, s.Sessions, s.TurnsOK, s.EndsOK)
	fmt.Fprintf(&b, "events: expected ≥%d  new on device %d (baseline %d)  device %d (not acked %d)  central %d  missing %d  extra %d  drain %.0fs\n",
		s.ExpectedEvents, s.NewDeviceEvents, s.BaselineEvents, s.DeviceEvents, s.DeviceNotAcked, s.CentralEvents, s.MissingCentrally, s.ExtraCentrally, s.DrainS)
	fmt.Fprintf(&b, "text latency  all n=%d p50 %.0fms p95 %.0fms max %.0fms\n", s.TextLatency.N, s.TextLatency.P50ms, s.TextLatency.P95ms, s.TextLatency.Maxms)
	fmt.Fprintf(&b, "text latency  no fault n=%d p50 %.0fms p95 %.0fms max %.0fms\n", s.TextLatencyCalm.N, s.TextLatencyCalm.P50ms, s.TextLatencyCalm.P95ms, s.TextLatencyCalm.Maxms)
	fmt.Fprintf(&b, "voice latency n=%d p50 %.0fms p95 %.0fms max %.0fms\n", s.VoiceLatency.N, s.VoiceLatency.P50ms, s.VoiceLatency.P95ms, s.VoiceLatency.Maxms)
	fmt.Fprintf(&b, "db %d bytes, wal %d bytes, outbox %v\n", s.DBBytes, s.WALBytes, s.OutboxFinalStates)
	keys := make([]string, 0, len(s.Errors))
	for k := range s.Errors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("errors by type:\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-40s %d\n", k, s.Errors[k])
	}
	b.WriteString("faults:\n")
	for _, f := range s.Faults {
		fmt.Fprintf(&b, "  %-8s at %-7s down %.0fs  recovered=%v  after-restart %.1fs  total %.1fs  %s %s\n", f.Kind, f.At, f.DownS, f.Recovered, f.RecoveredS, f.TotalS, f.Skipped, f.Note)
	}
	b.WriteString("trends (per agent process, warm-up skipped):\n")
	for _, t := range s.Trends {
		fmt.Fprintf(&b, "  pid %d %.0f–%.0fs n=%d rss %.1f→%.1f (max %.1f) MB slope %.2f MB/h; goroutines %d–%d slope %.2f/h; fds %d–%d slope %.2f/h\n",
			t.PID, t.FromS, t.ToS, t.Samples, t.RSSFirst, t.RSSLast, t.RSSMax, t.RSSSlope, t.GorMin, t.GorMax, t.GorSlope, t.FDMin, t.FDMax, t.FDSlope)
	}
	vk := make([]string, 0, len(s.Verdicts))
	for k := range s.Verdicts {
		vk = append(vk, k)
	}
	sort.Strings(vk)
	b.WriteString("verdicts:\n")
	for _, k := range vk {
		fmt.Fprintf(&b, "  %-24s %s\n", k, s.Verdicts[k])
	}
	return b.String()
}
