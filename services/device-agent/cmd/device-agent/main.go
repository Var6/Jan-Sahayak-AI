// Command device-agent is the Jan Sahayak kiosk controller: one Go process
// that owns sessions, the conversation engine, storage, sync, health, the
// hardware button and the loopback API that serves the kiosk UI.
//
//	device-agent -config config/device.dev.json
//	device-agent -config /etc/jan-sahayak/device.json -print-identity
//
// AI runtimes (speech worker, llama-server) are separate processes reached
// over loopback HTTP; the agent keeps working without them (text input,
// deterministic answers) and reports what is missing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/deviceapi"
	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/ai"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/api"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/config"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/dialogue"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/hal"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/identity"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/knowledge"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/sdnotify"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/session"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/store"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/syncer"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/webui"
)

// version is set at build time: -ldflags "-X main.version=…".
var version = "dev"

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("device-agent", flag.ContinueOnError)
	cfgPath := fs.String("config", os.Getenv("JS_CONFIG"), "config file (env JS_CONFIG)")
	showVersion := fs.Bool("version", false, "print version and exit")
	printIdentity := fs.Bool("print-identity", false, "print device id and public key (for provisioning) and exit")
	checkConfig := fs.Bool("check-config", false, "validate the config and exit")
	requeueFailed := fs.Bool("requeue-failed", false, "operator: move FAILED and CONFLICT outbox events back to PENDING and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	cfg, err := config.Load(*cfgPath, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if *checkConfig {
		fmt.Println("config ok: profile", cfg.Profile)
		return 0
	}
	if *requeueFailed {
		return requeueFailedEvents(cfg)
	}
	if *printIdentity {
		id, created, err := identity.LoadOrCreate(cfg.IdentityPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("{\"device_id\": %q, \"public_key\": %q, \"created\": %v}\n", id.DeviceID, id.PublicKeyBase64(), created)
		return 0
	}

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)
	// No core dumps: this process holds citizen audio and photos in
	// memory. Children (tesseract) inherit the limit; the systemd unit
	// also sets LimitCORE=0.
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		log.Warn("could not disable core dumps", "err", err)
	}
	if cfg.MemoryLimitMB > 0 && os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(int64(cfg.MemoryLimitMB) << 20)
	}
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := build(sigCtx, cfg, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		return 1
	}
	return a.serve(sigCtx)
}

// requeueFailedEvents is the operator command behind -requeue-failed. It is
// safe while the agent runs (SQLite WAL + busy timeout); events are never
// deleted, only given a fresh attempt budget.
func requeueFailedEvents(cfg config.Config) int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	st, rec, err := store.Open(ctx, store.Options{Path: cfg.DBPath(), BackupDir: cfg.BackupDir()})
	if err != nil {
		fmt.Fprintln(os.Stderr, "database:", err)
		return 1
	}
	defer st.Close()
	if rec != nil {
		fmt.Fprintln(os.Stderr, "warning: the database was recovered at open; see the agent log")
	}
	ob := outbox.New(st.DB, outbox.Config{MaxRows: cfg.Sync.OutboxMaxRows, AckRetention: cfg.Sync.AckRetention.D(), MaxAttempts: cfg.Sync.MaxAttempts}, nil)
	n, err := ob.RequeueFailed(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "requeue:", err)
		return 1
	}
	fmt.Printf("{\"requeued\": %d}\n", n)
	return 0
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

// agent holds the wired components.
type agent struct {
	cfg     config.Config
	log     *slog.Logger
	st      *store.Store
	mon     *health.Monitor
	sm      *session.Manager
	sync    *syncer.Worker
	api     *api.Server
	button  hal.Button
	httpSrv *http.Server
	ln      net.Listener
	notify  *sdnotify.Notifier
	stt     ai.STT
	tts     ai.TTS
	llm     ai.LLM // raw client for health checks (nil when disabled)
	ks      *knowledge.Store
	engine  *dialogue.Conversation
	ob      *outbox.Outbox
	conn    connectivity // heartbeat + knowledge-bundle sync (connectivity.go)

	// cancelBase cancels every in-flight HTTP request's context at
	// shutdown (http.Server.BaseContext).
	baseCtx    context.Context
	cancelBase context.CancelFunc
	hb         heartbeats
	btn        buttonHealth
	startedAt  time.Time
}

// heartbeats are UnixNano stamps written by each long-lived loop on every
// iteration; the systemd watchdog checks them after a startup grace period.
type heartbeats struct {
	health  atomic.Int64 // the health monitor finished a database check
	session atomic.Int64 // the session timer ticked
	http    atomic.Int64 // the API answered the liveness self-probe
}

func stamp(v *atomic.Int64) { v.Store(time.Now().UnixNano()) }

// age is how long ago v was stamped (ok=false: never).
func age(v *atomic.Int64) (time.Duration, bool) {
	t := v.Load()
	if t == 0 {
		return 0, false
	}
	return time.Since(time.Unix(0, t)), true
}

// buttonHealth tracks the GPIO button line for its health check.
type buttonHealth struct {
	held     atomic.Bool  // the line is requested and being read
	failures atomic.Int64 // failed attempts since the line was last held
}

func build(ctx context.Context, cfg config.Config, log *slog.Logger) (*agent, error) {
	a := &agent{cfg: cfg, log: log, notify: sdnotify.FromEnv(nil)}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, err
	}
	id, created, err := identity.LoadOrCreate(cfg.IdentityPath())
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	if created {
		log.Warn("generated a new device identity; register its public key with the central server",
			"device_id", id.DeviceID, "public_key", id.PublicKeyBase64())
	}

	st, rec, err := store.Open(ctx, store.Options{Path: cfg.DBPath(), BackupDir: cfg.BackupDir()})
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	a.st = st
	if rec != nil {
		log.Error("database failed its integrity check and was recovered; a technician should inspect the quarantined file",
			"reason", rec.Reason, "quarantined", rec.QuarantinedPath, "restored_from", rec.RestoredFrom, "fresh", rec.FreshDatabase)
	}

	serverKeys, err := cfg.ServerPublicKeys()
	if err != nil {
		return nil, err
	}

	// Knowledge: the only source of scheme facts.
	ks, err := knowledge.New(ctx, st.DB, knowledge.Config{DeviceProfile: cfg.Profile, AllowUnverifiedForDev: cfg.Knowledge.AllowUnverifiedForDev})
	if err != nil {
		return nil, fmt.Errorf("knowledge: %w", err)
	}
	a.ks = ks
	if p := cfg.Knowledge.Path; p != "" {
		var verify func(scheme.Bundle) error
		if cfg.Knowledge.RequireSignedBundle {
			verify = func(b scheme.Bundle) error { return deviceapi.VerifyBundle(serverKeys, b) }
		}
		var rep knowledge.Report
		if cfg.Knowledge.RequireSignedBundle {
			rep, err = ks.LoadBundleFile(ctx, p, verify)
		} else {
			rep, err = ks.LoadPath(ctx, p, "", nil)
		}
		if errors.Is(err, knowledge.ErrBundleRollback) {
			// Normal after a bundle sync: the device holds a newer bundle.
			log.Info("configured knowledge bundle is older than the loaded one; keeping the newer", "path", p, "err", err)
		} else if err != nil {
			log.Error("knowledge load failed; keeping the records already on the device", "path", p, "err", err)
		} else {
			log.Info("knowledge loaded", "bundle_version", rep.BundleVersion, "loaded", rep.Loaded,
				"skipped", len(rep.Skipped), "factual", rep.Factual, "unverified_allowed", ks.Config().UnverifiedAllowed())
			for _, s := range rep.Skipped {
				log.Warn("knowledge record skipped", "source", s.Source, "id", s.ID, "reason", s.Reason)
			}
		}
	}
	var gaz *knowledge.Gazetteer
	if p := cfg.Knowledge.GazetteerPath; p != "" {
		if gaz, err = knowledge.LoadGazetteer(p); err != nil {
			log.Warn("gazetteer not loaded (district names will not be recognised)", "err", err)
			gaz = nil
		}
	}
	var helplines *dialogue.Helplines
	if p := cfg.Knowledge.HelplinesPath; p != "" {
		if helplines, err = dialogue.LoadHelplines(p, time.Now()); err != nil {
			log.Warn("helplines not loaded (crisis replies give 112 only)", "err", err)
			helplines = nil
		} else {
			log.Info("helplines loaded", "verified", helplines.Len())
		}
	}

	a.mon = health.NewMonitor()

	// AI runtime clients (separate processes on loopback).
	switch cfg.Speech.STTEngine {
	case "speech-worker":
		a.stt = ai.NewSpeechSTT(ai.SpeechConfig{BaseURL: cfg.Speech.URL, Timeout: cfg.Speech.STTTimeout.D(), MaxAudioSecs: cfg.Speech.MaxAudioSecs})
	case "whisper":
		a.stt = ai.NewWhisper(ai.WhisperConfig{BaseURL: cfg.Speech.WhisperURL, Timeout: cfg.Speech.STTTimeout.D(), MaxAudioSecs: cfg.Speech.MaxAudioSecs})
	}
	if cfg.Speech.TTSEnabled {
		a.tts = ai.NewSpeechTTS(ai.SpeechConfig{BaseURL: cfg.Speech.URL, Timeout: cfg.Speech.TTSTimeout.D(), Speed: cfg.Speech.TTSSpeed})
	}
	var engineLLM ai.LLM // stays a nil interface when disabled
	if cfg.LLM.Enabled {
		key, err := cfg.LLMAPIKey()
		if err != nil {
			// The LLM is optional: without its key llama-server answers
			// 401, the LLM shows UNAVAILABLE and answers stay deterministic.
			log.Error("llm: API key not loaded; LLM calls will be refused by llama-server", "err", err)
		}
		raw := ai.NewLlama(ai.LlamaConfig{BaseURL: cfg.LLM.URL, Timeout: cfg.LLM.Timeout.D(), MaxTokens: cfg.LLM.MaxTokens, APIKey: key})
		a.llm = raw
		engineLLM = gatedLLM{inner: raw, usable: func() bool { return a.mon.IsUsable(api.CompLLM) }}
	}

	eng, err := dialogue.New(dialogue.Config{
		Knowledge: ks, Gazetteer: gaz, Helplines: helplines, LLM: engineLLM,
		EnableRephrase: cfg.LLM.Enabled && cfg.LLM.Rephrase, MinSTTConfidence: cfg.Dialogue.MinSTTConfidence,
		MaxMisses: cfg.Dialogue.MaxMisses, ClassifyTimeout: cfg.LLM.ClassifyTimeout.D(),
		RephraseTimeout: cfg.LLM.RephraseTimeout.D(), Logger: log.With("component", "dialogue"),
	})
	if err != nil {
		return nil, err
	}
	a.engine = eng

	a.sm = session.NewManager(session.Timeouts{
		Reminder: cfg.Session.ReminderAfter.D(), FinalWarning: cfg.Session.FinalWarningAfter.D(), Close: cfg.Session.CloseAfter.D(),
		// A turn may run TurnTimeout plus the TTS minimum budget; only
		// beyond that (plus margin) is PROCESSING treated as stuck.
		MaxProcessing: cfg.API.TurnTimeout.D() + api.DefaultTTSMinBudget + 15*time.Second,
		MaxSpeaking:   session.DefaultMaxSpeaking,
	}, nil)
	a.ob = outbox.New(st.DB, outbox.Config{
		MaxRows: cfg.Sync.OutboxMaxRows, AckRetention: cfg.Sync.AckRetention.D(), MaxAttempts: cfg.Sync.MaxAttempts,
		BaseBackoff: 30 * time.Second, MaxBackoff: 6 * time.Hour,
	}, nil)
	a.sync = syncer.New(syncer.Config{
		BaseURL: cfg.Sync.BaseURL, ServerKeys: serverKeys, BatchSize: cfg.Sync.BatchSize,
		Interval: cfg.Sync.Interval.D(), HTTPTimeout: cfg.Sync.HTTPTimeout.D(), HousekeepDur: cfg.Sync.HousekeepInterval.D(),
	}, a.ob, id, log.With("component", "sync"))
	a.buildConnectivity(ctx, id, serverKeys)

	switch cfg.HAL.Button {
	case "gpio":
		gc := hal.GPIOConfig{
			Chip: cfg.HAL.GPIOChip, Line: uint32(cfg.HAL.GPIOLine), ActiveLow: cfg.HAL.ActiveLow, PullUp: cfg.HAL.PullUp,
			Debounce: cfg.HAL.Debounce.D(), MinGap: cfg.HAL.MinPressGap.D(),
			OnError: func(err error) {
				a.btn.held.Store(false)
				if n := a.btn.failures.Add(1); n <= 3 || n%60 == 0 {
					log.Error("button: GPIO error (retrying)", "err", err, "failures", n)
				}
			},
			OnReady: func() {
				a.btn.held.Store(true)
				a.btn.failures.Store(0)
				log.Info("button: GPIO line ready", "chip", cfg.HAL.GPIOChip, "line", cfg.HAL.GPIOLine)
			},
		}
		g, err := hal.NewGPIO(gc)
		if err != nil {
			// Degrade instead of failing startup: the agent (health,
			// sync, technician API) keeps running, the button health
			// component reports the fault, and Run keeps retrying the
			// line (a chip that appears late at boot is picked up).
			log.Error("button: GPIO not available; continuing without a working button and retrying", "err", err)
			g = hal.NewGPIOUnchecked(gc)
		}
		a.button = g
	default:
		a.button = hal.Sim{}
	}

	// Sessions a killed predecessor left open get their end recorded now,
	// before any new session can start (Phase 12 fault soak finding).
	{
		octx, ocancel := context.WithTimeout(ctx, 10*time.Second)
		n, err := api.CloseOrphanedSessions(octx, st.DB, a.ob)
		ocancel()
		switch {
		case err != nil:
			log.Error("could not record the end of sessions left open by the previous run; retrying at next start", "err", err, "closed", n)
		case n > 0:
			log.Warn("recorded the end of sessions left open by the previous run", "count", n, "end_reason", "error")
		}
	}

	// Document assistance (Phase 11, documents.go): tesseract subprocess.
	docs, docOCR := a.buildDocuments()

	srv, err := api.New(api.Deps{
		Documents: docs,
		Sessions:  a.sm, Engine: eng, STT: a.stt, TTS: a.tts, TTSLangs: cfg.Speech.TTSLangs, Health: a.mon, DB: st.DB, Outbox: a.ob,
		Kick: a.sync.Kick, SimButton: a.button.Simulated(), DeviceID: id.DeviceID, Version: version,
		DefaultLang: cfg.DefaultLanguage, Listen: cfg.Listen, UI: webui.Handler(),
		HealthExtra: a.healthExtra, Log: log.With("component", "api"),
		Limits: api.Limits{
			MaxJSONBytes: cfg.API.MaxJSONBytes, MaxAudioBytes: cfg.API.MaxAudioBytes, MaxTextRunes: cfg.API.MaxTextRunes,
			MaxAudioSecs: cfg.Speech.MaxAudioSecs, TurnTimeout: cfg.API.TurnTimeout.D(), ControlTimeout: cfg.API.ControlTimeout.D(),
			StartTimeout: cfg.API.StartTimeout.D(), ReadTimeout: cfg.API.ReadTimeout.D(), SSEMaxClients: cfg.API.SSEMaxClients,
			SSEPing: cfg.API.SSEPingInterval.D(), SSEMaxLifetime: cfg.API.SSEMaxLifetime.D(),
			AudioCacheClips: cfg.API.AudioCacheClips, AudioCacheBytes: cfg.API.AudioCacheBytes, GoodbyeGrace: cfg.API.GoodbyeGrace.D(),
		},
	})
	if err != nil {
		return nil, err
	}
	a.api = srv
	a.mon.OnChange(func(prev, cur health.Status) {
		lvl := slog.LevelInfo
		if cur.State != health.Healthy && cur.State != health.Recovering {
			lvl = slog.LevelWarn
		}
		log.Log(context.Background(), lvl, "health change", "component", cur.Name, "from", prev.State, "to", cur.State, "detail", cur.Detail)
		srv.HealthChanged()
	})
	a.registerChecks()
	a.registerOCRCheck(docOCR)

	baseCtx, cancelBase := context.WithCancel(context.Background())
	a.baseCtx, a.cancelBase = baseCtx, cancelBase
	a.httpSrv = &http.Server{
		Addr: cfg.Listen, Handler: srv.Handler(),
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ReadHeaderTimeout: cfg.API.ReadHeaderTimeout.D(), ReadTimeout: cfg.API.ReadTimeout.D(),
		WriteTimeout: cfg.API.WriteTimeout.D(), IdleTimeout: cfg.API.IdleTimeout.D(),
		MaxHeaderBytes: 16 << 10, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	a.httpSrv.RegisterOnShutdown(srv.Close)
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}
	a.ln = ln
	log.Info("device agent starting", "version", version, "profile", cfg.Profile, "device_id", id.DeviceID,
		"listen", cfg.Listen, "button", a.button.Kind(), "ui_built", webui.Built(), "stt", cfg.Speech.STTEngine,
		"tts", cfg.Speech.TTSEnabled, "llm", cfg.LLM.Enabled, "sync", cfg.Sync.BaseURL != "")
	return a, nil
}

// gatedLLM skips the network entirely while the health monitor says the
// LLM is down, so a dead llama-server costs the citizen no waiting time.
type gatedLLM struct {
	inner  ai.LLM
	usable func() bool
}

func (g gatedLLM) Complete(ctx context.Context, r ai.CompletionRequest) (string, error) {
	if !g.usable() {
		return "", ai.ErrUnavailable
	}
	return g.inner.Complete(ctx, r)
}

func (g gatedLLM) Health(ctx context.Context) error {
	if !g.usable() {
		return ai.ErrUnavailable
	}
	return g.inner.Health(ctx)
}

// serve runs everything until a signal, then shuts down in order, each
// step with its own budget so one slow step cannot starve the next:
//
//  1. HTTP: cancel in-flight requests (BaseContext), stop accepting, wait
//     for handlers and button goroutines (their analytics writes);
//  2. workers: stop the loops; the session timer ends any open session,
//     which records session.ended while the database is still open;
//  3. database.
func (a *agent) serve(sigCtx context.Context) int {
	a.startedAt = time.Now()
	workCtx, cancelWork := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	goFn := func(name string, fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					a.log.Error("worker panicked", "worker", name, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				}
			}()
			fn()
		}()
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- a.httpSrv.Serve(a.ln) }()

	goFn("session", func() { a.sessionLoop(workCtx) })
	goFn("health", func() { a.mon.Run(workCtx) })
	goFn("sync", func() { a.sync.Run(workCtx) })
	a.startConnectivity(workCtx, goFn)
	goFn("button", func() {
		// PressButton returns at once: the press (session start, greeting
		// TTS) runs on its own single-flight goroutine, cancelled with the
		// HTTP requests when shutdown starts.
		if err := a.button.Run(workCtx, func() { a.api.PressButton(a.baseCtx) }); err != nil {
			a.log.Error("button stopped", "err", err)
		}
	})
	if a.cfg.Backup.Enabled {
		goFn("backup", func() { a.backupLoop(workCtx) })
	}
	var pprofSrv *http.Server
	if a.cfg.PprofAddr != "" {
		pprofSrv = a.startPprof()
	}
	// READY once the database answers and the API serves, retried until it
	// does (systemd's TimeoutStartSec bounds the wait); the watchdog starts
	// after READY.
	goFn("ready", func() { a.readyLoop(workCtx, goFn) })

	code := 0
	select {
	case <-sigCtx.Done():
		a.log.Info("shutdown requested")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("http server failed", "err", err)
			code = 1
		}
	}
	_ = a.notify.Stopping()
	budget := a.cfg.ShutdownTimeout.D()

	// 1. HTTP.
	a.cancelBase()
	hctx, hcancel := context.WithTimeout(context.Background(), budget/2)
	if err := a.httpSrv.Shutdown(hctx); err != nil {
		a.log.Warn("http shutdown", "err", err)
		_ = a.httpSrv.Close()
	}
	hcancel()
	if pprofSrv != nil {
		_ = pprofSrv.Close()
	}
	dctx, dcancel := context.WithTimeout(context.Background(), drainBudget)
	if err := a.api.Drain(dctx); err != nil {
		a.log.Error("api handlers still running at shutdown", "err", err)
		code = 1
	}
	dcancel()

	// 2. Workers.
	cancelWork()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	wctx, wcancel := context.WithTimeout(context.Background(), budget/2)
	select {
	case <-done:
	default:
		select {
		case <-done:
		case <-wctx.Done():
			a.log.Error("workers did not stop before the shutdown timeout")
			code = 1
		}
	}
	wcancel()
	// The session loop ends the session on exit; this covers a loop that
	// died. recordEnd runs synchronously in the end hook (bounded 5 s).
	_, _ = a.sm.End("", session.EndShutdown)

	// 3. Database.
	if err := a.st.Close(); err != nil {
		a.log.Error("database close", "err", err)
		code = 1
	}
	a.log.Info("device agent stopped", "code", code)
	return code
}

// drainBudget bounds the wait for API handlers after HTTP shutdown.
const drainBudget = 5 * time.Second

// sessionLoop drives the session inactivity rules once per second and
// stamps its heartbeat; on exit it ends any open session (EndShutdown) so
// the cleanup hooks record it.
func (a *agent) sessionLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	stamp(&a.hb.session)
	for {
		select {
		case <-ctx.Done():
			_, _ = a.sm.End("", session.EndShutdown)
			return
		case <-t.C:
			a.sm.Tick()
			stamp(&a.hb.session)
		}
	}
}

// readyLoop retries the liveness self-check until it passes, then sends
// READY=1 and starts the watchdog.
func (a *agent) readyLoop(ctx context.Context, goFn func(string, func())) {
	for attempt := 1; ; attempt++ {
		err := a.liveness(ctx)
		if err == nil {
			_ = a.notify.Ready()
			a.log.Info("device agent ready", "attempts", attempt)
			if iv := sdnotify.WatchdogInterval(nil); iv > 0 {
				goFn("watchdog", func() { a.watchdog(ctx, iv) })
			}
			return
		}
		if attempt <= 3 || attempt%15 == 0 {
			a.log.Error("liveness self-check failed at startup; retrying", "err", err, "attempt", attempt)
		}
		if !sleepCtx(ctx, 2*time.Second) {
			return
		}
	}
}

// liveness: the database answers and the HTTP stack serves /status.
func (a *agent) liveness(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := a.st.Ping(pctx); err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, "http://"+a.cfg.Listen+"/api/v1/status", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("self probe: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("self probe: HTTP %d", resp.StatusCode)
	}
	stamp(&a.hb.http)
	return nil
}

// heartbeatGrace: loops may not have stamped yet right after startup.
func (a *agent) heartbeatGrace() time.Duration {
	return max(2*a.cfg.Health.DBInterval.D()+30*time.Second, 90*time.Second)
}

// staleLoop names the first long-lived loop whose heartbeat is missing or
// too old ("" = all alive). A database check that never completed is a
// stall, not "healthy".
func (a *agent) staleLoop() string {
	if time.Since(a.startedAt) < a.heartbeatGrace() {
		return ""
	}
	if d, ok := age(&a.hb.health); !ok || d > 3*a.cfg.Health.DBInterval.D()+30*time.Second {
		return "health"
	}
	if d, ok := age(&a.hb.session); !ok || d > 30*time.Second {
		return "session"
	}
	if a.cfg.Sync.BaseURL != "" {
		// The sync loop stamps LastAttemptAt on every upload attempt; it
		// may legitimately wait up to its interval or its backoff cap
		// (~38 min) between attempts.
		limit := 2*max(a.cfg.Sync.Interval.D(), 40*time.Minute) + 2*a.cfg.Sync.HTTPTimeout.D()
		last := a.sync.Status().LastAttemptAt
		if last.IsZero() || time.Since(last) > limit {
			return "sync"
		}
	}
	return ""
}

func (a *agent) watchdog(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := a.liveness(ctx)
		if err == nil {
			if loop := a.staleLoop(); loop != "" {
				err = fmt.Errorf("%s loop stalled (no heartbeat)", loop)
			}
		}
		if err != nil {
			// No WATCHDOG=1: systemd restarts us if this persists.
			a.log.Error("watchdog: liveness failed; not notifying systemd", "err", err)
			continue
		}
		_ = a.notify.Watchdog()
	}
}

// backupLoop writes a verified online backup once a day at the configured
// quiet hour, postponing while a citizen session is open.
func (a *agent) backupLoop(ctx context.Context) {
	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), a.cfg.Backup.Hour, 0, 0, 0, now.Location())
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		if !sleepCtx(ctx, time.Until(next)) {
			return
		}
		for a.sm.Current().ID != "" {
			if !sleepCtx(ctx, 10*time.Minute) {
				return
			}
		}
		bctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		start := time.Now()
		path, err := a.st.Backup(bctx, a.cfg.BackupDir(), a.cfg.Backup.Keep)
		cancel()
		if err != nil {
			a.log.Error("daily backup failed", "err", err)
			r := health.Result{State: health.Degraded, Detail: "backup_failed"}
			backupStatus.set(r)
			a.mon.Report(compBackup, r)
			continue
		}
		a.log.Info("daily backup written", "path", path, "ms", time.Since(start).Milliseconds())
		r := health.Result{State: health.Healthy, Data: map[string]any{"last_backup": start.UTC().Format(time.RFC3339)}}
		backupStatus.set(r)
		a.mon.Report(compBackup, r)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (a *agent) startPprof() *http.Server {
	if !a.cfg.IsDev() {
		// config.Validate refuses pprof_addr off dev profiles; never
		// serve it on a kiosk even if that check is bypassed.
		a.log.Error("pprof refused on a non-dev profile")
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	_, port, _ := net.SplitHostPort(a.cfg.PprofAddr)
	hostOK := map[string]bool{a.cfg.PprofAddr: true, "127.0.0.1:" + port: true, "localhost:" + port: true}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostOK[r.Host] { // DNS-rebinding protection, as on the main API
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
	s := &http.Server{Addr: a.cfg.PprofAddr, Handler: h, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 2 * time.Minute}
	go func() {
		if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("pprof server", "err", err)
		}
	}()
	a.log.Warn("pprof enabled (loopback only)", "addr", a.cfg.PprofAddr)
	return s
}
