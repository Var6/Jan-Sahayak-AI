package dialogue

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/ai"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/knowledge"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/store"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/textnorm"
)

var testNow = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

const fixtureRecords = "../knowledge/testdata/records"
const gazetteerPath = "../../../../packages/scheme-knowledge/gazetteer/bihar_districts.json"

// fakeLLM is a test double for the llama-server process (an external
// runtime). Classification and rephrase answers are scripted.
type fakeLLM struct {
	mu        sync.Mutex
	healthy   bool
	classify  string // raw JSON returned for schema-constrained calls
	rephrase  string // returned for free-text calls
	err       error
	calls     int
	lastMsgs  []ai.Message
	allMsgs   [][]ai.Message
	schemaUse int
}

func (f *fakeLLM) Complete(ctx context.Context, req ai.CompletionRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastMsgs = req.Messages
	f.allMsgs = append(f.allMsgs, req.Messages)
	if f.err != nil {
		return "", f.err
	}
	if req.JSONSchema != nil {
		f.schemaUse++
		return f.classify, nil
	}
	return f.rephrase, nil
}

func (f *fakeLLM) Health(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.healthy {
		return ai.ErrUnavailable
	}
	return nil
}

func (f *fakeLLM) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type engOpts struct {
	kcfg           knowledge.Config
	llm            ai.LLM
	rephrase       bool
	noHelplines    bool
	emptyKnowledge bool
}

func newKnowledge(t testing.TB, kcfg knowledge.Config, load bool) *knowledge.Store {
	t.Helper()
	st, _, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "device.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if kcfg.Now == nil {
		kcfg.Now = func() time.Time { return testNow }
	}
	ks, err := knowledge.New(context.Background(), st.DB, kcfg)
	if err != nil {
		t.Fatal(err)
	}
	if load {
		if _, err := ks.LoadDir(context.Background(), fixtureRecords, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	return ks
}

func newEngine(t testing.TB, o engOpts) *Conversation {
	t.Helper()
	ks := newKnowledge(t, o.kcfg, !o.emptyKnowledge)
	g, err := knowledge.LoadGazetteer(gazetteerPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Knowledge: ks, Gazetteer: g, LLM: o.llm, EnableRephrase: o.rephrase}
	if !o.noHelplines {
		h, err := LoadHelplines("testdata/helplines_fixture.json", testNow)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Helplines = h
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func say(t testing.TB, e *Conversation, st *State, text string) Reply {
	t.Helper()
	r, err := e.Handle(context.Background(), st, Input{Text: text, Source: FromText})
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, st, r)
	return r
}

func tap(t testing.TB, e *Conversation, st *State, id string) Reply {
	t.Helper()
	r, err := e.Handle(context.Background(), st, Input{OptionID: id, Source: FromOption})
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, st, r)
	return r
}

func hasOption(r Reply, id string) bool {
	for _, o := range r.Options {
		if o.ID == id {
			return true
		}
	}
	return false
}

func cardIDs(r Reply) []string {
	var ids []string
	for _, c := range r.Cards {
		ids = append(ids, c.SchemeID)
	}
	return ids
}

var (
	analyticsValueRe = regexp.MustCompile(`^[a-z0-9_\-]*$`)
	forbiddenClaims  = []string{"ज़रूर मिलेगा", "जरूर मिलेगा", "ज़रूर मिलेगी", "पक्का मिलेगा", "आवेदन जमा हो गया", "आवेदन जमा कर दिया",
		"approved", "has been submitted", "application submitted", "guaranteed"}
)

// checkInvariants holds for every reply in every test.
func checkInvariants(t testing.TB, st *State, r Reply) {
	t.Helper()
	if r.Kind == "" || r.Text == "" {
		t.Fatalf("empty reply %+v", r)
	}
	for _, v := range []string{r.Analytics.Intent, r.Analytics.Category, r.Analytics.Service, r.Analytics.Resolution} {
		if !analyticsValueRe.MatchString(v) {
			t.Fatalf("analytics carries free text: %+v", r.Analytics)
		}
	}
	if r.Analytics.Intent == "" {
		t.Fatalf("analytics intent missing: %+v", r)
	}
	low := strings.ToLower(r.Text + " " + r.Speech)
	for _, c := range forbiddenClaims {
		if strings.Contains(low, strings.ToLower(c)) {
			t.Fatalf("reply makes forbidden claim %q: %s", c, r.Text)
		}
	}
	if urlRe.MatchString(r.Speech) {
		t.Fatalf("speech contains URL: %s", r.Speech)
	}
	if r.Kind != KindGoodbye && (!hasOption(r, optRepeat) || !hasOption(r, optEnd)) {
		t.Fatalf("reply must offer repeat and end: %+v", r.Options)
	}
	if len(r.Options) > maxOptions {
		t.Fatalf("too many options: %d", len(r.Options))
	}
	if len(st.History) > MaxHistory || len(st.Slots) > MaxSlots {
		t.Fatalf("state unbounded: history %d slots %d", len(st.History), len(st.Slots))
	}
	for _, c := range r.Cards {
		if c.Verified && c.Disclaimer == "" {
			t.Fatal("verified card without disclaimer")
		}
	}
}

func newState() *State { return NewState(Hindi, testNow) }

var errDown = errors.New("llama down")

func digits(s string) bool {
	for _, r := range textnorm.Fold(s) {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}
