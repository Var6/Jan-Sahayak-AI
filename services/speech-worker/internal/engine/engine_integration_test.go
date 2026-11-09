package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janmanindia/jan-sahayak/services/speech-worker/internal/audio"
)

// Integration tests load the real models. They run only when
// JS_MODELS_DIR points at models/weights (and JS_TESTAUDIO_DIR at
// var/testaudio for the STT test). They are memory heavy (~700 MB).

func modelsDir(t *testing.T) string {
	d := os.Getenv("JS_MODELS_DIR")
	if d == "" {
		t.Skip("JS_MODELS_DIR not set")
	}
	return d
}

func TestIntegrationSTT(t *testing.T) {
	md := modelsDir(t)
	ad := os.Getenv("JS_TESTAUDIO_DIR")
	if ad == "" {
		t.Skip("JS_TESTAUDIO_DIR not set")
	}
	s, err := NewSTT(STTConfig{
		VADModel:   filepath.Join(md, "vad/silero_vad.onnx"),
		ASRModel:   filepath.Join(md, "stt/indicconformer-hi/model.int8.onnx"),
		ASRTokens:  filepath.Join(md, "stt/indicconformer-hi/tokens.txt"),
		NumThreads: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Silence must be reported as no speech without running the recogniser.
	if r := s.Transcribe(make([]float32, SampleRate)); !r.NoSpeech || r.Text != "" {
		t.Fatalf("silence: %+v", r)
	}

	want := map[string]string{"t1.wav": "लोन", "t2.wav": "प्रमाण", "t3.wav": "पेंशन", "t4.wav": "किसान"}
	for name, word := range want {
		b, err := os.ReadFile(filepath.Join(ad, name))
		if err != nil {
			t.Fatal(err)
		}
		pcm, _, err := audio.DecodeMono16(b, SampleRate, 20)
		if err != nil {
			t.Fatal(err)
		}
		r := s.Transcribe(pcm)
		t.Logf("%s: %q conf=%.3f (%s) tokens=%d speech=%.2fs", name, r.Text, r.Confidence, r.ConfidenceSource, r.Tokens, r.SpeechSecs)
		if r.NoSpeech || !strings.Contains(r.Text, word) {
			t.Errorf("%s: transcript %q lacks %q", name, r.Text, word)
		}
		if r.Confidence <= 0 || r.Confidence > 1 {
			t.Errorf("%s: confidence %v", name, r.Confidence)
		}
	}
}

func TestIntegrationTTS(t *testing.T) {
	md := modelsDir(t)
	tts, err := NewTTS(TTSConfig{Dir: filepath.Join(md, "tts/sherpa-onnx-supertonic-3-tts-int8-2026-05-11"), NumThreads: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer tts.Close()
	t.Logf("rate=%d speakers=%d", tts.SampleRate(), tts.NumSpeakers())
	pcm, rate, err := tts.Synthesize(context.Background(), "नमस्ते। आपका स्वागत है।", "hi", 0, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rate <= 0 || len(pcm) < rate/2 {
		t.Fatalf("rate=%d samples=%d", rate, len(pcm))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := tts.Synthesize(ctx, "नमस्ते।", "hi", 0, 1, 0); err == nil {
		t.Fatal("cancelled context should abort")
	}
}
