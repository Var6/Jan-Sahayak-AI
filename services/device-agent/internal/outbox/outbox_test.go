package outbox

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/internal/syncproto"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/store"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func setup(t *testing.T, cfg Config) (*Outbox, *clock, *store.Store) {
	t.Helper()
	s, _, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &clock{t: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)}
	return New(s.DB, cfg, c.now), c, s
}

func state(t *testing.T, s *store.Store, id string) string {
	t.Helper()
	var st string
	if err := s.DB.QueryRow(`SELECT state FROM outbox_events WHERE id=?`, id).Scan(&st); err != nil {
		return "GONE"
	}
	return st
}

func okResult(e syncproto.Event) syncproto.Result {
	return syncproto.Result{ID: e.ID, Status: syncproto.StatusAccepted, PayloadSHA256: e.PayloadSHA256, Receipt: "r-" + e.ID}
}

func TestHappyPathThroughDeletion(t *testing.T) {
	ctx := context.Background()
	o, clk, s := setup(t, DefaultConfig)
	id, err := o.Enqueue(ctx, nil, "session.ended", map[string]any{"category": "pension"})
	if err != nil {
		t.Fatal(err)
	}
	batch, evs, err := o.Claim(ctx, 10)
	if err != nil || len(evs) != 1 || evs[0].ID != id {
		t.Fatalf("claim: %v %v", evs, err)
	}
	if state(t, s, id) != Uploading {
		t.Fatal("want UPLOADING")
	}
	out, err := o.ApplyAck(ctx, batch, map[string]syncproto.Result{id: okResult(evs[0])})
	if err != nil || out.Acknowledged != 1 {
		t.Fatalf("ack: %+v %v", out, err)
	}
	// Within retention nothing is deleted.
	if _, d, _ := o.Housekeep(ctx); d != 0 || state(t, s, id) != Acknowledged {
		t.Fatal("must keep acknowledged rows during retention")
	}
	clk.add(DefaultConfig.AckRetention + time.Second)
	p, d, err := o.Housekeep(ctx)
	if err != nil || p != 1 || d != 1 || state(t, s, id) != "GONE" {
		t.Fatalf("housekeep p=%d d=%d err=%v", p, d, err)
	}
}

func TestUntrustworthyAcksNeverAcknowledge(t *testing.T) {
	ctx := context.Background()
	o, _, s := setup(t, DefaultConfig)
	cases := map[string]func(e syncproto.Event) map[string]syncproto.Result{
		"missing result": func(e syncproto.Event) map[string]syncproto.Result { return map[string]syncproto.Result{} },
		"hash mismatch": func(e syncproto.Event) map[string]syncproto.Result {
			r := okResult(e)
			r.PayloadSHA256 = "deadbeef"
			return map[string]syncproto.Result{e.ID: r}
		},
		"no receipt": func(e syncproto.Event) map[string]syncproto.Result {
			r := okResult(e)
			r.Receipt = ""
			return map[string]syncproto.Result{e.ID: r}
		},
		"unknown status": func(e syncproto.Event) map[string]syncproto.Result {
			r := okResult(e)
			r.Status = "ok"
			return map[string]syncproto.Result{e.ID: r}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			id, _ := o.Enqueue(ctx, nil, "x", map[string]int{"a": 1})
			_, _ = s.DB.Exec(`UPDATE outbox_events SET next_attempt_at='2000-01-01T00:00:00Z' WHERE state='PENDING'`)
			batch, evs, _ := o.Claim(ctx, 200)
			var mine syncproto.Event
			for _, e := range evs {
				if e.ID == id {
					mine = e
				}
			}
			out, err := o.ApplyAck(ctx, batch, mk(mine))
			if err != nil {
				t.Fatal(err)
			}
			if out.Acknowledged != 0 || state(t, s, id) != Pending {
				t.Fatalf("%s: event must return to PENDING, got %s (%+v)", name, state(t, s, id), out)
			}
		})
	}
}

func TestConflictAndRejectAreKept(t *testing.T) {
	ctx := context.Background()
	o, clk, s := setup(t, DefaultConfig)
	a, _ := o.Enqueue(ctx, nil, "x", map[string]int{"a": 1})
	b, _ := o.Enqueue(ctx, nil, "x", map[string]int{"b": 1})
	batch, _, _ := o.Claim(ctx, 10)
	_, err := o.ApplyAck(ctx, batch, map[string]syncproto.Result{
		a: {ID: a, Status: syncproto.StatusConflict, Reason: "different hash"},
		b: {ID: b, Status: syncproto.StatusRejected, Reason: "schema"},
	})
	if err != nil {
		t.Fatal(err)
	}
	clk.add(365 * 24 * time.Hour)
	_, _, _ = o.Housekeep(ctx)
	if state(t, s, a) != Conflict || state(t, s, b) != Failed {
		t.Fatalf("got %s %s", state(t, s, a), state(t, s, b))
	}
	if n, _ := o.RequeueFailed(ctx); n != 2 {
		t.Fatalf("requeue %d", n)
	}
}

func TestRecoverInterruptedAfterPowerLoss(t *testing.T) {
	ctx := context.Background()
	o, _, s := setup(t, DefaultConfig)
	id, _ := o.Enqueue(ctx, nil, "x", map[string]int{"a": 1})
	_, _, _ = o.Claim(ctx, 10)
	// simulate crash: process restarts, nothing applied
	n, err := o.RecoverInterrupted(ctx)
	if err != nil || n != 1 || state(t, s, id) != Pending {
		t.Fatalf("recover n=%d err=%v state=%s", n, err, state(t, s, id))
	}
}

func TestBackoffAndGiveUpKeepsRow(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig
	cfg.MaxAttempts = 3
	o, clk, s := setup(t, cfg)
	if o.Backoff(1) != 30*time.Second || o.Backoff(2) != time.Minute || o.Backoff(40) != cfg.MaxBackoff {
		t.Fatalf("backoff %v %v %v", o.Backoff(1), o.Backoff(2), o.Backoff(40))
	}
	id, _ := o.Enqueue(ctx, nil, "x", map[string]int{"a": 1})
	for i := 0; i < 3; i++ {
		batch, evs, _ := o.Claim(ctx, 10)
		if len(evs) != 1 {
			t.Fatalf("attempt %d: nothing claimed (backoff not elapsed?)", i)
		}
		// Claimed again too early must return nothing.
		_ = o.RetryBatch(ctx, batch, "network down")
		if _, again, _ := o.Claim(ctx, 10); len(again) != 0 && i < 2 {
			t.Fatal("claimed during backoff")
		}
		clk.add(cfg.MaxBackoff)
	}
	if state(t, s, id) != Failed {
		t.Fatalf("want FAILED after max attempts, got %s", state(t, s, id))
	}
	_, d, _ := o.Housekeep(ctx)
	if d != 0 || state(t, s, id) != Failed {
		t.Fatal("FAILED rows must never be deleted")
	}
}

func TestFullOutboxRefusesNewButKeepsOld(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig
	cfg.MaxRows = 3
	o, _, _ := setup(t, cfg)
	for i := 0; i < 3; i++ {
		if _, err := o.Enqueue(ctx, nil, "x", map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := o.Enqueue(ctx, nil, "x", map[string]int{"i": 9}); err != ErrFull {
		t.Fatalf("want ErrFull, got %v", err)
	}
	st, _ := o.Stats(ctx)
	if st.Total != 3 {
		t.Fatalf("total %d", st.Total)
	}
}

func TestEnqueueValidation(t *testing.T) {
	ctx := context.Background()
	o, _, _ := setup(t, DefaultConfig)
	if _, err := o.Enqueue(ctx, nil, "x", []int{1}); err == nil {
		t.Fatal("non-object payload must be refused")
	}
	if _, err := o.Enqueue(ctx, nil, "bad|type", map[string]int{}); err == nil {
		t.Fatal("bad type must be refused")
	}
	big := map[string]string{"x": string(make([]byte, syncproto.MaxPayloadBytes))}
	if _, err := o.Enqueue(ctx, nil, "x", big); err == nil {
		t.Fatal("oversized payload must be refused")
	}
}

// TestRandomisedNoLossInvariant drives random operations (enqueue, claim,
// good/bad/partial acks, network failures, crashes, housekeeping, clock
// jumps) and asserts the core promise: an event disappears only if it was
// acknowledged with a durable, hash-matching result.
func TestRandomisedNoLossInvariant(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig
	cfg.AckRetention = time.Hour
	o, clk, s := setup(t, cfg)
	r := rand.New(rand.NewPCG(1, 2))
	durablyAcked := map[string]bool{}
	all := map[string]bool{}

	for step := 0; step < 3000; step++ {
		switch r.IntN(7) {
		case 0, 1:
			id, err := o.Enqueue(ctx, nil, "e", map[string]int{"s": step})
			if err != nil {
				t.Fatal(err)
			}
			all[id] = true
		case 2:
			batch, evs, err := o.Claim(ctx, 1+r.IntN(20))
			if err != nil {
				t.Fatal(err)
			}
			if len(evs) == 0 {
				continue
			}
			res := map[string]syncproto.Result{}
			for _, e := range evs {
				switch r.IntN(5) {
				case 0: // good
					res[e.ID] = okResult(e)
				case 1: // forged hash
					x := okResult(e)
					x.PayloadSHA256 = "00"
					res[e.ID] = x
				case 2: // duplicate (also good)
					x := okResult(e)
					x.Status = syncproto.StatusDuplicate
					res[e.ID] = x
				default: // missing
				}
			}
			switch r.IntN(3) {
			case 0:
				_ = o.RetryBatch(ctx, batch, "network")
			default:
				if _, err := o.ApplyAck(ctx, batch, res); err != nil {
					t.Fatal(err)
				}
				for _, e := range evs {
					if x, ok := res[e.ID]; ok && syncproto.IsDurablyStored(x, e.PayloadSHA256) {
						durablyAcked[e.ID] = true
					}
				}
			}
		case 3:
			_, _ = o.RecoverInterrupted(ctx)
		case 4:
			if _, _, err := o.Housekeep(ctx); err != nil {
				t.Fatal(err)
			}
		case 5:
			clk.add(time.Duration(r.IntN(120)) * time.Minute)
		case 6:
			// Attempt a raw unsafe delete as buggy code might; must not succeed for unacked rows.
			_, _ = s.DB.Exec(`DELETE FROM outbox_events WHERE state IN ('PENDING','UPLOADING','ACKNOWLEDGED','FAILED','CONFLICT')`)
		}
	}
	for id := range all {
		if state(t, s, id) == "GONE" && !durablyAcked[id] {
			t.Fatalf("event %s was deleted without a durable acknowledgement", id)
		}
	}
	st, _ := o.Stats(ctx)
	t.Logf("final stats: %+v (enqueued %d, durably acked %d)", st, len(all), len(durablyAcked))
	_ = fmt.Sprint()
}
