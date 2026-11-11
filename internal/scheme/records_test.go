package scheme_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
)

// recordsDir is the draft knowledge base, relative to this package.
var recordsDir = filepath.Join("..", "..", "packages", "scheme-knowledge", "records")

// TestKnowledgeRecords loads every record file in the repository and runs
// Validate. It also enforces the repository rules that Validate cannot know
// about: one record per file named <id>.json, unique ids, strict JSON, and
// no VERIFIED status in drafts (VERIFIED is set only by a human reviewer in
// central-api). Full policy checks live in cmd/kb.
func TestKnowledgeRecords(t *testing.T) {
	entries, err := os.ReadDir(recordsDir)
	if err != nil {
		t.Fatalf("read %s: %v", recordsDir, err)
	}
	seen := map[string]string{}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		n++
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(recordsDir, name))
			if err != nil {
				t.Fatal(err)
			}
			if len(data) > 256<<10 {
				t.Fatalf("record file is %d bytes; keep records under 256 KiB", len(data))
			}
			var r scheme.Record
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&r); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if _, err := dec.Token(); !errors.Is(err, io.EOF) {
				t.Fatal("trailing data after JSON value")
			}
			if err := r.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if want := r.ID + ".json"; name != want {
				t.Errorf("file name %s, want %s", name, want)
			}
			if prev, dup := seen[r.ID]; dup {
				t.Errorf("duplicate id %q (also in %s)", r.ID, prev)
			}
			seen[r.ID] = name
			if r.Verification.Status == scheme.Verified {
				t.Error("repository drafts must not be VERIFIED")
			}
			if r.IsFactual(time.Now()) {
				t.Error("draft record must not be factual")
			}
			if strings.TrimSpace(r.Verification.Notes) == "" {
				t.Error("verification.notes must tell the reviewer what to check")
			}
			// myScheme's terms forbid automated copying; a myScheme page is
			// never a source or an official_url (docs/KNOWLEDGE_SOURCES.md).
			for _, u := range append([]string{r.OfficialURL}, sourceURLs(r)...) {
				if strings.Contains(strings.ToLower(u), "myscheme") {
					t.Errorf("myScheme URL %q is not allowed as a source or official_url", u)
				}
			}
		})
	}
	if n == 0 {
		t.Fatalf("no records found in %s", recordsDir)
	}
}

func sourceURLs(r scheme.Record) []string {
	out := make([]string, 0, len(r.Sources))
	for _, s := range r.Sources {
		out = append(out, s.URL)
	}
	return out
}
