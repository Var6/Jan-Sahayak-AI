// Package engine wraps the sherpa-onnx C API (via the official Go
// bindings) for the three models the speech worker hosts: Silero VAD,
// IndicConformer-Hindi offline CTC recognition, and Supertonic-3 TTS.
//
// None of the types here are safe for concurrent use. The HTTP layer
// serialises access per engine (one request at a time).
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/v2/sherpa_onnx"

	"github.com/janmanindia/jan-sahayak/services/speech-worker/internal/textsplit"
)

// SampleRate is the only input rate the recogniser and VAD accept.
const SampleRate = 16000

// STTConfig locates the recogniser and VAD models.
type STTConfig struct {
	VADModel   string
	ASRModel   string // NeMo CTC model.int8.onnx
	ASRTokens  string // tokens.txt
	NumThreads int
	Provider   string // "cpu"

	VADThreshold   float32 // speech probability threshold, default 0.5
	VADMinSilence  float32 // seconds, default 0.3
	VADMinSpeech   float32 // seconds, default 0.25
	PadBeforeSecs  float64 // audio kept before first speech, default 0.3
	PadAfterSecs   float64 // audio kept after last speech, default 0.3
	MaxBufferSecs  float32 // VAD internal buffer, default 30
	DisableVADTrim bool    // transcribe the whole clip (VAD still reports speech_secs)
}

// STTResult is one transcription.
type STTResult struct {
	Text       string
	Confidence float64
	NoSpeech   bool
	SpeechSecs float64
	Tokens     int
	SNRDB      float64 // 0 when not measurable
	// ConfidenceSource is "ys_log_probs" when derived from the model's
	// per-token log-probabilities, "heuristic" otherwise.
	ConfidenceSource string
}

// STT is VAD + offline CTC recogniser.
type STT struct {
	cfg STTConfig
	vad *sherpa.VoiceActivityDetector
	rec *sherpa.OfflineRecognizer
}

func checkFiles(paths ...string) error {
	for _, p := range paths {
		if p == "" {
			return errors.New("engine: empty model path")
		}
		st, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("engine: %w", err)
		}
		if st.IsDir() || st.Size() == 0 {
			return fmt.Errorf("engine: %s is not a non-empty file", p)
		}
	}
	return nil
}

// NewSTT loads the VAD and recogniser. It is slow (seconds) and memory heavy.
func NewSTT(cfg STTConfig) (*STT, error) {
	if err := checkFiles(cfg.VADModel, cfg.ASRModel, cfg.ASRTokens); err != nil {
		return nil, err
	}
	if cfg.NumThreads <= 0 {
		cfg.NumThreads = 4
	}
	if cfg.Provider == "" {
		cfg.Provider = "cpu"
	}
	if cfg.VADThreshold <= 0 {
		cfg.VADThreshold = 0.5
	}
	if cfg.VADMinSilence <= 0 {
		cfg.VADMinSilence = 0.3
	}
	if cfg.VADMinSpeech <= 0 {
		cfg.VADMinSpeech = 0.25
	}
	if cfg.PadBeforeSecs <= 0 {
		cfg.PadBeforeSecs = 0.3
	}
	if cfg.PadAfterSecs <= 0 {
		cfg.PadAfterSecs = 0.3
	}
	if cfg.MaxBufferSecs <= 0 {
		cfg.MaxBufferSecs = 30
	}
	vc := sherpa.VadModelConfig{
		SileroVad: sherpa.SileroVadModelConfig{
			Model:              cfg.VADModel,
			Threshold:          cfg.VADThreshold,
			MinSilenceDuration: cfg.VADMinSilence,
			MinSpeechDuration:  cfg.VADMinSpeech,
			WindowSize:         512,
			MaxSpeechDuration:  cfg.MaxBufferSecs,
		},
		SampleRate: SampleRate,
		NumThreads: 1,
		Provider:   cfg.Provider,
	}
	vad := sherpa.NewVoiceActivityDetector(&vc, cfg.MaxBufferSecs)
	if vad == nil {
		return nil, errors.New("engine: failed to create VAD")
	}
	rc := sherpa.OfflineRecognizerConfig{
		FeatConfig: sherpa.FeatureConfig{SampleRate: SampleRate, FeatureDim: 80},
		ModelConfig: sherpa.OfflineModelConfig{
			NemoCTC:    sherpa.OfflineNemoEncDecCtcModelConfig{Model: cfg.ASRModel},
			Tokens:     cfg.ASRTokens,
			NumThreads: cfg.NumThreads,
			Provider:   cfg.Provider,
			ModelType:  "nemo_ctc",
		},
		DecodingMethod: "greedy_search",
	}
	rec := sherpa.NewOfflineRecognizer(&rc)
	if rec == nil {
		sherpa.DeleteVoiceActivityDetector(vad)
		return nil, errors.New("engine: failed to create recogniser")
	}
	return &STT{cfg: cfg, vad: vad, rec: rec}, nil
}

// Close frees native memory. The STT must not be used afterwards.
func (s *STT) Close() {
	if s.rec != nil {
		sherpa.DeleteOfflineRecognizer(s.rec)
		s.rec = nil
	}
	if s.vad != nil {
		sherpa.DeleteVoiceActivityDetector(s.vad)
		s.vad = nil
	}
}

// segments runs the VAD over the whole clip and returns speech spans in
// sample indices.
func (s *STT) segments(samples []float32) []Span {
	s.vad.Reset()
	const win = 512
	for off := 0; off < len(samples); off += win {
		end := min(off+win, len(samples))
		chunk := samples[off:end]
		if len(chunk) < win { // pad the tail so the last window is processed
			padded := make([]float32, win)
			copy(padded, chunk)
			chunk = padded
		}
		s.vad.AcceptWaveform(chunk)
	}
	s.vad.Flush()
	var spans []Span
	for !s.vad.IsEmpty() {
		seg := s.vad.Front()
		s.vad.Pop()
		spans = append(spans, Span{Start: int(seg.Start), End: int(seg.Start) + len(seg.Samples)})
	}
	s.vad.Reset()
	return spans
}

// Transcribe runs VAD then recognition on 16 kHz mono samples.
func (s *STT) Transcribe(samples []float32) STTResult {
	spans := s.segments(samples)
	speech := SpeechSamples(spans)
	res := STTResult{SpeechSecs: float64(speech) / SampleRate}
	if len(spans) == 0 {
		res.NoSpeech = true
		return res
	}
	from, to := 0, len(samples)
	if !s.cfg.DisableVADTrim {
		from, to = TrimBounds(spans, len(samples),
			int(s.cfg.PadBeforeSecs*SampleRate), int(s.cfg.PadAfterSecs*SampleRate))
	}
	r := s.decode(samples[from:to])
	res.Text = cleanText(r.Text)
	res.Tokens = len(r.Tokens)
	if res.Text == "" {
		res.NoSpeech = true
		return res
	}
	db, ok := SNR(samples, SampleRate)
	if ok {
		res.SNRDB = math.Round(db*10) / 10
	}
	res.Confidence, res.ConfidenceSource = Confidence(r.YsLogProbs, len(r.Tokens), res.SpeechSecs, db, ok)
	return res
}

func (s *STT) decode(samples []float32) *sherpa.OfflineRecognizerResult {
	st := sherpa.NewOfflineStream(s.rec)
	defer sherpa.DeleteOfflineStream(st)
	st.AcceptWaveform(SampleRate, samples)
	s.rec.Decode(st)
	return st.GetResult()
}

// Warmup runs one recognition on a synthetic signal so the first real
// request does not pay one-off allocation/initialisation costs.
func (s *STT) Warmup() {
	sig := make([]float32, SampleRate)
	for i := range sig { // low-level deterministic noise
		sig[i] = float32((i*7919)%200-100) / 20000
	}
	_ = s.decode(sig)
	_ = s.segments(sig)
}

func cleanText(t string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(t, "▁", " ")), " ")
}

// TTSConfig locates the Supertonic model files.
type TTSConfig struct {
	Dir        string // directory holding the files below (used when a field is empty)
	NumThreads int
	Provider   string
	NumSteps   int    // flow-matching steps, default 4
	Lang       string // default language, "hi"
	// MaxChunkRunes bounds each synthesis call; longer text is split into
	// sentences first.
	MaxChunkRunes int
	// GapSecs of silence inserted between chunks.
	GapSecs float64
}

// TTS is a Supertonic-3 synthesiser.
type TTS struct {
	cfg  TTSConfig
	tts  *sherpa.OfflineTts
	rate int
	nspk int
}

// NewTTS loads Supertonic.
func NewTTS(cfg TTSConfig) (*TTS, error) {
	if cfg.NumThreads <= 0 {
		cfg.NumThreads = 4
	}
	if cfg.Provider == "" {
		cfg.Provider = "cpu"
	}
	if cfg.NumSteps <= 0 {
		cfg.NumSteps = 4
	}
	if cfg.Lang == "" {
		cfg.Lang = "hi"
	}
	if cfg.MaxChunkRunes <= 0 {
		cfg.MaxChunkRunes = 200
	}
	if cfg.GapSecs <= 0 {
		cfg.GapSecs = 0.2
	}
	p := func(name string) string { return strings.TrimRight(cfg.Dir, "/") + "/" + name }
	m := sherpa.OfflineTtsSupertonicModelConfig{
		DurationPredictor: p("duration_predictor.int8.onnx"),
		TextEncoder:       p("text_encoder.int8.onnx"),
		VectorEstimator:   p("vector_estimator.int8.onnx"),
		Vocoder:           p("vocoder.int8.onnx"),
		TtsJson:           p("tts.json"),
		UnicodeIndexer:    p("unicode_indexer.bin"),
		VoiceStyle:        p("voice.bin"),
	}
	if err := checkFiles(m.DurationPredictor, m.TextEncoder, m.VectorEstimator, m.Vocoder,
		m.TtsJson, m.UnicodeIndexer, m.VoiceStyle); err != nil {
		return nil, err
	}
	c := sherpa.OfflineTtsConfig{
		Model: sherpa.OfflineTtsModelConfig{
			Supertonic: m,
			NumThreads: cfg.NumThreads,
			Provider:   cfg.Provider,
		},
		MaxNumSentences: 1,
	}
	t := sherpa.NewOfflineTts(&c)
	if t == nil {
		return nil, errors.New("engine: failed to create TTS")
	}
	return &TTS{cfg: cfg, tts: t, rate: t.SampleRate(), nspk: t.NumSpeakers()}, nil
}

// Close frees native memory.
func (t *TTS) Close() {
	if t.tts != nil {
		sherpa.DeleteOfflineTts(t.tts)
		t.tts = nil
	}
}

// SampleRate is the model's native output rate.
func (t *TTS) SampleRate() int { return t.rate }

// NumSpeakers is the number of preset voices (valid sid range 0..n-1).
func (t *TTS) NumSpeakers() int { return t.nspk }

// ErrEmptyAudio is returned when the model produced no samples.
var ErrEmptyAudio = errors.New("engine: TTS produced no audio")

// Synthesize renders text sentence by sentence, checking ctx between
// sentences. maxSamples bounds the output (0 = unbounded).
func (t *TTS) Synthesize(ctx context.Context, text, lang string, sid int, speed float32, maxSamples int) ([]float32, int, error) {
	if lang == "" {
		lang = t.cfg.Lang
	}
	extra, err := json.Marshal(map[string]string{"lang": lang})
	if err != nil {
		return nil, 0, err
	}
	gen := sherpa.GenerationConfig{
		Speed:    speed,
		Sid:      sid,
		NumSteps: t.cfg.NumSteps,
		Extra:    extra,
	}
	gap := make([]float32, int(t.cfg.GapSecs*float64(t.rate)))
	var out []float32
	for i, chunk := range textsplit.Split(text, t.cfg.MaxChunkRunes) {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		a := t.tts.GenerateWithConfig(chunk, &gen, nil)
		if a == nil || len(a.Samples) == 0 {
			continue
		}
		if i > 0 && len(out) > 0 {
			out = append(out, gap...)
		}
		out = append(out, a.Samples...)
		if maxSamples > 0 && len(out) > maxSamples {
			return nil, 0, fmt.Errorf("engine: synthesised audio exceeds %d samples", maxSamples)
		}
	}
	if len(out) == 0 {
		return nil, 0, ErrEmptyAudio
	}
	return out, t.rate, nil
}

// Warmup synthesises one short phrase.
func (t *TTS) Warmup() error {
	_, _, err := t.Synthesize(context.Background(), "नमस्ते।", t.cfg.Lang, 0, 1, 0)
	return err
}
