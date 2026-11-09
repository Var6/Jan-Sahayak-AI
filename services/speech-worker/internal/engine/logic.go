package engine

import (
	"math"
	"sort"
)

// Span is a speech region in sample indices [Start, End).
type Span struct{ Start, End int }

// SpeechSamples sums span lengths.
func SpeechSamples(spans []Span) int {
	n := 0
	for _, s := range spans {
		if s.End > s.Start {
			n += s.End - s.Start
		}
	}
	return n
}

// TrimBounds returns the [from, to) sample range that covers all speech
// plus padding, clamped to [0, total). Pauses between spans are kept so
// the recogniser sees natural phrasing.
func TrimBounds(spans []Span, total, padBefore, padAfter int) (int, int) {
	if len(spans) == 0 || total <= 0 {
		return 0, max(total, 0)
	}
	first, last := spans[0].Start, spans[0].End
	for _, s := range spans[1:] {
		first = min(first, s.Start)
		last = max(last, s.End)
	}
	from := max(0, first-padBefore)
	to := min(total, last+padAfter)
	if to <= from {
		return 0, total
	}
	return from, to
}

// SNR estimates the speech-to-background ratio in dB without relying on
// VAD boundaries: the clip is cut into 20 ms frames, and the ratio of the
// 90th-percentile frame power (loud speech) to the 10th-percentile frame
// power (background / pauses between words) is returned. ok is false for
// clips shorter than 10 frames. Digital silence is floored at -100 dBFS so
// the ratio stays finite.
func SNR(samples []float32, rate int) (db float64, ok bool) {
	frame := rate / 50
	if frame <= 0 || len(samples) < 10*frame {
		return 0, false
	}
	pw := make([]float64, 0, len(samples)/frame)
	for off := 0; off+frame <= len(samples); off += frame {
		var p float64
		for _, v := range samples[off : off+frame] {
			p += float64(v) * float64(v)
		}
		pw = append(pw, p/float64(frame))
	}
	sort.Float64s(pw)
	lo := pw[len(pw)/10]
	hi := pw[len(pw)*9/10]
	const floor = 1e-10
	return 10 * math.Log10(math.Max(hi, floor)/math.Max(lo, floor)), true
}

// Confidence turns recogniser output into a 0..1 score and names its
// source.
//
// "ys_log_probs": if the runtime returns per-token log-probabilities the
// score is exp(mean log-prob) — the geometric mean of the chosen tokens'
// posteriors, the usual utterance confidence — times rateFactor.
//
// "heuristic": sherpa-onnx 2.0.1 does NOT return log-probs for offline
// NeMo CTC models (verified: YsLogProbs is empty), so in practice this is
// the path used. The score is
//
//	0.8 × rateFactor(tokens/speech second) × snrFactor(SNR dB)
//
// It is not a probability. It only captures two observable failure modes:
// an implausible number of tokens for the amount of speech the VAD heard,
// and a noisy recording. 0.8 is the ceiling for clean, plausible input.
// Callers should treat < 0.5 as "ask the citizen to repeat".
func Confidence(logProbs []float32, tokens int, speechSecs, snrDB float64, snrOK bool) (float64, string) {
	rf := rateFactor(tokens, speechSecs)
	if len(logProbs) > 0 && len(logProbs) == tokens {
		var sum float64
		valid := true
		for _, lp := range logProbs {
			v := float64(lp)
			if math.IsNaN(v) || math.IsInf(v, 0) {
				valid = false
				break
			}
			sum += min(v, 0)
		}
		if valid {
			return clamp01(math.Exp(sum/float64(len(logProbs))) * rf), "ys_log_probs"
		}
	}
	return clamp01(0.8 * rf * snrFactor(snrDB, snrOK)), "heuristic"
}

// rateFactor penalises token rates outside what Hindi speech produces with
// this model's BPE vocabulary. On the synthetic test clips the rate is
// 7.7–9.5 tokens per second of VAD speech; 2–16 is accepted without
// penalty, with a linear fall-off outside that band.
func rateFactor(tokens int, speechSecs float64) float64 {
	if tokens <= 0 {
		return 0
	}
	if speechSecs < 0.2 {
		speechSecs = 0.2
	}
	r := float64(tokens) / speechSecs
	switch {
	case r < 2:
		return 0.4 + 0.3*r
	case r > 16:
		return math.Max(0.3, 16/r)
	}
	return 1
}

// snrFactor maps SNR to [0.4, 1]: ≥ 20 dB → 1, ≤ 0 dB → 0.4, linear in
// between. Unknown SNR (clip shorter than 200 ms) → 0.85.
func snrFactor(db float64, ok bool) float64 {
	if !ok || math.IsNaN(db) {
		return 0.85
	}
	switch {
	case db >= 20:
		return 1
	case db <= 0:
		return 0.4
	}
	return 0.4 + 0.6*db/20
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}
