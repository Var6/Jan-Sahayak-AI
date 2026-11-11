//go:build review2

package docassist

// Independent review (Phase 10–12), repro tests. Run with:
//
//	go test -tags review2 -run Review2 ./services/device-agent/internal/docassist/

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
)

// R2-3: the "bank" requirement rule is exclusive, so a requirement that
// lists several alternatives and merely ends with "bank statement" maps
// to the passbook only. On the real bihar-student-credit-card record a
// confirmed income certificate leaves this item "not_seen", while a bank
// passbook (which is neither income proof nor a 6-month statement) marks
// it "seen".
func TestReview2CompoundRequirementWithBankIsPassbookOnly(t *testing.T) {
	d := scheme.Doc{Name: scheme.Text{
		En: "Last year's income certificate / Form 16, last two years' income tax returns, last 6 months' bank statement",
		Hi: "पिछले साल का आय प्रमाण पत्र / फॉर्म 16, पिछले दो साल का आयकर रिटर्न, पिछले 6 महीने का बैंक स्टेटमेंट",
	}}
	got := TypesFor(d)
	hasIncome := false
	for _, ty := range got {
		if ty == IncomeCertificate {
			hasIncome = true
		}
	}
	if !hasIncome {
		t.Fatalf("TypesFor(%q) = %v; an income certificate is explicitly named and must count", d.Name.En, got)
	}
}

// R2-4: CheckJPEG reads only the header, so a truncated JPEG (valid SOF,
// missing scan data) passes. tesseract then exits 1 ("pix not read") and
// the runner reports ErrUnavailable, which the API treats as an engine
// fault: `ocr` is reported UNAVAILABLE and capabilities.camera turns off
// for every citizen until the next self-test (up to 2 min). A bad photo
// should be ErrBadImage (400), not an engine fault.
func TestReview2TruncatedJPEGIsEngineFault(t *testing.T) {
	ts := realTesseract(t)
	full, err := os.ReadFile(filepath.Join("testdata", "fake_income.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	trunc := full[:min(len(full), 3000)]
	if err := CheckJPEG(trunc, ImageLimits{}); err != nil {
		t.Skipf("truncated JPEG already refused by CheckJPEG: %v", err)
	}
	_, err = ts.Recognize(context.Background(), trunc)
	if err == nil {
		t.Fatal("expected an error for a truncated JPEG")
	}
	if errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrBadImage) {
		t.Fatalf("truncated photo reported as engine failure (%v); the API will mark OCR UNAVAILABLE for everyone", err)
	}
}
