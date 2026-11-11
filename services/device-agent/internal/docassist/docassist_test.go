package docassist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/knowledge"
)

// All document text and images in these tests are synthetic and FAKE
// ("टेस्ट नाम", "1234 5678 9012", "ABCDE1234F"); see testdata/*.txt for
// the text each fixture image was rendered from.

func fixtureText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fake_"+name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "#", "")
}

func TestClassifyFixtureTexts(t *testing.T) {
	cases := map[string]DocType{
		"aadhaar": Aadhaar, "ration": RationCard, "income": IncomeCertificate,
		"passbook": BankPassbook, "pan": PAN, "unknown": Unknown,
	}
	for name, want := range cases {
		got, conf := Classify(fixtureText(t, name))
		if got != want {
			t.Errorf("%s: got %s (%s), want %s; scores %v", name, got, conf, want, Scores(fixtureText(t, name)))
		}
	}
}

func TestClassifyRules(t *testing.T) {
	cases := []struct {
		text string
		want DocType
		conf string
	}{
		{"भारत निर्वाचन आयोग\nमतदाता फोटो पहचान पत्र\nABC1234567", VoterID, ConfHigh},
		{"ELECTION COMMISSION OF INDIA\nELECTOR'S PHOTO IDENTITY CARD", VoterID, ConfHigh},
		{"जाति प्रमाण-पत्र\nअंचल कार्यालय", CasteCertificate, ConfHigh},
		{"आवासीय प्रमाण पत्र", ResidenceCertificate, ConfHigh},
		{"निवास प्रमाणपत्र", ResidenceCertificate, ConfHigh},
		{"खतियान\nमौजा: टेस्टपुर\nखेसरा 12 रकबा", LandRecord, ConfHigh},
		{"जन्म प्रमाण पत्र\nजन्म एवं मृत्यु पंजीकरण", BirthCertificate, ConfHigh},
		{"BIHAR SCHOOL EXAMINATION BOARD\nMarksheet\nRoll No", SchoolCertificate, ConfHigh},
		{"राशन कार्ड\nजन वितरण प्रणाली", RationCard, ConfHigh},
		{"Unique Identification Authority of India\n1234 5678 9012", Aadhaar, ConfHigh},
		{"आयकर विभाग\nINCOME TAX DEPARTMENT\nABCDE1234F", PAN, ConfHigh},
		// Weak evidence only: recognised but low confidence.
		{"आधार 1234 5678 9012", Aadhaar, ConfHigh},
		{"मैट्रिक", SchoolCertificate, ConfLow},
		// Not enough to say anything.
		{"बैंक", Unknown, ConfNone},
		{"", Unknown, ConfNone},
		{"नमस्ते दुनिया 2024", Unknown, ConfNone},
		// A tie between two types is not guessed; a near-tie is low confidence.
		{"जन्म प्रमाण पत्र\nआय प्रमाण पत्र", Unknown, ConfNone},
		{"जाति प्रमाण पत्र\nआय प्रमाण पत्र", CasteCertificate, ConfLow},
	}
	for _, c := range cases {
		got, conf := Classify(c.text)
		if got != c.want || conf != c.conf {
			t.Errorf("%q: got %s/%s, want %s/%s (scores %v)", c.text, got, conf, c.want, c.conf, Scores(c.text))
		}
	}
}

func TestSquash(t *testing.T) {
	if squash("प्रमाण-पत्र") != squash("प्रमाण पत्र") || squash("प्रमाणपत्र") != squash("प्रमाण पत्र") {
		t.Fatal("hyphen/space variants must squash equal")
	}
	if squash("ज़मीन") != squash("जमीन") {
		t.Fatal("nukta must be ignored")
	}
	if squash("१२३") != "123" {
		t.Fatalf("Devanagari digits: %q", squash("१२३"))
	}
}

func TestAnalyzeFixtureTextsFields(t *testing.T) {
	r := Analyze(fixtureText(t, "income"))
	if r.Type != IncomeCertificate || r.Name != "टेस्ट नाम" || r.IssueDate != "10/04/2026" {
		t.Fatalf("income: %+v", r)
	}
	if r.Authority != "अंचल कार्यालय, टेस्टपुर" {
		t.Fatalf("income authority: %q", r.Authority)
	}

	r = Analyze(fixtureText(t, "aadhaar"))
	if r.Type != Aadhaar {
		t.Fatalf("aadhaar: %+v", r)
	}
	if r.Name != "" {
		t.Fatalf("unlabelled name must not be guessed: %q", r.Name)
	}
	if r.IssueDate != "" {
		t.Fatalf("a birth date must never be reported as issue date: %q", r.IssueDate)
	}
	if len(r.MaskedIDs) != 1 || r.MaskedIDs[0] != (MaskedID{Kind: "aadhaar", Value: "XXXX XXXX 9012"}) {
		t.Fatalf("aadhaar mask: %+v", r.MaskedIDs)
	}

	r = Analyze(fixtureText(t, "passbook"))
	if r.Type != BankPassbook || r.Name != "Test Name" {
		t.Fatalf("passbook: %+v", r)
	}
	if len(r.MaskedIDs) != 1 || r.MaskedIDs[0] != (MaskedID{Kind: "account", Value: "XXXXXXXX9012"}) {
		t.Fatalf("account mask: %+v", r.MaskedIDs)
	}

	r = Analyze(fixtureText(t, "pan"))
	if r.Type != PAN || r.Name != "TEST NAME" || r.Authority != "INCOME TAX DEPARTMENT" {
		t.Fatalf("pan: %+v", r)
	}
	if len(r.MaskedIDs) != 1 || r.MaskedIDs[0] != (MaskedID{Kind: "pan", Value: "XXXXXX234F"}) {
		t.Fatalf("pan mask: %+v", r.MaskedIDs)
	}

	r = Analyze(fixtureText(t, "ration"))
	if r.Type != RationCard || r.Name != "टेस्ट नाम" || r.IssueDate != "15/08/2023" {
		t.Fatalf("ration: %+v", r)
	}

	r = Analyze(fixtureText(t, "unknown"))
	if r.Type != Unknown || r.Name != "" || len(r.MaskedIDs) != 0 {
		t.Fatalf("unknown documents yield no fields: %+v", r)
	}
}

var longDigits = regexp.MustCompile(`[0-9०-९](?:[ \-]?[0-9०-९]){5,}`)

// TestNoIdentifierLeaves: whatever the OCR text contains, nothing in a
// Result (as JSON, i.e. what the UI receives) holds a run of 6+ digits or
// an unmasked PAN/EPIC.
func TestNoIdentifierLeaves(t *testing.T) {
	texts := []string{
		fixtureText(t, "aadhaar"), fixtureText(t, "passbook"), fixtureText(t, "pan"), fixtureText(t, "ration"),
		"आधार\nनाम: टेस्ट 1234 5678 9012\nजारी करने वाला कार्यालय: खाता 123456789012 शाखा",
		"Aadhaar\nName: ABCDE1234F\nIssued by: Office ABC1234567 9876543210",
		"PASSBOOK\nName: Test\nIssuing Authority: Bank branch 9999-8888-7777",
		"आधार\nVID: 1234 5678 9012 3456\nनाम: टेस्ट नाम",
		"आधार कार्ड १२३४ ५६७८ ९०१२",
	}
	for _, txt := range texts {
		r := Analyze(txt)
		b, _ := json.Marshal(r)
		out := string(b)
		// The masked values legitimately keep 4 digits; strip them first.
		for _, m := range r.MaskedIDs {
			if !strings.Contains(m.Value, "XXXX") {
				t.Errorf("%q: unmasked id %+v", txt, m)
			}
			out = strings.ReplaceAll(out, m.Value, "")
		}
		if longDigits.MatchString(out) || rePAN.MatchString(out) || reEPIC.MatchString(out) {
			t.Errorf("identifier leaked for %q: %s", txt, out)
		}
		if strings.Contains(out, "टेस्ट 1234") {
			t.Errorf("name with digits must be rejected: %s", out)
		}
	}
}

func TestMaskShapes(t *testing.T) {
	r := Analyze("आधार\nVID: 1234 5678 9012 3456")
	if len(r.MaskedIDs) != 1 || r.MaskedIDs[0] != (MaskedID{Kind: "vid", Value: "XXXX XXXX XXXX 3456"}) {
		t.Fatalf("vid: %+v", r.MaskedIDs)
	}
	r = Analyze("भारत निर्वाचन आयोग\nमतदाता\nABC1234567")
	if len(r.MaskedIDs) != 1 || r.MaskedIDs[0] != (MaskedID{Kind: "epic", Value: "XXXXXX4567"}) {
		t.Fatalf("epic: %+v", r.MaskedIDs)
	}
	// Hyphenated dates are not identifiers.
	r = Analyze("आय प्रमाण पत्र\nदिनांक: 10-04-2026")
	if len(r.MaskedIDs) != 0 || r.IssueDate != "10/04/2026" {
		t.Fatalf("date: %+v", r)
	}
	// At most maxMaskedIDs.
	r = Analyze("PASSBOOK\n11111111 22222222 33333333\n44444444\n55555555\n66666666\n77777777")
	if len(r.MaskedIDs) > maxMaskedIDs {
		t.Fatalf("too many ids: %d", len(r.MaskedIDs))
	}
}

func TestNameRules(t *testing.T) {
	cases := map[string]string{
		"आय प्रमाण पत्र\nनाम: टेस्ट नाम":                  "टेस्ट नाम",
		"आय प्रमाण पत्र\nनाम / Name : टेस्ट नाम":          "टेस्ट नाम",
		"आय प्रमाण पत्र\nपिता का नाम: टेस्ट पिता":         "",
		"PASSBOOK\nFather's Name: Test Father":            "",
		"PASSBOOK\nName: X":                               "",
		"PASSBOOK\nName: " + strings.Repeat("a", 61):      "",
		"PASSBOOK\nName: Test   Name  ":                   "Test Name",
		"PASSBOOK\nName: Test@Name":                       "",
		"PASSBOOK\nName - Test Name\nName: Second Person": "Test Name",
	}
	for in, want := range cases {
		if got := Analyze(in).Name; got != want {
			t.Errorf("%q: name %q, want %q", in, got, want)
		}
	}
}

func TestIssueDateRules(t *testing.T) {
	cases := map[string]string{
		"आय प्रमाण पत्र\nजारी करने की तिथि: 1/4/2026":          "01/04/2026",
		"आय प्रमाण पत्र\nIssue Date: 31.12.2025":               "31/12/2025",
		"आय प्रमाण पत्र\nजन्म दिनांक: 01/01/1990":              "",
		"आय प्रमाण पत्र\nDOB / Issue Date: 01/01/1990":         "",
		"आय प्रमाण पत्र\nदिनांक: 45/08/2023":                   "",
		"आय प्रमाण पत्र\nजारी करने की तिथि: १०/०४/२०२६":        "10/04/2026",
		"आय प्रमाण पत्र\nकोई तारीख नहीं":                       "",
		"आय प्रमाण पत्र\nदिनांक 10/04/2026\nदिनांक 11/04/2026": "10/04/2026",
	}
	for in, want := range cases {
		if got := Analyze(in).IssueDate; got != want {
			t.Errorf("%q: date %q, want %q", in, got, want)
		}
	}
}

// ----------------------------------------------------------------- coverage

func recordFixture() scheme.Record {
	// Test fixture only — not a real scheme.
	doc := func(hi, en string) scheme.Doc { return scheme.Doc{Name: scheme.Text{Hi: hi, En: en}} }
	return scheme.Record{
		ID:   "test-scheme-fixture",
		Name: scheme.Text{Hi: "परीक्षण योजना (FIXTURE)", En: "Test scheme (FIXTURE)"},
		MandatoryDocs: []scheme.Doc{
			doc("आधार कार्ड", "Aadhaar card"),
			doc("आधार से DBT के लिए सीड बैंक खाता", "Bank account seeded with Aadhaar for DBT"),
			doc("आय प्रमाण पत्र", "Income certificate"),
			doc("आवेदन पत्र", "Application form"),
		},
		OptionalDocs: []scheme.Doc{doc("ज़मीन से जुड़ा प्रमाण पत्र या आवासीय प्रमाण पत्र", "Land-related certificate or residence certificate")},
	}
}

func TestTypesFor(t *testing.T) {
	r := recordFixture()
	want := [][]DocType{{Aadhaar}, {BankPassbook}, {IncomeCertificate}, nil}
	for i, d := range r.MandatoryDocs {
		got := TypesFor(d)
		if len(got) != len(want[i]) || (len(got) > 0 && got[0] != want[i][0]) {
			t.Errorf("%s: %v, want %v", d.Name.En, got, want[i])
		}
	}
	got := TypesFor(r.OptionalDocs[0])
	if len(got) != 2 || got[0] != ResidenceCertificate || got[1] != LandRecord {
		t.Errorf("land/residence: %v", got)
	}
	// Real record wordings (from packages/scheme-knowledge) that must map.
	for en, want := range map[string]DocType{
		"Photocopy of the bank passbook":                                   BankPassbook,
		"Copy of PAN card of student and co-applicant":                     PAN,
		"Class 10 and 12 certificates and marksheets":                      SchoolCertificate,
		"Birth certificate of the girl":                                    BirthCertificate,
		"Proof of residence (ID / passport / voter ID / driving licence)":  VoterID,
		"Ration card issued by the State of application":                   RationCard,
		"Caste certificate":                                                CasteCertificate,
		"Land record showing your name":                                    LandRecord,
		"Matriculation certificate (showing date of birth)":                SchoolCertificate,
		"Identity proof — Aadhaar (OTP verification) or any one of the 12": Aadhaar,
	} {
		ts := TypesFor(scheme.Doc{Name: scheme.Text{En: en}})
		found := false
		for _, x := range ts {
			found = found || x == want
		}
		if !found {
			t.Errorf("%q: %v, want it to include %s", en, ts, want)
		}
	}
	// The Election Commission's list of identity documents is not "a voter
	// ID": no inference beyond the wording.
	ts := TypesFor(scheme.Doc{Name: scheme.Text{En: "any one of the 12 identity documents accepted by the Election Commission of India"}})
	if len(ts) != 0 {
		t.Errorf("ECI list: %v", ts)
	}
}

func TestCompareModes(t *testing.T) {
	rec := recordFixture()
	c := Compare(rec, knowledge.ModeFactual, []DocType{Aadhaar, ResidenceCertificate}, "hi")
	if !c.Verified || c.Mode != "factual" || c.Disclaimer != "" || c.Message != "" || len(c.Items) != 5 {
		t.Fatalf("factual: %+v", c)
	}
	want := []string{StatusSeen, StatusNotSeen, StatusNotSeen, StatusNotCheckable, StatusSeen}
	for i, it := range c.Items {
		if it.Status != want[i] {
			t.Errorf("item %d %q: %s, want %s", i, it.Name, it.Status, want[i])
		}
	}
	if !c.Items[4].Optional || c.Items[0].Optional {
		t.Error("optional flags wrong")
	}
	if c.Items[0].Name != "आधार कार्ड" {
		t.Errorf("hi name: %q", c.Items[0].Name)
	}

	c = Compare(rec, knowledge.ModeUnverifiedDev, nil, "en")
	if c.Verified || c.Disclaimer == "" || len(c.Items) != 5 || c.Items[0].Name != "Aadhaar card" {
		t.Fatalf("unverified dev: %+v", c)
	}

	// Name-only: the record's document list is never shown.
	c = Compare(rec, knowledge.ModeNameOnly, []DocType{Aadhaar}, "hi")
	if len(c.Items) != 0 || c.Message == "" || c.Verified {
		t.Fatalf("name only: %+v", c)
	}
	if !strings.Contains(c.Message, knowledge.NotVerifiedMessage("hi")) {
		t.Fatalf("name-only message: %q", c.Message)
	}

	rec.MandatoryDocs, rec.OptionalDocs = nil, nil
	c = Compare(rec, knowledge.ModeFactual, nil, "hi")
	if c.Message == "" {
		t.Fatal("empty list needs a message")
	}
}

func TestMessages(t *testing.T) {
	if AskConfirm(IncomeCertificate, "hi") != "यह आय प्रमाण पत्र लगता है — क्या यह सही है?" {
		t.Fatal(AskConfirm(IncomeCertificate, "hi"))
	}
	if !strings.Contains(AskConfirm(PAN, "en"), "PAN card") {
		t.Fatal(AskConfirm(PAN, "en"))
	}
	if AskConfirm(Unknown, "hi") == "" || Notice("hi") == "" || Notice("en") == "" {
		t.Fatal("missing messages")
	}
	for _, ty := range append(AllTypes, Unknown) {
		if Label(ty, "hi") == "" || Label(ty, "en") == "" || !ty.Valid() {
			t.Errorf("label for %s", ty)
		}
	}
	if DocType("passport").Valid() {
		t.Error("unknown type valid")
	}
}

// ----------------------------------------------------------------- images

func encodeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestCheckJPEG(t *testing.T) {
	ok := encodeJPEG(t, 1200, 800)
	if err := CheckJPEG(ok, ImageLimits{}); err != nil {
		t.Fatalf("valid: %v", err)
	}
	fx, err := os.ReadFile("testdata/fake_aadhaar.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckJPEG(fx, ImageLimits{}); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := CheckJPEG(encodeJPEG(t, 1700, 800), ImageLimits{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too wide: %v", err)
	}
	if err := CheckJPEG(encodeJPEG(t, 800, 100), ImageLimits{}); !errors.Is(err, ErrBadImage) {
		t.Fatalf("too thin: %v", err)
	}
	if err := CheckJPEG(ok, ImageLimits{MaxBytes: 100}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too many bytes: %v", err)
	}
	var pb bytes.Buffer
	_ = png.Encode(&pb, image.NewGray(image.Rect(0, 0, 400, 400)))
	if err := CheckJPEG(pb.Bytes(), ImageLimits{}); !errors.Is(err, ErrBadImage) {
		t.Fatalf("png: %v", err)
	}
	if err := CheckJPEG(ok[:10], ImageLimits{}); !errors.Is(err, ErrBadImage) {
		t.Fatalf("truncated: %v", err)
	}
	if err := CheckJPEG(nil, ImageLimits{}); !errors.Is(err, ErrBadImage) {
		t.Fatalf("empty: %v", err)
	}
}

func TestCappedBuffer(t *testing.T) {
	c := &cappedBuffer{max: 5}
	n, err := c.Write([]byte("hello world"))
	if n != 11 || err != nil || c.String() != "hello" || c.n != 11 {
		t.Fatalf("%d %v %q", n, err, c.String())
	}
	_, _ = c.Write([]byte("more"))
	if c.String() != "hello" {
		t.Fatal(c.String())
	}
}

// ----------------------------------------------------------------- runner (fake engine)

// fakeEngine writes a shell script standing in for tesseract (test only).
func fakeEngine(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script engine")
	}
	p := filepath.Join(t.TempDir(), "fake-tesseract")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunnerCapsOutputAndPassesStdin(t *testing.T) {
	// Echo stdin back, then flood stdout: the runner keeps MaxOutputBytes.
	eng := fakeEngine(t, `cat; i=0; while [ $i -lt 2000 ]; do echo "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"; i=$((i+1)); done`)
	tr, err := NewTesseract(TesseractConfig{Path: eng, MaxOutputBytes: 1000, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tr.Recognize(context.Background(), []byte("IMAGE-BYTES\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1000 || !strings.HasPrefix(out, "IMAGE-BYTES\n") {
		t.Fatalf("len %d prefix %q", len(out), out[:20])
	}
}

func TestRunnerTimeoutKills(t *testing.T) {
	eng := fakeEngine(t, `sleep 30`)
	tr, _ := NewTesseract(TesseractConfig{Path: eng, Timeout: 300 * time.Millisecond})
	start := time.Now()
	_, err := tr.Recognize(context.Background(), []byte("x"))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestRunnerBusyAndFailures(t *testing.T) {
	eng := fakeEngine(t, `sleep 2; echo done`)
	tr, _ := NewTesseract(TesseractConfig{Path: eng, Timeout: 5 * time.Second})
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := tr.Recognize(context.Background(), []byte("x"))
		done <- err
	}()
	<-started
	deadline := time.Now().Add(2 * time.Second)
	for len(tr.sem) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := tr.Recognize(context.Background(), []byte("x")); !errors.Is(err, ErrBusy) {
		t.Fatalf("second concurrent call: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	bad := fakeEngine(t, `echo oops >&2; exit 3`)
	tr, _ = NewTesseract(TesseractConfig{Path: bad})
	if _, err := tr.Recognize(context.Background(), []byte("x")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("exit 3: %v", err)
	}
	tr, _ = NewTesseract(TesseractConfig{Path: filepath.Join(t.TempDir(), "missing")})
	if _, err := tr.Recognize(context.Background(), []byte("x")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing binary: %v", err)
	}
	if err := tr.Health(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing binary health: %v", err)
	}
	if _, err := NewTesseract(TesseractConfig{Languages: "eng;rm -rf"}); err == nil {
		t.Fatal("bad languages accepted")
	}
	// Cancelled by the caller (citizen left): not a timeout.
	slow := fakeEngine(t, `sleep 30`)
	tr, _ = NewTesseract(TesseractConfig{Path: slow, Timeout: 10 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := tr.Recognize(ctx, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestHealthWithFakeEngine(t *testing.T) {
	// A fake that answers --version, --list-langs and the self-test.
	eng := fakeEngine(t, `case "$1" in
--version) echo "tesseract 5.5.4";;
--list-langs) printf 'List of available languages (2):\neng\nhin\n';;
*) cat >/dev/null; echo "OCR TEST 2580";;
esac`)
	tr, _ := NewTesseract(TesseractConfig{Path: eng})
	if err := tr.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr.SelfTestDuration() <= 0 {
		t.Fatal("self-test duration not recorded")
	}
	missing := fakeEngine(t, `case "$1" in
--version) echo "tesseract 5.5.4";;
--list-langs) printf 'eng\n';;
esac`)
	tr, _ = NewTesseract(TesseractConfig{Path: missing})
	if err := tr.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "hin") {
		t.Fatalf("missing hin: %v", err)
	}
	wrong := fakeEngine(t, `case "$1" in
--version) echo "tesseract 5.5.4";;
--list-langs) printf 'eng\nhin\n';;
*) cat >/dev/null; echo "garbage";;
esac`)
	tr, _ = NewTesseract(TesseractConfig{Path: wrong})
	if err := tr.Health(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wrong self-test text: %v", err)
	}
}

// ----------------------------------------------------------------- real tesseract

func realTesseract(t *testing.T) *Tesseract {
	t.Helper()
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("tesseract not installed")
	}
	tr, err := NewTesseract(TesseractConfig{Timeout: 15 * time.Second, MaxMemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Health(context.Background()); err != nil {
		t.Skipf("tesseract present but not usable (eng+hin installed?): %v", err)
	}
	return tr
}

// TestRealTesseractFixtures runs the real engine on the synthetic JPEGs.
func TestRealTesseractFixtures(t *testing.T) {
	tr := realTesseract(t)
	cases := map[string]DocType{
		"aadhaar": Aadhaar, "ration": RationCard, "income": IncomeCertificate,
		"passbook": BankPassbook, "pan": PAN, "unknown": Unknown,
	}
	for name, want := range cases {
		img, err := os.ReadFile(filepath.Join("testdata", "fake_"+name+".jpg"))
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		text, err := tr.Recognize(context.Background(), img)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r := Analyze(text)
		t.Logf("%s: %s/%s in %s", name, r.Type, r.Confidence, time.Since(start).Round(time.Millisecond))
		if r.Type != want {
			t.Errorf("%s: classified %s, want %s", name, r.Type, want)
		}
		if name == "aadhaar" && (len(r.MaskedIDs) == 0 || r.MaskedIDs[0].Value != "XXXX XXXX 9012") {
			t.Errorf("aadhaar mask after real OCR: %+v", r.MaskedIDs)
		}
		if name == "income" && (r.Name != "टेस्ट नाम" || r.IssueDate != "10/04/2026") {
			t.Errorf("income fields after real OCR: %+v", r)
		}
	}
}
