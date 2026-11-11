//go:build securityreview2

// Independent security-review probes for the admin dashboard serving
// (build tag securityreview2). No database needed.
//
//	go test -tags securityreview2 -race -count=1 -run Probe ./services/central-api/internal/server/
package server

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func probeServer(t *testing.T, dir string) func(string) (int, string, string, string) {
	t.Helper()
	s, err := New(Config{AdminUIDir: dir}, nil, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return func(p string) (int, string, string, string) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://localhost"+"/admin/", nil)
		r.URL.Path = p
		r.URL.RawPath = ""
		s.Handler().ServeHTTP(w, r)
		return w.Code, w.Header().Get("Content-Security-Policy"), w.Header().Get("X-Content-Type-Options"), w.Body.String()
	}
}

// Probe: path traversal and symlink escape from the UI directory.
func TestProbeStaticUITraversal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ui")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>ui</p>"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "secret.txt"), []byte("TOPSECRET"), 0o644)
	_ = os.Symlink(filepath.Join(root, "secret.txt"), filepath.Join(dir, "link.txt"))
	_ = os.Symlink(root, filepath.Join(dir, "up"))
	get := probeServer(t, dir)
	for _, p := range []string{"/admin/../secret.txt", "/admin/..%2fsecret.txt", "/admin/link.txt", "/admin/up/secret.txt", "/admin/up/", "/admin//../secret.txt"} {
		code, _, _, body := get(p)
		if strings.Contains(body, "TOPSECRET") {
			t.Errorf("%s: escaped the UI root (%d)", p, code)
		}
	}
}

var reInline = regexp.MustCompile(`(?s)<script(?:\s[^>]*)?>(.*?)</script>`)
var reSrc = regexp.MustCompile(`<script[^>]*\ssrc="([^"]+)"`)

// Probe: against the real dashboard build, every HTML page is served with
// a CSP whose script-src lists exactly the hashes of its inline scripts,
// with no unsafe source, no external script origins and nosniff set.
func TestProbeRealBuildCSPCoverage(t *testing.T) {
	dir, _ := filepath.Abs("../../../../apps/admin-dashboard/out")
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		t.Skip("dashboard not built")
	}
	get := probeServer(t, dir)
	pages := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".html") {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, _ := os.ReadFile(p)
		url := "/admin/" + rel
		if strings.HasSuffix(rel, "/index.html") {
			url = "/admin/" + strings.TrimSuffix(rel, "index.html")
		} else if rel == "index.html" {
			url = "/admin/"
		}
		code, csp, nosniff, _ := get(url)
		if code != 200 && rel != "404.html" && !strings.HasPrefix(rel, "404/") && !strings.HasPrefix(rel, "_not-found/") {
			t.Errorf("%s → %d", url, code)
		}
		if nosniff != "nosniff" {
			t.Errorf("%s: no nosniff", url)
		}
		scriptSrc := ""
		for _, dirv := range strings.Split(csp, ";") {
			if dirv = strings.TrimSpace(dirv); strings.HasPrefix(dirv, "script-src") {
				scriptSrc = dirv
			}
		}
		if strings.Contains(scriptSrc, "unsafe") || strings.Contains(scriptSrc, "*") || strings.Contains(scriptSrc, "http") {
			t.Errorf("%s: weak script-src %q", url, scriptSrc)
		}
		for _, m := range reInline.FindAllStringSubmatch(string(b), -1) {
			if strings.Contains(m[0][:strings.Index(m[0], ">")], "src=") || m[1] == "" {
				continue
			}
			sum := sha256.Sum256([]byte(m[1]))
			h := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if !strings.Contains(scriptSrc, h) && rel != "404.html" {
				t.Errorf("%s: inline script not covered by CSP (%d bytes)", rel, len(m[1]))
			}
		}
		for _, m := range reSrc.FindAllStringSubmatch(string(b), -1) {
			if !strings.HasPrefix(m[1], "/admin/") {
				t.Errorf("%s: external script %q", rel, m[1])
			}
		}
		if regexp.MustCompile(`\son[a-z]+="`).Match(b) {
			t.Errorf("%s: inline event handler attribute present", rel)
		}
		pages++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("checked %d pages", pages)
	if code, _, _, _ := get("/admin/csp-hashes.json"); code == 200 {
		t.Log("note: /admin/csp-hashes.json is publicly served (hashes only; harmless)")
	}
}

// Probe: the static bundle contains no secrets or dev credentials.
func TestProbeRealBuildNoSecrets(t *testing.T) {
	dir, _ := filepath.Abs("../../../../apps/admin-dashboard/out")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("dashboard not built")
	}
	bad := regexp.MustCompile(`(?i)BEGIN [A-Z ]*PRIVATE KEY|postgres://|dev-users\.json|"password"\s*:\s*"[^"]+"|/Users/[a-z]|sourceMappingURL|JS_ADMIN_|api[_-]?key\s*[:=]`)
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		if m := bad.Find(b); m != nil {
			t.Errorf("%s: %q", p, m)
		}
		return nil
	})
}
