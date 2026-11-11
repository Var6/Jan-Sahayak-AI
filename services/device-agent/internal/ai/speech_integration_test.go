package ai

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Runs only against a live speech worker:
//
//	JS_SPEECH_URL=http://127.0.0.1:8743 JS_TESTAUDIO_DIR=$PWD/var/testaudio go test ./services/device-agent/internal/ai -run Integration -v
func liveSpeech(t *testing.T) string {
	u := os.Getenv("JS_SPEECH_URL")
	if u == "" {
		t.Skip("JS_SPEECH_URL not set")
	}
	return u
}

func TestIntegrationSpeechSTT(t *testing.T) {
	u := liveSpeech(t)
	dir := os.Getenv("JS_TESTAUDIO_DIR")
	if dir == "" {
		t.Skip("JS_TESTAUDIO_DIR not set")
	}
	stt := NewSpeechSTT(SpeechConfig{BaseURL: u})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := stt.Health(ctx); err != nil {
		t.Fatal(err)
	}
	wav, err := os.ReadFile(filepath.Join(dir, "t4.wav"))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := stt.Transcribe(ctx, wav, "hi")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", tr)
	if !strings.Contains(tr.Text, "किसान") || tr.Confidence <= 0 {
		t.Fatalf("unexpected transcript %+v", tr)
	}
}

// TestIntegrationSpeechRoundTrip: display text with digits/symbols →
// SpeechTTS (PrepareForTTS) → 44.1 kHz WAV → resample → SpeechSTT. The
// recogniser should hear the number words, proving the normalised text is
// spoken intelligibly.
func TestIntegrationSpeechRoundTrip(t *testing.T) {
	u := liveSpeech(t)
	tts := NewSpeechTTS(SpeechConfig{BaseURL: u})
	stt := NewSpeechSTT(SpeechConfig{BaseURL: u})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cases := []struct{ text, expect string }{
		{"आपकी उम्र 70 साल है।", "सत्तर"},
		{"राशि ₹1,20,000 है।", "लाख"},
		{"अंतिम तिथि 15/12/2026 है।", "पंद्रह"}, // STT spells दिसंबर as दिसमबर
		{"CSC पर e-KYC कराएँ।", "सीएससी"},       // e-KYC comes back as "ईके वई सी"
	}
	for _, c := range cases {
		wav, err := tts.Synthesize(ctx, c.text, "hi")
		if err != nil {
			t.Fatalf("%q: %v", c.text, err)
		}
		info, err := parsePCMOutputWAV(wav)
		if err != nil {
			t.Fatal(err)
		}
		pcm := make([]float64, info.DataBytes/2)
		for i := range pcm {
			pcm[i] = float64(int16(binary.LittleEndian.Uint16(wav[44+2*i:]))) / 32768
		}
		in16 := resampleTo16k(pcm, info.SampleRate)
		tr, err := stt.Transcribe(ctx, testWAVFrom(in16), "hi")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%q → TTS %q → STT %q (conf %.2f)", c.text, PrepareForTTS(c.text), tr.Text, tr.Confidence)
		if !strings.Contains(tr.Text, c.expect) {
			t.Errorf("%q: STT %q lacks %q", c.text, tr.Text, c.expect)
		}
	}
}

func resampleTo16k(in []float64, rate int) []float64 {
	const W = 32
	ratio := float64(rate) / 16000
	fc := 0.45 * 16000 / float64(rate)
	out := make([]float64, int(float64(len(in))/ratio))
	for i := range out {
		c := float64(i) * ratio
		k0 := int(c)
		var acc float64
		for k := k0 - W; k <= k0+W; k++ {
			if k < 0 || k >= len(in) {
				continue
			}
			x := float64(k) - c
			h := 2 * fc
			if x != 0 {
				h = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
			}
			acc += h * 0.5 * (1 + math.Cos(math.Pi*x/(W+1))) * in[k]
		}
		out[i] = acc
	}
	return out
}

func testWAVFrom(s []float64) []byte {
	b := testWAV(16000, len(s))
	for i, v := range s {
		v = math.Max(-1, math.Min(1, v))
		binary.LittleEndian.PutUint16(b[44+2*i:], uint16(int16(v*32767)))
	}
	return b
}
