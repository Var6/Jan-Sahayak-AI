// Package scheme defines the government scheme / service record shared by
// the central knowledge base, signed bundles and the device.
//
// Golden rule: facts shown to citizens come only from records whose Status
// is VERIFIED and whose verification was done by a named human reviewer.
// Machine-generated or imported records always start as UNVERIFIED, and no
// code path in this repository sets VERIFIED except the reviewer workflow
// (central-api: POST /v1/admin/schemes/{id}/verify by a user with the
// scheme_reviewer role).
package scheme

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Status of a record.
type Status string

const (
	Verified    Status = "VERIFIED"
	NeedsReview Status = "NEEDS_REVIEW"
	Outdated    Status = "OUTDATED"
	Unverified  Status = "UNVERIFIED"
)

// Kind distinguishes schemes from certificate/document services.
type Kind string

const (
	KindScheme  Kind = "scheme"  // benefit / pension / loan / scholarship
	KindService Kind = "service" // certificate, ration card, ID document
)

// Category values used by the dialogue engine for routing.
var Categories = []string{
	"pension", "scholarship", "education", "farmer", "loan", "subsidy",
	"self_employment", "housing", "health", "food_security",
	"certificate", "identity_document", "women_child", "disability", "labour",
}

// Text is a bilingual string. Hindi (Devanagari) is primary; English is for
// staff and English-speaking citizens.
type Text struct {
	Hi string `json:"hi"`
	En string `json:"en"`
}

// Doc is a document requirement.
type Doc struct {
	Name Text `json:"name"`
	// Note explains alternatives or caveats ("Aadhaar proves identity, not caste").
	Note *Text `json:"note,omitempty"`
}

// Source is an official page the record was checked against.
type Source struct {
	URL        string `json:"url"`
	Title      string `json:"title"`
	AccessedOn string `json:"accessed_on"` // YYYY-MM-DD
	// Excerpt is a short verbatim quote supporting the record (optional).
	Excerpt string `json:"excerpt,omitempty"`
}

// Verification records who verified the record and when.
type Verification struct {
	Status     Status `json:"status"`
	VerifiedBy string `json:"verified_by,omitempty"` // reviewer user id/name
	VerifiedAt string `json:"verified_at,omitempty"` // RFC 3339
	ReviewDue  string `json:"review_due,omitempty"`  // YYYY-MM-DD
	Notes      string `json:"notes,omitempty"`
}

// Question is an eligibility question the kiosk may ask. Answers are used
// only within the session for guidance and never stored.
type Question struct {
	ID   string `json:"id"`   // e.g. "age_60_plus"
	Ask  Text   `json:"ask"`  // "क्या आपकी उम्र 60 साल या उससे ज़्यादा है?"
	Kind string `json:"kind"` // "yes_no" | "choice"
	// Choices for kind=choice.
	Choices []Text `json:"choices,omitempty"`
	// IneligibleIf: answer value(s) that mean "probably not eligible —
	// suggest verifying at the office" (never a final rejection).
	IneligibleIf []string `json:"ineligible_if,omitempty"`
}

// Record is one scheme or service.
type Record struct {
	ID         string   `json:"id"` // stable slug: "bihar-vridhjan-pension"
	Version    int      `json:"version"`
	Kind       Kind     `json:"kind"`
	Name       Text     `json:"name"`
	Aliases    []string `json:"aliases"` // spoken variants, Hinglish, abbreviations
	State      string   `json:"state"`   // "BR" or "" for central schemes
	Department Text     `json:"department"`
	Category   string   `json:"category"`
	Keywords   []string `json:"keywords"` // intent-matching terms (hi/en/Hinglish)

	Summary     Text       `json:"summary"` // 1–2 sentence plain-language description
	Eligibility []Text     `json:"eligibility"`
	Benefits    []Text     `json:"benefits"`
	AgeRule     *Text      `json:"age_rule,omitempty"`
	IncomeRule  *Text      `json:"income_rule,omitempty"`
	Questions   []Question `json:"questions,omitempty"`

	MandatoryDocs       []Doc  `json:"mandatory_docs"`
	OptionalDocs        []Doc  `json:"optional_docs,omitempty"`
	AlternativeEvidence []Text `json:"alternative_evidence,omitempty"`

	ApplicationSteps []Text `json:"application_steps"`
	WhereToApply     []Text `json:"where_to_apply"` // e.g. RTPS counter, CSC, block office
	OfficialURL      string `json:"official_url"`
	Helpline         string `json:"helpline,omitempty"`
	FeesNote         *Text  `json:"fees_note,omitempty"`
	Exceptions       []Text `json:"exceptions,omitempty"`

	Sources      []Source     `json:"sources"`
	LastVerified string       `json:"last_verified,omitempty"` // YYYY-MM-DD
	Verification Verification `json:"verification"`
}

var (
	idRe    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	phoneRe = regexp.MustCompile(`^[0-9 +\-]{3,20}$`)
)

// officialHost reports whether a URL host is a government domain we accept
// as an official source or application channel.
func officialHost(host string) bool {
	host = strings.ToLower(host)
	return strings.HasSuffix(host, ".gov.in") || strings.HasSuffix(host, ".nic.in") ||
		host == "gov.in" || strings.HasSuffix(host, ".bihar.gov.in")
}

// Validate checks structural rules. It does not (and cannot) check that the
// facts are true — that is the human reviewer's job.
func (r Record) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !idRe.MatchString(r.ID) {
		add("id %q must be a lowercase slug", r.ID)
	}
	if r.Version < 1 {
		add("version must be >= 1")
	}
	if r.Kind != KindScheme && r.Kind != KindService {
		add("kind must be scheme|service")
	}
	if r.Name.Hi == "" || r.Name.En == "" {
		add("name.hi and name.en are required")
	}
	if !contains(Categories, r.Category) {
		add("unknown category %q", r.Category)
	}
	if r.State != "" && r.State != "BR" {
		add("state must be BR or empty (central) for now, got %q", r.State)
	}
	if r.Summary.Hi == "" {
		add("summary.hi required")
	}
	if len(r.Sources) == 0 {
		add("at least one source is required")
	}
	for _, s := range r.Sources {
		u, err := url.Parse(s.URL)
		if err != nil || u.Scheme != "https" && u.Scheme != "http" {
			add("source url %q invalid", s.URL)
			continue
		}
		if _, err := time.Parse("2006-01-02", s.AccessedOn); err != nil {
			add("source %q accessed_on must be YYYY-MM-DD", s.URL)
		}
	}
	if r.OfficialURL != "" {
		u, err := url.Parse(r.OfficialURL)
		if err != nil || u.Scheme != "https" && u.Scheme != "http" {
			add("official_url invalid")
		} else if !officialHost(u.Hostname()) {
			add("official_url host %q is not a government domain", u.Hostname())
		}
	}
	if r.Helpline != "" && !phoneRe.MatchString(r.Helpline) {
		add("helpline %q malformed", r.Helpline)
	}
	switch r.Verification.Status {
	case Verified:
		if r.Verification.VerifiedBy == "" || r.Verification.VerifiedAt == "" {
			add("VERIFIED requires verified_by and verified_at (a human reviewer)")
		}
		if strings.Contains(strings.ToLower(r.Verification.VerifiedBy), "ai") ||
			strings.Contains(strings.ToLower(r.Verification.VerifiedBy), "claude") ||
			strings.Contains(strings.ToLower(r.Verification.VerifiedBy), "llm") {
			add("verified_by must be a human reviewer")
		}
	case NeedsReview, Outdated, Unverified:
	default:
		add("verification.status %q invalid", r.Verification.Status)
	}
	return errors.Join(errs...)
}

// IsFactual reports whether this record may be presented as fact.
func (r Record) IsFactual(now time.Time) bool {
	if r.Verification.Status != Verified {
		return false
	}
	if r.Verification.ReviewDue != "" {
		if due, err := time.Parse("2006-01-02", r.Verification.ReviewDue); err == nil && now.After(due.Add(24*time.Hour)) {
			return false // overdue for review → treat as needing verification
		}
	}
	return true
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// AllText returns every citizen-facing string in the record. The response
// validator extracts numbers, URLs and phone numbers from it and rejects LLM
// output that introduces facts not present in the record.
func (r Record) AllText() []string {
	var out []string
	addT := func(t Text) { out = append(out, t.Hi, t.En) }
	addP := func(t *Text) {
		if t != nil {
			addT(*t)
		}
	}
	addT(r.Name)
	addT(r.Department)
	addT(r.Summary)
	for _, x := range r.Eligibility {
		addT(x)
	}
	for _, x := range r.Benefits {
		addT(x)
	}
	addP(r.AgeRule)
	addP(r.IncomeRule)
	for _, d := range append(append([]Doc{}, r.MandatoryDocs...), r.OptionalDocs...) {
		addT(d.Name)
		addP(d.Note)
	}
	for _, x := range r.AlternativeEvidence {
		addT(x)
	}
	for _, x := range r.ApplicationSteps {
		addT(x)
	}
	for _, x := range r.WhereToApply {
		addT(x)
	}
	addP(r.FeesNote)
	for _, x := range r.Exceptions {
		addT(x)
	}
	out = append(out, r.OfficialURL, r.Helpline)
	return out
}

// Bundle is a versioned set of records distributed to devices.
type Bundle struct {
	BundleVersion string   `json:"bundle_version"` // e.g. 2026-11-09.1
	CreatedAt     string   `json:"created_at"`
	Records       []Record `json:"records"`
	KeyID         string   `json:"key_id,omitempty"`
	Signature     string   `json:"signature,omitempty"` // Ed25519 over canonical records JSON
}
