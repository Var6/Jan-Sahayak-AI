package dialogue

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/ai"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/knowledge"
)

// TestIntegrationRealLLM runs the classifier and the rephrase+validator
// against a real llama-server. Opt-in:
//
//	JS_LLM_URL=http://127.0.0.1:8742 go test -run TestIntegrationRealLLM -v ./services/device-agent/internal/dialogue/
//
// It reports latency and validator accept/reject counts; it fails only on
// transport errors or if the validator lets through an output that
// violates the rules (which would be a validator bug).
func TestIntegrationRealLLM(t *testing.T) {
	url := os.Getenv("JS_LLM_URL")
	if url == "" {
		t.Skip("set JS_LLM_URL to run against a real llama-server")
	}
	llm := &recordingLLM{LLM: ai.NewLlama(ai.LlamaConfig{BaseURL: url, Timeout: 60 * time.Second, MaxTokens: 256})}
	ks := newKnowledge(t, knowledge.Config{}, true)
	e, err := New(Config{Knowledge: ks, LLM: llm, EnableRephrase: true,
		ClassifyTimeout: 45 * time.Second, RephraseTimeout: 60 * time.Second, HealthTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if !e.llmUsable(ctx) {
		t.Fatal("llama-server not healthy at " + url)
	}

	// --- classifier
	classCases := []struct{ in, want string }{
		{"मुझे लोन चाहिए।", IntentLoan},
		{"मुझे जाति प्रमाण पत्र बनवाना है।", IntentCertificate},
		{"मेरी माँ की उम्र सत्तर साल है, उन्हें वृद्धावस्था पेंशन कैसे मिलेगी?", IntentPension},
		{"मैं गया जिले का किसान हूँ, पीएम किसान का पैसा नहीं आया।", IntentFarmer},
		{"मेरे बेटे की कॉलेज की फीस भरने में मदद चाहिए", IntentScholarship},
		{"गर्भवती महिलाओं के लिए कोई सरकारी मदद है?", IntentWomenChild},
		{"मुझे अपनी दुकान खोलनी है", IntentSelfEmployment},
		{"कल भारत का मैच कौन जीतेगा", IntentEntertainment},
		{"सिर में दर्द है कौन सी गोली लूँ", IntentMedical},
		{"मेरा राशन बंद हो गया है", IntentRationCard},
	}
	var lat []time.Duration
	correct := 0
	for _, c := range classCases {
		t0 := time.Now()
		got, ok := e.llmClassify(ctx, c.in)
		d := time.Since(t0)
		lat = append(lat, d)
		if !ok {
			t.Errorf("classify %q failed", c.in)
			continue
		}
		if got == c.want {
			correct++
		}
		t.Logf("classify %-6s %5dms want=%-18s got=%-18s | %s", map[bool]string{true: "OK", false: "MISS"}[got == c.want], d.Milliseconds(), c.want, got, c.in)
	}
	t.Logf("CLASSIFIER: %d/%d correct, latency %s", correct, len(classCases), latencySummary(lat))

	// --- rephrase + validator over every factual fixture record
	var recs []scheme.Record
	for _, id := range []string{"test-elderly-pension", "test-caste-certificate", "test-income-certificate", "test-farmer-support", "test-business-loan", "test-farm-loan", "test-ration-card"} {
		r, err := ks.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	lat = nil
	for _, r := range recs {
		p := composeAnswer(r, knowledge.ModeFactual, Hindi, nil, false)
		before := e.Stats()
		t0 := time.Now()
		out, used := e.rephrase(ctx, p.summary, []scheme.Record{r}, Hindi)
		d := time.Since(t0)
		lat = append(lat, d)
		after := e.Stats()
		status := "REJECTED"
		if used {
			status = "ACCEPTED"
			// Re-validate independently: an accepted output must pass.
			if reason := e.validateRephrase(out, p.summary, []scheme.Record{r}); reason != "" {
				t.Errorf("validator inconsistency: accepted output fails re-validation (%s)", reason)
			}
		} else if after.RephraseFailed > before.RephraseFailed {
			status = "LLM_ERROR"
		}
		t.Logf("rephrase %-9s %5dms | src: %s", status, d.Milliseconds(), p.summary)
		if used {
			t.Logf("                          out: %s", out)
		} else if status == "REJECTED" {
			t.Logf("                          out: %s  [reason: %s]", llm.last, e.validateRephrase(strings.TrimSpace(llm.last), p.summary, []scheme.Record{r}))
		}
	}
	// Show what the model produced for rejected cases (reason in log line).
	s := e.Stats()
	t.Logf("REPHRASE: accepted=%d rejected=%d llm_errors=%d, latency %s", s.RephraseAccepted, s.RephraseRejected, s.RephraseFailed, latencySummary(lat))

	// --- full turn through Handle with the real LLM
	st := newState()
	say(t, e, st, "वृद्धावस्था पेंशन")
	t0 := time.Now()
	r := say(t, e, st, "हाँ")
	t.Logf("FULL TURN: kind=%s used_llm=%v latency=%dms speech=%q", r.Kind, r.UsedLLM, time.Since(t0).Milliseconds(), r.Speech)
	if r.Kind != KindAnswer || len(r.Cards) != 1 || !r.Cards[0].Verified {
		t.Fatalf("full turn: %+v", r)
	}
	if strings.Contains(r.Speech, "5000") {
		t.Fatal("hallucination leaked")
	}
}

// recordingLLM keeps the last raw output for the report.
type recordingLLM struct {
	ai.LLM
	last string
}

func (r *recordingLLM) Complete(ctx context.Context, req ai.CompletionRequest) (string, error) {
	out, err := r.LLM.Complete(ctx, req)
	r.last = out
	return out, err
}

func latencySummary(ds []time.Duration) string {
	if len(ds) == 0 {
		return "n/a"
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var sum time.Duration
	for _, d := range s {
		sum += d
	}
	return "min=" + s[0].Round(time.Millisecond).String() + " median=" + s[len(s)/2].Round(time.Millisecond).String() +
		" max=" + s[len(s)-1].Round(time.Millisecond).String() + " mean=" + (sum / time.Duration(len(s))).Round(time.Millisecond).String()
}
