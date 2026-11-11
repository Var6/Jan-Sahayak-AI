package bundlesync

// Opt-in integration test against a REAL central API (no fakes): heartbeat,
// signed event upload with verified acks, and signed knowledge bundles.
//
//	JS_CENTRAL_URL=http://127.0.0.1:8750 \
//	JS_CENTRAL_DEV_USERS=$PWD/var/central/dev-users.json \
//	JS_CENTRAL_SERVER_PUB="$(var/central/bin/central-admin server-public-key -key var/central/server.key)" \
//	go test -race -run TestAgainstRealCentral -v ./services/device-agent/internal/bundlesync/
//
// Prerequisites: scripts/dev/central.sh running and scripts/dev/provision.sh
// run once (it creates dev-admin, dev-editor and dev-reviewer). The test
// registers a fresh device (district "DEV (FIXTURE)"), revokes it at the
// end, and publishes new bundles in the target database containing a
// FIXTURE record "fixture-it-elderly-pension" (fake scheme, verified by the
// dev reviewer user). Use it only against a development database.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/deviceapi"
	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/heartbeat"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/identity"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/knowledge"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/store"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/syncer"
)

type adminClient struct {
	t    *testing.T
	base string
	c    *http.Client
}

func login(t *testing.T, base, user, pw string) *adminClient {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	a := &adminClient{t: t, base: base, c: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
	a.do(http.MethodPost, "/v1/admin/login", map[string]string{"username": user, "password": pw}, http.StatusOK, nil)
	return a
}

func (a *adminClient) do(method, path string, body any, want int, out any) {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.c.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != want {
		a.t.Fatalf("%s %s: HTTP %d (want %d): %.300s", method, path, resp.StatusCode, want, rb)
	}
	if out != nil {
		if err := json.Unmarshal(rb, out); err != nil {
			a.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
}

func TestAgainstRealCentral(t *testing.T) {
	base := os.Getenv("JS_CENTRAL_URL")
	if base == "" {
		t.Skip("set JS_CENTRAL_URL (and JS_CENTRAL_DEV_USERS, JS_CENTRAL_SERVER_PUB) to run against a real central API")
	}
	var creds map[string]string
	b, err := os.ReadFile(os.Getenv("JS_CENTRAL_DEV_USERS"))
	if err != nil || json.Unmarshal(b, &creds) != nil {
		t.Fatalf("JS_CENTRAL_DEV_USERS must name provision.sh's dev-users.json: %v", err)
	}
	var pub struct {
		KeyID     string `json:"key_id"`
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal([]byte(os.Getenv("JS_CENTRAL_SERVER_PUB")), &pub); err != nil {
		t.Fatalf("JS_CENTRAL_SERVER_PUB must be central-admin server-public-key output: %v", err)
	}
	pk, err := base64.StdEncoding.DecodeString(pub.PublicKey)
	if err != nil || len(pk) != ed25519.PublicKeySize {
		t.Fatal("bad server public key")
	}
	serverKeys := map[string]ed25519.PublicKey{pub.KeyID: ed25519.PublicKey(pk)}
	ctx := context.Background()

	// A fresh device: identity, database, registration.
	dir := t.TempDir()
	id, _, err := identity.LoadOrCreate(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "device.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	admin := login(t, base, "dev-admin", creds["dev-admin"])
	admin.do(http.MethodPost, "/v1/admin/devices", map[string]string{
		"id": id.DeviceID, "public_key": id.PublicKeyBase64(), "name": "it-" + id.DeviceID[:8], "district": "DEV (FIXTURE)",
	}, http.StatusCreated, nil)
	defer admin.do(http.MethodPost, "/v1/admin/devices/"+id.DeviceID+"/revoke", map[string]string{"reason": "integration test finished"}, http.StatusOK, nil)

	// 1. Events: signed upload, verified ack, ACKNOWLEDGED.
	ob := outbox.New(st.DB, outbox.Config{MaxRows: 1000}, nil)
	sid := "0b6a3a52-7c1e-4d8e-9a3b-2f6c1d9e8a7b"
	if _, err := ob.Enqueue(ctx, nil, "dialogue.turn", deviceapi.Turn{SessionID: sid, Seq: 1, Language: "hi", Source: "text", Intent: "pension"}); err != nil {
		t.Fatal(err)
	}
	minute := time.Now().UTC().Truncate(time.Minute).Format(time.RFC3339)
	if _, err := ob.Enqueue(ctx, nil, "session.ended", deviceapi.SessionEnded{SessionID: sid, Language: "hi", StartedAt: minute, EndedAt: minute, EndReason: "citizen_ended", Turns: 1}); err != nil {
		t.Fatal(err)
	}
	sw := syncer.New(syncer.Config{BaseURL: base, ServerKeys: serverKeys, HTTPTimeout: 10 * time.Second}, ob, id, slog.New(slog.DiscardHandler))
	if n, err := sw.SyncOnce(ctx); err != nil || n != 2 {
		t.Fatalf("sync: n=%d err=%v", n, err)
	}
	stats, _ := ob.Stats(ctx)
	if stats.ByState["ACKNOWLEDGED"] != 2 {
		t.Fatalf("outbox %+v", stats)
	}

	// 2. Bundle sync: whatever is published now, then a newer one.
	ks, err := knowledge.New(ctx, st.DB, knowledge.Config{DeviceProfile: "pi5"})
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "knowledge", "bundle.json")
	bw := New(Config{BaseURL: base, ServerKeys: serverKeys, HTTPTimeout: 10 * time.Second, CachePath: cache}, ks, id, nil)
	res, err := bw.CheckOnce(ctx)
	if err != nil {
		t.Fatalf("first bundle check: %v", err)
	}
	t.Logf("first check: %s %s", res.Outcome, res.Report.BundleVersion)
	var older []byte
	if res.Outcome == ResultLoaded {
		older, _ = os.ReadFile(cache)
	}

	editor := login(t, base, "dev-editor", creds["dev-editor"])
	reviewer := login(t, base, "dev-reviewer", creds["dev-reviewer"])
	const fid = "fixture-it-elderly-pension"
	var rec scheme.Record
	for _, r := range fixtureRecords(t) {
		if r.ID == "test-elderly-pension" { // a FIXTURE (fake) scheme
			rec = r
		}
	}
	if rec.ID == "" {
		t.Fatal("fixture test-elderly-pension missing")
	}
	rec.ID = fid
	rec.Name.En += " (FIXTURE)"
	rec.Name.Hi += " (FIXTURE)"
	rec.Verification = scheme.Verification{Status: scheme.NeedsReview, Notes: "TEST FIXTURE ONLY - not a real scheme (integration test)"}
	rec.LastVerified = ""
	publish := func(note string) string {
		var cur struct {
			ID      string `json:"id"`
			Version struct {
				Version int `json:"version"`
			} `json:"version"`
		}
		baseVersion := 0
		req, _ := http.NewRequest(http.MethodGet, base+"/v1/admin/schemes/"+fid, nil)
		if resp, err := editor.c.Do(req); err == nil {
			if resp.StatusCode == http.StatusOK {
				_ = json.NewDecoder(resp.Body).Decode(&cur)
				baseVersion = cur.Version.Version
			}
			resp.Body.Close()
		}
		rec.Summary.En = "A test fixture scheme that pays a monthly pension to elderly people. " + note
		var saved struct {
			Version struct {
				Version int `json:"version"`
			} `json:"version"`
		}
		editor.do(http.MethodPut, "/v1/admin/schemes/"+fid, map[string]any{"base_version": baseVersion, "change_note": note, "record": rec}, http.StatusOK, &saved)
		reviewer.do(http.MethodPost, "/v1/admin/schemes/"+fid+"/verify", map[string]any{"version": saved.Version.Version, "notes": "FIXTURE verified by dev reviewer (integration test)"}, http.StatusOK, nil)
		var out struct {
			Bundle struct {
				BundleVersion string `json:"bundle_version"`
			} `json:"bundle"`
		}
		reviewer.do(http.MethodPost, "/v1/admin/bundles", nil, http.StatusCreated, &out)
		return out.Bundle.BundleVersion
	}

	if older == nil {
		v := publish(fmt.Sprintf("it-a-%d", time.Now().Unix()))
		res, err := bw.CheckOnce(ctx)
		if err != nil || res.Outcome != ResultLoaded || res.Report.BundleVersion != v {
			t.Fatalf("load %s: %+v %v", v, res, err)
		}
		older, _ = os.ReadFile(cache)
	}
	newer := publish(fmt.Sprintf("it-b-%d", time.Now().Unix()))
	res, err = bw.CheckOnce(ctx)
	if err != nil || res.Outcome != ResultLoaded || res.Report.BundleVersion != newer {
		t.Fatalf("load newer %s: %+v %v", newer, res, err)
	}
	if v, _ := ks.BundleVersion(ctx); v != newer {
		t.Fatalf("device_meta bundle_version %q, want %q", v, newer)
	}
	if r, err := ks.Get(ctx, fid); err != nil || !r.IsFactual(time.Now()) || !strings.Contains(r.Verification.VerifiedBy, "-") {
		t.Fatalf("fixture record not loaded as VERIFIED by a reviewer id: %+v %v", r.Verification, err)
	}
	res, err = bw.CheckOnce(ctx)
	if err != nil || res.Outcome != ResultUpToDate {
		t.Fatalf("expected 204/up to date: %+v %v", res, err)
	}

	// 3. Replay of a genuine, correctly signed but OLDER central bundle
	// (e.g. by a captive portal or a stale mirror) is refused.
	replay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(older)
	}))
	defer replay.Close()
	rw := New(Config{BaseURL: replay.URL, ServerKeys: serverKeys, HTTPTimeout: 10 * time.Second}, ks, id, nil)
	if _, err := rw.CheckOnce(ctx); err == nil || !strings.HasPrefix(err.Error(), ReasonRollback) {
		t.Fatalf("older genuine bundle must be refused as rollback: %v", err)
	}
	if v, _ := ks.BundleVersion(ctx); v != newer {
		t.Fatalf("rollback changed knowledge to %q", v)
	}

	// 4. Heartbeat, stored centrally with the loaded bundle version.
	hw := heartbeat.New(heartbeat.Config{BaseURL: base, AgentVersion: "it-test", HTTPTimeout: 10 * time.Second}, id, heartbeat.Sources{
		Outbox: ob.Stats, Sync: sw.Status, BundleVersion: ks.BundleVersion, DataDir: dir, StartedAt: time.Now().Add(-time.Minute),
	}, nil)
	if err := hw.SendOnce(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	var dev struct {
		Sync struct {
			EventsTotal int64 `json:"events_total"`
		} `json:"sync"`
		LastHeartbeat struct {
			BundleVersion      string `json:"bundle_version"`
			AgentVersion       string `json:"agent_version"`
			OutboxAcknowledged int    `json:"outbox_acknowledged"`
		} `json:"last_heartbeat"`
	}
	admin.do(http.MethodGet, "/v1/admin/devices/"+id.DeviceID, nil, http.StatusOK, &dev)
	if dev.Sync.EventsTotal != 2 || dev.LastHeartbeat.BundleVersion != newer || dev.LastHeartbeat.AgentVersion != "it-test" ||
		dev.LastHeartbeat.OutboxAcknowledged != 2 {
		t.Fatalf("central view of the device: %+v", dev)
	}
	t.Logf("central stored 2 events and the heartbeat; device loaded %s and refused the older genuine bundle", newer)
}
