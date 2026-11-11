// Command speech-worker hosts sherpa-onnx speech models (Silero VAD,
// IndicConformer-Hindi CTC STT, Supertonic-3 TTS) behind a loopback HTTP
// API for the device agent. It runs as its own process so native crashes
// or OOMs never touch the agent.
//
// Endpoints: POST /v1/transcribe, POST /v1/synthesize, GET /health.
// Audio is processed in memory only; nothing is written to disk.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/janmanindia/jan-sahayak/services/speech-worker/internal/engine"
	"github.com/janmanindia/jan-sahayak/services/speech-worker/internal/server"
)

type config struct {
	addr       string
	modelsDir  string
	vadModel   string
	asrModel   string
	asrTokens  string
	ttsDir     string
	threads    int
	ttsSteps   int
	ttsSID     int
	ttsLangs   string
	sttTimeout time.Duration
	ttsTimeout time.Duration
	busyWait   time.Duration
	ttsMaxRate int
	noWarmup   bool
}

// Shutdown budget. systemd's TimeoutStopSec for the unit is 45 s: HTTP
// shutdown (10 s) + draining in-flight inference (≤ 25 s) leaves margin
// before SIGKILL. An inference still running after the drain budget is
// abandoned without freeing native engines (the process exits anyway).
const (
	httpShutdownBudget = 10 * time.Second
	maxDrainBudget     = 25 * time.Second
)

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func parseFlags(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("speech-worker", flag.ContinueOnError)
	fs.StringVar(&c.addr, "addr", envStr("JS_SPEECH_ADDR", "127.0.0.1:8743"), "loopback listen address (env JS_SPEECH_ADDR)")
	fs.StringVar(&c.modelsDir, "models-dir", envStr("JS_MODELS_DIR", "models/weights"), "model weights root (env JS_MODELS_DIR)")
	fs.StringVar(&c.vadModel, "vad-model", os.Getenv("JS_VAD_MODEL"), "Silero VAD onnx (default <models-dir>/vad/silero_vad.onnx)")
	fs.StringVar(&c.asrModel, "asr-model", os.Getenv("JS_ASR_MODEL"), "NeMo CTC onnx (default <models-dir>/stt/indicconformer-hi/model.int8.onnx)")
	fs.StringVar(&c.asrTokens, "asr-tokens", os.Getenv("JS_ASR_TOKENS"), "tokens.txt (default next to the ASR model)")
	fs.StringVar(&c.ttsDir, "tts-dir", os.Getenv("JS_TTS_DIR"), "Supertonic model dir (default <models-dir>/tts/sherpa-onnx-supertonic-3-tts-int8-2026-05-11)")
	fs.IntVar(&c.threads, "threads", envInt("JS_SPEECH_THREADS", 4), "onnxruntime threads per engine (env JS_SPEECH_THREADS)")
	fs.IntVar(&c.ttsSteps, "tts-steps", envInt("JS_TTS_STEPS", 4), "Supertonic flow-matching steps (env JS_TTS_STEPS)")
	fs.IntVar(&c.ttsSID, "tts-sid", envInt("JS_TTS_SID", 0), "default Supertonic voice id (env JS_TTS_SID)")
	fs.StringVar(&c.ttsLangs, "tts-langs", envStr("JS_TTS_LANGS", "hi"), "comma-separated languages accepted by /v1/synthesize")
	fs.DurationVar(&c.sttTimeout, "stt-timeout", envDur("JS_STT_TIMEOUT", 15*time.Second), "per-request STT timeout")
	fs.DurationVar(&c.ttsTimeout, "tts-timeout", envDur("JS_TTS_TIMEOUT", 30*time.Second), "per-request TTS timeout")
	fs.DurationVar(&c.busyWait, "busy-wait", envDur("JS_BUSY_WAIT", 2*time.Second), "max wait for a busy engine before 503")
	fs.IntVar(&c.ttsMaxRate, "tts-max-rate", envInt("JS_TTS_MAX_RATE", 22050), "cap synthesised audio at this sample rate by halving (0 = native rate)")
	fs.BoolVar(&c.noWarmup, "no-warmup", false, "skip warm-up inference at boot")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.vadModel == "" {
		c.vadModel = filepath.Join(c.modelsDir, "vad", "silero_vad.onnx")
	}
	if c.asrModel == "" {
		c.asrModel = filepath.Join(c.modelsDir, "stt", "indicconformer-hi", "model.int8.onnx")
	}
	if c.asrTokens == "" {
		c.asrTokens = filepath.Join(filepath.Dir(c.asrModel), "tokens.txt")
	}
	if c.ttsDir == "" {
		c.ttsDir = filepath.Join(c.modelsDir, "tts", "sherpa-onnx-supertonic-3-tts-int8-2026-05-11")
	}
	if c.threads < 1 || c.threads > 16 {
		return c, fmt.Errorf("-threads must be 1..16")
	}
	if c.ttsMaxRate != 0 && (c.ttsMaxRate < 8000 || c.ttsMaxRate > 48000) {
		return c, fmt.Errorf("-tts-max-rate must be 0 or 8000..48000")
	}
	if c.ttsSteps < 1 || c.ttsSteps > 32 {
		return c, fmt.Errorf("-tts-steps must be 1..32")
	}
	host, _, err := net.SplitHostPort(c.addr)
	if err != nil {
		return c, fmt.Errorf("-addr: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return c, fmt.Errorf("-addr must be a loopback address, got %q", c.addr)
	}
	return c, nil
}

// sttAdapter converts engine results to the server's type.
type sttAdapter struct{ s *engine.STT }

func (a sttAdapter) Transcribe(samples []float32) server.STTResult {
	r := a.s.Transcribe(samples)
	return server.STTResult{
		Text: r.Text, Confidence: r.Confidence, ConfidenceSource: r.ConfidenceSource,
		NoSpeech: r.NoSpeech, SpeechSecs: r.SpeechSecs, SNRDB: r.SNRDB,
	}
}

type loaded struct {
	stt *engine.STT
	tts *engine.TTS
}

func load(c config, log *slog.Logger) (*loaded, map[string]any, error) {
	t0 := time.Now()
	stt, err := engine.NewSTT(engine.STTConfig{
		VADModel: c.vadModel, ASRModel: c.asrModel, ASRTokens: c.asrTokens, NumThreads: c.threads,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("load STT: %w", err)
	}
	sttMS := time.Since(t0).Milliseconds()
	log.Info("stt loaded", "ms", sttMS, "model", c.asrModel)

	t1 := time.Now()
	tts, err := engine.NewTTS(engine.TTSConfig{Dir: c.ttsDir, NumThreads: c.threads, NumSteps: c.ttsSteps, Lang: "hi"})
	if err != nil {
		stt.Close()
		return nil, nil, fmt.Errorf("load TTS: %w", err)
	}
	ttsMS := time.Since(t1).Milliseconds()
	log.Info("tts loaded", "ms", ttsMS, "sample_rate", tts.SampleRate(), "speakers", tts.NumSpeakers())
	if c.ttsSID < 0 || c.ttsSID >= max(tts.NumSpeakers(), 1) {
		stt.Close()
		tts.Close()
		return nil, nil, fmt.Errorf("-tts-sid %d out of range [0,%d)", c.ttsSID, tts.NumSpeakers())
	}

	var warmMS int64
	if !c.noWarmup {
		t2 := time.Now()
		stt.Warmup()
		if err := tts.Warmup(); err != nil {
			stt.Close()
			tts.Close()
			return nil, nil, fmt.Errorf("TTS warm-up: %w", err)
		}
		warmMS = time.Since(t2).Milliseconds()
		log.Info("warm-up done", "ms", warmMS)
	}
	info := map[string]any{
		"vad":              filepath.Base(c.vadModel),
		"stt":              "indicconformer-hi (nemo ctc, " + filepath.Base(c.asrModel) + ")",
		"stt_lang":         "hi",
		"tts":              "supertonic-3 (" + filepath.Base(c.ttsDir) + ")",
		"tts_langs":        strings.Split(c.ttsLangs, ","),
		"tts_sample_rate":  tts.SampleRate(),
		"tts_num_speakers": tts.NumSpeakers(),
		"tts_num_steps":    c.ttsSteps,
		"tts_default_sid":  c.ttsSID,
		"num_threads":      c.threads,
		"load_ms":          map[string]int64{"stt": sttMS, "tts": ttsMS, "warmup": warmMS},
	}
	return &loaded{stt: stt, tts: tts}, info, nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		log.Error("bad configuration", "err", err)
		return 2
	}

	srv := server.New(server.Config{
		STTTimeout: c.sttTimeout,
		TTSTimeout: c.ttsTimeout,
		BusyWait:   c.busyWait,
		TTSLangs:   strings.Split(c.ttsLangs, ","),
		DefaultSID: c.ttsSID,
		TTSMaxRate: c.ttsMaxRate,
	}, log)

	ln, err := net.Listen("tcp", c.addr)
	if err != nil {
		log.Error("listen failed", "addr", c.addr, "err", err)
		return 1
	}
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      max(c.sttTimeout, c.ttsTimeout) + 30*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()
	log.Info("listening (models loading)", "addr", c.addr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// A native inference that never returns holds its engine slot forever
	// and cannot be interrupted. Once it has run for 3× its timeout, exit
	// non-zero so systemd (Restart=always) starts a fresh worker.
	go srv.WatchWedged(ctx, time.Second, func(engine string, held time.Duration) {
		log.Error("inference hung; exiting so systemd restarts the worker",
			"engine", engine, "held_ms", held.Milliseconds(), "limit_factor", server.WedgedFactor)
		os.Exit(1)
	})

	type loadResult struct {
		l   *loaded
		err error
	}
	loadDone := make(chan loadResult, 1)
	go func() {
		l, info, err := load(c, log)
		if err == nil {
			srv.SetEngines(sttAdapter{l.stt}, l.tts, info)
			log.Info("ready")
		}
		loadDone <- loadResult{l, err}
	}()

	var eng *loaded
	exit := 0
wait:
	for {
		select {
		case r := <-loadDone:
			if r.err != nil {
				log.Error("model load failed", "err", r.err)
				exit = 1
				break wait
			}
			eng = r.l
			loadDone = nil // keep serving until a signal or server error
		case err := <-serveErr:
			log.Error("server stopped", "err", err)
			exit = 1
			break wait
		case <-ctx.Done():
			log.Info("shutdown signal received")
			break wait
		}
	}

	shCtx, cancel := context.WithTimeout(context.Background(), httpShutdownBudget)
	defer cancel()
	if err := hs.Shutdown(shCtx); err != nil {
		log.Warn("http shutdown", "err", err)
		_ = hs.Close()
	}
	if eng != nil {
		// Native engines may still be running a request that timed out;
		// free them only once every inference goroutine has returned.
		dCtx, dCancel := context.WithTimeout(context.Background(), min(max(c.sttTimeout, c.ttsTimeout), maxDrainBudget))
		defer dCancel()
		if err := srv.Drain(dCtx); err != nil {
			log.Warn("inference still running at exit; not freeing native engines", "err", err)
		} else {
			eng.stt.Close()
			eng.tts.Close()
		}
	}
	log.Info("stopped", "exit", exit)
	return exit
}
