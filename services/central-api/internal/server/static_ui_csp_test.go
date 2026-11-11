package server

import (
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaticAdminUICSPHashes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `self.__next_f.push([1,"x"])`
	sum := sha256.Sum256([]byte(script))
	hash := "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
	write("index.html", "<script>"+script+"</script>")
	write("devices/index.html", "<p>devices</p>")
	write("404.html", "<p>missing</p>")
	write(cspHashesFile, `{"version":1,"files":{"index.html":["`+hash+`"],"devices/index.html":[],"404.html":["`+hash+`"]}}`)

	log := slog.New(slog.DiscardHandler)
	s, err := New(Config{AdminUIDir: dir}, nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) (int, string) {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Code, w.Header().Get("Content-Security-Policy")
	}
	code, csp := get("/admin/")
	if code != 200 || !strings.Contains(csp, "script-src 'self' '"+hash+"';") {
		t.Fatalf("index: %d %q", code, csp)
	}
	for _, d := range strings.Split(csp, ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "script-src") && strings.Contains(d, "unsafe") {
			t.Fatalf("unsafe script source allowed: %q", d)
		}
	}
	if code, csp := get("/admin/devices/"); code != 200 || strings.Contains(csp, "sha256-") || !strings.Contains(csp, "script-src 'self';") {
		t.Fatalf("page without inline scripts: %d %q", code, csp)
	}
	if code, csp := get("/admin/nope"); code != 404 || !strings.Contains(csp, hash) {
		t.Fatalf("404 page: %d %q", code, csp)
	}

	// Malformed manifests fail startup instead of serving pages that cannot hydrate.
	for _, bad := range []string{
		`not json`,
		`{"version":2,"files":{}}`,
		`{"version":1,"files":{"../x.html":[]}}`,
		`{"version":1,"files":{"index.html":["sha256-abc"]}}`,
		`{"version":1,"files":{"index.html":["'unsafe-inline'"]}}`,
		`{"version":1,"files":{"app.js":[]}}`,
	} {
		write(cspHashesFile, bad)
		if _, err := New(Config{AdminUIDir: dir}, nil, nil, log); err == nil {
			t.Errorf("accepted malformed manifest %s", bad)
		}
	}
}
