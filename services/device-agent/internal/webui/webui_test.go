package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Header().Get("Content-Security-Policy") == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing")
	}
	if Built() {
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<html") {
			t.Fatalf("index: %d", rec.Code)
		}
		if rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatal("content type")
		}
	} else if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unbuilt: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
	if Built() {
		for _, p := range []string{"/nope.js", "/../../etc/passwd", "/assets"} {
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s: %d", p, rec.Code)
			}
		}
	}
}
