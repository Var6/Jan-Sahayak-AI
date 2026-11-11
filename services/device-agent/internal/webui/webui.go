// Package webui serves the citizen kiosk UI (apps/citizen-kiosk, built with
// Vite) from files embedded in the agent binary. No Node runs on the device.
//
// scripts/dev/build-webui.sh copies apps/citizen-kiosk/dist into
// assets/dist before `go build`. Without it the agent still compiles and
// serves a plain "UI not built" page.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:assets
var assets embed.FS

// CSP mirrors the meta tag Vite injects (apps/citizen-kiosk/vite.config.ts)
// and is also sent as a header so it applies even if the meta is stripped.
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"font-src 'self'; media-src 'self'; connect-src 'self'; worker-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Explicit types: Linux mime tables vary between distributions and the
// AudioWorklet module must be served as JavaScript.
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
	".woff":  "font/woff",
	".txt":   "text/plain; charset=utf-8",
	".wav":   "audio/wav",
}

// Built reports whether a built UI is embedded.
func Built() bool {
	_, err := fs.Stat(assets, "assets/dist/index.html")
	return err == nil
}

// Handler serves GET/HEAD for the UI. Unknown paths return 404 (the UI is a
// single page at "/"; it has no client-side routes).
func Handler() http.Handler {
	dist, err := fs.Sub(assets, "assets/dist")
	built := err == nil && Built()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "microphone=(self), camera=(), geolocation=(), payment=(), usb=()")
		if !built {
			h.Set("Content-Type", "text/html; charset=utf-8")
			h.Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("<!doctype html><meta charset=utf-8><title>Jan Sahayak</title>" +
				"<p>Kiosk UI not built. Run <code>make build</code>.</p>"))
			return
		}
		p := path.Clean("/" + r.URL.Path)
		if p == "/" {
			p = "/index.html"
		}
		name := strings.TrimPrefix(p, "/")
		fi, err := fs.Stat(dist, name)
		if err != nil || fi.IsDir() {
			http.NotFound(w, r)
			return
		}
		ext := path.Ext(name)
		if ct, ok := contentTypes[ext]; ok {
			h.Set("Content-Type", ct)
		} else {
			h.Set("Content-Type", "application/octet-stream")
		}
		if strings.HasPrefix(name, "assets/") {
			// Vite puts content hashes in these names.
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		http.ServeFileFS(w, r, dist, name)
	})
}
