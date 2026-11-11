package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
)

// staticUI serves a static export of the admin dashboard (for example a
// Next.js `output: "export"` build with basePath "/admin") under /admin/.
//
//   - files are opened through os.Root, so symlinks or ".." cannot escape
//     the directory;
//   - no directory listings: "/admin/x/" serves x/index.html, "/admin/x"
//     serves x, then x.html, then x/index.html, else 404.html or a 404;
//   - only GET and HEAD are routed here;
//   - responses carry a strict same-origin CSP. The dashboard talks only
//     to /v1/admin/* on the same origin, so no other source is allowed;
//   - inline scripts: a Next.js static export bootstraps each page with
//     inline <script> elements. The build writes csp-hashes.json (page →
//     sha256 hashes of that page's inline scripts, made by
//     apps/admin-dashboard/scripts/csp-hashes.mjs); it is read once at
//     startup and each HTML page gets `script-src 'self'` plus exactly its
//     own hashes. No 'unsafe-inline'. Without the file (or for a page not
//     listed) the policy stays `script-src 'self'`, so unlisted inline
//     scripts never run. Restart the server after deploying a new build.
type staticUI struct {
	root fs.FS
	// pageCSP maps an HTML file (relative name) to its full CSP.
	pageCSP map[string]string
}

const (
	uiCSPPrefix = "default-src 'self'; script-src 'self'"
	uiCSPSuffix = "; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
		"font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	uiCSP = uiCSPPrefix + uiCSPSuffix

	// cspHashesFile is written by the dashboard build next to the pages.
	cspHashesFile = "csp-hashes.json"
	// Bounds on csp-hashes.json, mirrored by the build script.
	maxCSPHashesBytes   = 4 << 20
	maxCSPPages         = 2000
	maxCSPHashesPerPage = 256
)

var cspHashRe = regexp.MustCompile(`^sha256-[A-Za-z0-9+/]{43}=$`)

// loadCSPHashes reads csp-hashes.json from the UI root. A missing file is
// not an error (every page then gets the plain policy); a malformed one is,
// so a broken build fails at startup instead of serving pages that cannot
// hydrate.
func loadCSPHashes(root fs.FS) (map[string]string, error) {
	f, err := root.Open(cspHashesFile)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("admin ui %s: %w", cspHashesFile, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCSPHashesBytes+1))
	if err != nil {
		return nil, fmt.Errorf("admin ui %s: %w", cspHashesFile, err)
	}
	if len(b) > maxCSPHashesBytes {
		return nil, fmt.Errorf("admin ui %s: larger than %d bytes", cspHashesFile, maxCSPHashesBytes)
	}
	var m struct {
		Version int                 `json:"version"`
		Files   map[string][]string `json:"files"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("admin ui %s: %w", cspHashesFile, err)
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("admin ui %s: unsupported version %d", cspHashesFile, m.Version)
	}
	if len(m.Files) > maxCSPPages {
		return nil, fmt.Errorf("admin ui %s: more than %d pages", cspHashesFile, maxCSPPages)
	}
	out := make(map[string]string, len(m.Files))
	for name, hashes := range m.Files {
		if !fs.ValidPath(name) || !strings.HasSuffix(name, ".html") {
			return nil, fmt.Errorf("admin ui %s: invalid page name %q", cspHashesFile, name)
		}
		if len(hashes) > maxCSPHashesPerPage {
			return nil, fmt.Errorf("admin ui %s: %s has more than %d hashes", cspHashesFile, name, maxCSPHashesPerPage)
		}
		var sb strings.Builder
		sb.WriteString(uiCSPPrefix)
		for _, h := range hashes {
			if !cspHashRe.MatchString(h) {
				return nil, fmt.Errorf("admin ui %s: %s: malformed hash %q", cspHashesFile, name, h)
			}
			sb.WriteString(" '")
			sb.WriteString(h)
			sb.WriteString("'")
		}
		sb.WriteString(uiCSPSuffix)
		out[name] = sb.String()
	}
	return out, nil
}

// csp returns the policy for a served file.
func (u *staticUI) csp(name string) string {
	if p, ok := u.pageCSP[name]; ok {
		return p
	}
	return uiCSP
}

func newStaticUI(dir string) (*staticUI, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("admin ui dir: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("admin ui dir %s is not a directory", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("admin ui dir: %w", err)
	}
	// The Root stays open for the life of the process.
	ui := &staticUI{root: root.FS()}
	if ui.pageCSP, err = loadCSPHashes(ui.root); err != nil {
		root.Close()
		return nil, err
	}
	return ui, nil
}

// open returns a regular file for name, or fs.ErrNotExist.
func (u *staticUI) open(name string) (fs.File, fs.FileInfo, error) {
	if name == "" || !fs.ValidPath(name) {
		return nil, nil, fs.ErrNotExist
	}
	f, err := u.root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, fs.ErrNotExist
	}
	return f, st, nil
}

func (u *staticUI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/admin")), "/")
	var candidates []string
	switch {
	case rel == "" || rel == ".":
		candidates = []string{"index.html"}
	case strings.HasSuffix(r.URL.Path, "/"):
		candidates = []string{rel + "/index.html"}
	default:
		candidates = []string{rel, rel + ".html", rel + "/index.html"}
	}
	status := http.StatusOK
	var (
		f    fs.File
		st   fs.FileInfo
		name string
		err  error
	)
	for _, c := range candidates {
		if f, st, err = u.open(c); err == nil {
			name = c
			break
		}
	}
	if f == nil {
		if f, st, err = u.open("404.html"); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				writeError(w, http.StatusInternalServerError, "internal", "admin ui unavailable")
				return
			}
			writeError(w, http.StatusNotFound, "not_found", "no such page")
			return
		}
		name, status = "404.html", http.StatusNotFound
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Security-Policy", u.csp(name))
	if strings.HasPrefix(name, "_next/static/") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable") // content-hashed names
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "admin ui unavailable")
		return
	}
	if status != http.StatusOK {
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, io.LimitReader(rs, st.Size()))
		}
		return
	}
	http.ServeContent(w, r, path.Base(name), st.ModTime(), rs)
}
