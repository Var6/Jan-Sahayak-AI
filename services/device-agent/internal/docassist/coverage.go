package docassist

import (
	"strings"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/knowledge"
)

// Requirement matching: which document types could satisfy a requirement
// line of a scheme record ("Photocopy of the bank passbook" → passbook).
// This only says "this kind of paper"; conditions in the requirement
// (whose document, issued when, by which office) are not checked, and
// every coverage reply says so.
//
// Keywords are squashed like the classifier's. A requirement that names a
// bank account is about the account even when it also says "Aadhaar"
// ("Bank account seeded with Aadhaar"), so a bank match drops Aadhaar.
// A bare "bank" (as in "6 months' bank statement") is only one
// alternative among other named papers; when another document type is
// also named, it does not make a passbook count (a passbook is neither
// income proof nor a bank statement). Passbook-specific words
// (passbook, account number, IFSC) always count.
type reqRule struct {
	t   DocType
	kws []string
	// weak keywords count only when no other (non-Aadhaar) type matches.
	weak []string
}

var reqRules = func() []reqRule {
	rs := []reqRule{
		{t: BankPassbook, kws: []string{"passbook", "पासबुक", "bankaccount", "बैंकखाता", "accountnumber", "खातानंबर", "खातासंख्या", "ifsc"}, weak: []string{"bank", "बैंक"}},
		{t: Aadhaar, kws: []string{"aadhaar", "aadhar", "आधार"}},
		{t: VoterID, kws: []string{"voterid", "वोटरआईडी", "वोटरपहचानपत्र", "मतदातापहचानपत्र", "electorsphotoidentitycard"}},
		{t: PAN, kws: []string{"pancard", "पैनकार्ड", "permanentaccountnumber"}},
		{t: RationCard, kws: []string{"rationcard", "राशनकार्ड"}},
		{t: CasteCertificate, kws: []string{"castecertificate", "जातिप्रमाणपत्र"}},
		{t: IncomeCertificate, kws: []string{"incomecertificate", "आयप्रमाणपत्र"}},
		{t: ResidenceCertificate, kws: []string{"residencecertificate", "residentialcertificate", "निवासप्रमाणपत्र", "आवासीयप्रमाणपत्र", "proofofresidence", "निवासकाप्रमाण"}},
		{t: LandRecord, kws: []string{"landrecord", "landrelated", "जमीनकारिकॉर्ड", "जमीनसेजुड़ा", "खतियान", "जमाबंदी", "भूस्वामित्व"}},
		{t: BirthCertificate, kws: []string{"birthcertificate", "जन्मप्रमाणपत्र"}},
		{t: SchoolCertificate, kws: []string{"matriculation", "मैट्रिक", "marksheet", "अंकपत्र", "class10", "10वीं", "intermediate", "इंटरमीडिएट", "academicdocuments", "शैक्षणिकदस्तावेज"}},
	}
	for i := range rs {
		for j, k := range rs[i].kws {
			rs[i].kws[j] = squash(k)
		}
		for j, k := range rs[i].weak {
			rs[i].weak[j] = squash(k)
		}
	}
	return rs
}()

// TypesFor returns the document types that look like they satisfy doc
// (empty: a camera cannot check it — a form, a photograph, an affidavit).
func TypesFor(doc scheme.Doc) []DocType {
	sq := squash(doc.Name.Hi + " " + doc.Name.En)
	has := func(kws []string) bool {
		for _, k := range kws {
			if strings.Contains(sq, k) {
				return true
			}
		}
		return false
	}
	var strong, weak []DocType
	for _, r := range reqRules {
		switch {
		case has(r.kws):
			strong = append(strong, r.t)
		case has(r.weak):
			weak = append(weak, r.t)
		}
	}
	bank := false
	for _, t := range append(append([]DocType{}, strong...), weak...) {
		if t == BankPassbook {
			bank = true
		}
	}
	var out []DocType
	for _, t := range strong {
		if bank && t == Aadhaar {
			continue // "bank account seeded with Aadhaar"
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		out = weak
	}
	return out
}

// Item statuses.
const (
	StatusSeen         = "seen"          // a confirmed document of a matching type was shown
	StatusNotSeen      = "not_seen"      // checkable, but no matching document shown yet
	StatusNotCheckable = "not_checkable" // not a kind of paper the camera check knows
)

// CoverageItem is one requirement of the scheme record.
type CoverageItem struct {
	Name     string `json:"name"`
	Note     string `json:"note,omitempty"`
	Optional bool   `json:"optional"`
	Status   string `json:"status"`
}

// Coverage compares the confirmed document types with a scheme record.
type Coverage struct {
	SchemeID   string         `json:"scheme_id"`
	SchemeName string         `json:"scheme_name"`
	Mode       string         `json:"mode"` // knowledge.Mode: factual | unverified_dev | name_only
	Verified   bool           `json:"verified"`
	Items      []CoverageItem `json:"items,omitempty"`
	// Message replaces Items when the record's document list may not be
	// shown (name_only): verification is required first.
	Message    string `json:"message,omitempty"`
	Disclaimer string `json:"disclaimer,omitempty"`
}

// Compare builds the coverage of rec for the confirmed types, honouring
// the knowledge display policy: the record's document list is used only in
// ModeFactual, or in ModeUnverifiedDev under the "not verified" disclaimer.
func Compare(rec scheme.Record, mode knowledge.Mode, have []DocType, lang string) Coverage {
	c := Coverage{SchemeID: rec.ID, SchemeName: pick(rec.Name, lang), Mode: mode.String(), Verified: mode == knowledge.ModeFactual}
	if mode == knowledge.ModeNameOnly {
		c.Message = knowledge.NotVerifiedMessage(lang) + " " + msg(lang, msgListNeedsVerification)
		return c
	}
	if mode == knowledge.ModeUnverifiedDev {
		c.Disclaimer = knowledge.UnverifiedDisclaimer(lang)
	}
	got := map[DocType]bool{}
	for _, t := range have {
		got[t] = true
	}
	add := func(d scheme.Doc, optional bool) {
		it := CoverageItem{Name: pick(d.Name, lang), Optional: optional, Status: StatusNotCheckable}
		if d.Note != nil {
			it.Note = pick(*d.Note, lang)
		}
		if ts := TypesFor(d); len(ts) > 0 {
			it.Status = StatusNotSeen
			for _, t := range ts {
				if got[t] {
					it.Status = StatusSeen
					break
				}
			}
		}
		c.Items = append(c.Items, it)
	}
	for _, d := range rec.MandatoryDocs {
		add(d, false)
	}
	for _, d := range rec.OptionalDocs {
		add(d, true)
	}
	if len(c.Items) == 0 {
		c.Message = msg(lang, msgNoDocList)
	}
	return c
}

func pick(t scheme.Text, lang string) string {
	if lang == "en" && t.En != "" {
		return t.En
	}
	if t.Hi != "" {
		return t.Hi
	}
	return t.En
}
