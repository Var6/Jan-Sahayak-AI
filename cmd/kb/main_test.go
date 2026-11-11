package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
)

var testNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// validRecord returns a structurally valid draft. Its content is a test
// fixture only (no real scheme facts).
func validRecord(id string) scheme.Record {
	t := func(hi, en string) scheme.Text { return scheme.Text{Hi: hi, En: en} }
	return scheme.Record{
		ID: id, Version: 1, Kind: scheme.KindService,
		Name:        t("परीक्षण सेवा", "Test service"),
		Aliases:     []string{"test"},
		State:       "BR",
		Department:  t("परीक्षण विभाग", "Test department"),
		Category:    "certificate",
		Keywords:    []string{"test"},
		Summary:     t("परीक्षण", "test"),
		Eligibility: []scheme.Text{t("क", "a")},
		Benefits:    []scheme.Text{t("ख", "b")},
		Questions: []scheme.Question{{
			ID: "is_test", Ask: t("क्या?", "Is it?"), Kind: "yes_no", IneligibleIf: []string{"no"},
		}},
		MandatoryDocs:    []scheme.Doc{{Name: t("कागज़", "paper")}},
		ApplicationSteps: []scheme.Text{t("ग", "c")},
		WhereToApply:     []scheme.Text{t("घ", "d")},
		OfficialURL:      "https://example.gov.in/",
		Sources: []scheme.Source{{
			URL: "https://example.gov.in/doc.pdf", Title: "Doc", AccessedOn: "2026-11-09", Excerpt: "x",
		}},
		Verification: scheme.Verification{Status: scheme.Unverified, Notes: "check everything"},
	}
}

func writeRecord(t *testing.T, dir, name string, r any) {
	t.Helper()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runKB(args ...string) (string, string, error) {
	var out, errb bytes.Buffer
	err := run(args, &out, &errb, testNow)
	return out.String(), errb.String(), err
}

func TestValidateOK(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "a-one.json", validRecord("a-one"))
	writeRecord(t, dir, "b-two.json", validRecord("b-two"))
	out, stderr, err := runKB("validate", dir)
	if err != nil {
		t.Fatalf("validate: %v\n%s", err, stderr)
	}
	if !strings.Contains(out, "2 records valid") || !strings.Contains(out, "UNVERIFIED=2") {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]struct {
		file   string
		mutate func(*scheme.Record)
		raw    string
		want   string
	}{
		"filename mismatch": {file: "wrong.json", want: "file name must be"},
		"verified in repo": {mutate: func(r *scheme.Record) {
			r.Verification = scheme.Verification{Status: scheme.Verified, VerifiedBy: "Asha Devi", VerifiedAt: "2026-11-09T10:00:00Z"}
		}, want: "VERIFIED is not allowed"},
		"last_verified on draft": {mutate: func(r *scheme.Record) { r.LastVerified = "2026-11-01" }, want: "must be empty unless VERIFIED"},
		"no notes":               {mutate: func(r *scheme.Record) { r.Verification.Notes = "" }, want: "verification.notes"},
		"non-gov official url":   {mutate: func(r *scheme.Record) { r.OfficialURL = "https://example.com/" }, want: "not a government domain"},
		"http source":            {mutate: func(r *scheme.Record) { r.Sources[0].URL = "http://example.gov.in/x" }, want: "must use https"},
		"future access":          {mutate: func(r *scheme.Record) { r.Sources[0].AccessedOn = "2027-01-01" }, want: "in the future"},
		"bad question kind":      {mutate: func(r *scheme.Record) { r.Questions[0].Kind = "free_text" }, want: "kind must be"},
		"bad ineligible value":   {mutate: func(r *scheme.Record) { r.Questions[0].IneligibleIf = []string{"maybe"} }, want: "must be yes|no"},
		"latin hindi name":       {mutate: func(r *scheme.Record) { r.Name.Hi = "Test" }, want: "Devanagari"},
		"missing en doc":         {mutate: func(r *scheme.Record) { r.MandatoryDocs[0].Name.En = "" }, want: "document #1"},
		"unknown field":          {raw: `{"id":"x-rec","version":1,"surprise":true}`, want: "unknown field"},
		"trailing data":          {raw: `{"id":"x-rec"} {}`, want: "trailing data"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			file := tc.file
			if file == "" {
				file = "x-rec.json"
			}
			if tc.raw != "" {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(tc.raw), 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				r := validRecord("x-rec")
				if tc.mutate != nil {
					tc.mutate(&r)
				}
				writeRecord(t, dir, file, r)
			}
			_, stderr, err := runKB("validate", dir)
			if err == nil {
				t.Fatal("expected failure")
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr %q does not contain %q", stderr, tc.want)
			}
		})
	}
}

func TestValidateDuplicateAndOversize(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "a-one.json", validRecord("a-one"))
	// Same id in another file: filename check and duplicate check both fire.
	writeRecord(t, dir, "b-two.json", validRecord("a-one"))
	_, stderr, err := runKB("validate", dir)
	if err == nil || !strings.Contains(stderr, "duplicate id") {
		t.Fatalf("want duplicate error, got %v %q", err, stderr)
	}

	dir2 := t.TempDir()
	big := bytes.Repeat([]byte(" "), maxFileBytes+10)
	if err := os.WriteFile(filepath.Join(dir2, "big.json"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runKB("validate", dir2)
	if err == nil || !strings.Contains(stderr, "larger than") {
		t.Fatalf("want size error, got %v %q", err, stderr)
	}
}

func TestValidateAllowVerified(t *testing.T) {
	dir := t.TempDir()
	r := validRecord("a-one")
	r.Verification = scheme.Verification{Status: scheme.Verified, VerifiedBy: "Asha Devi", VerifiedAt: "2026-11-09T10:00:00Z"}
	r.LastVerified = "2026-11-09"
	writeRecord(t, dir, "a-one.json", r)
	if _, stderr, err := runKB("validate", dir, "-allow-verified"); err != nil {
		t.Fatalf("validate -allow-verified: %v %s", err, stderr)
	}
	// Machine names as reviewers are still rejected by scheme.Validate.
	r.Verification.VerifiedBy = "claude"
	writeRecord(t, dir, "a-one.json", r)
	if _, _, err := runKB("validate", dir, "-allow-verified"); err == nil {
		t.Fatal("expected machine reviewer to be rejected")
	}
}

func TestBundle(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "z-last.json", validRecord("z-last"))
	writeRecord(t, dir, "a-first.json", validRecord("a-first"))
	out := filepath.Join(t.TempDir(), "bundle.json")

	if _, _, err := runKB("bundle", dir); err == nil {
		t.Fatal("expected error without -out")
	}
	stdout, stderr, err := runKB("bundle", dir, "-out", out)
	if err != nil {
		t.Fatalf("bundle: %v %s", err, stderr)
	}
	if !strings.Contains(stdout, "UNSIGNED") {
		t.Fatalf("stdout %q", stdout)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var b scheme.Bundle
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		t.Fatal(err)
	}
	if b.BundleVersion != "2026-11-09.1" || b.CreatedAt != "2026-11-09T12:00:00Z" {
		t.Fatalf("bundle header %+v", b)
	}
	if b.Signature != "" || b.KeyID != "" {
		t.Fatal("bundle must be unsigned")
	}
	if len(b.Records) != 2 || b.Records[0].ID != "a-first" || b.Records[1].ID != "z-last" {
		t.Fatalf("records not sorted: %v", b.Records)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(out))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}

	// Flags before the positional argument also work; custom version.
	if _, stderr, err := runKB("bundle", "-out", out, "-version", "2026-11-10.3", dir); err != nil {
		t.Fatalf("bundle (flags first): %v %s", err, stderr)
	}
	if _, _, err := runKB("bundle", dir, "-out", out, "-version", "latest"); err == nil {
		t.Fatal("expected bad version error")
	}
	if _, _, err := runKB("bundle", dir, "-out", out, "-require-verified"); err == nil {
		t.Fatal("expected -require-verified to fail on drafts")
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"validate"}, {"frobnicate", "x"}, {"validate", "a", "b"}} {
		if _, _, err := runKB(args...); err == nil {
			t.Fatalf("expected error for %v", args)
		}
	}
	if _, _, err := runKB("validate", t.TempDir()); err == nil {
		t.Fatal("expected error for empty dir")
	}
}

const validHelplines = `{"schema_version":1,"helplines":[{"id":"test-line","number":"1234",
"name":{"hi":"परीक्षण","en":"Test"},"purpose":{"hi":"परीक्षण","en":"Test"},"use_for":["crisis"],
"scope":"national","source_url":"https://example.gov.in/","source_title":"T","source_excerpt":"call 1234",
"accessed_on":"2026-11-09","status":"UNVERIFIED"}]}`

func TestHelplines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "h.json")
	write := func(s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(validHelplines)
	if _, _, err := runKB("helplines", p); err != nil {
		t.Fatalf("valid helplines: %v", err)
	}
	bad := map[string]string{
		"verified":       strings.Replace(validHelplines, `"UNVERIFIED"`, `"VERIFIED"`, 1),
		"non-gov source": strings.Replace(validHelplines, "example.gov.in", "example.com", 1),
		"bad number":     strings.Replace(validHelplines, `"number":"1234"`, `"number":"12a4"`, 1),
		"excerpt lacks":  strings.Replace(validHelplines, "call 1234", "call us", 1),
		"unknown field":  strings.Replace(validHelplines, `"scope"`, `"extra":1,"scope"`, 1),
		"bad use_for":    strings.Replace(validHelplines, `["crisis"]`, `["party"]`, 1),
	}
	for name, s := range bad {
		write(s)
		if _, _, err := runKB("helplines", p); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// TestRepoFiles checks the real knowledge-base files in the repository.
func TestRepoFiles(t *testing.T) {
	root := filepath.Join("..", "..", "packages", "scheme-knowledge")
	// -strict: every number shown to citizens must be backed by an excerpt
	// (correctness review finding 4).
	if _, stderr, err := runKB("validate", "-strict", filepath.Join(root, "records")); err != nil {
		t.Fatalf("repo records: %v\n%s", err, stderr)
	}
	if _, _, err := runKB("helplines", filepath.Join(root, "helplines.json")); err != nil {
		t.Fatalf("repo helplines: %v", err)
	}
}

func TestNumberTokens(t *testing.T) {
	cases := map[string][]string{
		"₹1,20,000 and Rs. 12,000/-":           {"120000", "12000"},
		"₹1.20 lakh, 1.30":                     {"1.2", "1.3"},
		"०६ माह, 06 months":                    {"6"},
		"call 1800-345-6444 or 1800 345 6444":  {"18003456444"},
		"18003456444":                          {"18003456444"},
		"within 21 working days.":              {"21"},
		"7nishchay-yuvaupmission.bihar.gov.in": nil,
		"https://pmayg.dord.gov.in/x/2024.pdf": nil,
		"aged 18–79, 80% or more":              {"18", "79", "80"},
		"10+2 पास":                             {"10", "2"},
	}
	for in, want := range cases {
		got := numberTokens(in)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("numberTokens(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestExcerptNumbersSpelledOut(t *testing.T) {
	cases := map[string][]string{
		"Rupees One Thousand per month": {"1", "1000"},
		"शिक्षा ऋण अधिकतम चार लाख रूपये": {"4", "400000"},
		"Rs. 1.20 lakh in plains":                 {"1.2", "120000"},
		"रू0 10.00 लाख":                           {"10", "1000000"},
		"Xth and XIIth Certificate":               {"10", "12"},
		"valid for one year (ten days to appeal)": {"1", "10"},
	}
	for in, want := range cases {
		got := excerptNumbers(in)
		for _, w := range want {
			if !got[w] {
				t.Errorf("excerptNumbers(%q) lacks %q (got %v)", in, w, got)
			}
		}
	}
}

func TestValidateUnsupportedNumber(t *testing.T) {
	dir := t.TempDir()
	r := validRecord("a-one")
	r.Sources[0].Excerpt = "assistance of Rs. 12,000/- for a toilet; helpline 1800-11-2233"
	r.Benefits = []scheme.Text{{Hi: "शौचालय के लिए ₹12,000", En: "₹12,000 for a toilet; ₹50,000 loan"}}
	r.Helpline = "1800112233"
	writeRecord(t, dir, "a-one.json", r)

	// Default: warning only, exit 0.
	_, stderr, err := runKB("validate", dir)
	if err != nil {
		t.Fatalf("validate: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, `warning: a-one.json: number "50000" in benefits[0].en appears in no source excerpt`) {
		t.Fatalf("missing warning for 50000:\n%s", stderr)
	}
	if strings.Contains(stderr, `"12000"`) || strings.Contains(stderr, `"1800112233"`) {
		t.Fatalf("supported numbers flagged:\n%s", stderr)
	}

	// -strict: the same finding is an error.
	_, stderr, err = runKB("validate", "-strict", dir)
	if err == nil {
		t.Fatal("validate -strict accepted an unsupported number")
	}
	if !strings.Contains(stderr, `error: a-one.json: number "50000" in benefits[0].en`) {
		t.Fatalf("missing strict error:\n%s", stderr)
	}
	// bundle honours -strict too.
	if _, _, err := runKB("bundle", "-strict", dir, "-out", filepath.Join(dir, "b.json")); err == nil {
		t.Fatal("bundle -strict accepted an unsupported number")
	}

	// Removing the unsupported fact makes -strict pass.
	r.Benefits[0].En = "₹12,000 for a toilet"
	writeRecord(t, dir, "a-one.json", r)
	if _, stderr, err := runKB("validate", "-strict", dir); err != nil {
		t.Fatalf("validate -strict after fix: %v\n%s", err, stderr)
	}
}

func TestValidateUnreadOfficialURL(t *testing.T) {
	dir := t.TempDir()
	r := validRecord("a-one")
	r.OfficialURL = "https://other.gov.in/scheme" // no source on this host
	writeRecord(t, dir, "a-one.json", r)
	_, stderr, err := runKB("validate", dir)
	if err != nil || !strings.Contains(stderr, "official_url host other.gov.in is not the host of any source") {
		t.Fatalf("want warning, got err=%v\n%s", err, stderr)
	}
	if _, _, err := runKB("validate", "-strict", dir); err == nil {
		t.Fatal("validate -strict accepted an official_url that was never read")
	}
}
