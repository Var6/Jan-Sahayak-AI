//go:build reliabilityrepro

package syncer

// Reliability review repro (run with -tags reliabilityrepro). Expected to
// FAIL until fixed: on an offline-first kiosk, every failed upload attempt
// (no network, DNS failure, clock-skew 401) consumes the per-event attempt
// budget. After MaxAttempts (50, ≈10 days offline with the 30 s→6 h
// backoff) events go to FAILED, and no code path ever re-queues FAILED
// events (Outbox.RequeueFailed has no caller), so they are never uploaded
// even after connectivity returns, and they count against the 100k cap.

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/identity"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/outbox"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/store"
)

type relClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *relClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *relClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestReproLongOfflinePeriodPermanentlyFailsEvents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, _, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, _, err := identity.LoadOrCreate(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	clk := &relClock{t: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
	ob := outbox.New(s.DB, outbox.DefaultConfig, clk.now)
	if _, err := ob.Enqueue(ctx, nil, "session.ended", map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	w := New(Config{BaseURL: "http://127.0.0.1:1", ServerKeys: map[string]ed25519.PublicKey{}, HTTPTimeout: time.Second}, ob, id, slog.New(slog.DiscardHandler))
	w.now = clk.now
	start := clk.now()
	for i := 0; i < 60; i++ { // the worker wakes at least every ~37 min; jump straight to each due time
		_, _ = w.SyncOnce(ctx)
		clk.add(6*time.Hour + time.Minute)
	}
	c := counts(t, ob)
	t.Logf("after %v offline: %v", clk.now().Sub(start).Round(time.Hour), c)
	if c[outbox.Failed] > 0 {
		// Connectivity is back: does anything ever retry it?
		n, _ := w.SyncOnce(ctx)
		t.Fatalf("event moved to FAILED purely because the device was offline (%v); a later SyncOnce sends %d events — it is never retried automatically", c, n)
	}
}
