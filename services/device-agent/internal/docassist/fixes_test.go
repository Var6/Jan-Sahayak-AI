package docassist

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
)

// Regression tests for the Phase 10–12 review findings. All text is
// synthetic and FAKE.

func TestAuthorityNeverShowsIdentifierDigits(t *testing.T) {
	const head = "नमूना — नकली दस्तावेज़ / FAKE SPECIMEN\nआय प्रमाण पत्र\nनाम: टेस्ट नाम\n"
	for _, line := range []string{
		"जारी करने वाला कार्यालय: अंचल कार्यालय 1234.5678.9012",
		"Issued by: Circle Office 1234/5678/9012",
		"Issued by: Circle Office 1234–5678–9012",
		"Issued by: Circle Office 1234_5678_9012",
		"Issued by: Circle Office 1234 S678 9O12",
		"जारी करने वाला कार्यालय: अंचल कार्यालय १२३४.५६७८.९०१२",
		"Issued by: Circle Office +91 98765 43210",
		"अंचल कार्यालय टेस्टपुर 1234  5678  9012",
	} {
		res := Analyze(head + line + "\n")
		if countDigits(res.Authority) > maxAuthorityDigits {
			t.Errorf("%q → authority %q shows more than %d digits", line, res.Authority, maxAuthorityDigits)
		}
	}
	for _, line := range []string{"Issued by: Circle Office abcde1234f", "Issued by: Circle Office ABCDEl234F", "Issued by: Circle Office abc1234567"} {
		res := Analyze(head + line + "\n")
		low := strings.ToLower(res.Authority)
		if strings.Contains(low, "abcde") || strings.Contains(low, "abc1") {
			t.Errorf("%q → authority %q shows the identifier", line, res.Authority)
		}
	}
	// An ordinary office keeps its name (and a short ward number).
	if res := Analyze(head + "जारी करने वाला कार्यालय: अंचल कार्यालय, वार्ड 12\n"); res.Authority != "अंचल कार्यालय, वार्ड 12" {
		t.Errorf("plain office: %q", res.Authority)
	}
}

func TestScrubLooseSeparators(t *testing.T) {
	for in, leak := range map[string]string{
		"x 1234.5678.9012 y":   "12345678",
		"x 1234, 5678, 9012 y": "5678",
		"x 1234:5678:9012 y":   "5678",
		"x ABCDE1234F y":       "ABCDE",
		"x abcde1234f y":       "abcde",
	} {
		if out := scrub(in); strings.Contains(strings.ReplaceAll(out, ".", ""), leak) {
			t.Errorf("scrub(%q) = %q", in, out)
		}
	}
	if out := scrub("Office of the Collector"); out != "Office of the Collector" {
		t.Errorf("plain words changed: %q", out)
	}
}

func TestAuthorityFallbackSkipsAddress(t *testing.T) {
	for _, text := range []string{
		"FAKE SPECIMEN\nभारत सरकार\nमेरा आधार, मेरी पहचान\nपता:\nग्राम टेस्टपुर, प्रखंड कार्यालय के पास, पिन 854301\n",
		"FAKE SPECIMEN\nAddress:\nNear Block Office, Testpur\n",
		"FAKE SPECIMEN\nS/O Test Father, Block Office Road\n",
	} {
		if res := Analyze(text); res.Authority != "" {
			t.Errorf("address line reported as issuing authority: %q", res.Authority)
		}
	}
	// A header naming the office is still found.
	if res := Analyze("FAKE SPECIMEN\nकार्यालय अंचल अधिकारी, टेस्टपुर\nआय प्रमाण पत्र\n"); res.Authority != "कार्यालय अंचल अधिकारी, टेस्टपुर" {
		t.Errorf("header office: %q", res.Authority)
	}
}

func TestTypesForCompoundRequirement(t *testing.T) {
	d := func(en string) scheme.Doc { return scheme.Doc{Name: scheme.Text{En: en}} }
	cases := map[string][]DocType{
		"Last year's income certificate / Form 16, last two years' income tax returns, last 6 months' bank statement": {IncomeCertificate},
		"Bank account seeded with Aadhaar for DBT": {BankPassbook},
		"Bank account number and IFSC":             {BankPassbook},
		"Photocopy of the bank passbook":           {BankPassbook},
		"Aadhaar number":                           {Aadhaar},
		"Last 6 months' bank statement":            {BankPassbook},
	}
	for name, want := range cases {
		got := TypesFor(d(name))
		if strings.Join(typeStrings(got), ",") != strings.Join(typeStrings(want), ",") {
			t.Errorf("TypesFor(%q) = %v, want %v", name, got, want)
		}
	}
}

func typeStrings(ts []DocType) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}

// A truncated JPEG passes the header check; tesseract cannot decode it.
// That is the photo's fault (ErrBadImage), not an engine failure.
func TestTruncatedJPEGIsBadImage(t *testing.T) {
	ts := realTesseract(t)
	full, err := os.ReadFile(filepath.Join("testdata", "fake_income.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	trunc := full[:min(len(full), 3000)]
	if err := CheckJPEG(trunc, ImageLimits{}); err != nil {
		t.Skipf("header check already refuses it: %v", err)
	}
	_, err = ts.Recognize(context.Background(), trunc)
	if !errors.Is(err, ErrBadImage) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("truncated JPEG: %v; want ErrBadImage only", err)
	}
}

func TestBadImageStderrFakeEngine(t *testing.T) {
	bad := fakeEngine(t, `case "$1" in
--version) echo "tesseract 5.5.4";;
--list-langs) printf 'eng\nhin\n';;
*) cat >/dev/null; echo "Error in pixReadMem: jpeg: no pix returned" >&2; exit 1;;
esac`)
	tr, _ := NewTesseract(TesseractConfig{Path: bad})
	if _, err := tr.Recognize(context.Background(), []byte("x")); !errors.Is(err, ErrBadImage) {
		t.Fatalf("decode failure: %v", err)
	}
	// The self-test image is known good: there it means the engine is broken.
	if err := tr.Health(context.Background()); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrBadImage) ||
		!strings.Contains(err.Error(), "self-test image not decoded") {
		t.Fatalf("health: %v", err)
	}
}
