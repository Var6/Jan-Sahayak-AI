package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/deviceapi"
	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/core"
	"github.com/janmanindia/jan-sahayak/services/central-api/internal/db"
)

// ---- Security finding 1: four-eyes rule across versions ----

func TestFourEyesAcrossVersions(t *testing.T) {
	e := newEnv(t)
	aliceID := e.createUser("alice", "scheme_editor", "scheme_reviewer")
	bobID := e.createUser("bob", "scheme_editor")
	e.createUser("carol", "scheme_reviewer")
	a, b, c := e.adminClient(), e.adminClient(), e.adminClient()
	a.login("alice")
	b.login("bob")
	c.login("carol")
	path := "/v1/admin/schemes/test-scheme-fe2"
	rec := fixtureRecord("test-scheme-fe2")

	// A -> B -> A is refused by the handler ...
	if code, body, _ := a.req("PUT", path, putBody(0, rec)); code != 200 {
		t.Fatalf("alice v1: %d %s", code, body)
	}
	if code, body, _ := b.req("PUT", path, putBody(1, rec)); code != 200 {
		t.Fatalf("bob v2: %d %s", code, body)
	}
	if code, body, _ := a.req("POST", path+"/verify", map[string]any{"version": 2}); code != http.StatusForbidden ||
		!strings.Contains(string(body), "own_edit") {
		t.Fatalf("A->B->A verify: %d %s", code, body)
	}
	// ... and by the database trigger, whatever code inserts the row.
	r3 := rec
	r3.Version = 3
	r3.Verification = scheme.Verification{Status: scheme.Verified, VerifiedBy: aliceID, VerifiedAt: "2026-11-09T00:00:00Z"}
	_, err := e.pool.Exec(context.Background(), `INSERT INTO scheme_versions (scheme_id, version, record, status, edited_by, verified_by, verified_at)
		VALUES ('test-scheme-fe2', 3, $1, 'VERIFIED', $2, $3, now())`, r3, bobID, aliceID)
	if err == nil || !strings.Contains(err.Error(), "four-eyes") {
		t.Fatalf("DB must refuse A->B->A verification, got %v", err)
	}
	// An independent reviewer may verify.
	if code, body, _ := c.req("POST", path+"/verify", map[string]any{"version": 2}); code != 200 {
		t.Fatalf("carol verify: %d %s", code, body)
	}
	// Edits before the last VERIFIED version no longer count: alice's v1
	// content was verified by carol, so alice may verify bob's next draft.
	rec.Summary.En = "Software test fixture only (bob v4)."
	if code, body, _ := b.req("PUT", path, putBody(3, rec)); code != 200 {
		t.Fatalf("bob v4: %d %s", code, body)
	}
	if code, body, _ := a.req("POST", path+"/verify", map[string]any{"version": 4}); code != 200 {
		t.Fatalf("alice verifies bob's post-verification draft: %d %s", code, body)
	}
}

// ---- Knowledge reviewer gap: withdrawal and bundle selection ----

func bundleContents(t *testing.T, e *env) map[string]string {
	t.Helper()
	var raw string
	if err := e.pool.QueryRow(context.Background(), `SELECT contents::text FROM knowledge_bundles ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var items []struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
		Status  string `json:"status"`
	}
	mustJSON(t, []byte(raw), &items)
	out := map[string]string{}
	for _, it := range items {
		out[it.ID] = it.Status + "@" + itoa(it.Version)
	}
	return out
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestWithdrawScheme(t *testing.T) {
	e := newEnv(t)
	edID := e.createUser("edw", "scheme_editor")
	e.createUser("revw", "scheme_reviewer")
	e.createUser("vieww", "viewer")
	ed, rev, vw := e.adminClient(), e.adminClient(), e.adminClient()
	ed.login("edw")
	rev.login("revw")
	vw.login("vieww")
	beta, gamma := "/v1/admin/schemes/test-scheme-wb", "/v1/admin/schemes/test-scheme-wg"
	publish := func() map[string]string {
		t.Helper()
		if code, body, _ := rev.req("POST", "/v1/admin/bundles", nil); code != http.StatusCreated {
			t.Fatalf("publish: %d %s", code, body)
		}
		return bundleContents(t, e)
	}

	if code, _, _ := ed.req("PUT", gamma, putBody(0, fixtureRecord("test-scheme-wg"))); code != 200 {
		t.Fatal("gamma draft")
	}
	if code, _, _ := ed.req("PUT", beta, putBody(0, fixtureRecord("test-scheme-wb"))); code != 200 {
		t.Fatal("beta draft")
	}
	if code, body, _ := rev.req("POST", beta+"/withdraw", map[string]any{"version": 1, "reason": "nothing verified yet"}); code != http.StatusConflict {
		t.Fatalf("withdraw without a verified version: %d %s", code, body)
	}
	if code, _, _ := rev.req("POST", beta+"/verify", map[string]any{"version": 1}); code != 200 {
		t.Fatal("verify v1")
	}
	// v3: a pending correction drafted while v2 is still live.
	corr := fixtureRecord("test-scheme-wb")
	corr.Summary.En = "Software test fixture only (correction)."
	if code, _, _ := ed.req("PUT", beta, putBody(2, corr)); code != 200 {
		t.Fatal("beta v3 draft")
	}
	if got := publish()["test-scheme-wb"]; got != "VERIFIED@2" {
		t.Fatalf("before withdrawal the live verified version ships, got %s", got)
	}

	// Permissions, validation and optimistic concurrency.
	if code, _, _ := vw.req("POST", beta+"/withdraw", map[string]any{"version": 3, "reason": "source withdrawn"}); code != http.StatusForbidden {
		t.Fatalf("viewer withdraw: %d", code)
	}
	if code, _, _ := ed.req("POST", beta+"/withdraw", map[string]any{"version": 3, "reason": "source withdrawn"}); code != http.StatusForbidden {
		t.Fatalf("editor withdraw: %d", code)
	}
	if code, _, _ := rev.req("POST", beta+"/withdraw", map[string]any{"version": 3, "reason": ""}); code != http.StatusBadRequest {
		t.Fatalf("empty reason: %d", code)
	}
	if code, _, _ := rev.req("POST", beta+"/withdraw", map[string]any{"version": 2, "reason": "source withdrawn"}); code != http.StatusConflict {
		t.Fatalf("stale version: %d", code)
	}
	if code, _, _ := rev.req("POST", "/v1/admin/schemes/no-such-scheme/withdraw", map[string]any{"version": 1, "reason": "x y z"}); code != http.StatusNotFound {
		t.Fatalf("missing scheme: %d", code)
	}

	code, body, _ := rev.req("POST", beta+"/withdraw", map[string]any{"version": 3, "reason": "amount changed by new resolution"})
	if code != 200 || !strings.Contains(string(body), `"status":"OUTDATED"`) || !strings.Contains(string(body), `"withdrawn_by":"revw"`) ||
		!strings.Contains(string(body), `"withdrawn_version":2`) || !strings.Contains(string(body), `"edited_by":"edw"`) {
		t.Fatalf("withdraw: %d %s", code, body)
	}
	var notes string
	if err := e.pool.QueryRow(context.Background(), `SELECT record->'verification'->>'notes' FROM scheme_versions
		WHERE scheme_id = 'test-scheme-wb' AND version = 4 AND status = 'OUTDATED' AND verified_by IS NULL`).Scan(&notes); err != nil ||
		!strings.HasPrefix(notes, "OUTDATED: ") {
		t.Fatalf("stored OUTDATED version: %q %v", notes, err)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'scheme.withdrawn' AND target_id = 'test-scheme-wb'`); n != 1 {
		t.Fatalf("audit rows: %d", n)
	}
	if code, _, _ := rev.req("POST", beta+"/withdraw", map[string]any{"version": 4, "reason": "again please"}); code != http.StatusConflict {
		t.Fatalf("double withdraw: %d", code)
	}
	// The withdrawn VERIFIED v2 is never shipped again; the pending
	// correction v3 ships as NEEDS_REVIEW.
	if got := publish(); got["test-scheme-wb"] != "NEEDS_REVIEW@3" || got["test-scheme-wg"] != "NEEDS_REVIEW@1" {
		t.Fatalf("after withdrawal: %v", got)
	}
	if code, body, _ := rev.req("GET", "/v1/admin/schemes", nil); code != 200 || !strings.Contains(string(body), `"withdrawn":true`) {
		t.Fatalf("list: %d %s", code, body)
	}

	// A scheme withdrawn with no pending correction drops out entirely.
	if code, _, _ := rev.req("POST", gamma+"/verify", map[string]any{"version": 1}); code != 200 {
		t.Fatal("verify gamma")
	}
	if code, _, _ := rev.req("POST", gamma+"/withdraw", map[string]any{"version": 2, "reason": "scheme closed"}); code != 200 {
		t.Fatal("withdraw gamma")
	}
	if got := publish(); got["test-scheme-wg"] != "" || got["test-scheme-wb"] != "NEEDS_REVIEW@3" {
		t.Fatalf("withdrawn gamma must be excluded: %v", got)
	}

	// A new correction after the withdrawal, then verification by the
	// reviewer who withdrew (they authored nothing): shipped as VERIFIED.
	corr.Summary.En = "Software test fixture only (correction 2)."
	if code, body, _ := ed.req("PUT", beta, putBody(4, corr)); code != 200 {
		t.Fatalf("correction v5: %d %s", code, body)
	}
	if code, body, _ := rev.req("POST", beta+"/verify", map[string]any{"version": 5}); code != 200 {
		t.Fatalf("verify correction: %d %s", code, body)
	}
	if got := publish()["test-scheme-wb"]; got != "VERIFIED@6" {
		t.Fatalf("verified correction must ship, got %s", got)
	}

	// The database requires withdrawn_by on (and only on) OUTDATED rows.
	r := fixtureRecord("test-scheme-wb")
	r.Version = 7
	r.Verification = scheme.Verification{Status: scheme.Outdated}
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO scheme_versions (scheme_id, version, record, status, edited_by)
		VALUES ('test-scheme-wb', 7, $1, 'OUTDATED', $2)`, r, edID); err == nil || !strings.Contains(err.Error(), "withdrawn_by") {
		t.Fatalf("OUTDATED without withdrawn_by: %v", err)
	}
}

// ---- Security finding 8: revocation during an in-flight request ----

func TestDeviceRevokedDuringRequest(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-rv")
	revoke := func() {
		if err := core.RevokeDevice(context.Background(), e.pool, core.Actor{Label: "test"}, d.id, "stolen"); err != nil {
			t.Error(err)
		}
	}
	e.srv.afterDeviceAuth.Store(&revoke)
	defer e.srv.afterDeviceAuth.Store(nil)
	code, body := e.upload(d, d.batch(mkEvent("session.ended", sessionEnded(ids.NewV4(), "hi", 0))))
	if code != http.StatusUnauthorized || strings.Contains(string(body), "signature") {
		t.Fatalf("revoked mid-request: %d %s", code, body)
	}
	if n := e.count(`SELECT count(*) FROM sync_events`); n != 0 {
		t.Fatalf("%d events stored from a device revoked mid-request", n)
	}

	d2 := e.newDevice("kiosk-rv2")
	revoke2 := func() {
		if err := core.RevokeDevice(context.Background(), e.pool, core.Actor{Label: "test"}, d2.id, "stolen"); err != nil {
			t.Error(err)
		}
	}
	e.srv.afterDeviceAuth.Store(&revoke2)
	hb := deviceapi.Heartbeat{DeviceID: d2.id, SentAt: time.Now().UTC().Format(time.RFC3339), AgentVersion: "1", Overall: "HEALTHY"}
	hbBody, _ := json.Marshal(hb)
	if code, body := e.do(d2.sign(http.MethodPost, deviceapi.HeartbeatPath, hbBody, time.Now())); code != http.StatusUnauthorized {
		t.Fatalf("heartbeat revoked mid-request: %d %s", code, body)
	}
	if n := e.count(`SELECT count(*) FROM device_heartbeats`); n != 0 {
		t.Fatalf("%d heartbeats stored", n)
	}
}

// ---- Security finding 9: the query string is signed ----

func TestKnowledgeBundleQueryIsSigned(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("kiosk-q")
	send := func(signedQuery, sentQuery string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.hs.URL+deviceapi.KnowledgeBundlePath+signedQuery, nil)
		if err := deviceapi.SignHTTP(req, d.id, d.priv, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
		req.URL.RawQuery = strings.TrimPrefix(sentQuery, "?")
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if code := send("?since=2026-11-09.1", "?since=2026-11-09.1"); code != http.StatusNotFound { // authenticated; no bundle yet
		t.Fatalf("signed query: %d", code)
	}
	if code := send("?since=2026-11-09.1", "?since=2026-11-09.2"); code != http.StatusUnauthorized {
		t.Fatalf("altered query: %d", code)
	}
	if code := send("?since=2026-11-09.1", ""); code != http.StatusUnauthorized {
		t.Fatalf("stripped query: %d", code)
	}
	if code := send("?since=x", "?since=x"); code != http.StatusBadRequest {
		t.Fatalf("malformed since: %d", code)
	}
	if code := send("?since=2026-11-09.1&x=1", "?since=2026-11-09.1&x=1"); code != http.StatusBadRequest {
		t.Fatalf("unknown parameter: %d", code)
	}
	// Sync uploads (no query) are signed exactly as before.
	e.uploadOK(d, d.batch(mkEvent("session.ended", sessionEnded(ids.NewV4(), "hi", 0))))
}

// ---- Security finding 4: runtime role vs owner role ----

func TestRuntimeRoleCannotTamper(t *testing.T) {
	e := newEnv(t)
	if e.rtRole == "" {
		t.Skip("test database user cannot create roles")
	}
	ctx := context.Background()
	e.createUser("someone", "viewer") // writes audit rows as the owner
	// The runtime role can append.
	if err := core.Audit(ctx, e.rtPool, core.Actor{Label: "test"}, "test.appended", "", "", nil); err != nil {
		t.Fatalf("runtime insert into audit_logs: %v", err)
	}
	for _, sql := range []string{
		`UPDATE audit_logs SET action = 'x.y'`,
		`DELETE FROM audit_logs`,
		`TRUNCATE audit_logs`,
		`ALTER TABLE audit_logs DISABLE TRIGGER ALL`,
		`DROP TRIGGER audit_logs_no_update_delete ON audit_logs`,
		`UPDATE scheme_versions SET status = 'VERIFIED'`,
		`ALTER TABLE scheme_versions DISABLE TRIGGER scheme_versions_four_eyes`,
		`DELETE FROM knowledge_bundles`,
		`UPDATE sync_events SET type = 'x'`,
		`UPDATE users SET password_hash = '$argon2id$x'`,
		`UPDATE users SET disabled = false`,
		`INSERT INTO user_roles (user_id, role) SELECT id, 'admin' FROM users LIMIT 1`,
		`UPDATE devices SET public_key = public_key`,
		`CREATE TABLE sneaky (id int)`,
	} {
		_, err := e.rtPool.Exec(ctx, sql)
		if err == nil {
			t.Errorf("runtime role could run %q", sql)
			continue
		}
		if !strings.Contains(err.Error(), "permission denied") && !strings.Contains(err.Error(), "must be owner") {
			t.Errorf("%q: unexpected error %v", sql, err)
		}
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'test.appended'`); n != 1 {
		t.Fatal("audit row missing")
	}
	if p, err := db.CheckRuntimeRole(ctx, e.rtPool); err != nil || len(p) != 0 {
		t.Fatalf("runtime role check: %v %v", p, err)
	}
	if p, err := db.CheckRuntimeRole(ctx, e.pool); err != nil || len(p) == 0 {
		t.Fatalf("owner must be reported as unsafe: %v %v", p, err)
	}
}

// ---- Security findings 3 and 15: login throttling, uniform answers ----

func TestLoginUniformFailures(t *testing.T) {
	e := newEnv(t)
	e.createUser("real", "viewer")
	e.createUser("gone", "viewer")
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET disabled = true WHERE username = 'gone'`); err != nil {
		t.Fatal(err)
	}
	c := e.adminClient()
	var first string
	for _, cred := range [][2]string{{"real", "wrong-password-xx"}, {"nosuchuser", "wrong-password-xx"}, {"gone", "correct-horse-gone"},
		{strings.Repeat("x", 41), "wrong-password-xx"}} {
		code, body, _ := c.req("POST", "/v1/admin/login", map[string]string{"username": cred[0], "password": cred[1]})
		if code != http.StatusUnauthorized {
			t.Fatalf("%s: %d %s", cred[0], code, body)
		}
		if first == "" {
			first = string(body)
		} else if string(body) != first {
			t.Fatalf("%s: body %s differs from %s", cred[0], body, first)
		}
	}
	// Unknown users are audited too (same database work as a real user),
	// without storing the submitted name.
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'admin.login_failed' AND actor = 'anonymous' AND details->>'reason' = 'unknown_user'`); n != 1 {
		t.Fatalf("unknown-user audit rows: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE details::text LIKE '%nosuchuser%'`); n != 0 {
		t.Fatal("submitted unknown username stored in the audit log")
	}
}

func TestLoginThrottleUnit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	th := newLoginThrottle(clock, 100*time.Millisecond, 3*time.Second)

	fail := func(ip, user string) time.Duration {
		tk, ok := th.admit(ip, user)
		if !ok {
			t.Fatalf("admit %s %s refused", ip, user)
		}
		d := tk.delay
		tk.failed()
		tk.done()
		return d
	}
	var delays []time.Duration
	for range maxFailedLogins + 1 {
		delays = append(delays, fail("10.0.0.1", "victim"))
	}
	if delays[0] != 0 || delays[loginFreeFailures-1] != 0 || delays[loginFreeFailures] != 100*time.Millisecond ||
		delays[loginFreeFailures+1] != 200*time.Millisecond || delays[maxFailedLogins] != 3*time.Second {
		t.Fatalf("delays %v", delays)
	}
	for i := 1; i < len(delays); i++ {
		if delays[i] < delays[i-1] {
			t.Fatalf("delay decreased: %v", delays)
		}
	}
	// Per-username delay applies from a fresh IP, and the same for an
	// unknown username as for a real one (keys are the submitted strings).
	tk, _ := th.admit("10.0.0.2", "victim")
	if tk.delay != 3*time.Second || !tk.trusted {
		t.Fatalf("fresh IP, targeted user: delay %v trusted %v", tk.delay, tk.trusted)
	}
	// Per-IP concurrency cap (the ticket above is still in flight).
	tk2, ok := th.admit("10.0.0.2", "other")
	if !ok {
		t.Fatal("second concurrent attempt refused")
	}
	if _, ok := th.admit("10.0.0.2", "third"); ok {
		t.Fatal("third concurrent attempt from one IP admitted")
	}
	tk.succeeded()
	tk.done()
	tk2.done()
	if tk, _ := th.admit("10.0.0.3", "victim"); tk.delay != 0 {
		t.Fatalf("success must clear the username counter, delay %v", tk.delay)
	} else {
		tk.done()
	}
	// The failing IP is not trusted (no reserved argon2 slot) ...
	if tk, _ := th.admit("10.0.0.1", "someone"); tk.trusted || tk.delay != 3*time.Second {
		t.Fatalf("failing IP: trusted %v delay %v", tk.trusted, tk.delay)
	} else {
		tk.done()
	}
	// ... until its failures expire.
	advance(loginWindow + time.Second)
	if tk, _ := th.admit("10.0.0.1", "someone"); !tk.trusted || tk.delay != 0 {
		t.Fatalf("expired failures: trusted %v delay %v", tk.trusted, tk.delay)
	} else {
		tk.done()
	}
	// Maps stay bounded under a flood of distinct keys.
	for i := range loginMaxKeys + 500 {
		fail("10.1."+itoa(i/250)+"."+itoa(i%250), "u"+itoa(i))
	}
	if ips, users := th.sizes(); ips > loginMaxKeys || users > loginMaxKeys {
		t.Fatalf("maps unbounded: %d ips, %d users", ips, users)
	}
}

func TestClientIP(t *testing.T) {
	mk := func(remote string, xff ...string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/admin/login", nil)
		r.RemoteAddr = remote
		for _, v := range xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		return r
	}
	cases := []struct {
		r     *http.Request
		trust bool
		want  string
	}{
		{mk("203.0.113.7:5555"), false, "203.0.113.7"},
		{mk("127.0.0.1:5555", "198.51.100.9"), false, "127.0.0.1"},
		{mk("127.0.0.1:5555", "6.6.6.6, 198.51.100.9"), true, "198.51.100.9"},
		{mk("127.0.0.1:5555", "6.6.6.6", "198.51.100.9"), true, "198.51.100.9"},
		{mk("203.0.113.7:5555", "198.51.100.9"), true, "203.0.113.7"}, // not from the local proxy
		{mk("[2001:db8:1:2:3:4:5:6]:443"), false, "2001:db8:1:2::/64"},
		{mk("[::ffff:192.0.2.1]:80"), false, "192.0.2.1"},
	}
	for _, c := range cases {
		if got := clientIP(c.r, c.trust); got != c.want {
			t.Errorf("%s %v trust=%v: got %s want %s", c.r.RemoteAddr, c.r.Header.Values("X-Forwarded-For"), c.trust, got, c.want)
		}
	}
}

// ---- Static admin dashboard ----

func TestStaticAdminUI(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>home</h1>"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "devices.html"), []byte("<h1>devices</h1>"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "_next", "static"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "_next", "static", "app.js"), []byte("console.log(1)"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "empty"), 0o755))
	must(os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644))
	must(os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "leak.txt")))

	log := slog.New(slog.DiscardHandler)
	s, err := New(Config{AdminUIDir: dir}, nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	do := func(method, path string) (int, string, http.Header) {
		r := httptest.NewRequest(method, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String(), w.Header()
	}
	if code, body, hdr := do("GET", "/admin/"); code != 200 || body != "<h1>home</h1>" || !strings.Contains(hdr.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("index: %d %q %v", code, body, hdr)
	}
	if code, body, _ := do("GET", "/admin/devices"); code != 200 || body != "<h1>devices</h1>" {
		t.Fatalf("clean URL: %d %q", code, body)
	}
	if code, _, hdr := do("GET", "/admin/_next/static/app.js"); code != 200 || !strings.Contains(hdr.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %v", code, hdr)
	}
	for _, p := range []string{"/admin/leak.txt", "/admin/empty/", "/admin/empty", "/admin/nope"} {
		if code, body, _ := do("GET", p); code != http.StatusNotFound || strings.Contains(body, "secret") {
			t.Errorf("%s: %d %q", p, code, body)
		}
	}
	// The mux itself redirects unclean paths out of /admin/.
	if code, body, _ := do("GET", "/admin/../etc/passwd"); code == http.StatusOK || strings.Contains(body, "root:") {
		t.Errorf("dot-dot: %d %q", code, body)
	}
	if code, _, hdr := do("GET", "/admin"); code != http.StatusMovedPermanently || hdr.Get("Location") != "/admin/" {
		t.Fatalf("redirect: %d %v", code, hdr)
	}
	if code, _, _ := do("POST", "/admin/"); code != http.StatusNotFound { // only GET/HEAD are routed to the UI
		t.Fatalf("POST: %d", code)
	}
	if _, err := New(Config{AdminUIDir: filepath.Join(dir, "missing")}, nil, nil, log); err == nil {
		t.Fatal("missing UI dir accepted")
	}
	// Without the option nothing is served there.
	s2, _ := New(Config{}, nil, nil, log)
	r := httptest.NewRequest("GET", "/admin/", nil)
	w := httptest.NewRecorder()
	s2.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("no UI dir: %d", w.Code)
	}
}
