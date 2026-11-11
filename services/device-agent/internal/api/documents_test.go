package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/docassist"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/knowledge"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/session"
)

// All document text and images here are synthetic and FAKE.

// fakeOCR stands in for the tesseract subprocess.
type fakeOCR struct {
	mu    sync.Mutex
	text  string
	err   error
	block chan struct{} // when set, Recognize waits for it (or ctx)
	calls int
	sizes []int
}

func (f *fakeOCR) Recognize(ctx context.Context, img []byte) (string, error) {
	f.mu.Lock()
	f.calls++
	f.sizes = append(f.sizes, len(img))
	text, err, block := f.text, f.err, f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return text, err
}
func (f *fakeOCR) Health(context.Context) error { return nil }

func (f *fakeOCR) set(text string, err error) {
	f.mu.Lock()
	f.text, f.err = text, err
	f.mu.Unlock()
}

const fakeAadhaarText = "नमूना — नकली दस्तावेज़ / FAKE SPECIMEN\nभारत सरकार\nGovernment of India\nटेस्ट नाम\nTest Name\nजन्म तिथि / DOB: 01/01/1990\n1234 5678 9012\nमेरा आधार, मेरी पहचान\n"

// testDocRecord is a fixture record (not a real scheme).
func testDocRecord() scheme.Record {
	d := func(hi, en string) scheme.Doc { return scheme.Doc{Name: scheme.Text{Hi: hi, En: en}} }
	return scheme.Record{
		ID: "test-doc-scheme", Name: scheme.Text{Hi: "परीक्षण योजना (FIXTURE)", En: "Test scheme (FIXTURE)"},
		MandatoryDocs: []scheme.Doc{d("आधार कार्ड", "Aadhaar card"), d("बैंक पासबुक की फोटोकॉपी", "Photocopy of the bank passbook"), d("आवेदन पत्र", "Application form")},
	}
}

type docHarness struct {
	*harness
	ocr  *fakeOCR
	mode knowledge.Mode
	ocrS health.State
}

func newDocHarness(t *testing.T) *docHarness {
	h := newHarness(t, hopts{})
	d := &docHarness{harness: h, ocr: &fakeOCR{text: fakeAadhaarText}, mode: knowledge.ModeFactual, ocrS: health.Healthy}
	h.mon.Register(health.Component{Name: CompOCR, Check: func(context.Context) health.Result {
		h.mu.Lock()
		defer h.mu.Unlock()
		return health.Result{State: d.ocrS}
	}})
	h.mon.CheckAll(context.Background())
	h.srv.d.Documents = &DocumentDeps{
		OCR: d.ocr,
		Scheme: func(ctx context.Context, id string) (scheme.Record, knowledge.Mode, error) {
			if id != "test-doc-scheme" {
				return scheme.Record{}, knowledge.ModeNameOnly, knowledge.ErrNotFound
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			return testDocRecord(), d.mode, nil
		},
		Timeout: 3 * time.Second,
	}
	return d
}

func (d *docHarness) setOCR(s health.State) {
	d.mu.Lock()
	d.ocrS = s
	d.mu.Unlock()
	d.mon.CheckAll(context.Background())
}

// ready starts a session and releases the greeting (READY).
func (d *docHarness) ready() string {
	d.t.Helper()
	res := d.start()
	sid := res.Session.ID
	if res.Session.Activity == session.Speaking {
		if w := d.post("/api/v1/session/"+sid+"/speaking-done", nil); w.Code != 200 {
			d.t.Fatalf("speaking-done: %d %s", w.Code, w.Body)
		}
	}
	return sid
}

func (d *docHarness) consent(sid string, v bool) *http.Response {
	w := d.post("/api/v1/session/"+sid+"/camera-consent", map[string]bool{"consent": v})
	return w.Result()
}

func (d *docHarness) upload(sid string, img []byte, ct string) (int, string, []byte) {
	w := d.req(http.MethodPost, "/api/v1/session/"+sid+"/document", img, ct, nil)
	return w.Code, errCode(d.t, w), w.Body.Bytes()
}

func (d *docHarness) setScheme(sid, id string) {
	d.srv.mu.Lock()
	c := d.srv.convo
	d.srv.mu.Unlock()
	if c == nil || c.id != sid {
		d.t.Fatal("no convo")
	}
	c.turnMu.Lock()
	c.st.Scheme = id
	c.turnMu.Unlock()
}

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDocumentCapability(t *testing.T) {
	h := newHarness(t, hopts{})
	if h.srv.Capabilities().Camera {
		t.Fatal("camera without document assistance")
	}
	d := newDocHarness(t)
	if !d.srv.Capabilities().Camera {
		t.Fatal("camera should be on: OCR healthy")
	}
	d.setOCR(health.Unavailable)
	if d.srv.Capabilities().Camera {
		t.Fatal("camera must be off while OCR is unavailable")
	}
	sid := d.ready()
	if r := d.consent(sid, true); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("consent with OCR down: %d", r.StatusCode)
	}
	if code, ec, _ := d.upload(sid, testJPEG(t, 800, 600), "image/jpeg"); code != 503 || ec != "unavailable" {
		t.Fatalf("upload with OCR down: %d %s", code, ec)
	}
}

func TestDocumentRequiresConsentAndValidImage(t *testing.T) {
	d := newDocHarness(t)
	sid := d.ready()
	img := testJPEG(t, 1200, 800)

	if code, ec, _ := d.upload(sid, img, "image/jpeg"); code != 403 || ec != "no_consent" {
		t.Fatalf("no consent: %d %s", code, ec)
	}
	if d.ocr.calls != 0 {
		t.Fatal("OCR ran without consent")
	}
	// Consent body must be explicit.
	if w := d.post("/api/v1/session/"+sid+"/camera-consent", map[string]any{}); w.Code != 400 {
		t.Fatalf("missing consent field: %d", w.Code)
	}
	if w := d.post("/api/v1/session/wrong/camera-consent", map[string]bool{"consent": true}); w.Code != 409 || errCode(t, w) != "wrong_session" {
		t.Fatalf("wrong session: %d %s", w.Code, w.Body)
	}
	w := d.post("/api/v1/session/"+sid+"/camera-consent", map[string]bool{"consent": true})
	if w.Code != 200 || !decode[sessionResult](t, w).Session.CameraConsent {
		t.Fatalf("consent: %d %s", w.Code, w.Body)
	}

	before := d.sm.Current().Activity
	cases := []struct {
		name string
		body []byte
		ct   string
		code int
		ec   string
	}{
		{"png content type", img, "image/png", 415, "bad_image"},
		{"no content type", img, "", 415, "bad_image"},
		{"not a jpeg", []byte("this is not an image at all, just text"), "image/jpeg", 400, "bad_image"},
		{"too many pixels", testJPEG(t, 2000, 1000), "image/jpeg", 413, "too_large"},
		{"too thin", testJPEG(t, 800, 50), "image/jpeg", 400, "bad_image"},
		{"too many bytes", append(append([]byte{}, img...), make([]byte, 2<<20)...), "image/jpeg", 413, "too_large"},
	}
	for _, c := range cases {
		if code, ec, _ := d.upload(sid, c.body, c.ct); code != c.code || ec != c.ec {
			t.Errorf("%s: %d %s, want %d %s", c.name, code, ec, c.code, c.ec)
		}
	}
	if d.ocr.calls != 0 {
		t.Fatalf("OCR ran on rejected uploads: %d", d.ocr.calls)
	}
	if got := d.sm.Current().Activity; got != before {
		t.Fatalf("rejected uploads changed the activity: %s → %s", before, got)
	}
}

func TestDocumentWrongState(t *testing.T) {
	d := newDocHarness(t)
	res := d.start() // greeting audio → SPEAKING
	sid := res.Session.ID
	if res.Session.Activity != session.Speaking {
		t.Fatalf("expected SPEAKING after start, got %s", res.Session.Activity)
	}
	if r := d.consent(sid, true); r.StatusCode != 200 {
		t.Fatalf("consent: %d", r.StatusCode)
	}
	if code, ec, _ := d.upload(sid, testJPEG(t, 800, 600), "image/jpeg"); code != 409 || ec != "bad_state" {
		t.Fatalf("upload while SPEAKING: %d %s", code, ec)
	}
	// LISTENING: the UI stopped its recording to open the camera → accepted.
	if w := d.post("/api/v1/session/"+sid+"/speaking-done", nil); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := d.post("/api/v1/session/"+sid+"/listen", nil); w.Code != 200 {
		t.Fatalf("listen: %d %s", w.Code, w.Body)
	}
	if code, _, body := d.upload(sid, testJPEG(t, 800, 600), "image/jpeg"); code != 200 {
		t.Fatalf("upload while LISTENING: %d %s", code, body)
	}
	if got := d.sm.Current().Activity; got != session.Ready {
		t.Fatalf("after upload: %s", got)
	}
	// No session / wrong session.
	if code, ec, _ := d.upload("nope", testJPEG(t, 800, 600), "image/jpeg"); code != 409 || ec != "wrong_session" {
		t.Fatalf("wrong session: %d %s", code, ec)
	}
}

func TestDocumentFlowMasksAndCompares(t *testing.T) {
	d := newDocHarness(t)
	sid := d.ready()
	d.consent(sid, true)
	img := testJPEG(t, 1200, 800)

	// Confirm before any photo.
	if w := d.post("/api/v1/session/"+sid+"/document/confirm", map[string]bool{"correct": true}); w.Code != 409 {
		t.Fatalf("confirm without photo: %d", w.Code)
	}

	code, _, body := d.upload(sid, img, "image/jpeg")
	if code != 200 {
		t.Fatalf("upload: %d %s", code, body)
	}
	var res DocumentResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if res.Document.Type != docassist.Aadhaar || !res.CanConfirm || res.Document.Label != "आधार कार्ड" {
		t.Fatalf("result: %+v", res)
	}
	if res.Text != "यह आधार कार्ड लगता है — क्या यह सही है?" || res.Notice == "" {
		t.Fatalf("texts: %q / %q", res.Text, res.Notice)
	}
	if len(res.Document.MaskedIDs) != 1 || res.Document.MaskedIDs[0].Value != "XXXX XXXX 9012" {
		t.Fatalf("masked: %+v", res.Document.MaskedIDs)
	}
	// The raw OCR text never reaches the UI.
	for _, leak := range []string{"1234 5678", "123456789012", "टेस्ट नाम", "Test Name", "01/01/1990", "मेरा आधार"} {
		if bytes.Contains(body, []byte(leak)) {
			t.Errorf("response leaks %q: %s", leak, body)
		}
	}
	if res.Session.Activity != session.Ready {
		t.Fatalf("activity: %s", res.Session.Activity)
	}

	// "No, retake": nothing is confirmed.
	w := d.post("/api/v1/session/"+sid+"/document/confirm", map[string]bool{"correct": false})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	cr := decode[ConfirmResult](t, w)
	if len(cr.Confirmed) != 0 || cr.Coverage != nil || cr.Text == "" {
		t.Fatalf("retake: %+v", cr)
	}

	// Again, confirm, but no scheme is being discussed.
	d.upload(sid, img, "image/jpeg")
	cr = decode[ConfirmResult](t, d.post("/api/v1/session/"+sid+"/document/confirm", map[string]bool{"correct": true}))
	if len(cr.Confirmed) != 1 || cr.Confirmed[0].Type != docassist.Aadhaar || cr.Coverage != nil {
		t.Fatalf("no scheme: %+v", cr)
	}
	if !strings.Contains(cr.Text, docassist.NoSchemeText("hi")) {
		t.Fatalf("no-scheme text: %q", cr.Text)
	}

	// A verified (factual) scheme: compare with its document list.
	d.setScheme(sid, "test-doc-scheme")
	d.ocr.set("TEST BANK PASSBOOK\nName: Test Name\nAccount No: 123456789012\nIFSC: TEST0001234\n", nil)
	code, _, body = d.upload(sid, img, "image/jpeg")
	if code != 200 || bytes.Contains(body, []byte("123456789012")) {
		t.Fatalf("passbook: %d %s", code, body)
	}
	cr = decode[ConfirmResult](t, d.post("/api/v1/session/"+sid+"/document/confirm", map[string]bool{"correct": true}))
	if cr.Coverage == nil || !cr.Coverage.Verified || len(cr.Coverage.Items) != 3 || cr.Caveat == "" {
		t.Fatalf("coverage: %+v", cr)
	}
	want := []string{docassist.StatusSeen, docassist.StatusSeen, docassist.StatusNotCheckable}
	for i, it := range cr.Coverage.Items {
		if it.Status != want[i] {
			t.Errorf("item %d %q: %s, want %s", i, it.Name, it.Status, want[i])
		}
	}
	if len(cr.Confirmed) != 2 {
		t.Fatalf("confirmed: %+v", cr.Confirmed)
	}

	// The same scheme, not verified (production policy): no document list.
	d.mu.Lock()
	d.mode = knowledge.ModeNameOnly
	d.mu.Unlock()
	d.upload(sid, img, "image/jpeg")
	cr = decode[ConfirmResult](t, d.post("/api/v1/session/"+sid+"/document/confirm", map[string]bool{"correct": true}))
	if cr.Coverage == nil || len(cr.Coverage.Items) != 0 || cr.Coverage.Message == "" || cr.Coverage.Verified || cr.Caveat != "" {
		t.Fatalf("name-only coverage: %+v", cr.Coverage)
	}

	// Unknown document: no confirm offered, nothing pending.
	d.ocr.set("आज मौसम अच्छा है", nil)
	code, _, body = d.upload(sid, img, "image/jpeg")
	if code != 200 {
		t.Fatal(string(body))
	}
	var unk DocumentResult
	_ = json.Unmarshal(body, &unk)
	if unk.Document.Type != docassist.Unknown || unk.CanConfirm {
		t.Fatalf("unknown: %+v", unk)
	}
	if w := d.post("/api/v1/session/"+sid+"/document/confirm", map[string]bool{"correct": true}); w.Code != 409 {
		t.Fatalf("confirm after unknown: %d", w.Code)
	}

	// Withdrawing consent forgets results and blocks uploads.
	d.upload(sid, img, "image/jpeg")
	if r := d.consent(sid, false); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	d.srv.mu.Lock()
	st := d.srv.convo.doc
	d.srv.mu.Unlock()
	if st.pending != nil || len(st.confirmed) != 0 {
		t.Fatalf("doc state kept after withdrawal: %+v", st)
	}
	if code, ec, _ := d.upload(sid, img, "image/jpeg"); code != 403 || ec != "no_consent" {
		t.Fatalf("after withdrawal: %d %s", code, ec)
	}
}

func TestDocumentSessionEndResetsEverything(t *testing.T) {
	d := newDocHarness(t)
	sid := d.ready()
	d.consent(sid, true)
	d.upload(sid, testJPEG(t, 800, 600), "image/jpeg")
	d.post("/api/v1/session/"+sid+"/document/confirm", map[string]bool{"correct": true})
	if w := d.post("/api/v1/session/"+sid+"/end", nil); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if code, _, _ := d.upload(sid, testJPEG(t, 800, 600), "image/jpeg"); code == 200 {
		t.Fatal("upload accepted after session end")
	}
	sid2 := d.ready()
	if d.sm.Current().CameraConsent {
		t.Fatal("camera consent carried into the next session")
	}
	if code, ec, _ := d.upload(sid2, testJPEG(t, 800, 600), "image/jpeg"); code != 403 || ec != "no_consent" {
		t.Fatalf("new session without consent: %d %s", code, ec)
	}
	d.consent(sid2, true)
	d.srv.mu.Lock()
	st := d.srv.convo.doc
	d.srv.mu.Unlock()
	if st.pending != nil || len(st.confirmed) != 0 {
		t.Fatalf("doc state leaked into the next session: %+v", st)
	}
}

func TestDocumentOCRErrors(t *testing.T) {
	d := newDocHarness(t)
	sid := d.ready()
	d.consent(sid, true)
	img := testJPEG(t, 800, 600)

	d.ocr.set("", docassist.ErrBusy)
	if code, ec, _ := d.upload(sid, img, "image/jpeg"); code != 503 || ec != "unavailable" {
		t.Fatalf("busy: %d %s", code, ec)
	}
	d.ocr.set("", docassist.ErrTimeout)
	if code, _, _ := d.upload(sid, img, "image/jpeg"); code != 503 {
		t.Fatalf("timeout: %d", code)
	}
	if !d.srv.Capabilities().Camera {
		t.Fatal("a slow photo must not turn the camera off")
	}
	if got := d.sm.Current().Activity; got != session.Ready {
		t.Fatalf("activity after OCR error: %s", got)
	}
	// A photo the engine could not decode (truncated JPEG): 400, and the
	// camera stays on for everyone.
	d.ocr.set("", fmt.Errorf("%w: tesseract could not decode the image", docassist.ErrBadImage))
	if code, ec, _ := d.upload(sid, img, "image/jpeg"); code != 400 || ec != "bad_image" {
		t.Fatalf("undecodable photo: %d %s", code, ec)
	}
	if !d.srv.Capabilities().Camera {
		t.Fatal("an undecodable photo must not turn the camera off")
	}
	if got := d.sm.Current().Activity; got != session.Ready {
		t.Fatalf("activity after bad image: %s", got)
	}
	d.ocr.set("", errors.New("exec: tesseract crashed"))
	if code, _, _ := d.upload(sid, img, "image/jpeg"); code != 503 {
		t.Fatalf("crash: %d", code)
	}
	if d.srv.Capabilities().Camera {
		t.Fatal("an OCR crash must turn the camera off until the health check passes")
	}
}

// Consent withdrawn (or session ended) while OCR runs: the result is dropped.
func TestDocumentConsentWithdrawnDuringOCR(t *testing.T) {
	d := newDocHarness(t)
	sid := d.ready()
	d.consent(sid, true)
	release := make(chan struct{})
	d.ocr.mu.Lock()
	d.ocr.block = release
	d.ocr.mu.Unlock()
	done := make(chan [2]any, 1)
	go func() {
		code, ec, _ := d.upload(sid, testJPEG(t, 800, 600), "image/jpeg")
		done <- [2]any{code, ec}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for d.sm.Current().Activity != session.Processing && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// A turn cannot run meanwhile.
	if w := d.post("/api/v1/session/"+sid+"/text", map[string]string{"text": "पेंशन"}); w.Code != 409 {
		t.Fatalf("turn during OCR: %d", w.Code)
	}
	if r := d.consent(sid, false); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	close(release)
	got := <-done
	if got[0] != 403 || got[1] != "no_consent" {
		t.Fatalf("result after withdrawal: %v", got)
	}
	d.srv.mu.Lock()
	pending := d.srv.convo.doc.pending
	d.srv.mu.Unlock()
	if pending != nil {
		t.Fatal("result kept after consent withdrawal")
	}
	if a := d.sm.Current().Activity; a != session.Ready {
		t.Fatalf("activity: %s", a)
	}
}

// TestDocumentRealTesseract runs the real engine end to end on a synthetic
// fixture (skipped when tesseract with eng+hin is not installed).
func TestDocumentRealTesseract(t *testing.T) {
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("tesseract not installed")
	}
	ocr, err := docassist.NewTesseract(docassist.TesseractConfig{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := ocr.Health(context.Background()); err != nil {
		t.Skipf("tesseract not usable: %v", err)
	}
	d := newDocHarness(t)
	d.srv.d.Documents.OCR = ocr
	sid := d.ready()
	d.consent(sid, true)
	img, err := os.ReadFile("../docassist/testdata/fake_income.jpg")
	if err != nil {
		t.Fatal(err)
	}
	code, _, body := d.upload(sid, img, "image/jpeg")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var res DocumentResult
	_ = json.Unmarshal(body, &res)
	if res.Document.Type != docassist.IncomeCertificate || res.Document.Name != "टेस्ट नाम" || res.Document.IssueDate != "10/04/2026" {
		t.Fatalf("real OCR result: %s", body)
	}
}
