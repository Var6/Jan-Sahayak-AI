// Command kb is the knowledge-base tool for scheme records.
//
//	kb validate <dir> [-allow-verified] [-strict]
//	kb bundle <dir> -out bundle.json [-version 2026-11-09.1] [-require-verified] [-allow-verified] [-strict]
//	kb helplines <file>
//
// validate loads every *.json record in <dir> (non-recursive), decodes it
// strictly (unknown fields are errors), runs scheme.Record.Validate and the
// knowledge-base policy checks below, and exits non-zero on any error.
//
// It also warns when a number shown to citizens (amount, age, deadline,
// phone number) appears in none of the record's source excerpts, or when
// official_url is on a site no source was read from (numbers.go). With
// -strict those warnings are errors; run validate -strict before handing
// records to a reviewer.
//
// bundle does the same and then writes an UNSIGNED scheme.Bundle (records
// sorted by id, key_id/signature empty). Signing is done only by
// central-api; devices reject unsigned bundles.
//
// helplines validates packages/scheme-knowledge/helplines.json against the
// schema described in packages/scheme-knowledge/README.md.
//
// Policy: records in the repository are drafts. VERIFIED is set only by a
// human reviewer through central-api, so a VERIFIED record in a repo
// directory is an error unless -allow-verified is given (for example when
// checking an export from central-api).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
)

// Bounds. The knowledge base is small; these limits keep a bad input from
// exhausting memory.
const (
	maxFileBytes    = 256 << 10 // per record / helplines file
	maxRecords      = 2000
	maxExcerptRunes = 1200
	maxNotesRunes   = 4000
)

var ist = time.FixedZone("IST", 5*60*60+30*60)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "kb:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  kb validate <dir> [-allow-verified] [-strict]
  kb bundle <dir> -out bundle.json [-version YYYY-MM-DD.N] [-require-verified] [-allow-verified] [-strict]
  kb helplines <file>`

func run(args []string, stdout, stderr io.Writer, now time.Time) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	allowVerified := fs.Bool("allow-verified", false, "accept VERIFIED records (exports from central-api only)")
	out := fs.String("out", "", "bundle: output file")
	version := fs.String("version", "", "bundle: bundle_version (default <today IST>.1)")
	requireVerified := fs.Bool("require-verified", false, "bundle: fail unless every record is VERIFIED")
	strict := fs.Bool("strict", false, "treat source-support findings (numbers in no excerpt, unread official_url) as errors")

	pos, err := parseInterspersed(fs, rest)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New(usage)
	}
	target := pos[0]

	switch cmd {
	case "validate":
		recs, err := loadAndCheck(target, checkOpts{allowVerified: *allowVerified, strict: *strict}, now, stderr)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "ok: %d records valid (%s)\n", len(recs), statusSummary(recs))
		return nil

	case "bundle":
		if *out == "" {
			return errors.New("bundle: -out is required")
		}
		recs, err := loadAndCheck(target, checkOpts{allowVerified: *allowVerified || *requireVerified, strict: *strict}, now, stderr)
		if err != nil {
			return err
		}
		if *requireVerified {
			for _, r := range recs {
				if r.Verification.Status != scheme.Verified {
					return fmt.Errorf("bundle: -require-verified: %s is %s", r.ID, r.Verification.Status)
				}
			}
		}
		bv := *version
		if bv == "" {
			bv = now.In(ist).Format("2006-01-02") + ".1"
		}
		if !bundleVersionRe.MatchString(bv) {
			return fmt.Errorf("bundle: -version %q must look like YYYY-MM-DD.N", bv)
		}
		b := scheme.Bundle{
			BundleVersion: bv,
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Records:       recs,
		}
		data, err := json.MarshalIndent(b, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if err := writeFileAtomic(*out, data); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote UNSIGNED bundle %s: %d records (%s), %d bytes — sign via central-api before distribution\n",
			*out, len(recs), statusSummary(recs), len(data))
		return nil

	case "helplines":
		n, err := checkHelplinesFile(target, now)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "ok: %d helplines valid\n", n)
		return nil
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

// parseInterspersed lets flags appear before or after the positional
// argument ("kb bundle dir -out x" and "kb bundle -out x dir").
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
		if len(pos) > 4 {
			return pos, nil
		}
	}
}

var (
	bundleVersionRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.\d{1,4}$`)
	slugRe          = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)
	phoneRe         = regexp.MustCompile(`^[0-9][0-9 \-]{1,18}[0-9]$`)
)

// fileErr collects errors for one file.
type fileErr struct {
	file string
	errs []error
}

func (f *fileErr) add(format string, a ...any) { f.errs = append(f.errs, fmt.Errorf(format, a...)) }

// checkOpts selects optional policy for loadAndCheck.
type checkOpts struct {
	allowVerified bool // accept VERIFIED (central-api exports)
	strict        bool // unsupported numbers are errors
}

// loadAndCheck reads, decodes and checks every record in dir. All problems
// are reported (to stderr) before returning a single summary error.
func loadAndCheck(dir string, opts checkOpts, now time.Time, stderr io.Writer) ([]scheme.Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("%s: no *.json records found", dir)
	}
	if len(names) > maxRecords {
		return nil, fmt.Errorf("%s: %d files exceeds limit of %d", dir, len(names), maxRecords)
	}

	var (
		recs   []scheme.Record
		failed []*fileErr
		seen   = map[string]string{}
	)
	for _, name := range names {
		fe := &fileErr{file: name}
		rec, err := decodeRecordFile(filepath.Join(dir, name))
		if err != nil {
			fe.add("%v", err)
			failed = append(failed, fe)
			continue
		}
		checkRecord(fe, name, rec, opts.allowVerified, now)
		supportMsgs := sourceSupport(rec)
		if opts.strict {
			for _, m := range supportMsgs {
				fe.add("%s", m)
			}
			supportMsgs = nil
		}
		if prev, dup := seen[rec.ID]; dup {
			fe.add("duplicate id %q (also in %s)", rec.ID, prev)
		}
		seen[rec.ID] = name
		if len(fe.errs) > 0 {
			failed = append(failed, fe)
			continue
		}
		for _, w := range append(warnings(rec), supportMsgs...) {
			fmt.Fprintf(stderr, "warning: %s: %s\n", name, w)
		}
		recs = append(recs, rec)
	}
	if len(failed) > 0 {
		total := 0
		for _, f := range failed {
			for _, e := range f.errs {
				for _, line := range strings.Split(e.Error(), "\n") {
					fmt.Fprintf(stderr, "error: %s: %s\n", f.file, line)
					total++
				}
			}
		}
		return nil, fmt.Errorf("%d of %d files invalid (%d errors)", len(failed), len(names), total)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	return recs, nil
}

// readBounded reads at most maxFileBytes, failing if the file is larger.
func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("file larger than %d bytes", maxFileBytes)
	}
	return data, nil
}

// decodeStrict decodes exactly one JSON value, rejecting unknown fields and
// trailing data.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("decode: trailing data after JSON value")
	}
	return nil
}

func decodeRecordFile(path string) (scheme.Record, error) {
	var r scheme.Record
	data, err := readBounded(path)
	if err != nil {
		return r, err
	}
	return r, decodeStrict(data, &r)
}

func hasDevanagari(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Devanagari, r) {
			return true
		}
	}
	return false
}

func blank(t scheme.Text) bool { return strings.TrimSpace(t.Hi) == "" || strings.TrimSpace(t.En) == "" }

// checkRecord applies scheme.Record.Validate plus knowledge-base policy.
func checkRecord(fe *fileErr, name string, r scheme.Record, allowVerified bool, now time.Time) {
	if err := r.Validate(); err != nil {
		fe.add("%v", err)
	}
	if want := r.ID + ".json"; name != want {
		fe.add("file name must be %q (id + .json)", want)
	}
	if !hasDevanagari(r.Name.Hi) {
		fe.add("name.hi must be in Devanagari")
	}
	if blank(r.Department) {
		fe.add("department.hi and department.en are required")
	}
	if strings.TrimSpace(r.Summary.En) == "" {
		fe.add("summary.en required")
	}
	if len(r.Aliases) == 0 {
		fe.add("at least one alias required")
	}
	if len(r.Keywords) == 0 {
		fe.add("at least one keyword required")
	}
	if len(r.ApplicationSteps) == 0 {
		fe.add("application_steps required")
	}
	if len(r.WhereToApply) == 0 {
		fe.add("where_to_apply required")
	}
	texts := make([]scheme.Text, 0, len(r.Eligibility)+len(r.Benefits)+len(r.ApplicationSteps)+len(r.WhereToApply))
	texts = append(texts, r.Eligibility...)
	texts = append(texts, r.Benefits...)
	texts = append(texts, r.ApplicationSteps...)
	texts = append(texts, r.WhereToApply...)
	for i, t := range texts {
		if blank(t) {
			fe.add("bilingual text #%d in eligibility/benefits/steps/where_to_apply has an empty hi or en", i+1)
		}
	}
	for i, d := range append(append([]scheme.Doc{}, r.MandatoryDocs...), r.OptionalDocs...) {
		if blank(d.Name) {
			fe.add("document #%d name needs hi and en", i+1)
		}
		if d.Note != nil && blank(*d.Note) {
			fe.add("document #%d note needs hi and en", i+1)
		}
	}

	// Sources: https, title, not accessed in the future, bounded excerpt.
	tomorrow := now.In(ist).AddDate(0, 0, 1).Format("2006-01-02")
	for _, s := range r.Sources {
		if u, err := url.Parse(s.URL); err == nil && u.Scheme != "https" {
			fe.add("source %q must use https", s.URL)
		}
		if strings.TrimSpace(s.Title) == "" {
			fe.add("source %q needs a title", s.URL)
		}
		if s.AccessedOn > tomorrow {
			fe.add("source %q accessed_on %s is in the future", s.URL, s.AccessedOn)
		}
		if n := len([]rune(s.Excerpt)); n > maxExcerptRunes {
			fe.add("source %q excerpt is %d chars (max %d)", s.URL, n, maxExcerptRunes)
		}
	}

	// Questions.
	qids := map[string]bool{}
	for _, q := range r.Questions {
		if !slugRe.MatchString(q.ID) {
			fe.add("question id %q must be snake_case", q.ID)
		}
		if qids[q.ID] {
			fe.add("duplicate question id %q", q.ID)
		}
		qids[q.ID] = true
		if blank(q.Ask) {
			fe.add("question %q ask needs hi and en", q.ID)
		}
		switch q.Kind {
		case "yes_no":
			if len(q.Choices) > 0 {
				fe.add("question %q: yes_no must not have choices", q.ID)
			}
			for _, v := range q.IneligibleIf {
				if v != "yes" && v != "no" {
					fe.add("question %q: ineligible_if %q must be yes|no", q.ID, v)
				}
			}
		case "choice":
			if len(q.Choices) < 2 {
				fe.add("question %q: choice needs at least 2 choices", q.ID)
			}
		default:
			fe.add("question %q: kind must be yes_no|choice", q.ID)
		}
	}

	// Verification policy.
	v := r.Verification
	if v.Status == scheme.Verified && !allowVerified {
		fe.add("status VERIFIED is not allowed in repository drafts; verification happens only in central-api (use -allow-verified for central exports)")
	}
	if v.Status != scheme.Verified {
		if r.LastVerified != "" || v.VerifiedBy != "" || v.VerifiedAt != "" {
			fe.add("last_verified / verified_by / verified_at must be empty unless VERIFIED")
		}
		if strings.TrimSpace(v.Notes) == "" {
			fe.add("verification.notes must tell the reviewer what to check")
		}
	}
	if n := len([]rune(v.Notes)); n > maxNotesRunes {
		fe.add("verification.notes is %d chars (max %d)", n, maxNotesRunes)
	}
	if v.ReviewDue != "" {
		if _, err := time.Parse("2006-01-02", v.ReviewDue); err != nil {
			fe.add("review_due must be YYYY-MM-DD")
		}
	}
}

// warnings are non-fatal hints for the reviewer.
func warnings(r scheme.Record) []string {
	var w []string
	if r.OfficialURL == "" {
		w = append(w, "official_url empty (no .gov.in/.nic.in page) — see verification.notes")
	}
	for _, s := range r.Sources {
		if u, err := url.Parse(s.URL); err == nil {
			h := strings.ToLower(u.Hostname())
			if !strings.HasSuffix(h, ".gov.in") && !strings.HasSuffix(h, ".nic.in") {
				w = append(w, fmt.Sprintf("source host %s is not .gov.in/.nic.in — confirm it is the official body", h))
			}
		}
		if s.Excerpt == "" {
			w = append(w, fmt.Sprintf("source %s has no excerpt", s.URL))
		}
	}
	return w
}

func statusSummary(recs []scheme.Record) string {
	counts := map[scheme.Status]int{}
	for _, r := range recs {
		counts[r.Verification.Status]++
	}
	var parts []string
	for _, s := range []scheme.Status{scheme.Verified, scheme.NeedsReview, scheme.Unverified, scheme.Outdated} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", s, counts[s]))
		}
	}
	return strings.Join(parts, " ")
}

// writeFileAtomic writes to a temp file in the same directory and renames it
// into place, so a crash never leaves a half-written bundle.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".kb-bundle-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ---- helplines.json -------------------------------------------------------

// Helpline is one entry of helplines.json (schema in the package README).
type Helpline struct {
	ID            string      `json:"id"`
	Number        string      `json:"number"`
	AltNumbers    []string    `json:"alt_numbers,omitempty"`
	Name          scheme.Text `json:"name"`
	Purpose       scheme.Text `json:"purpose"`
	UseFor        []string    `json:"use_for"`
	Scope         string      `json:"scope"`
	SourceURL     string      `json:"source_url"`
	SourceTitle   string      `json:"source_title"`
	SourceExcerpt string      `json:"source_excerpt"`
	AccessedOn    string      `json:"accessed_on"`
	Status        string      `json:"status"`
	Notes         string      `json:"notes,omitempty"`
}

// HelplineFile is the top-level helplines.json document.
type HelplineFile struct {
	SchemaVersion int        `json:"schema_version"`
	Helplines     []Helpline `json:"helplines"`
}

var (
	helplineIDRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	useForValues = map[string]bool{
		"crisis": true, "emergency": true, "police": true, "fire": true, "ambulance": true,
		"women": true, "domestic_violence": true, "child": true, "mental_health": true,
		"suicide": true, "legal_aid": true, "general_help": true, "scheme": true,
	}
)

func checkHelplinesFile(path string, now time.Time) (int, error) {
	data, err := readBounded(path)
	if err != nil {
		return 0, err
	}
	var hf HelplineFile
	if err := decodeStrict(data, &hf); err != nil {
		return 0, err
	}
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if hf.SchemaVersion != 1 {
		add("schema_version must be 1")
	}
	if len(hf.Helplines) == 0 {
		add("no helplines")
	}
	tomorrow := now.In(ist).AddDate(0, 0, 1).Format("2006-01-02")
	ids, numbers := map[string]bool{}, map[string]bool{}
	for i, h := range hf.Helplines {
		p := fmt.Sprintf("helplines[%d] %q", i, h.ID)
		if !helplineIDRe.MatchString(h.ID) {
			add("%s: id must be a lowercase slug", p)
		}
		if ids[h.ID] {
			add("%s: duplicate id", p)
		}
		ids[h.ID] = true
		for _, n := range append([]string{h.Number}, h.AltNumbers...) {
			if !phoneRe.MatchString(n) {
				add("%s: number %q malformed", p, n)
			}
			if numbers[n] {
				add("%s: number %q listed twice", p, n)
			}
			numbers[n] = true
		}
		if blank(h.Name) || !hasDevanagari(h.Name.Hi) {
			add("%s: name needs hi (Devanagari) and en", p)
		}
		if blank(h.Purpose) {
			add("%s: purpose needs hi and en", p)
		}
		if len(h.UseFor) == 0 {
			add("%s: use_for required", p)
		}
		for _, u := range h.UseFor {
			if !useForValues[u] {
				add("%s: unknown use_for %q", p, u)
			}
		}
		if h.Scope != "national" && h.Scope != "BR" {
			add("%s: scope must be national|BR", p)
		}
		u, err := url.Parse(h.SourceURL)
		if err != nil || u.Scheme != "https" {
			add("%s: source_url must be https", p)
		} else if host := strings.ToLower(u.Hostname()); !strings.HasSuffix(host, ".gov.in") && !strings.HasSuffix(host, ".nic.in") {
			add("%s: source_url host %s is not a government domain", p, host)
		}
		if strings.TrimSpace(h.SourceTitle) == "" || strings.TrimSpace(h.SourceExcerpt) == "" {
			add("%s: source_title and source_excerpt required", p)
		}
		if h.SourceExcerpt != "" && !strings.Contains(h.SourceExcerpt, h.Number) {
			add("%s: source_excerpt must contain the number %s", p, h.Number)
		}
		if _, err := time.Parse("2006-01-02", h.AccessedOn); err != nil || h.AccessedOn > tomorrow {
			add("%s: accessed_on must be a past YYYY-MM-DD", p)
		}
		switch scheme.Status(h.Status) {
		case scheme.Unverified, scheme.NeedsReview, scheme.Outdated:
		case scheme.Verified:
			add("%s: VERIFIED is set only through the review procedure, not in the repo file", p)
		default:
			add("%s: status %q invalid", p, h.Status)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return 0, err
	}
	return len(hf.Helplines), nil
}
