package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/janmanindia/jan-sahayak/internal/ids"
	"github.com/janmanindia/jan-sahayak/internal/scheme"
)

func TestAdminAuthAndRBAC(t *testing.T) {
	e := newEnv(t)
	e.createUser("viewer1", "viewer")
	e.createUser("devadmin", "device_admin")
	e.createUser("auditor1", "auditor")
	e.createUser("noroles")

	anon := e.adminClient()
	if code, _, _ := anon.req("GET", "/v1/admin/devices", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
	if code, _, _ := anon.req("POST", "/v1/admin/login", map[string]string{"username": "viewer1", "password": "wrong-password-123"}); code != http.StatusUnauthorized {
		t.Fatalf("bad password: %d", code)
	}
	if code, _, _ := anon.req("POST", "/v1/admin/login", map[string]string{"username": "nobody", "password": "wrong-password-123"}); code != http.StatusUnauthorized {
		t.Fatalf("unknown user: %d", code)
	}

	v := e.adminClient()
	code, _, resp := v.req("POST", "/v1/admin/login", map[string]string{"username": "viewer1", "password": "correct-horse-viewer1"})
	if code != http.StatusOK {
		t.Fatalf("login: %d", code)
	}
	var ck *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			ck = c
		}
	}
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || len(ck.Value) != 43 {
		t.Fatalf("cookie: %+v", ck)
	}
	if n := e.count(`SELECT count(*) FROM admin_sessions WHERE token_sha256 = sha256(convert_to($1, 'UTF8'))`, ck.Value); n != 1 {
		t.Fatal("session must be stored as sha256 of the token")
	}
	if code, b, _ := v.req("GET", "/v1/admin/me", nil); code != 200 || !strings.Contains(string(b), `"viewer"`) {
		t.Fatalf("me: %d %s", code, b)
	}
	if code, _, _ := v.req("GET", "/v1/admin/devices", nil); code != 200 {
		t.Fatalf("viewer list devices: %d", code)
	}
	if code, _, _ := v.req("GET", "/v1/admin/analytics/summary", nil); code != 200 {
		t.Fatalf("viewer analytics: %d", code)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	newDev := map[string]string{"id": ids.NewV4(), "public_key": base64.StdEncoding.EncodeToString(pub), "name": "k", "district": "D"}
	if code, _, _ := v.req("POST", "/v1/admin/devices", newDev); code != http.StatusForbidden {
		t.Fatalf("viewer register device: %d", code)
	}
	if code, _, _ := v.req("GET", "/v1/admin/audit", nil); code != http.StatusForbidden {
		t.Fatalf("viewer audit: %d", code)
	}
	if code, _, _ := v.req("PUT", "/v1/admin/schemes/x-y", putBody(0, fixtureRecord("x-y"))); code != http.StatusForbidden {
		t.Fatalf("viewer put scheme: %d", code)
	}

	// No roles at all → cannot read anything.
	nr := e.adminClient()
	nr.login("noroles")
	if code, _, _ := nr.req("GET", "/v1/admin/devices", nil); code != http.StatusForbidden {
		t.Fatalf("no roles: %d", code)
	}

	// device_admin registers and revokes.
	da := e.adminClient()
	da.login("devadmin")
	if code, b, _ := da.req("POST", "/v1/admin/devices", newDev); code != http.StatusCreated {
		t.Fatalf("register: %d %s", code, b)
	}
	if code, _, _ := da.req("POST", "/v1/admin/devices", newDev); code != http.StatusConflict {
		t.Fatalf("re-register: %d", code)
	}
	bad := map[string]string{"id": ids.NewV4(), "public_key": "AAAA", "name": "k", "district": "D"}
	if code, _, _ := da.req("POST", "/v1/admin/devices", bad); code != http.StatusBadRequest {
		t.Fatalf("bad key: %d", code)
	}
	if code, b, _ := da.req("GET", "/v1/admin/devices/"+newDev["id"], nil); code != 200 || !strings.Contains(string(b), `"status":"active"`) {
		t.Fatalf("detail: %d %s", code, b)
	}
	if code, b, _ := da.req("POST", "/v1/admin/devices/"+newDev["id"]+"/revoke", map[string]string{"reason": "lost"}); code != 200 || !strings.Contains(string(b), `"revoked"`) {
		t.Fatalf("revoke: %d %s", code, b)
	}
	if code, _, _ := da.req("POST", "/v1/admin/devices/"+newDev["id"]+"/revoke", map[string]string{"reason": "lost"}); code != http.StatusConflict {
		t.Fatalf("double revoke: %d", code)
	}
	if code, _, _ := da.req("GET", "/v1/admin/devices/"+ids.NewV4(), nil); code != http.StatusNotFound {
		t.Fatalf("missing device: %d", code)
	}

	// CSRF: a browser cross-site POST is refused even with a valid cookie.
	if code, _, _ := da.req("POST", "/v1/admin/devices/"+ids.NewV4()+"/revoke", map[string]string{"reason": "x"},
		"Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Fatalf("csrf: %d", code)
	}
	if code, _, _ := da.req("POST", "/v1/admin/devices", newDev, "Origin", "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("csrf origin: %d", code)
	}

	// Auditor sees every mutation.
	au := e.adminClient()
	au.login("auditor1")
	code, b, _ := au.req("GET", "/v1/admin/audit?limit=200", nil)
	if code != 200 {
		t.Fatalf("audit: %d", code)
	}
	for _, action := range []string{"user.created", "user.role_granted", "admin.login", "admin.login_failed", "device.registered", "device.revoked"} {
		if !strings.Contains(string(b), `"`+action+`"`) {
			t.Errorf("audit missing %s", action)
		}
	}
	if code, b, _ := au.req("GET", "/v1/admin/audit?action=device.revoked", nil); code != 200 || strings.Count(string(b), `"device.revoked"`) != 1 {
		t.Fatalf("audit filter: %d %s", code, b)
	}

	// Logout invalidates the server-side session.
	if code, _, _ := v.req("POST", "/v1/admin/logout", nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	replay := e.adminClient()
	replay.c.Jar.SetCookies(mustURL(e.hs.URL+"/v1/admin"), []*http.Cookie{{Name: cookieName, Value: ck.Value, Path: "/v1/admin"}})
	if code, _, _ := replay.req("GET", "/v1/admin/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("token after logout: %d", code)
	}

	// No hard lock (decision 7): after many failures every bad attempt is
	// still a plain 401, and the right password still works (after the
	// progressive delay).
	for i := 0; i < maxFailedLogins+2; i++ {
		if code, _, _ := anon.req("POST", "/v1/admin/login", map[string]string{"username": "auditor1", "password": "wrong-password-123"}); code != http.StatusUnauthorized {
			t.Fatalf("failed attempt %d: %d", i, code)
		}
	}
	if code, _, _ := anon.req("POST", "/v1/admin/login", map[string]string{"username": "auditor1", "password": "correct-horse-auditor1"}); code != http.StatusOK {
		t.Fatalf("login after failures: %d", code)
	}
	if n := e.count(`SELECT count(*) FROM users WHERE locked_until IS NOT NULL`); n != 0 {
		t.Fatalf("%d users locked", n)
	}
}

func TestSchemeReviewWorkflow(t *testing.T) {
	e := newEnv(t)
	e.createUser("edt", "scheme_editor", "scheme_reviewer") // editor who is also a reviewer
	e.createUser("rev", "scheme_reviewer")
	e.createUser("aud", "auditor")
	ed, rev := e.adminClient(), e.adminClient()
	ed.login("edt")
	rev.login("rev")
	path := "/v1/admin/schemes/test-scheme-beta"

	// Invalid record → 422 with details.
	badRec := fixtureRecord("test-scheme-beta")
	badRec.Category = "nonsense"
	badRec.Sources = nil
	if code, b, _ := ed.req("PUT", path, putBody(0, badRec)); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "details") {
		t.Fatalf("invalid record: %d %s", code, b)
	}
	// Reviewer without editor role cannot edit.
	if code, _, _ := rev.req("PUT", path, putBody(0, fixtureRecord("test-scheme-beta"))); code != http.StatusForbidden {
		t.Fatalf("reviewer edit: %d", code)
	}
	// Draft: the client claims VERIFIED; the server stores NEEDS_REVIEW.
	code, b, _ := ed.req("PUT", path, putBody(0, fixtureRecord("test-scheme-beta")))
	if code != 200 {
		t.Fatalf("put: %d %s", code, b)
	}
	if !strings.Contains(string(b), `"status":"NEEDS_REVIEW"`) || strings.Contains(string(b), `"verified_by":"someone"`) {
		t.Fatalf("draft not forced to NEEDS_REVIEW: %s", b)
	}
	// Stale base version → 409.
	if code, _, _ := ed.req("PUT", path, putBody(0, fixtureRecord("test-scheme-beta"))); code != http.StatusConflict {
		t.Fatalf("stale base: %d", code)
	}
	// Path/record id mismatch → 400.
	if code, _, _ := ed.req("PUT", path, putBody(1, fixtureRecord("other-id"))); code != http.StatusBadRequest {
		t.Fatalf("id mismatch: %d", code)
	}
	// The editor cannot verify their own edit, even holding scheme_reviewer.
	if code, b, _ := ed.req("POST", path+"/verify", map[string]any{"version": 1}); code != http.StatusForbidden || !strings.Contains(string(b), "own_edit") {
		t.Fatalf("self-verify: %d %s", code, b)
	}
	// Wrong version → 409.
	if code, _, _ := rev.req("POST", path+"/verify", map[string]any{"version": 7}); code != http.StatusConflict {
		t.Fatalf("wrong version: %d", code)
	}
	// Another reviewer verifies.
	code, b, _ = rev.req("POST", path+"/verify", map[string]any{"version": 1, "notes": "checked against source", "review_due": "2099-01-01"})
	if code != 200 || !strings.Contains(string(b), `"status":"VERIFIED"`) || !strings.Contains(string(b), `"verified_by":"rev"`) {
		t.Fatalf("verify: %d %s", code, b)
	}
	// Already verified → 409.
	if code, _, _ := rev.req("POST", path+"/verify", map[string]any{"version": 2}); code != http.StatusConflict {
		t.Fatalf("re-verify: %d", code)
	}
	var revID string
	_ = e.pool.QueryRow(context.Background(), `SELECT id::text FROM users WHERE username = 'rev'`).Scan(&revID)
	var recVerifiedBy, status string
	if err := e.pool.QueryRow(context.Background(), `SELECT record->'verification'->>'verified_by', status FROM scheme_versions
		WHERE scheme_id = 'test-scheme-beta' AND version = 2`).Scan(&recVerifiedBy, &status); err != nil {
		t.Fatal(err)
	}
	if recVerifiedBy != revID || status != "VERIFIED" {
		t.Fatalf("stored verification: %s %s", recVerifiedBy, status)
	}

	// New edit after verification → v3 NEEDS_REVIEW; bundle still carries v2.
	rec := fixtureRecord("test-scheme-beta")
	rec.Summary.En = "Software test fixture only (edited)."
	if code, b, _ := ed.req("PUT", path, putBody(2, rec)); code != 200 || !strings.Contains(string(b), `"version":3`) {
		t.Fatalf("second edit: %d %s", code, b)
	}
	// A second scheme that is only a draft is shipped as NEEDS_REVIEW.
	if code, _, _ := ed.req("PUT", "/v1/admin/schemes/test-scheme-gamma", putBody(0, fixtureRecord("test-scheme-gamma"))); code != 200 {
		t.Fatal("gamma draft")
	}
	if code, _, _ := ed.req("POST", "/v1/admin/bundles", nil); code != http.StatusCreated { // ed is also a reviewer
		t.Fatalf("publish: %d", code)
	}
	var contents string
	_ = e.pool.QueryRow(context.Background(), `SELECT contents::text FROM knowledge_bundles`).Scan(&contents)
	if !strings.Contains(contents, `"id": "test-scheme-beta", "status": "VERIFIED", "version": 2`) ||
		!strings.Contains(contents, `"id": "test-scheme-gamma", "status": "NEEDS_REVIEW", "version": 1`) {
		t.Fatalf("bundle contents: %s", contents)
	}

	// History and specific versions.
	code, b, _ = rev.req("GET", path, nil)
	if code != 200 || strings.Count(string(b), `"edited_by":"edt"`) != 4 { // 3 history rows + current
		t.Fatalf("history: %d %s", code, b)
	}
	if code, b, _ := rev.req("GET", path+"/versions/2", nil); code != 200 || !strings.Contains(string(b), `"VERIFIED"`) {
		t.Fatalf("version 2: %d %s", code, b)
	}
	if code, _, _ := rev.req("GET", "/v1/admin/schemes", nil); code != 200 {
		t.Fatal("list schemes")
	}

	// Database-level four-eyes guard: a VERIFIED row by its own editor is refused.
	var edID string
	_ = e.pool.QueryRow(context.Background(), `SELECT id::text FROM users WHERE username = 'edt'`).Scan(&edID)
	r2 := fixtureRecord("test-scheme-beta")
	r2.Version = 4
	r2.Verification = scheme.Verification{Status: scheme.Verified, VerifiedBy: edID, VerifiedAt: "2026-11-09T00:00:00Z"}
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO scheme_versions (scheme_id, version, record, status, edited_by, verified_by, verified_at)
		VALUES ('test-scheme-beta', 4, $1, 'VERIFIED', $2, $2, now())`, r2, edID); err == nil || !strings.Contains(err.Error(), "violates check constraint") {
		t.Fatalf("DB must refuse a self-verified version, got %v", err)
	}

	// Audit trail for every mutation.
	for _, action := range []string{"scheme.draft_saved", "scheme.verified", "bundle.published"} {
		if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = $1`, action); n == 0 {
			t.Errorf("no audit row for %s", action)
		}
	}
}

func TestAppendOnlyTables(t *testing.T) {
	e := newEnv(t)
	e.createUser("someone", "viewer")
	ctx := context.Background()
	for _, sql := range []string{
		`UPDATE audit_logs SET action = 'x.y'`,
		`DELETE FROM audit_logs`,
		`TRUNCATE audit_logs`,
		`TRUNCATE audit_logs CASCADE`,
	} {
		if _, err := e.pool.Exec(ctx, sql); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: want append-only error, got %v", sql, err)
		}
	}
	if n := e.count(`SELECT count(*) FROM audit_logs`); n < 2 {
		t.Fatal("audit rows lost")
	}
	// Sync events are immutable too.
	d := e.newDevice("k")
	e.uploadOK(d, d.batch(mkEvent("session.ended", sessionEnded(ids.NewV4(), "hi", 0))))
	if _, err := e.pool.Exec(ctx, `UPDATE sync_events SET type = 'x'`); err == nil {
		t.Error("sync_events UPDATE allowed")
	}
	// Scheme versions cannot be edited or deleted.
	ed := e.adminClient()
	e.createUser("ed2", "scheme_editor")
	ed.login("ed2")
	if code, _, _ := ed.req("PUT", "/v1/admin/schemes/test-scheme-delta", putBody(0, fixtureRecord("test-scheme-delta"))); code != 200 {
		t.Fatal("put")
	}
	for _, sql := range []string{`UPDATE scheme_versions SET status = 'VERIFIED'`, `DELETE FROM scheme_versions`} {
		if _, err := e.pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s allowed", sql)
		}
	}
}

func TestAnalyticsSummary(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("k1")
	s1, s2, s3 := ids.NewV4(), ids.NewV4(), ids.NewV4()
	e.uploadOK(d, d.batch(
		mkEvent("dialogue.turn", turn(s1, 1, "pension", "answered")),
		mkEvent("session.ended", sessionEnded(s1, "hi", 1)),
		mkEvent("dialogue.turn", turn(s2, 1, "certificate", "unresolved")),
		mkEvent("dialogue.turn", turn(s2, 2, "", "redirected")),
		mkEvent("session.ended", sessionEnded(s2, "hi", 2)),
		mkEvent("dialogue.turn", turn(s3, 1, "loan", "escalated")),
		mkEvent("session.ended", sessionEnded(s3, "en", 1)),
	))
	e.createUser("adm", "admin")
	c := e.adminClient()
	c.login("adm")
	code, b, _ := c.req("GET", "/v1/admin/analytics/summary", nil)
	if code != 200 {
		t.Fatalf("summary: %d %s", code, b)
	}
	// Admin sees exact numbers (no small-cell suppression).
	var s analyticsView
	mustJSON(t, b, &s)
	n := func(p *int64) int64 {
		if p == nil {
			t.Fatalf("unexpected suppressed cell in admin view: %s", b)
		}
		return *p
	}
	if s.Suppression.Applied || n(s.SessionsTotal) != 3 || n(s.TurnsTotal) != 4 || n(s.SessionsWithTurns) != 3 ||
		n(s.SessionsAnswered) != 1 || n(s.SessionsUnresolved) != 1 {
		t.Fatalf("summary: %s", b)
	}
	if len(s.SessionsPerDay) != 1 || n(s.SessionsPerDay[0].Sessions) != 3 {
		t.Fatalf("per day: %+v", s.SessionsPerDay)
	}
	if len(s.Languages) != 2 || s.Languages[0].Key != "hi" || n(s.Languages[0].Count) != 2 {
		t.Fatalf("languages: %+v", s.Languages)
	}
	if s.ResolutionRate == nil || *s.ResolutionRate < 0.33 || *s.ResolutionRate > 0.34 {
		t.Fatalf("rate %v", s.ResolutionRate)
	}
	found := false
	for _, cr := range s.Categories {
		if cr.Category == "certificate" && n(cr.Unresolved) == 1 && n(cr.Sessions) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("categories: %+v", s.Categories)
	}

	// Viewer (decision 9): every count between 1 and 4 is suppressed, and
	// rates built on them are null.
	e.createUser("viw", "viewer")
	vc := e.adminClient()
	vc.login("viw")
	code, b, _ = vc.req("GET", "/v1/admin/analytics/summary", nil)
	if code != 200 {
		t.Fatalf("viewer summary: %d %s", code, b)
	}
	var vs analyticsView
	mustJSON(t, b, &vs)
	if !vs.Suppression.Applied || vs.Suppression.MinCount != 5 || vs.SessionsTotal != nil || vs.TurnsTotal != nil ||
		vs.AvgTurns != nil || vs.ResolutionRate != nil || vs.SessionsAnswered != nil {
		t.Fatalf("viewer view not suppressed: %s", b)
	}
	for _, l := range vs.Languages {
		if l.Count != nil {
			t.Fatalf("viewer language cell shown: %s", b)
		}
	}
	for _, cr := range vs.Categories {
		if cr.Turns != nil || cr.Sessions != nil || (cr.Unresolved != nil && *cr.Unresolved != 0) {
			t.Fatalf("viewer category cell shown: %s", b)
		}
	}
	if !strings.Contains(string(b), `"sessions_total":null`) {
		t.Fatalf("suppressed cells must be null: %s", b)
	}

	// Filters and validation.
	if code, b, _ := c.req("GET", "/v1/admin/analytics/summary?district=Nowhere", nil); code != 200 || !strings.Contains(string(b), `"sessions_total":0`) {
		t.Fatalf("district filter: %d %s", code, b)
	}
	if code, _, _ := c.req("GET", "/v1/admin/analytics/summary?from=2026-01-01&to=2025-01-01", nil); code != 400 {
		t.Fatalf("bad range: %d", code)
	}
	if code, _, _ := c.req("GET", "/v1/admin/analytics/summary?from=2020-01-01&to=2026-01-01", nil); code != 400 {
		t.Fatalf("range too long: %d", code)
	}
}

// Cells at or above the threshold are shown to viewers; a single hidden
// cell in an additive group drags the next smallest with it.
func TestAnalyticsSuppressionGroups(t *testing.T) {
	sp := suppressor{on: true}
	got := sp.group([]int64{20, 7, 3, 0})
	if got[0] == nil || *got[0] != 20 || got[1] != nil || got[2] != nil || got[3] == nil || *got[3] != 0 {
		t.Fatalf("secondary suppression: %v", got)
	}
	got = sp.group([]int64{20, 9, 3, 2})
	if got[0] == nil || got[1] == nil || got[2] != nil || got[3] != nil {
		t.Fatalf("two small cells: %v", got)
	}
	got = suppressor{on: false}.group([]int64{1, 2})
	if got[0] == nil || *got[0] != 1 || got[1] == nil {
		t.Fatalf("admin must see exact counts: %v", got)
	}
	v := sp.view(analyticsSummary{SessionsTotal: 12, AvgTurns: 2, SessionsWithTurns: 12, SessionsAnswered: 6,
		Languages: []countRow{{Key: "hi", Count: 10}, {Key: "en", Count: 2}}})
	if v.SessionsTotal == nil || *v.SessionsTotal != 12 || v.AvgTurns == nil || v.ResolutionRate == nil {
		t.Fatalf("large totals must be shown: %+v", v)
	}
	if v.Languages[0].Count != nil || v.Languages[1].Count != nil {
		t.Fatalf("total 12 with en=2 must hide both language cells: %+v", v.Languages)
	}
}

func TestJanitor(t *testing.T) {
	e := newEnv(t)
	d := e.newDevice("k")
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO sync_nonces VALUES ($1, 'aaaaaaaaaaaaaaaaaaaa', now() - interval '1 hour'),
		($1, 'bbbbbbbbbbbbbbbbbbbb', now() + interval '1 hour')`, d.id); err != nil {
		t.Fatal(err)
	}
	for _, age := range []string{"40 days", "35 days", "1 day"} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO device_heartbeats (id, device_id, received_at, device_time, agent_version,
			overall_health, outbox_pending, outbox_uploading, outbox_acknowledged, outbox_delete_eligible, outbox_failed,
			outbox_conflict, outbox_total, outbox_capacity) VALUES ($1, $2, now() - $3::interval, now(), 'v', 'HEALTHY', 0,0,0,0,0,0,0,0)`,
			ids.NewV7(), d.id, age); err != nil {
			t.Fatal(err)
		}
	}
	// Second device whose only heartbeat is old: it must be kept.
	d2 := e.newDevice("k2")
	if _, err := e.pool.Exec(ctx, `INSERT INTO device_heartbeats (id, device_id, received_at, device_time, agent_version,
		overall_health, outbox_pending, outbox_uploading, outbox_acknowledged, outbox_delete_eligible, outbox_failed,
		outbox_conflict, outbox_total, outbox_capacity) VALUES ($1, $2, now() - interval '90 days', now(), 'v', 'HEALTHY', 0,0,0,0,0,0,0,0)`,
		ids.NewV7(), d2.id); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.JanitorOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM sync_nonces`); n != 1 {
		t.Fatalf("nonces left %d", n)
	}
	if n := e.count(`SELECT count(*) FROM device_heartbeats WHERE device_id = $1`, d.id); n != 1 {
		t.Fatalf("heartbeats left for d1: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM device_heartbeats WHERE device_id = $1`, d2.id); n != 1 {
		t.Fatal("latest heartbeat of a silent device must be kept")
	}
}
