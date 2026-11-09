package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/store"
)

var fixedNow = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

func openStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	st, _, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "device.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return fixedNow }
	}
	ks, err := New(context.Background(), st.DB, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

func loadFixtures(t *testing.T, ks *Store) Report {
	t.Helper()
	rep, err := ks.LoadDir(context.Background(), "testdata/records", "fixture-1")
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestLoadDirValidatesAndRecordsVersion(t *testing.T) {
	ks := openStore(t, Config{})
	rep := loadFixtures(t, ks)
	if rep.Loaded != 10 || len(rep.Skipped) != 0 {
		t.Fatalf("report %+v", rep)
	}
	// VERIFIED and not overdue: elderly, caste, income, farmer, business, farm loan, ration.
	if rep.Factual != 7 {
		t.Fatalf("factual = %d", rep.Factual)
	}
	v, err := ks.BundleVersion(context.Background())
	if err != nil || v != "fixture-1" {
		t.Fatalf("bundle version %q %v", v, err)
	}
	var n int
	if err := ks.db.QueryRow(`SELECT count(*) FROM schemes_fts`).Scan(&n); err != nil || n != 10 {
		t.Fatalf("fts rows %d %v", n, err)
	}
}

func TestInvalidRecordsSkippedAndReported(t *testing.T) {
	dir := t.TempDir()
	for _, src := range []string{"testdata/records/test-elderly-pension.json", "testdata/invalid/bad-id.json",
		"testdata/invalid/ai-verified.json", "testdata/invalid/unknown-field.json", "testdata/invalid/broken.json"} {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(src)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// duplicate id under another file name
	b, _ := os.ReadFile("testdata/records/test-elderly-pension.json")
	os.WriteFile(filepath.Join(dir, "zz-duplicate.json"), b, 0o600)

	ks := openStore(t, Config{})
	rep, err := ks.LoadDir(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Loaded != 1 || len(rep.Skipped) != 5 {
		t.Fatalf("report %+v", rep)
	}
	if !strings.HasPrefix(rep.BundleVersion, "local-") {
		t.Fatalf("version %q", rep.BundleVersion)
	}
	reasons := ""
	for _, s := range rep.Skipped {
		reasons += s.Source + ":" + s.Reason + "\n"
	}
	for _, want := range []string{"lowercase slug", "human reviewer", "unknown field", "duplicate id", "broken.json"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("skip reasons missing %q:\n%s", want, reasons)
		}
	}
}

func TestReplaceIsAtomicAndRefusesEmptyResult(t *testing.T) {
	ks := openStore(t, Config{})
	loadFixtures(t, ks)
	ctx := context.Background()
	// All-invalid input must not wipe the existing knowledge base.
	_, err := ks.Replace(ctx, "bad-2", []scheme.Record{{ID: "Bad"}})
	if !errors.Is(err, ErrNothingValid) {
		t.Fatalf("want ErrNothingValid, got %v", err)
	}
	if v, _ := ks.BundleVersion(ctx); v != "fixture-1" {
		t.Fatalf("version changed to %q", v)
	}
	if _, err := ks.Get(ctx, "test-elderly-pension"); err != nil {
		t.Fatal("old record lost:", err)
	}
	// A valid smaller set replaces everything.
	r, _ := ks.Get(ctx, "test-ration-card")
	rep, err := ks.Replace(ctx, "v3", []scheme.Record{r})
	if err != nil || rep.Loaded != 1 {
		t.Fatal(rep, err)
	}
	if _, err := ks.Get(ctx, "test-elderly-pension"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale record survived replace: %v", err)
	}
	var n int
	ks.db.QueryRow(`SELECT count(*) FROM schemes_fts`).Scan(&n)
	if n != 1 {
		t.Fatalf("fts rows %d", n)
	}
	if hits := ks.MatchNames("टेस्ट वृद्धजन पेंशन"); len(hits) != 0 {
		t.Fatalf("name index stale: %+v", hits)
	}
}

func TestLoadBundleFile(t *testing.T) {
	ks := openStore(t, Config{})
	var recs []scheme.Record
	for _, f := range []string{"test-caste-certificate.json", "test-income-certificate.json"} {
		b, _ := os.ReadFile(filepath.Join("testdata/records", f))
		var r scheme.Record
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	bun := scheme.Bundle{BundleVersion: "2026-11-09.test", CreatedAt: "2026-11-09T00:00:00Z", Records: recs}
	p := filepath.Join(t.TempDir(), "bundle.json")
	b, _ := json.Marshal(bun)
	os.WriteFile(p, b, 0o600)

	if _, err := ks.LoadPath(context.Background(), p, "", func(scheme.Bundle) error { return errors.New("bad signature") }); err == nil {
		t.Fatal("verify error must abort")
	}
	if v, _ := ks.BundleVersion(context.Background()); v != "" {
		t.Fatal("aborted load wrote meta")
	}
	rep, err := ks.LoadPath(context.Background(), p, "", nil)
	if err != nil || rep.Loaded != 2 || rep.BundleVersion != "2026-11-09.test" {
		t.Fatal(rep, err)
	}
	cs, _ := ks.ByCategory(context.Background(), "certificate")
	if len(cs) != 2 {
		t.Fatalf("by category %d", len(cs))
	}
}

func TestSearchRanksGarbledQueries(t *testing.T) {
	ks := openStore(t, Config{})
	loadFixtures(t, ks)
	cases := []struct{ q, want string }{
		{"मुझे जाति प्रमाण पत्र बनवाना है", "test-caste-certificate"},
		{"जाती प्रमान पत्र", "test-caste-certificate"},
		{"jaati praman patra", "test-caste-certificate"},
		{"caste certificate", "test-caste-certificate"},
		{"व्रिधावस्था पेशन", "test-elderly-pension"},
		{"old age pension", "test-elderly-pension"},
		{"राशन कोड", "test-ration-card"},
		{"आय प्रमाण पत्र", "test-income-certificate"},
		{"टेस्ट किसान सहायता", "test-farmer-support"},
		{"खेती के लिए लोन", "test-farm-loan"},
		{"dukan ke liye loan", "test-business-loan"},
	}
	for _, c := range cases {
		got, err := ks.Search(context.Background(), c.q, SearchOptions{Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 || got[0].Record.ID != c.want {
			ids := []string{}
			for _, g := range got {
				ids = append(ids, g.Record.ID)
			}
			t.Errorf("Search(%q) = %v, want first %s", c.q, ids, c.want)
		}
	}
	// category filter
	got, _ := ks.Search(context.Background(), "पेंशन", SearchOptions{Limit: 10, Categories: []string{"certificate"}})
	for _, g := range got {
		if g.Record.Category != "certificate" {
			t.Fatalf("category filter leaked %s", g.Record.ID)
		}
	}
	// unrelated text finds nothing
	if got, _ := ks.Search(context.Background(), "क्रिकेट मैच", SearchOptions{}); len(got) != 0 {
		t.Fatalf("unrelated search returned %d", len(got))
	}
	// FTS syntax characters cannot break the query
	if _, err := ks.Search(context.Background(), `"pension" OR NEAR( * ) AND -`, SearchOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestMatchNamesIgnoresGenericAliases(t *testing.T) {
	ks := openStore(t, Config{})
	loadFixtures(t, ks)
	if h := ks.MatchNames("वृद्धावस्था पेंशन"); len(h) != 0 {
		t.Fatalf("generic phrase matched as name: %+v", h)
	}
	h := ks.MatchNames("टेस्ट वृद्धजन पेंशन के बारे में बताओ")
	if len(h) != 1 || h[0].ID != "test-elderly-pension" {
		t.Fatalf("hits %+v", h)
	}
}

func TestPresentationPolicy(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		cfg  Config
		id   string
		want Mode
	}{
		{"verified", Config{}, "test-elderly-pension", ModeFactual},
		{"needs review", Config{}, "test-widow-pension", ModeNameOnly},
		{"unverified", Config{}, "test-education-loan", ModeNameOnly},
		{"overdue review", Config{}, "test-disability-pension", ModeNameOnly},
		{"dev override on dev profile", Config{DeviceProfile: "dev-mac", AllowUnverifiedForDev: true}, "test-widow-pension", ModeUnverifiedDev},
		{"dev override overdue", Config{DeviceProfile: "dev-mac", AllowUnverifiedForDev: true}, "test-disability-pension", ModeUnverifiedDev},
		{"dev override ignored on pi5", Config{DeviceProfile: "pi5", AllowUnverifiedForDev: true}, "test-widow-pension", ModeNameOnly},
		{"dev override ignored on empty profile", Config{AllowUnverifiedForDev: true}, "test-education-loan", ModeNameOnly},
		{"dev profile without flag", Config{DeviceProfile: "dev-mac"}, "test-widow-pension", ModeNameOnly},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ks := openStore(t, c.cfg)
			loadFixtures(t, ks)
			r, err := ks.Get(ctx, c.id)
			if err != nil {
				t.Fatal(err)
			}
			if got := ks.Mode(r); got != c.want {
				t.Fatalf("mode %v want %v", got, c.want)
			}
		})
	}
	if strings.Contains(NotVerifiedMessage("hi"), "फिक्स्चर") {
		t.Fatal("generic message must not contain record content")
	}
}

func TestReopenRebuildsIndexes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "device.db")
	ctx := context.Background()
	st, _, err := store.Open(ctx, store.Options{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	ks, _ := New(ctx, st.DB, Config{Now: func() time.Time { return fixedNow }})
	loadFixtures(t, ks)
	st.Close()
	st2, _, err := store.Open(ctx, store.Options{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	ks2, err := New(ctx, st2.DB, Config{Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	if h := ks2.MatchNames("टेस्ट किसान सहायता"); len(h) != 1 {
		t.Fatalf("names not rebuilt: %+v", h)
	}
}

func TestConcurrentSearchDuringReload(t *testing.T) {
	ks := openStore(t, Config{})
	loadFixtures(t, ks)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := ks.Search(ctx, "पेंशन", SearchOptions{}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for j := 0; j < 3; j++ {
		if _, err := ks.LoadDir(ctx, "testdata/records", "fixture-r"); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestGazetteer(t *testing.T) {
	g, err := LoadGazetteer("../../../../packages/scheme-knowledge/gazetteer/bihar_districts.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Districts) != 38 {
		t.Fatalf("Bihar has 38 districts, gazetteer has %d", len(g.Districts))
	}
	cases := []struct{ in, want string }{
		{"मैं खगड़िया से हूँ", "khagaria"},
		{"मैं खगरिया से हूँ", "khagaria"}, // STT misspelling
		{"मैं गया जिले का किसान हूँ", "gaya"},
		{"मुजफ्फरपुर", "muzaffarpur"},
		{"I live in Purnea", "purnia"},
		{"पूर्वी चंपारण", "east-champaran"},
		{"सीतामढी", "sitamarhi"},
	}
	for _, c := range cases {
		d, ok := g.Match(c.in)
		if !ok || d.ID != c.want {
			t.Errorf("Match(%q) = %v %v, want %s", c.in, d.ID, ok, c.want)
		}
	}
	for _, neg := range []string{"मैं बाजार गया था", "मुझे पढ़ना है", "लोन चाहिए"} {
		if d, ok := g.Match(neg); ok {
			t.Errorf("Match(%q) false positive %s", neg, d.ID)
		}
	}
}
