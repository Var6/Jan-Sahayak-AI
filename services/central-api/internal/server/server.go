// Package server is the central API HTTP layer: signed device endpoints
// (sync ingest, heartbeat, knowledge bundle) and the admin API.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/janmanindia/jan-sahayak/services/central-api/internal/keys"
)

// Config tunes the server.
type Config struct {
	// CookieSecure sets the Secure flag on the admin session cookie. Only
	// disable for plain-HTTP localhost development.
	CookieSecure bool
	// SessionIdle / SessionMax bound admin sessions.
	SessionIdle time.Duration
	SessionMax  time.Duration
	// HeartbeatRetention is how long heartbeat rows are kept (the latest
	// row per device is always kept).
	HeartbeatRetention time.Duration
	// MaxConcurrentDevice bounds concurrent signed device requests.
	MaxConcurrentDevice int
	// RequestTimeout bounds database work per request.
	RequestTimeout time.Duration
	// LoginBaseDelay / LoginMaxDelay shape the progressive login delay
	// (login_throttle.go). Defaults 500 ms and 8 s; LoginMaxDelay must stay
	// well below RequestTimeout.
	LoginBaseDelay time.Duration
	LoginMaxDelay  time.Duration
	// TrustProxy makes login throttling key on the X-Forwarded-For address
	// set by a reverse proxy on the same host (loopback peers only).
	TrustProxy bool
	// AdminUIDir, if set, is a directory of static files (the admin
	// dashboard's static export) served under /admin/.
	AdminUIDir string
}

func (c *Config) defaults() {
	if c.SessionIdle <= 0 {
		c.SessionIdle = 30 * time.Minute
	}
	if c.SessionMax <= 0 {
		c.SessionMax = 12 * time.Hour
	}
	if c.HeartbeatRetention <= 0 {
		c.HeartbeatRetention = 30 * 24 * time.Hour
	}
	if c.MaxConcurrentDevice <= 0 {
		c.MaxConcurrentDevice = 16
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 15 * time.Second
	}
	if c.LoginBaseDelay <= 0 {
		c.LoginBaseDelay = 500 * time.Millisecond
	}
	if c.LoginMaxDelay <= 0 {
		c.LoginMaxDelay = 8 * time.Second
	}
	if c.LoginMaxDelay > c.RequestTimeout/2 {
		c.LoginMaxDelay = c.RequestTimeout / 2
	}
}

// Server holds dependencies.
type Server struct {
	cfg     Config
	pool    *pgxpool.Pool
	keys    atomic.Pointer[keys.Keyring] // swappable for key rotation
	log     *slog.Logger
	now     func() time.Time
	devSem  chan struct{}
	logins  *loginThrottle
	handler http.Handler

	// beforeCommit, if set (tests only), runs inside the ingest transaction
	// just before COMMIT; returning an error aborts the transaction.
	beforeCommit atomic.Pointer[hookFunc]
	// afterDeviceAuth, if set (tests only), runs after a device request is
	// authenticated and before its database work starts.
	afterDeviceAuth atomic.Pointer[func()]
}

type hookFunc func(ctx context.Context, tx pgx.Tx) error

// New creates a server.
func New(cfg Config, pool *pgxpool.Pool, kr *keys.Keyring, log *slog.Logger) (*Server, error) {
	cfg.defaults()
	s := &Server{cfg: cfg, pool: pool, log: log, now: time.Now,
		devSem: make(chan struct{}, cfg.MaxConcurrentDevice)}
	s.logins = newLoginThrottle(func() time.Time { return s.now() }, cfg.LoginBaseDelay, cfg.LoginMaxDelay)
	s.keys.Store(kr)
	var ui http.Handler
	if cfg.AdminUIDir != "" {
		var err error
		if ui, err = newStaticUI(cfg.AdminUIDir); err != nil {
			return nil, err
		}
	}
	s.handler = s.routes(ui)
	return s, nil
}

// SetKeyring atomically replaces the signing keyring (key rotation).
func (s *Server) SetKeyring(kr *keys.Keyring) { s.keys.Store(kr) }

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes(ui http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.handleReady)

	// Device endpoints: authenticated by Ed25519 request signatures.
	mux.HandleFunc("POST /v1/sync/events", s.handleSyncEvents)
	mux.HandleFunc("POST /v1/devices/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /v1/devices/knowledge-bundle", s.handleKnowledgeBundle)

	// Admin endpoints: session cookie + RBAC, wrapped in CSRF protection.
	admin := http.NewServeMux()
	admin.HandleFunc("POST /v1/admin/login", s.handleLogin)
	admin.HandleFunc("POST /v1/admin/logout", s.handleLogout)
	admin.Handle("GET /v1/admin/me", s.require(roleAny, s.handleMe))
	admin.Handle("GET /v1/admin/analytics/summary", s.require(roleAny, s.handleAnalytics))
	admin.Handle("GET /v1/admin/devices", s.require(roleAny, s.handleListDevices))
	admin.Handle("GET /v1/admin/devices/{id}", s.require(roleAny, s.handleGetDevice))
	admin.Handle("POST /v1/admin/devices", s.require(roleDeviceAdmin, s.handleRegisterDevice))
	admin.Handle("POST /v1/admin/devices/{id}/revoke", s.require(roleDeviceAdmin, s.handleRevokeDevice))
	admin.Handle("GET /v1/admin/schemes", s.require(roleAny, s.handleListSchemes))
	admin.Handle("GET /v1/admin/schemes/{id}", s.require(roleAny, s.handleGetScheme))
	admin.Handle("GET /v1/admin/schemes/{id}/versions/{version}", s.require(roleAny, s.handleGetSchemeVersion))
	admin.Handle("PUT /v1/admin/schemes/{id}", s.require(roleSchemeEditor, s.handlePutScheme))
	admin.Handle("POST /v1/admin/schemes/{id}/verify", s.require(roleSchemeReviewer, s.handleVerifyScheme))
	admin.Handle("POST /v1/admin/schemes/{id}/withdraw", s.require(roleSchemeReviewer, s.handleWithdrawScheme))
	admin.Handle("GET /v1/admin/bundles", s.require(roleAny, s.handleListBundles))
	admin.Handle("POST /v1/admin/bundles", s.require(roleSchemeReviewer, s.handlePublishBundle))
	admin.Handle("GET /v1/admin/audit", s.require(roleAuditor, s.handleAudit))
	admin.HandleFunc("/v1/admin/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	csrf := http.NewCrossOriginProtection()
	csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "csrf", "cross-origin request refused")
	}))
	mux.Handle("/v1/admin/", noStore(csrf.Handler(admin)))
	if ui != nil {
		mux.Handle("GET /admin/", ui)
		mux.Handle("GET /admin", http.RedirectHandler("/admin/", http.StatusMovedPermanently))
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return s.recoverer(s.logRequests(securityHeaders(mux)))
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		// Never log bodies, cookies or signatures.
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"dur_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("http: panic", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// RunJanitor deletes expired nonces and sessions and old heartbeats every
// interval until ctx is done.
func (s *Server) RunJanitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.JanitorOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Error("janitor", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// JanitorOnce runs one cleanup pass.
func (s *Server) JanitorOnce(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var errs []error
	if _, err := s.pool.Exec(ctx, `DELETE FROM sync_nonces WHERE expires_at < now()`); err != nil {
		errs = append(errs, err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM admin_sessions WHERE expires_at < now() OR idle_expires_at < now()`); err != nil {
		errs = append(errs, err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM device_heartbeats h
		WHERE h.received_at < now() - make_interval(secs => $1)
		  AND h.id <> (SELECT l.id FROM device_heartbeats l WHERE l.device_id = h.device_id
		               ORDER BY l.received_at DESC LIMIT 1)`, s.cfg.HeartbeatRetention.Seconds()); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
