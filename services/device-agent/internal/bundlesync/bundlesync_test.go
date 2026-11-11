package bundlesync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/deviceapi"
	"github.com/janmanindia/jan-sahayak/internal/scheme"
	"github.com/janmanindia/jan-sahayak/internal/syncproto"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/identity"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/knowledge"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/store"
)

// fixtureRecords loads the knowledge package's FIXTURE records (fake
// schemes for tests only).
func fixtureRecords(t *testing.T) []scheme.Record {
	t.Helper()
	dir := filepath.Join("..", "knowledge", "testdata", "records")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []scheme.Record
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var r scheme.Record
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if len(out) < 2 {
		t.Fatal("expected fixture records")
	}
	return out
}

// fakeCentral mimics GET /v1/devices/knowledge-bundle: it checks the
// device signature over path+query (real deviceapi/syncproto code) and
// serves the current signed bundle bytes, 204 when ?since matches.
type fakeCentral struct {
	t      *testing.T
	devPub ed25519.PublicKey

	mu       sync.Mutex
	body     []byte // signed bundle JSON ("" = none published)
	version  string
	badSum   bool
	status   int // non-zero: answer with this status instead
	requests int
	sinces   []string
	nonces   map[string]bool
}

func (f *fakeCentral) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.Method != http.MethodGet || r.URL.Path != deviceapi.KnowledgeBundlePath {
		http.Error(w, "wrong endpoint", http.StatusNotFound)
		return
	}
	err := syncproto.VerifyRequest(f.devPub, r.Method, deviceapi.SignedTarget(r.URL), r.Header.Get(syncproto.HeaderTS),
		r.Header.Get(syncproto.HeaderNonce), r.Header.Get(syncproto.HeaderSig), nil, time.Now())
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"x"}}`))
		return
	}
	if n := r.Header.Get(syncproto.HeaderNonce); f.nonces[n] {
		w.WriteHeader(http.StatusUnauthorized)
		return
	} else {
		f.nonces[n] = true
	}
	since := r.URL.Query().Get("since")
	f.sinces = append(f.sinces, since)
	if f.status != 0 {
		if f.status == http.StatusFound {
			http.Redirect(w, r, "http://example.invalid/", http.StatusFound)
			return
		}
		w.WriteHeader(f.status)
		return
	}
	if f.body == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"no_bundle","message":"none"}}`))
		return
	}
	if since != "" && since == f.version {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sum := sha256.Sum256(f.body)
	if f.badSum {
		sum[0] ^= 0xff
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-JS-Bundle-SHA256", hex.EncodeToString(sum[:]))
	_, _ = w.Write(f.body)
}

func (f *fakeCentral) publish(body []byte, version string) {
	f.mu.Lock()
	f.body, f.version, f.status, f.badSum = body, version, 0, false
	f.mu.Unlock()
}

type env struct {
	t       *testing.T
	srv     *httptest.Server
	central *fakeCentral
	ks      *knowledge.Store
	st      *store.Store
	w       *Worker
	srvKey  ed25519.PrivateKey
	keyID   string
	cache   string
	busy    atomic.Bool
	id      identity.Identity
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	id, _, err := identity.LoadOrCreate(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	srvPub, srvKey, _ := ed25519.GenerateKey(rand.Reader)
	st, _, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "device.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ks, err := knowledge.New(ctx, st.DB, knowledge.Config{DeviceProfile: "pi5"})
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeCentral{t: t, devPub: id.PrivateKey.Public().(ed25519.PublicKey), nonces: map[string]bool{}}
	srv := httptest.NewServer(fc)
	t.Cleanup(srv.Close)
	e := &env{t: t, srv: srv, central: fc, ks: ks, st: st, srvKey: srvKey, keyID: "srv-test-1",
		cache: filepath.Join(dir, "knowledge", "bundle.json"), id: id}
	e.w = New(Config{
		BaseURL: srv.URL, ServerKeys: map[string]ed25519.PublicKey{"srv-test-1": srvPub},
		HTTPTimeout: 5 * time.Second, CachePath: e.cache, Busy: e.busy.Load,
	}, ks, id, nil)
	return e
}

// signed returns the served bytes of a bundle signed with key.
func (e *env) signed(version string, recs []scheme.Record, key ed25519.PrivateKey) []byte {
	e.t.Helper()
	b := scheme.Bundle{BundleVersion: version, CreatedAt: "2026-11-11T00:00:00Z", Records: recs}
	if err := deviceapi.SignBundle(key, e.keyID, &b); err != nil {
		e.t.Fatal(err)
	}
	out, err := json.Marshal(b)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) version() string {
	e.t.Helper()
	v, err := e.ks.BundleVersion(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

func TestLoadThenUpToDateThenNewer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	recs := fixtureRecords(t)

	// Nothing published yet.
	res, err := e.w.CheckOnce(ctx)
	if err != nil || res.Outcome != ResultNoBundle {
		t.Fatalf("no bundle: %+v %v", res, err)
	}

	v1 := e.signed("2026-11-10.1", recs[:3], e.srvKey)
	e.central.publish(v1, "2026-11-10.1")
	res, err = e.w.CheckOnce(ctx)
	if err != nil || res.Outcome != ResultLoaded || res.Report.Loaded != 3 {
		t.Fatalf("load v1: %+v %v", res, err)
	}
	if got := e.version(); got != "2026-11-10.1" {
		t.Fatalf("device_meta bundle_version = %q", got)
	}
	cached, err := os.ReadFile(e.cache)
	if err != nil || !bytes.Equal(cached, v1) {
		t.Fatalf("cache file must hold the exact signed bytes: %v", err)
	}
	if fi, _ := os.Stat(e.cache); fi.Mode().Perm() != 0o640 {
		t.Fatalf("cache mode %v", fi.Mode().Perm())
	}

	// Same version again: the device asks with ?since and gets 204.
	res, err = e.w.CheckOnce(ctx)
	if err != nil || res.Outcome != ResultUpToDate {
		t.Fatalf("up to date: %+v %v", res, err)
	}
	if s := e.central.sinces; s[len(s)-1] != "2026-11-10.1" {
		t.Fatalf("since sent = %v", s)
	}

	v2 := e.signed("2026-11-11.1", recs, e.srvKey)
	e.central.publish(v2, "2026-11-11.1")
	res, err = e.w.CheckOnce(ctx)
	if err != nil || res.Outcome != ResultLoaded || res.Report.Loaded != len(recs) {
		t.Fatalf("load v2: %+v %v", res, err)
	}
	if e.version() != "2026-11-11.1" || e.w.Status().LastLoadedVersion != "2026-11-11.1" {
		t.Fatalf("v2 not recorded: %q %+v", e.version(), e.w.Status())
	}
}

func TestRefusals(t *testing.T) {
	ctx := context.Background()
	recs := fixtureRecords(t)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)

	cases := []struct {
		name   string
		setup  func(e *env)
		reason string
	}{
		{"rollback", func(e *env) {
			e.central.publish(e.signed("2026-11-09.4", recs, e.srvKey), "2026-11-09.4")
		}, ReasonRollback},
		{"same day lower number", func(e *env) {
			e.central.publish(e.signed("2026-11-10.0", recs, e.srvKey), "2026-11-10.0")
		}, ReasonRollback},
		{"signed by an unpinned key", func(e *env) {
			e.central.publish(e.signed("2026-11-12.1", recs, otherKey), "2026-11-12.1")
		}, ReasonSignature},
		{"record altered after signing", func(e *env) {
			body := e.signed("2026-11-12.1", recs, e.srvKey)
			body = bytes.Replace(body, []byte("999"), []byte("998"), 1)
			e.central.publish(body, "2026-11-12.1")
		}, ReasonSignature},
		{"version altered after signing", func(e *env) {
			body := e.signed("2026-11-12.1", recs, e.srvKey)
			body = bytes.Replace(body, []byte("2026-11-12.1"), []byte("2026-11-12.2"), 1)
			e.central.publish(body, "2026-11-12.2")
		}, ReasonSignature},
		{"checksum header mismatch", func(e *env) {
			e.central.publish(e.signed("2026-11-12.1", recs, e.srvKey), "2026-11-12.1")
			e.central.mu.Lock()
			e.central.badSum = true
			e.central.mu.Unlock()
		}, ReasonChecksum},
		{"not a central version", func(e *env) {
			e.central.publish(e.signed("local-abc", recs, e.srvKey), "local-abc")
		}, ReasonVersion},
		{"no valid record", func(e *env) {
			bad := recs[0]
			bad.ID = "Not A Slug"
			e.central.publish(e.signed("2026-11-12.1", []scheme.Record{bad}, e.srvKey), "2026-11-12.1")
		}, ReasonNoValid},
		{"undecodable", func(e *env) { e.central.publish([]byte("{not json"), "2026-11-12.1") }, ReasonDecode},
		{"too large", func(e *env) {
			e.w.cfg.MaxBytes = 512
			e.central.publish(e.signed("2026-11-12.1", recs, e.srvKey), "2026-11-12.1")
		}, ReasonTooLarge},
		{"server error", func(e *env) {
			e.central.mu.Lock()
			e.central.status = http.StatusInternalServerError
			e.central.mu.Unlock()
		}, "http_500"},
		{"redirect is not followed", func(e *env) {
			e.central.mu.Lock()
			e.central.status = http.StatusFound
			e.central.mu.Unlock()
		}, "http_302"},
		{"unregistered device", func(e *env) {
			pub, _, _ := ed25519.GenerateKey(rand.Reader)
			e.central.mu.Lock()
			e.central.devPub = pub
			e.central.mu.Unlock()
		}, ReasonUnauthorize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			v1 := e.signed("2026-11-10.1", recs[:2], e.srvKey)
			e.central.publish(v1, "2026-11-10.1")
			if _, err := e.w.CheckOnce(ctx); err != nil {
				t.Fatal(err)
			}
			tc.setup(e)
			_, err := e.w.CheckOnce(ctx)
			if err == nil || !strings.HasPrefix(err.Error(), tc.reason) {
				t.Fatalf("want %s, got %v", tc.reason, err)
			}
			st := e.w.Status()
			if st.LastError != tc.reason || st.ConsecutiveFailures != 1 {
				t.Fatalf("status %+v", st)
			}
			// The previous knowledge is untouched, and so is the cache file.
			if e.version() != "2026-11-10.1" {
				t.Fatalf("knowledge changed to %q", e.version())
			}
			n := 0
			_ = e.st.DB.QueryRow(`SELECT count(*) FROM schemes`).Scan(&n)
			if n != 2 {
				t.Fatalf("schemes = %d", n)
			}
			if b, _ := os.ReadFile(e.cache); !bytes.Equal(b, v1) {
				t.Fatal("cache file changed after a refusal")
			}
		})
	}
}

func TestBusyPostponesWithoutNetwork(t *testing.T) {
	e := newEnv(t)
	e.busy.Store(true)
	if _, err := e.w.CheckOnce(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	if e.central.requests != 0 {
		t.Fatal("a busy kiosk must not download")
	}
	if d := e.w.checkAndSchedule(context.Background()); d != busyRetry {
		t.Fatalf("busy retry = %v", d)
	}
}

func TestOfflineBacksOffAndKeepsKnowledge(t *testing.T) {
	e := newEnv(t)
	recs := fixtureRecords(t)
	e.central.publish(e.signed("2026-11-10.1", recs, e.srvKey), "2026-11-10.1")
	if _, err := e.w.CheckOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.srv.Close() // offline
	var prev time.Duration
	for i := 1; i <= 6; i++ {
		d := e.w.checkAndSchedule(context.Background())
		if e.w.Status().LastError != ReasonNetwork {
			t.Fatalf("reason %q", e.w.Status().LastError)
		}
		if d > maxFailBackoff+maxFailBackoff/9 || d < minFailBackoff-minFailBackoff/9 {
			t.Fatalf("attempt %d backoff %v out of bounds", i, d)
		}
		if i > 1 && i < 5 && d < prev/2 {
			t.Fatalf("backoff shrank: %v after %v", d, prev)
		}
		prev = d
	}
	if e.version() != "2026-11-10.1" {
		t.Fatal("offline must not change knowledge")
	}
}

func TestRestoreFromDisk(t *testing.T) {
	ctx := context.Background()
	recs := fixtureRecords(t)
	e := newEnv(t)
	v2 := e.signed("2026-11-11.2", recs, e.srvKey)
	e.central.publish(v2, "2026-11-11.2")
	if _, err := e.w.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// Same version in the database: nothing to do.
	if _, loaded, err := e.w.RestoreFromDisk(ctx); err != nil || loaded {
		t.Fatalf("same version: loaded=%v err=%v", loaded, err)
	}

	// A fresh database (e.g. rebuilt after corruption) gets the cached bundle.
	st2, _, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "device.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	ks2, err := knowledge.New(ctx, st2.DB, knowledge.Config{DeviceProfile: "pi5"})
	if err != nil {
		t.Fatal(err)
	}
	w2 := New(e.w.cfg, ks2, e.id, nil)
	rep, loaded, err := w2.RestoreFromDisk(ctx)
	if err != nil || !loaded || rep.Loaded != len(recs) {
		t.Fatalf("restore: %+v loaded=%v err=%v", rep, loaded, err)
	}
	if v, _ := ks2.BundleVersion(ctx); v != "2026-11-11.2" {
		t.Fatalf("restored version %q", v)
	}

	// A database holding a newer bundle refuses the older cached file.
	if _, err := ks2.LoadBundle(ctx, mustDecode(t, e.signed("2026-11-12.1", recs[:1], e.srvKey)), nil); err != nil {
		t.Fatal(err)
	}
	if _, loaded, err := w2.RestoreFromDisk(ctx); loaded || err == nil || !errors.Is(err, knowledge.ErrBundleRollback) {
		t.Fatalf("older cache must be refused: loaded=%v err=%v", loaded, err)
	}

	// A tampered cache file is refused.
	b, _ := os.ReadFile(e.cache)
	if err := os.WriteFile(e.cache, bytes.Replace(b, []byte("999"), []byte("998"), 1), 0o640); err != nil {
		t.Fatal(err)
	}
	st3, _, _ := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "device.db")})
	defer st3.Close()
	ks3, _ := knowledge.New(ctx, st3.DB, knowledge.Config{DeviceProfile: "pi5"})
	w3 := New(e.w.cfg, ks3, e.id, nil)
	if _, loaded, err := w3.RestoreFromDisk(ctx); loaded || err == nil || !strings.HasPrefix(err.Error(), ReasonSignature) {
		t.Fatalf("tampered cache: loaded=%v err=%v", loaded, err)
	}
}

func mustDecode(t *testing.T, b []byte) scheme.Bundle {
	t.Helper()
	var x scheme.Bundle
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatal(err)
	}
	return x
}

func TestLocalVersionIsReplacedWithoutSince(t *testing.T) {
	ctx := context.Background()
	recs := fixtureRecords(t)
	e := newEnv(t)
	if _, err := e.ks.Replace(ctx, "local-0123456789ab", recs[:1]); err != nil {
		t.Fatal(err)
	}
	e.central.publish(e.signed("2026-11-10.1", recs, e.srvKey), "2026-11-10.1")
	res, err := e.w.CheckOnce(ctx)
	if err != nil || res.Outcome != ResultLoaded {
		t.Fatalf("%+v %v", res, err)
	}
	if e.central.sinces[0] != "" {
		t.Fatalf("a local version must not be sent as since: %q", e.central.sinces[0])
	}
}

func TestNotifyLatestRateLimit(t *testing.T) {
	e := newEnv(t)
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	e.w.now = func() time.Time { return now }
	drain := func() bool {
		select {
		case <-e.w.kick:
			return true
		default:
			return false
		}
	}
	e.w.NotifyLatest("not-a-version")
	if drain() {
		t.Fatal("malformed version must not kick")
	}
	e.w.NotifyLatest("2026-11-11.1")
	if !drain() {
		t.Fatal("new version must kick")
	}
	e.w.NotifyLatest("2026-11-11.2")
	if drain() {
		t.Fatal("second kick within 10 min must be suppressed")
	}
	now = now.Add(11 * time.Minute)
	e.w.mu.Lock()
	e.w.status.LastRefusedVersion = "2026-11-11.2"
	e.w.mu.Unlock()
	e.w.NotifyLatest("2026-11-11.2")
	if drain() {
		t.Fatal("a refused version must not kick again")
	}
	e.w.NotifyLatest("2026-11-11.3")
	if !drain() {
		t.Fatal("another new version after the gap must kick")
	}
}

func TestRunChecksOnStartAndStops(t *testing.T) {
	e := newEnv(t)
	e.w.cfg.InitialDelay = 10 * time.Millisecond
	e.central.publish(e.signed("2026-11-10.1", fixtureRecords(t), e.srvKey), "2026-11-10.1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.w.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for e.w.Status().LastLoadedVersion == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.w.Status().LastLoadedVersion != "2026-11-10.1" {
		t.Fatalf("not loaded on start: %+v", e.w.Status())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestAge(t *testing.T) {
	now := time.Date(2026, 12, 20, 12, 0, 0, 0, time.UTC)
	if _, ok := Age("local-abc", now); ok {
		t.Fatal("local version has no age")
	}
	a, ok := Age("2026-11-10.3", now)
	if !ok || a < 39*24*time.Hour || a > 40*24*time.Hour {
		t.Fatalf("age %v %v", a, ok)
	}
	if a, _ := Age("2026-12-20.1", now); a != 0 {
		t.Fatalf("same-day bundle age %v", a)
	}
}

// An older (refused) cache file is rewritten on the next check: the device
// asks for the full bundle instead of ?since, and keeps the current bytes.
func TestStaleCacheIsRewritten(t *testing.T) {
	ctx := context.Background()
	recs := fixtureRecords(t)
	e := newEnv(t)
	v1 := e.signed("2026-11-10.1", recs[:1], e.srvKey)
	v2 := e.signed("2026-11-11.1", recs, e.srvKey)
	e.central.publish(v1, "2026-11-10.1")
	if _, err := e.w.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	e.central.publish(v2, "2026-11-11.1")
	if _, err := e.w.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.cache, v1, 0o640); err != nil { // replayed older file
		t.Fatal(err)
	}
	w2 := New(e.w.cfg, e.ks, e.id, nil)
	if _, loaded, err := w2.RestoreFromDisk(ctx); loaded || !errors.Is(err, knowledge.ErrBundleRollback) {
		t.Fatalf("restore: loaded=%v err=%v", loaded, err)
	}
	res, err := w2.CheckOnce(ctx)
	if err != nil || res.Outcome != ResultUpToDate {
		t.Fatalf("%+v %v", res, err)
	}
	if s := e.central.sinces; s[len(s)-1] != "" {
		t.Fatalf("stale cache must request the full bundle, sent since=%q", s[len(s)-1])
	}
	if b, _ := os.ReadFile(e.cache); !bytes.Equal(b, v2) {
		t.Fatal("cache not rewritten with the loaded bundle")
	}
	if _, err := w2.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if s := e.central.sinces; s[len(s)-1] != "2026-11-11.1" {
		t.Fatalf("since after rewrite = %q", s[len(s)-1])
	}
	if e.version() != "2026-11-11.1" {
		t.Fatal("knowledge changed")
	}
}
