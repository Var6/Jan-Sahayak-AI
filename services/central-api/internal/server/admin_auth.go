package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/janmanindia/jan-sahayak/services/central-api/internal/auth"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/core"
)

const cookieName = "js_admin"

// role requirements.
type roleReq int

const (
	roleAny roleReq = iota // any authenticated staff user (all roles may read)
	roleDeviceAdmin
	roleSchemeEditor
	roleSchemeReviewer
	roleAuditor
)

func (r roleReq) satisfiedBy(roles []string) bool {
	if slices.Contains(roles, core.RoleAdmin) {
		return true
	}
	switch r {
	case roleAny:
		return len(roles) > 0
	case roleDeviceAdmin:
		return slices.Contains(roles, core.RoleDeviceAdmin)
	case roleSchemeEditor:
		return slices.Contains(roles, core.RoleSchemeEditor)
	case roleSchemeReviewer:
		return slices.Contains(roles, core.RoleSchemeReviewer)
	case roleAuditor:
		return slices.Contains(roles, core.RoleAuditor)
	}
	return false
}

// principal is the authenticated staff user.
type principal struct {
	UserID      string   `json:"id"`
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles"`
}

func (p principal) actor() core.Actor { return core.Actor{UserID: p.UserID, Label: p.Username} }

type ctxKey struct{}

func principalFrom(r *http.Request) principal {
	p, _ := r.Context().Value(ctxKey{}).(principal)
	return p
}

// require wraps a handler with session authentication and an RBAC check.
func (s *Server) require(need roleReq, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
		defer cancel()
		p, err := s.sessionPrincipal(ctx, r)
		if err != nil {
			if errors.Is(err, errNoSession) {
				writeError(w, http.StatusUnauthorized, "unauthorized", "login required")
			} else {
				s.dbUnavailable(w, "admin: session lookup", err)
			}
			return
		}
		if !need.satisfiedBy(p.Roles) {
			writeError(w, http.StatusForbidden, "forbidden", "your role does not allow this action")
			return
		}
		h(w, r.WithContext(context.WithValue(ctx, ctxKey{}, p)))
	})
}

var errNoSession = errors.New("no session")

func (s *Server) sessionPrincipal(ctx context.Context, r *http.Request) (principal, error) {
	var p principal
	c, err := r.Cookie(cookieName)
	if err != nil {
		return p, errNoSession
	}
	sum, ok := auth.TokenSum(c.Value)
	if !ok {
		return p, errNoSession
	}
	var lastSeen time.Time
	err = s.pool.QueryRow(ctx, `SELECT u.id, u.username, u.display_name, s.last_seen_at,
			COALESCE(array_agg(ur.role ORDER BY ur.role) FILTER (WHERE ur.role IS NOT NULL), '{}')
		FROM admin_sessions s JOIN users u ON u.id = s.user_id
		LEFT JOIN user_roles ur ON ur.user_id = u.id
		WHERE s.token_sha256 = $1 AND s.expires_at > now() AND s.idle_expires_at > now() AND NOT u.disabled
		GROUP BY u.id, s.last_seen_at`, sum).Scan(&p.UserID, &p.Username, &p.DisplayName, &lastSeen, &p.Roles)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, errNoSession
	}
	if err != nil {
		return p, err
	}
	// Slide the idle deadline at most once a minute to limit writes.
	if s.now().Sub(lastSeen) > time.Minute {
		if _, err := s.pool.Exec(ctx, `UPDATE admin_sessions SET last_seen_at = now(),
			idle_expires_at = LEAST(expires_at, now() + make_interval(secs => $2)) WHERE token_sha256 = $1`,
			sum, s.cfg.SessionIdle.Seconds()); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (s *Server) setCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/v1/admin",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin authenticates a staff user. Every credential failure (unknown
// user, wrong password, disabled user) gets the same 401 after the same
// work: a progressive throttle delay (login_throttle.go), one argon2
// verification and one audit row. There is no hard lock.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	var req loginReq
	if !decodeAdmin(w, r, &req) {
		return
	}
	badCredentials := func() {
		writeError(w, http.StatusUnauthorized, "bad_credentials", "invalid username or password")
	}
	username := strings.ToLower(strings.TrimSpace(req.Username))
	if len(username) > 40 || len(req.Password) > auth.MaxPasswordLen {
		badCredentials() // such a user cannot exist (users.username CHECK)
		return
	}
	tk, ok := s.logins.admit(clientIP(r, s.cfg.TrustProxy), username)
	if !ok {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "throttled", "too many login attempts in progress; retry shortly")
		return
	}
	defer tk.done()
	if tk.delay > 0 {
		t := time.NewTimer(tk.delay)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return // client gone or timed out; nothing was checked
		}
	}
	prio := auth.PriorityNormal
	if tk.trusted {
		prio = auth.PriorityTrusted
	}
	busy := func() {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "busy", "server busy, retry")
	}
	var (
		uid, hash string
		disabled  bool
	)
	err := s.pool.QueryRow(ctx, `SELECT id, password_hash, disabled FROM users WHERE username = $1`, username).
		Scan(&uid, &hash, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		// Same work as a real user: argon2 through the same slots (busy is
		// reported identically), then one audit row.
		if err := auth.VerifyDummy(ctx, req.Password, prio); errors.Is(err, auth.ErrBusy) {
			busy()
			return
		}
		tk.failed()
		if err := core.Audit(ctx, s.pool, core.Actor{Label: "anonymous"}, "admin.login_failed", "user", "",
			map[string]any{"reason": "unknown_user"}); err != nil {
			s.dbUnavailable(w, "login: record failure", err)
			return
		}
		badCredentials()
		return
	}
	if err != nil {
		s.dbUnavailable(w, "login: lookup", err)
		return
	}
	okPw, err := auth.VerifyPasswordPriority(ctx, hash, req.Password, prio)
	if errors.Is(err, auth.ErrBusy) {
		busy()
		return
	}
	if err != nil {
		s.log.Error("login: verify", "err", err)
		okPw = false
	}
	actor := core.Actor{UserID: uid, Label: username}
	if !okPw || disabled {
		tk.failed()
		err := core.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			// failed_logins is informational only (shown to admins); it no
			// longer locks the account.
			if _, err := tx.Exec(ctx, `UPDATE users SET failed_logins = failed_logins + 1 WHERE id = $1`, uid); err != nil {
				return err
			}
			reason := "bad_password"
			if okPw {
				reason = "disabled"
			}
			return core.Audit(ctx, tx, actor, "admin.login_failed", "user", uid, map[string]any{"disabled": disabled, "reason": reason})
		})
		if err != nil {
			s.dbUnavailable(w, "login: record failure", err)
			return
		}
		badCredentials()
		return
	}
	tk.succeeded()
	token, sum, err := auth.NewSessionToken()
	if err != nil {
		s.dbUnavailable(w, "login: token", err)
		return
	}
	err = core.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO admin_sessions (token_sha256, user_id, expires_at, idle_expires_at)
			VALUES ($1, $2, now() + make_interval(secs => $3), now() + make_interval(secs => $4))`,
			sum, uid, s.cfg.SessionMax.Seconds(), s.cfg.SessionIdle.Seconds()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET failed_logins = 0, locked_until = NULL, last_login_at = now() WHERE id = $1`, uid); err != nil {
			return err
		}
		return core.Audit(ctx, tx, actor, "admin.login", "user", uid, nil)
	})
	if err != nil {
		s.dbUnavailable(w, "login: create session", err)
		return
	}
	s.setCookie(w, token, int(s.cfg.SessionMax.Seconds()))
	p, err := s.loadPrincipal(ctx, uid)
	if err != nil {
		s.dbUnavailable(w, "login: load user", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": p, "expires_in": int(s.cfg.SessionMax.Seconds())})
}

func (s *Server) loadPrincipal(ctx context.Context, uid string) (principal, error) {
	var p principal
	err := s.pool.QueryRow(ctx, `SELECT u.id, u.username, u.display_name,
		COALESCE(array_agg(ur.role ORDER BY ur.role) FILTER (WHERE ur.role IS NOT NULL), '{}')
		FROM users u LEFT JOIN user_roles ur ON ur.user_id = u.id WHERE u.id = $1 GROUP BY u.id`, uid).
		Scan(&p.UserID, &p.Username, &p.DisplayName, &p.Roles)
	return p, err
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	s.setCookie(w, "", -1) // header must be set before any WriteHeader
	c, err := r.Cookie(cookieName)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sum, ok := auth.TokenSum(c.Value)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	err = core.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var uid, username string
		err := tx.QueryRow(ctx, `DELETE FROM admin_sessions s USING users u WHERE s.token_sha256 = $1 AND u.id = s.user_id
			RETURNING u.id, u.username`, sum).Scan(&uid, &username)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return core.Audit(ctx, tx, core.Actor{UserID: uid, Label: username}, "admin.logout", "user", uid, nil)
	})
	if err != nil {
		s.dbUnavailable(w, "logout", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": principalFrom(r)})
}
