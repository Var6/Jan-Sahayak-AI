package outbox

// Independent-review test (correctness & spec conformance).

import (
	"context"
	"os"
	"testing"
	"time"
)

// OFFLINE_SYNC.md: "Every citizen interaction completes without internet.
// Events are written to SQLite first and synced later". With the default
// config (MaxAttempts 50, backoff 30 s doubling to 6 h) an event whose
// uploads fail because the kiosk is offline reaches FAILED after roughly ten
// days, and nothing in the agent ever moves FAILED back to PENDING
// (RequeueFailed has no caller), so the event is kept but never uploaded
// once the network returns.
func TestReviewLongOfflineStrandsEventsInFailed(t *testing.T) {
	reviewGate(t)
	ctx := context.Background()
	o, clk, s := setup(t, DefaultConfig)
	id, err := o.Enqueue(ctx, nil, "session.ended", map[string]any{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}
	start := clk.now()
	// Fix-up (workstream B): the original loop only advanced the clock when
	// nothing was due. Under decision 1 a transport failure makes the batch
	// due again immediately (the sync worker's own 15 s → 30 min backoff
	// paces retries), so the original loop spun 20,000 times without
	// simulated time passing and asserted nothing about a long offline
	// period. Advance by the worker's maximum backoff after every failed
	// upload instead, covering 30 days offline.
	const workerMaxBackoff = 30 * time.Minute
	for clk.now().Sub(start) < 30*24*time.Hour {
		batch, evs, err := o.Claim(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) > 0 {
			// Network unreachable: the worker returns the batch to PENDING.
			if err := o.RetryBatch(ctx, batch, "sync: request: dial tcp: network is unreachable"); err != nil {
				t.Fatal(err)
			}
		}
		clk.add(workerMaxBackoff)
	}
	offline := clk.now().Sub(start)
	st := state(t, s, id)
	t.Logf("state after %.1f days of being offline: %s", offline.Hours()/24, st)
	if st != Pending {
		t.Errorf("event in %s after %.1f days offline; transport failures must not consume attempts", st, offline.Hours()/24)
	}
	// Network is back: the event is claimed on the next pass.
	if _, evs, _ := o.Claim(ctx, 10); len(evs) != 1 || evs[0].ID != id {
		t.Errorf("event stranded after %.1f days offline; not claimable once the network returns", offline.Hours()/24)
	}
}

// reviewGate keeps these spec-mismatch reproductions out of the default
// suite until the defects are fixed. Run with JS_REVIEW=1.
func reviewGate(t *testing.T) {
	t.Helper()
	if os.Getenv("JS_REVIEW") != "1" {
		t.Skip("review reproduction; set JS_REVIEW=1")
	}
}
