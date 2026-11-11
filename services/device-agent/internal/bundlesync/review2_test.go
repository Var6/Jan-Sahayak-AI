//go:build review2

package bundlesync

// Independent review (Phase 10–12), repro tests. Run with:
//
//	go test -tags review2 -run Review2 ./services/device-agent/internal/bundlesync/

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// R2-1: after a restart the loaded bundle is never recorded as
// LastLoadedVersion (RestoreFromDisk returns early when the cached version
// equals the DB version, and CheckOnce's 204/up-to-date path does not set
// it). Every heartbeat that reports that same latest version then kicks a
// bundle check, at most every 10 minutes, forever: a signed GET every
// 10 min instead of every 6 h on a metered link.
func TestReview2NotifyLatestKicksForAlreadyLoadedVersionAfterRestart(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	v1 := e.signed("2026-11-10.1", fixtureRecords(t), e.srvKey)
	e.central.publish(v1, "2026-11-10.1")
	if _, err := e.w.CheckOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// "Restart": a new worker on the same database and cache file.
	w2 := New(e.w.cfg, e.ks, e.id, nil)
	now := time.Now() // real time: requests are signed with w.now
	w2.now = func() time.Time { return now }
	if _, loaded, err := w2.RestoreFromDisk(ctx); err != nil || loaded {
		t.Fatalf("restore: loaded=%v err=%v", loaded, err)
	}
	if res, err := w2.CheckOnce(ctx); err != nil || res.Outcome != ResultUpToDate {
		t.Fatalf("check: %+v %v", res, err)
	}
	kicks := 0
	for i := 0; i < 6; i++ { // one hour of heartbeats reporting the loaded version
		w2.NotifyLatest("2026-11-10.1")
		select {
		case <-w2.kick:
			kicks++
		default:
		}
		now = now.Add(11 * time.Minute)
	}
	if kicks != 0 {
		t.Fatalf("heartbeats announcing the already-loaded version kicked %d bundle checks in ~1 h (LastLoadedVersion=%q); want 0",
			kicks, w2.Status().LastLoadedVersion)
	}
}

// R2-2: Busy is checked only before the download. A citizen session that
// starts while the bundle downloads still gets its facts replaced
// mid-conversation, contrary to the package rule "the facts never change
// in the middle of a conversation".
func TestReview2BundleLoadedAfterSessionStartedDuringDownload(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.central.publish(e.signed("2026-11-10.1", fixtureRecords(t), e.srvKey), "2026-11-10.1")
	// A proxy that flips "session open" while the request is in flight.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.busy.Store(true) // the citizen pressed the button during the download
		r.URL.Scheme, r.URL.Host = "http", e.srv.Listener.Addr().String()
		e.central.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	e.w.cfg.BaseURL = proxy.URL

	_, err := e.w.CheckOnce(ctx)
	v, _ := e.ks.BundleVersion(ctx)
	if v == "2026-11-10.1" {
		t.Fatalf("bundle loaded although a session opened during the download (err=%v); want postponed (ErrBusy)", err)
	}
}
