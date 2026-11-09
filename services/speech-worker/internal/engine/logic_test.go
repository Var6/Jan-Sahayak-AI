package engine

import (
	"math"
	"testing"
)

func TestTrimBounds(t *testing.T) {
	cases := []struct {
		spans          []Span
		total, pb, pa  int
		wantFrom, want int
	}{
		{nil, 100, 5, 5, 0, 100},
		{[]Span{{10, 20}}, 100, 5, 5, 5, 25},
		{[]Span{{2, 20}, {50, 98}}, 100, 5, 5, 0, 100},
		{[]Span{{30, 40}, {10, 20}}, 100, 3, 4, 7, 44},
	}
	for _, c := range cases {
		f, to := TrimBounds(c.spans, c.total, c.pb, c.pa)
		if f != c.wantFrom || to != c.want {
			t.Errorf("%v: got [%d,%d) want [%d,%d)", c.spans, f, to, c.wantFrom, c.want)
		}
	}
	if n := SpeechSamples([]Span{{0, 10}, {20, 25}, {5, 1}}); n != 15 {
		t.Fatalf("SpeechSamples=%d", n)
	}
}

func TestConfidence(t *testing.T) {
	// all-certain tokens at a normal rate → 1
	c, src := Confidence([]float32{0, 0, 0, 0}, 4, 1, 30, true)
	if src != "ys_log_probs" || math.Abs(c-1) > 1e-9 {
		t.Fatalf("got %v %s", c, src)
	}
	// geometric mean of 0.5
	lp := float32(math.Log(0.5))
	c, _ = Confidence([]float32{lp, lp, lp}, 3, 1, 30, true)
	if math.Abs(c-0.5) > 1e-6 {
		t.Fatalf("got %v", c)
	}
	// no log-probs, clean audio, plausible rate → heuristic ceiling 0.8
	c, src = Confidence(nil, 8, 1, 40, true)
	if src != "heuristic" || math.Abs(c-0.8) > 1e-9 {
		t.Fatalf("got %v %s", c, src)
	}
	// noisy audio lowers the heuristic
	noisy, _ := Confidence(nil, 8, 1, 5, true)
	if noisy >= c || noisy < 0.4 {
		t.Fatalf("noisy=%v clean=%v", noisy, c)
	}
	// unknown SNR sits between
	unk, _ := Confidence(nil, 8, 1, 0, false)
	if unk >= c || unk <= noisy {
		t.Fatalf("unknown=%v", unk)
	}
	// one token for 10 s of speech is penalised below the repeat threshold
	c, _ = Confidence(nil, 1, 10, 40, true)
	if c >= 0.5 {
		t.Fatalf("low-rate not penalised: %v", c)
	}
	// zero tokens → 0
	if c, _ := Confidence(nil, 0, 1, 40, true); c != 0 {
		t.Fatalf("got %v", c)
	}
	// NaN log-prob → heuristic, still within range
	c, src = Confidence([]float32{float32(math.NaN())}, 1, 0.3, 0, false)
	if src != "heuristic" || c < 0 || c > 1 {
		t.Fatalf("got %v %s", c, src)
	}
}

func TestSNR(t *testing.T) {
	s := make([]float32, 16000) // 50 frames of 20 ms
	for i := range s {
		s[i] = 0.005 // background
	}
	for i := 4000; i < 12000; i++ {
		s[i] = 0.5 // "speech"
	}
	// 20·log10(0.5/0.005) = 40 dB
	db, ok := SNR(s, 16000)
	if !ok || math.Abs(db-40) > 0.1 {
		t.Fatalf("db=%v ok=%v", db, ok)
	}
	if _, ok := SNR(s[:1000], 16000); ok {
		t.Fatal("too short → not measurable")
	}
	if db, ok := SNR(make([]float32, 16000), 16000); !ok || db != 0 {
		t.Fatalf("digital silence: db=%v", db)
	}
}
