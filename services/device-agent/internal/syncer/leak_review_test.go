package syncer

// Reliability review (goroutine leaks / cancellation): a central server
// that accepts the request and never answers must not keep the worker (or
// any HTTP goroutine) alive after shutdown, and the claimed batch must go
// back to PENDING.

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/identity"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/store"
)

func TestReliabilityHangingServerShutdownNoLeak(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	dir := t.TempDir()
	s, _, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := identity.LoadOrCreate(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	ob := outbox.New(s.DB, outbox.DefaultConfig, nil)
	if _, err := ob.Enqueue(context.Background(), nil, "x", map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	w := New(Config{BaseURL: hang.URL, ServerKeys: map[string]ed25519.PublicKey{}, HTTPTimeout: time.Minute}, ob, id, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	w.Kick()
	time.Sleep(200 * time.Millisecond) // request is now in flight and hanging
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop while a request was hanging")
	}
	if c := counts(t, ob); c[outbox.Pending] != 1 || c[outbox.Uploading] != 0 {
		t.Fatalf("interrupted batch must be PENDING again, got %v", c)
	}
	close(release)
	w.client.CloseIdleConnections()
	hang.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	goleak.VerifyNone(t, ignore)
}
