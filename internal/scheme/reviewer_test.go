package scheme

import (
	"testing"
	"time"
)

func TestValidReviewer(t *testing.T) {
	ok := []string{
		"Asha Devi", "Kailash Paswan", "Vaishali Kumari", "Sairam", "Test Fixture Reviewer", "someone",
		"0192f3a4-5b6c-7d8e-9fab-0123456789ab", // central-api reviewer user id (UUID v7)
		"reviewer:asha.devi", "reviewer:rev-42", "आशा देवी",
	}
	for _, s := range ok {
		if err := ValidReviewer(s); err != nil {
			t.Errorf("ValidReviewer(%q) = %v, want ok", s, err)
		}
	}
	bad := []string{
		"", " ", "GPT-4o", "gpt4", "Gemma auto-check", "auto-verifier bot", "machine", "model", "Copilot", "claude",
		"AI importer", "LLM", "ChatGPT", "llama-3 reviewer", "reviewer:", "reviewer:Bad Id", "x", "42",
	}
	for _, s := range bad {
		if err := ValidReviewer(s); err == nil {
			t.Errorf("ValidReviewer(%q) accepted, want refusal", s)
		}
	}
}

func TestReviewDueMustParse(t *testing.T) {
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	r := Record{
		ID: "x-test", Version: 1, Kind: KindScheme, Name: Text{Hi: "क", En: "k"}, Category: Categories[0],
		Summary: Text{Hi: "क"}, Sources: []Source{{URL: "https://x.gov.in", AccessedOn: "2026-10-01"}},
		Verification: Verification{Status: Verified, VerifiedBy: "Asha Devi", VerifiedAt: "2026-10-01T00:00:00Z", ReviewDue: "2027-01-01"},
	}
	if err := r.Validate(); err != nil || !r.IsFactual(now) {
		t.Fatalf("well-formed record: %v factual=%v", err, r.IsFactual(now))
	}
	for _, due := range []string{"31-12-2020", "2027/01/01", "soon"} {
		r.Verification.ReviewDue = due
		if r.Validate() == nil {
			t.Errorf("review_due %q passed Validate", due)
		}
		if r.IsFactual(now) {
			t.Errorf("review_due %q kept the record factual", due)
		}
	}
	r.Verification.ReviewDue = "2026-11-08"
	if r.IsFactual(now) {
		t.Error("overdue record is factual")
	}
	r.Verification.ReviewDue = ""
	r.Verification.VerifiedAt = "yesterday"
	if r.Validate() == nil {
		t.Error("malformed verified_at passed Validate")
	}
	r.Verification.VerifiedAt = "2026-10-01T00:00:00Z"
	r.Verification.VerifiedBy = "Gemma auto-check"
	if r.Validate() == nil || r.IsFactual(now) {
		t.Error("machine reviewer accepted")
	}
}
