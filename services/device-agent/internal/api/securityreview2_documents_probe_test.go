//go:build securityreview2

// Independent security-review probes for the document check (build tag
// securityreview2). All document text is synthetic and FAKE.
//
//	go test -tags securityreview2 -race -count=1 -run Probe ./services/device-agent/internal/api/
package api

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const probeIncomeText = "नमूना — नकली दस्तावेज़ / FAKE SPECIMEN\nआय प्रमाण पत्र\nनाम: प्रोबनाम टेस्ट\n" +
	"जारी करने वाला कार्यालय: अंचल कार्यालय 1234.5678.9012\nजारी करने की तिथि: 10/04/2026\n" +
	"PROBERAWLINE पति रोज़ मारता है\n"

// dumpDB returns every value of every table as text.
func dumpDB(t *testing.T, h *harness) string {
	t.Helper()
	rows, err := h.st.DB.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	var sb strings.Builder
	for _, tb := range tables {
		r, err := h.st.DB.Query(`SELECT * FROM "` + tb + `"`)
		if err != nil {
			continue
		}
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			_ = r.Scan(ptrs...)
			for _, v := range vals {
				if b, ok := v.([]byte); ok {
					sb.Write(b)
				} else {
					fmt.Fprint(&sb, v)
				}
				sb.WriteByte('|')
			}
			sb.WriteByte('\n')
		}
		r.Close()
	}
	return sb.String()
}

// Probe: a full document flow (consent, upload, confirm, end) leaves no
// document content in logs (debug level), in the device database (outbox,
// conversation records) or in the API response beyond the masked fields.
func TestProbeDocumentFlowLeavesNoTrace(t *testing.T) {
	d := newDocHarness(t)
	var logs bytes.Buffer
	d.srv.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d.ocr.set(probeIncomeText, nil)
	sid := d.ready()
	if r := d.consent(sid, true); r.StatusCode != 200 {
		t.Fatalf("consent %d", r.StatusCode)
	}
	code, _, body := d.upload(sid, testJPEG(t, 900, 700), "image/jpeg")
	if code != 200 {
		t.Fatalf("upload %d %s", code, body)
	}
	t.Logf("document response: %s", body)
	if bytes.Contains(body, []byte("PROBERAWLINE")) || bytes.Contains(body, []byte("मारता")) {
		t.Error("raw OCR text in the API response")
	}
	if bytes.Contains(body, []byte("1234.5678")) {
		t.Error("FINDING: full Aadhaar-shaped number (dot separated) visible in issuing_authority of the API response")
	}
	if w := d.post("/api/v1/session/"+sid+"/document/confirm", map[string]bool{"correct": true}); w.Code != 200 {
		t.Fatalf("confirm %d %s", w.Code, w.Body)
	}
	if w := d.post("/api/v1/session/"+sid+"/end", map[string]string{"reason": "completed"}); w.Code != 200 {
		t.Logf("end: %d %s", w.Code, w.Body)
	}
	db := dumpDB(t, d.harness)
	for _, s := range []string{"प्रोबनाम", "PROBERAWLINE", "1234.5678", "9012", "अंचल कार्यालय", "income_certificate"} {
		if strings.Contains(db, s) {
			t.Errorf("device database holds %q", s)
		}
		if strings.Contains(logs.String(), s) {
			t.Errorf("logs hold %q", s)
		}
	}
	t.Logf("debug logs:\n%s", logs.String())
}

// Probe: withdrawing and re-granting consent does not resurrect a photo
// read before the withdrawal.
func TestProbeReconsentDoesNotResurrectPending(t *testing.T) {
	d := newDocHarness(t)
	sid := d.ready()
	d.consent(sid, true)
	if code, _, b := d.upload(sid, testJPEG(t, 900, 700), "image/jpeg"); code != 200 {
		t.Fatalf("upload %d %s", code, b)
	}
	d.consent(sid, false)
	d.consent(sid, true)
	w := d.post("/api/v1/session/"+sid+"/document/confirm", map[string]bool{"correct": true})
	if w.Code != http.StatusConflict {
		t.Fatalf("confirm after re-consent: %d %s", w.Code, w.Body)
	}
}

// Probe: consent from one session does not carry into the next.
func TestProbeConsentDoesNotCarryOver(t *testing.T) {
	d := newDocHarness(t)
	sid := d.ready()
	d.consent(sid, true)
	if w := d.post("/api/v1/session/"+sid+"/end", map[string]string{"reason": "completed"}); w.Code != 200 {
		t.Fatalf("end %d %s", w.Code, w.Body)
	}
	sid2 := d.ready()
	if code, ec, _ := d.upload(sid2, testJPEG(t, 900, 700), "image/jpeg"); code != 403 || ec != "no_consent" {
		t.Fatalf("new session upload: %d %s", code, ec)
	}
	if code, ec, _ := d.upload(sid, testJPEG(t, 900, 700), "image/jpeg"); code == 200 {
		t.Fatalf("old session id accepted: %d %s", code, ec)
	}
}

// Probe: cross-origin / no kiosk header uploads are refused before OCR.
func TestProbeDocumentCSRF(t *testing.T) {
	d := newDocHarness(t)
	sid := d.ready()
	d.consent(sid, true)
	img := testJPEG(t, 900, 700)
	cases := []map[string]string{
		{"X-JS-Kiosk": ""},
		{"Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site"},
	}
	for _, hdr := range cases {
		w := d.req(http.MethodPost, "/api/v1/session/"+sid+"/document", img, "image/jpeg", hdr)
		if w.Code == 200 {
			t.Errorf("upload accepted with %v", hdr)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "http://evil.example:8730/api/v1/session/"+sid+"/document", bytes.NewReader(img))
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("X-JS-Kiosk", "1")
	r := httptest.NewRecorder()
	d.h.ServeHTTP(r, req)
	if r.Code == 200 {
		t.Error("upload accepted with foreign Host header")
	}
	if d.ocr.calls != 0 {
		t.Fatalf("OCR ran %d times on refused uploads", d.ocr.calls)
	}
}
