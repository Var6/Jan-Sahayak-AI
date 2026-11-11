// Package docassist helps a citizen check which of the documents a scheme
// asks for they already have. It is NOT authentication and NOT storage:
//
//   - one photo at a time is read in memory by a tesseract subprocess
//     (image on stdin, text on stdout, nothing written to disk) and then
//     dropped;
//   - the OCR text is classified into a document type by keyword rules and
//     only a few confirmable, non-sensitive fields are kept (type, a
//     clearly labelled name, the issuing office, the issue date);
//   - identifiers (Aadhaar, account, PAN, EPIC numbers) are masked to their
//     last four characters; the raw OCR text never leaves this package;
//   - the citizen confirms the type, and the confirmed types are compared
//     with the scheme record's document list — only when the knowledge
//     display policy allows the record's facts to be shown.
//
// OCR cannot tell a genuine document from a forged one, a photocopy or a
// document that belongs to someone else; every reply says so.
package docassist

import (
	"errors"
	"strings"
)

// DocType is a document category the classifier knows.
type DocType string

// Document types. Unknown covers photo-less pages, unreadable photos and
// anything the rules do not recognise.
const (
	Aadhaar              DocType = "aadhaar"
	RationCard           DocType = "ration_card"
	CasteCertificate     DocType = "caste_certificate"
	IncomeCertificate    DocType = "income_certificate"
	ResidenceCertificate DocType = "residence_certificate"
	BankPassbook         DocType = "bank_passbook"
	VoterID              DocType = "voter_id"
	PAN                  DocType = "pan"
	LandRecord           DocType = "land_record"
	BirthCertificate     DocType = "birth_certificate"
	SchoolCertificate    DocType = "school_certificate"
	Unknown              DocType = "unknown"
)

// AllTypes lists the known types (Unknown excluded), in display order.
var AllTypes = []DocType{
	Aadhaar, RationCard, CasteCertificate, IncomeCertificate, ResidenceCertificate,
	BankPassbook, VoterID, PAN, LandRecord, BirthCertificate, SchoolCertificate,
}

// Valid reports whether t is a known type (Unknown included).
func (t DocType) Valid() bool {
	if t == Unknown {
		return true
	}
	for _, k := range AllTypes {
		if k == t {
			return true
		}
	}
	return false
}

// Confidence of a classification.
const (
	ConfHigh = "high" // clear winner on strong keywords
	ConfLow  = "low"  // some evidence; the citizen must confirm
	ConfNone = "none" // not recognised
)

// MaskedID is an identifier seen on the document, masked to its last four
// characters ("XXXX XXXX 1234"). It lets the citizen recognise their own
// document without the full number ever reaching the screen.
type MaskedID struct {
	Kind  string `json:"kind"` // aadhaar | vid | pan | epic | account | number
	Value string `json:"value"`
}

// Result is what one photo yields. It never contains the OCR text.
type Result struct {
	Type       DocType    `json:"doc_type"`
	Confidence string     `json:"confidence"`
	Name       string     `json:"name,omitempty"`
	Authority  string     `json:"issuing_authority,omitempty"`
	IssueDate  string     `json:"issue_date,omitempty"`
	MaskedIDs  []MaskedID `json:"masked_ids,omitempty"`
	// Chars is how many characters OCR returned (0 = nothing readable);
	// it helps the UI suggest a better photo, and holds no content.
	Chars int `json:"-"`
}

// Analyze classifies OCR text and extracts the confirmable fields. The
// caller must drop text afterwards; Result holds no raw text.
func Analyze(text string) Result {
	text = strings.ToValidUTF8(text, "")
	if len(text) > maxAnalyzeBytes {
		text = text[:maxAnalyzeBytes]
		text = strings.ToValidUTF8(text, "")
	}
	t, conf := Classify(text)
	r := Result{Type: t, Confidence: conf, Chars: len([]rune(strings.TrimSpace(text)))}
	if t == Unknown {
		return r
	}
	r.Name = extractName(text)
	r.Authority = extractAuthority(text)
	r.IssueDate = extractIssueDate(text)
	r.MaskedIDs = maskedIDs(text, t)
	return r
}

// maxAnalyzeBytes bounds the analysis work (the OCR output is already
// capped by the runner).
const maxAnalyzeBytes = 64 << 10

// Errors of the OCR runner.
var (
	ErrBusy        = errors.New("docassist: OCR is busy with another image")
	ErrTimeout     = errors.New("docassist: OCR timed out")
	ErrUnavailable = errors.New("docassist: OCR engine unavailable")
	ErrBadImage    = errors.New("docassist: not an acceptable JPEG image")
	ErrTooLarge    = errors.New("docassist: image too large")
)
