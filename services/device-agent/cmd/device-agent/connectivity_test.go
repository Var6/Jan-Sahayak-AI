package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/config"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/store"
)

func TestBumpStartCount(t *testing.T) {
	ctx := context.Background()
	st, _, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "device.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for want := int64(1); want <= 3; want++ {
		n, err := bumpStartCount(ctx, st.DB)
		if err != nil || n != want {
			t.Fatalf("start %d: got %d %v", want, n, err)
		}
	}
}

func TestKnowledgeFreshness(t *testing.T) {
	a := &agent{cfg: config.Defaults()} // stale_after 30 days, no sync
	today := time.Now().In(time.FixedZone("IST", 5*3600+1800)).Format("2006-01-02")
	old := time.Now().Add(-40 * 24 * time.Hour).In(time.FixedZone("IST", 5*3600+1800)).Format("2006-01-02")

	r := health.Result{State: health.Healthy}
	a.knowledgeFreshness(today+".1", &r)
	if r.State != health.Healthy || r.Data["bundle_age_days"] != 0 {
		t.Fatalf("fresh: %+v", r)
	}
	r = health.Result{State: health.Healthy}
	a.knowledgeFreshness(old+".3", &r)
	if r.State != health.Degraded || r.Detail != "bundle_stale" {
		t.Fatalf("stale: %+v", r)
	}
	// A worse state set by the record checks is kept.
	r = health.Result{State: health.Degraded, Detail: "no_records"}
	a.knowledgeFreshness(old+".3", &r)
	if r.Detail != "no_records" {
		t.Fatalf("kept: %+v", r)
	}
	// Local/dev versions have no date and are never stale.
	r = health.Result{State: health.Healthy}
	a.knowledgeFreshness("local-0123456789ab", &r)
	if r.State != health.Healthy {
		t.Fatalf("local: %+v", r)
	}
	if _, ok := r.Data["bundle_age_days"]; ok {
		t.Fatal("local version must not report an age")
	}
}
