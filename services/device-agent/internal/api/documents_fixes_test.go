package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression tests for the Phase 10–12 review findings (synthetic data).

// readImage keeps the photo in one buffer: sized from Content-Length, or
// maxBytes+1 for a body of unknown length; oversize is refused either way.
func TestReadImageSingleBuffer(t *testing.T) {
	const max = 1000
	mk := func(body []byte, cl int64) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(bytes.NewReader(body)))
		r.ContentLength = cl
		return r
	}
	body := bytes.Repeat([]byte{0xAB}, 600)

	buf, img, err := readImage(httptest.NewRecorder(), mk(body, 600), max)
	if err != nil || len(img) != 600 || len(buf) != 601 || !bytes.Equal(img, body) {
		t.Fatalf("known length: len(img)=%d len(buf)=%d err=%v", len(img), len(buf), err)
	}
	buf, img, err = readImage(httptest.NewRecorder(), mk(body, -1), max)
	if err != nil || len(img) != 600 || len(buf) != max+1 {
		t.Fatalf("unknown length: len(img)=%d len(buf)=%d err=%v", len(img), len(buf), err)
	}
	clear(buf)
	if bytes.Contains(buf, []byte{0xAB}) {
		t.Fatal("clear(buf) left photo bytes")
	}
	if _, _, err := readImage(httptest.NewRecorder(), mk(make([]byte, 1001), 1001), max); err != errImageTooLarge {
		t.Fatalf("declared too large: %v", err)
	}
	if _, _, err := readImage(httptest.NewRecorder(), mk(make([]byte, 1500), -1), max); err != errImageTooLarge {
		t.Fatalf("chunked too large: %v", err)
	}
	if _, _, err := readImage(httptest.NewRecorder(), mk(nil, 0), max); err == nil {
		t.Fatal("empty body accepted")
	}
}

// Nothing about the document (type, fields, text) is logged, even at debug.
func TestDocumentNotLoggedAtDebug(t *testing.T) {
	d := newDocHarness(t)
	var logs bytes.Buffer
	d.srv.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sid := d.ready()
	d.consent(sid, true)
	if code, _, b := d.upload(sid, testJPEG(t, 900, 700), "image/jpeg"); code != 200 {
		t.Fatalf("upload %d %s", code, b)
	}
	for _, s := range []string{"aadhaar", "9012", "टेस्ट"} {
		if strings.Contains(strings.ToLower(logs.String()), s) {
			t.Errorf("logs hold %q:\n%s", s, logs.String())
		}
	}
}

// Withdrawing consent forgets the pending photo, and consent is off by the
// time the state is cleared (an upload finishing meanwhile is discarded).
func TestWithdrawConsentClearsPending(t *testing.T) {
	d := newDocHarness(t)
	sid := d.ready()
	d.consent(sid, true)
	if code, _, b := d.upload(sid, testJPEG(t, 900, 700), "image/jpeg"); code != 200 {
		t.Fatalf("upload %d %s", code, b)
	}
	if r := d.consent(sid, false); r.StatusCode != 200 {
		t.Fatalf("withdraw %d", r.StatusCode)
	}
	if err := d.sm.RequireCameraConsent(sid); err == nil {
		t.Fatal("consent still on after withdrawal")
	}
	d.srv.mu.Lock()
	p := d.srv.convo.doc.pending
	d.srv.mu.Unlock()
	if p != nil {
		t.Fatal("pending photo survived the withdrawal")
	}
}
