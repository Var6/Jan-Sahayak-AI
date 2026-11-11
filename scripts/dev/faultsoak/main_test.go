package main

import (
	"math"
	"testing"
	"time"
)

func TestParseFaults(t *testing.T) {
	got, err := parseFaults("central@18m, speech@6m,agent@1h")
	if err != nil {
		t.Fatal(err)
	}
	want := []plannedFault{{"speech", 6 * time.Minute}, {"central", 18 * time.Minute}, {"agent", time.Hour}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	for _, bad := range []string{"speech", "disk@5m", "llama@soon"} {
		if _, err := parseFaults(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if f, err := parseFaults(""); err != nil || len(f) != 0 {
		t.Fatalf("empty schedule: %v %v", f, err)
	}
}

func TestSlopePerHour(t *testing.T) {
	// y grows 2 units per minute → 120 per hour.
	var ts, ys []float64
	for i := 0; i < 30; i++ {
		ts = append(ts, float64(i*60))
		ys = append(ys, 10+2*float64(i))
	}
	if s := slopePerHour(ts, ys); math.Abs(s-120) > 1e-9 {
		t.Fatalf("slope %v, want 120", s)
	}
	if !math.IsNaN(slopePerHour([]float64{1, 2}, []float64{1, 2})) {
		t.Fatal("two points must give NaN")
	}
}

// Trends are computed per agent pid, skipping each segment's warm-up.
func TestTrendsSplitByPID(t *testing.T) {
	var s []sample
	for i := 0; i < 40; i++ { // pid 1: flat 20 MB after a warm-up spike
		rss := 20.0
		if i < 5 {
			rss = 40
		}
		s = append(s, sample{t: float64(i * 10), pid: 1, rss: rss, gor: 15, fds: 20})
	}
	for i := 40; i < 80; i++ { // pid 2: leaking 1 MB per sample (360 MB/h)
		s = append(s, sample{t: float64(i * 10), pid: 2, rss: float64(i), gor: 15, fds: 20})
	}
	tr := trends(s, 50*time.Second)
	if len(tr) != 2 {
		t.Fatalf("got %d segments", len(tr))
	}
	if tr[0].PID != 1 || tr[0].RSSSlope != 0 || tr[0].RSSMax != 20 {
		t.Fatalf("pid 1 trend %+v", tr[0])
	}
	if tr[1].PID != 2 || math.Abs(tr[1].RSSSlope-360) > 0.01 {
		t.Fatalf("pid 2 trend %+v", tr[1])
	}
}
