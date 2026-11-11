package heartbeat

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/deviceapi"
	"github.com/janmanindia/jan-sahayak/internal/syncproto"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/identity"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/store"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/syncer"
)

// fakeCentral mimics POST /v1/devices/heartbeat: verifies the device
// signature (real syncproto/deviceapi code), rejects replays, validates the
// body with deviceapi.Heartbeat.Validate and checks device_id, as the
// central API does.
type fakeCentral struct {
	devPub ed25519.PublicKey

	mu       sync.Mutex
	got      []deviceapi.Heartbeat
	raw      [][]byte
	nonces   map[string]bool
	status   int // non-zero: answer with this
	skewNow  int64
	latest   string
	lastSeen time.Time
}

func (f *fakeCentral) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != deviceapi.HeartbeatPath {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, deviceapi.MaxHeartbeatBytes+1))
	switch f.status {
	case 0:
	case http.StatusTooManyRequests:
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"error":{"code":"too_frequent","message":"x"}}`))
		return
	case http.StatusUnauthorized:
		if f.skewNow != 0 {
			w.Header().Set("X-JS-Server-Time", strconv.FormatInt(f.skewNow, 10))
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"clock_skew","message":"x"}}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"x"}}`))
		return
	case http.StatusFound:
		http.Redirect(w, r, "http://example.invalid/", http.StatusFound)
		return
	default:
		w.WriteHeader(f.status)
		return
	}
	err := syncproto.VerifyRequest(f.devPub, r.Method, deviceapi.SignedTarget(r.URL), r.Header.Get(syncproto.HeaderTS),
		r.Header.Get(syncproto.HeaderNonce), r.Header.Get(syncproto.HeaderSig), body, time.Now())
	if err != nil || f.nonces[r.Header.Get(syncproto.HeaderNonce)] {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.nonces[r.Header.Get(syncproto.HeaderNonce)] = true
	var hb deviceapi.Heartbeat
	if err := json.Unmarshal(body, &hb); err != nil || hb.Validate() != nil || hb.DeviceID != r.Header.Get(syncproto.HeaderDevice) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.got = append(f.got, hb)
	f.raw = append(f.raw, body)
	_ = json.NewEncoder(w).Encode(deviceapi.HeartbeatResponse{ServerTime: time.Now().UTC().Format(time.RFC3339), LatestBundleVersion: f.latest})
}

func (f *fakeCentral) set(status int, skew int64) {
	f.mu.Lock()
	f.status, f.skewNow = status, skew
	f.mu.Unlock()
}

type env struct {
	t       *testing.T
	srv     *httptest.Server
	central *fakeCentral
	w       *Worker
	mon     *health.Monitor
	ob      *outbox.Outbox
	latest  []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	id, _, err := identity.LoadOrCreate(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "device.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ob := outbox.New(st.DB, outbox.Config{MaxRows: 1000}, nil)

	mon := health.NewMonitor()
	mon.Register(health.Component{Name: "database", Critical: true, Interval: time.Hour, Timeout: time.Second,
		Check: func(context.Context) health.Result { return health.Result{State: health.Healthy} }})
	mon.Register(health.Component{Name: "stt", Interval: time.Hour, Timeout: time.Second,
		Check: func(context.Context) health.Result {
			// A long detail must be clipped rather than make the
			// heartbeat invalid.
			return health.Result{State: health.Unavailable, Detail: "stt_unreachable" + strings.Repeat("x", 300)}
		}})
	mon.CheckAll(context.Background())

	fc := &fakeCentral{devPub: id.PrivateKey.Public().(ed25519.PublicKey), nonces: map[string]bool{}, latest: "2026-11-11.1"}
	srv := httptest.NewServer(fc)
	t.Cleanup(srv.Close)
	e := &env{t: t, srv: srv, central: fc, mon: mon, ob: ob}
	sw := syncer.New(syncer.Config{}, ob, id, nil)
	e.w = New(Config{BaseURL: srv.URL, AgentVersion: "test-1.2.3", Interval: time.Minute, HTTPTimeout: 5 * time.Second}, id, Sources{
		Health: mon.Snapshot, Outbox: ob.Stats, Sync: sw.Status,
		BundleVersion: func(context.Context) (string, error) { return "2026-11-10.2", nil },
		Models:        map[string]string{"stt": "indicconformer-hi-int8", "bad\nkey": strings.Repeat("m", 500)},
		DataDir:       dir, StartedAt: time.Now().Add(-90 * time.Second),
		OnLatestBundle: func(v string) { e.latest = append(e.latest, v) },
	}, nil)
	return e
}

func TestHeartbeatAcceptedAndCarriesSnapshot(t *testing.T) {
	e := newEnv(t)
	if err := e.w.SendOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.central.got) != 1 {
		t.Fatalf("server got %d heartbeats", len(e.central.got))
	}
	hb := e.central.got[0]
	if hb.AgentVersion != "test-1.2.3" || hb.BundleVersion != "2026-11-10.2" || hb.Overall != "DEGRADED" {
		t.Fatalf("heartbeat %+v", hb)
	}
	if len(hb.Components) != 2 || hb.Components[1].Name != "stt" || len(hb.Components[1].Detail) > deviceapi.MaxDetailText ||
		!strings.HasPrefix(hb.Components[1].Detail, "stt_unreachable") || !hb.Components[0].Critical {
		t.Fatalf("components %+v", hb.Components)
	}
	if hb.Outbox.Capacity != 1000 {
		t.Fatalf("outbox %+v", hb.Outbox)
	}
	if hb.Models["stt"] != "indicconformer-hi-int8" || hb.Models["badkey"] == "" || len(hb.Models["badkey"]) > 2*deviceapi.MaxShortText {
		t.Fatalf("models %+v", hb.Models)
	}
	r := hb.Resources
	if r.UptimeSeconds == nil || *r.UptimeSeconds < 89 || *r.UptimeSeconds > 200 {
		t.Fatalf("uptime %v", r.UptimeSeconds)
	}
	if r.DiskTotalBytes == nil || r.DiskFreeBytes == nil {
		t.Fatal("disk numbers missing (statfs works on darwin and linux)")
	}
	if len(e.latest) != 1 || e.latest[0] != "2026-11-11.1" {
		t.Fatalf("latest bundle callback %v", e.latest)
	}
	st := e.w.Status()
	if st.Sent != 1 || st.ConsecutiveFailures != 0 || st.LastSuccessAt.IsZero() {
		t.Fatalf("status %+v", st)
	}
	// Each send uses a fresh nonce.
	if err := e.w.SendOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.central.got) != 2 {
		t.Fatal("second heartbeat refused")
	}
}

// The heartbeat must not carry free text: only the documented fields.
func TestHeartbeatHasOnlyContractFields(t *testing.T) {
	e := newEnv(t)
	if err := e.w.SendOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(e.central.raw[0], &m); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"device_id": true, "sent_at": true, "agent_version": true, "os": true, "bundle_version": true,
		"models": true, "overall": true, "components": true, "outbox": true, "sync": true, "resources": true}
	for k := range m {
		if !allowed[k] {
			t.Fatalf("unexpected field %q", k)
		}
	}
}

func TestFailuresAreCountedAndBackOff(t *testing.T) {
	cases := []struct {
		name   string
		status int
		skew   int64
		reason string
	}{
		{"server error", http.StatusInternalServerError, 0, "http_500"},
		{"unregistered or revoked", http.StatusUnauthorized, 0, ReasonUnauthorize},
		{"clock skew", http.StatusUnauthorized, time.Now().Unix() + 3600, ReasonClockSkew},
		{"redirect not followed", http.StatusFound, 0, "http_302"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.central.set(tc.status, tc.skew)
			err := e.w.SendOnce(context.Background())
			if err == nil || !strings.HasPrefix(err.Error(), tc.reason) {
				t.Fatalf("want %s, got %v", tc.reason, err)
			}
			st := e.w.Status()
			if st.LastError != tc.reason || st.ConsecutiveFailures != 1 {
				t.Fatalf("status %+v", st)
			}
			if tc.skew != 0 && (st.ClockSkewSeconds < 3590 || st.ClockSkewSeconds > 3610) {
				t.Fatalf("skew %d", st.ClockSkewSeconds)
			}
		})
	}
}

func TestOfflineBackoffIsBounded(t *testing.T) {
	e := newEnv(t)
	e.srv.Close()
	var waits []time.Duration
	for range 8 {
		waits = append(waits, e.w.sendAndSchedule(context.Background()))
	}
	if e.w.Status().LastError != ReasonNetwork || e.w.Status().ConsecutiveFailures != 8 {
		t.Fatalf("status %+v", e.w.Status())
	}
	// Interval 1 min doubling: ~1, 2, 4, 8, 16, 30, 30, 30 min (±10 %).
	for i, d := range waits {
		if d < 54*time.Second || d > MaxBackoff+MaxBackoff/10 {
			t.Fatalf("wait %d = %v out of bounds", i, d)
		}
	}
	if waits[3] < 7*time.Minute || waits[7] < 27*time.Minute {
		t.Fatalf("backoff not growing: %v", waits)
	}
}

func TestTooFrequentWaitsRetryAfterWithoutCountingFailure(t *testing.T) {
	e := newEnv(t)
	e.central.set(http.StatusTooManyRequests, 0)
	err := e.w.SendOnce(context.Background())
	var tf *tooFrequentError
	if !errors.As(err, &tf) || tf.retryAfter != 45*time.Second {
		t.Fatalf("got %v", err)
	}
	if e.w.Status().ConsecutiveFailures != 0 || e.w.Status().LastError != ReasonTooFrequent {
		t.Fatalf("status %+v", e.w.Status())
	}
}

func TestRunSendsAfterInitialDelayAndStops(t *testing.T) {
	e := newEnv(t)
	e.w.cfg.InitialDelay = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.w.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for e.w.Status().Sent == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.w.Status().Sent != 1 {
		t.Fatalf("status %+v", e.w.Status())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestHungServerIsBoundedByTimeout(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer hung.Close()
	defer close(release)
	e.w.cfg.BaseURL = hung.URL
	e.w.client.Timeout = 200 * time.Millisecond
	start := time.Now()
	err := e.w.SendOnce(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), ReasonNetwork) || time.Since(start) > 3*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestClip(t *testing.T) {
	if got := clip("ab\ncd", 10); got != "abcd" {
		t.Fatalf("%q", got)
	}
	if got := clip("हिन्दी", 7); got != "हि" { // 3-byte runes, never split
		t.Fatalf("%q", got)
	}
}
