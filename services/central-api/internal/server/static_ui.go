package server

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
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
//     to /v1/admin/* on the same origin, so no other source is allowed. A
//     build that needs inline scripts must ship them as files instead.
type staticUI struct {
	root fs.FS
}

const uiCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

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
	return &staticUI{root: root.FS()}, nil
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
	h.Set("Content-Security-Policy", uiCSP)
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
