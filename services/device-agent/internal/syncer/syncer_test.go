package syncer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/syncproto"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/identity"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/store"
)

type mode int

const (
	modeGood mode = iota
	modeForgedSig
	modeWrongBatch
	mode500
	modeGarbage200
	modeExtraResult
	modeDropAfterCommit // server stores, but response is lost
)

type fakeServer struct {
	mu     sync.Mutex
	mode   mode
	devPub ed25519.PublicKey
	key    ed25519.PrivateKey
	stored map[string]string // id -> hash
	nonces map[string]bool
	t      *testing.T
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	err := syncproto.VerifyRequest(f.devPub, r.Method, r.URL.Path, r.Header.Get(syncproto.HeaderTS),
		r.Header.Get(syncproto.HeaderNonce), r.Header.Get(syncproto.HeaderSig), body, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	n := r.Header.Get(syncproto.HeaderNonce)
	if f.nonces[n] {
		http.Error(w, "replay", http.StatusUnauthorized)
		return
	}
	f.nonces[n] = true
	var req syncproto.BatchRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Validate() != nil {
		http.Error(w, "bad", 400)
		return
	}
	if f.mode == mode500 {
		http.Error(w, "boom", 500)
		return
	}
	if f.mode == modeGarbage200 {
		w.Write([]byte(`{"ok":true}`))
		return
	}
	ack := syncproto.BatchAck{BatchID: req.BatchID, DeviceID: req.DeviceID, ServerTime: time.Now().UTC().Format(time.RFC3339), KeyID: "k1"}
	for _, e := range req.Events {
		res := syncproto.Result{ID: e.ID, PayloadSHA256: e.PayloadSHA256, Receipt: "srv-" + e.ID}
		if e.Validate() != nil {
			res = syncproto.Result{ID: e.ID, Status: syncproto.StatusRejected, Reason: "invalid"}
		} else if h, ok := f.stored[e.ID]; ok {
			if h == e.PayloadSHA256 {
				res.Status = syncproto.StatusDuplicate
			} else {
				res = syncproto.Result{ID: e.ID, Status: syncproto.StatusConflict}
			}
		} else {
			f.stored[e.ID] = e.PayloadSHA256
			res.Status = syncproto.StatusAccepted
		}
		ack.Results = append(ack.Results, res)
	}
	if f.mode == modeDropAfterCommit {
		// Simulate connection loss after commit.
		hj, ok := w.(http.Hijacker)
		if ok {
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
	}
	if f.mode == modeWrongBatch {
		ack.BatchID = "00000000-0000-7000-8000-000000000000"
	}
	if f.mode == modeExtraResult {
		ack.Results = append(ack.Results, syncproto.Result{ID: "00000000-0000-7000-8000-000000000001", Status: "accepted"})
	}
	syncproto.SignAck(f.key, &ack)
	if f.mode == modeForgedSig {
		ack.Results[0].Receipt = "tampered"
	}
	json.NewEncoder(w).Encode(ack)
}

func (f *fakeServer) setMode(m mode) { f.mu.Lock(); f.mode = m; f.mu.Unlock() }

func (f *fakeServer) storedCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.stored) }

func setup(t *testing.T) (*Worker, *outbox.Outbox, *fakeServer, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, _, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	id, created, err := identity.LoadOrCreate(filepath.Join(dir, "identity.json"))
	if err != nil || !created {
		t.Fatal(err)
	}
	spub, spriv, _ := ed25519.GenerateKey(rand.Reader)
	fs := &fakeServer{devPub: id.PrivateKey.Public().(ed25519.PublicKey), key: spriv, stored: map[string]string{}, nonces: map[string]bool{}, t: t}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	ob := outbox.New(s.DB, outbox.DefaultConfig, nil)
	w := New(Config{BaseURL: srv.URL, ServerKeys: map[string]ed25519.PublicKey{"k1": spub}, HTTPTimeout: 5 * time.Second}, ob, id, slog.New(slog.DiscardHandler))
	return w, ob, fs, s
}

func counts(t *testing.T, ob *outbox.Outbox) map[string]int {
	st, err := ob.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st.ByState
}

func TestSyncHappyPath(t *testing.T) {
	ctx := context.Background()
	w, ob, fs, _ := setup(t)
	for i := 0; i < 5; i++ {
		ob.Enqueue(ctx, nil, "session.ended", map[string]int{"i": i})
	}
	n, err := w.SyncOnce(ctx)
	if err != nil || n != 5 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if c := counts(t, ob); c[outbox.Acknowledged] != 5 {
		t.Fatalf("counts %v", c)
	}
	if fs.storedCount() != 5 {
		t.Fatal("server should hold 5")
	}
	if !w.Status().Online {
		t.Fatal("should be online")
	}
}

func TestBadServerResponsesNeverAcknowledge(t *testing.T) {
	for name, m := range map[string]mode{
		"forged signature": modeForgedSig, "wrong batch": modeWrongBatch, "HTTP 500": mode500,
		"HTTP 200 without valid ack": modeGarbage200, "ack mentions unknown event": modeExtraResult,
		"response lost after commit": modeDropAfterCommit,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			w, ob, fs, _ := setup(t)
			fs.setMode(m)
			ob.Enqueue(ctx, nil, "x", map[string]int{"a": 1})
			if _, err := w.SyncOnce(ctx); err == nil {
				t.Fatal("expected error")
			}
			c := counts(t, ob)
			if c[outbox.Acknowledged] != 0 || c[outbox.Pending] != 1 {
				t.Fatalf("event must stay PENDING, got %v", c)
			}
			if w.Status().Online {
				t.Fatal("should not be online")
			}
		})
	}
}

func TestResendAfterLostResponseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	w, ob, fs, s := setup(t)
	fs.setMode(modeDropAfterCommit)
	ob.Enqueue(ctx, nil, "x", map[string]int{"a": 1})
	_, _ = w.SyncOnce(ctx)
	// Make it due again and restore the server.
	s.DB.Exec(`UPDATE outbox_events SET next_attempt_at='2000-01-01T00:00:00Z'`)
	fs.setMode(modeGood)
	if _, err := w.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if c := counts(t, ob); c[outbox.Acknowledged] != 1 {
		t.Fatalf("duplicate should be acknowledged, got %v", c)
	}
	if fs.storedCount() != 1 {
		t.Fatalf("server must store exactly once, has %d", fs.storedCount())
	}
}

func TestUnreachableServer(t *testing.T) {
	ctx := context.Background()
	w, ob, _, _ := setup(t)
	w.cfg.BaseURL = "http://127.0.0.1:1" // nothing listens
	ob.Enqueue(ctx, nil, "x", map[string]int{"a": 1})
	if _, err := w.SyncOnce(ctx); err == nil {
		t.Fatal("expected error")
	}
	if c := counts(t, ob); c[outbox.Pending] != 1 {
		t.Fatalf("got %v", c)
	}
}

func TestRunStopsCleanly(t *testing.T) {
	w, _, _, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	w.Kick()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
}
