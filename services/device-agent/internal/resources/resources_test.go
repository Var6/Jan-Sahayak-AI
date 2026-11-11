package resources

import (
	"context"
	"testing"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
)

var th = Thresholds{MemWarnMB: 4000, MemCritMB: 8000, SysMemWarnPct: 1, SysMemCritPct: 0.5,
	DiskWarnMB: 2, DiskCritMB: 1, TempWarnC: 75, TempCritC: 82, GoroutineWarn: 5000, GoroutineCrit: 10000}

func TestParseThrottled(t *testing.T) {
	x, err := ParseThrottled("throttled=0x50005\n")
	if err != nil {
		t.Fatal(err)
	}
	if !x.UnderVoltageNow || !x.ThrottledNow || x.FreqCappedNow || !x.UnderVoltageSince || !x.ThrottledSince {
		t.Fatalf("%+v", x)
	}
	if x, _ := ParseThrottled("throttled=0x0"); x.Raw != 0 || x.ThrottledNow {
		t.Fatal("zero")
	}
	for _, bad := range []string{"", "garbage", "throttled=zz"} {
		if _, err := ParseThrottled(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestChecksOnThisMachine(t *testing.T) {
	ctx := context.Background()
	m := ReadMemory(ctx)
	if m.GoTotal == 0 || m.GoHeap == 0 {
		t.Fatalf("go metrics empty: %+v", m)
	}
	t.Logf("memory sample: %+v", m)
	if r := MemoryCheck(th)(ctx); r.State != health.Healthy {
		t.Fatalf("memory: %+v", r)
	}
	if r := GoroutineCheck(th)(ctx); r.State != health.Healthy {
		t.Fatalf("goroutines: %+v", r)
	}
	if r := DiskCheck(t.TempDir(), th)(ctx); r.State != health.Healthy || r.Data["free_mb"] == nil {
		t.Fatalf("disk: %+v", r)
	}
	if r := TemperatureCheck(th)(ctx); r.State == health.Unavailable {
		t.Fatalf("temperature: %+v", r)
	}
}

func TestThresholdsTrip(t *testing.T) {
	ctx := context.Background()
	low := th
	low.GoroutineWarn, low.GoroutineCrit = 1, 2
	if r := GoroutineCheck(low)(ctx); r.State != health.Unavailable {
		t.Fatalf("goroutines: %+v", r)
	}
	low.MemWarnMB, low.MemCritMB = 0, 1<<30
	if r := MemoryCheck(low)(ctx); r.State != health.Degraded {
		t.Fatalf("memory warn: %+v", r)
	}
	big := th
	big.DiskWarnMB, big.DiskCritMB = 1<<40, 1<<39
	if r := DiskCheck(t.TempDir(), big)(ctx); r.State != health.Unavailable {
		t.Fatalf("disk: %+v", r)
	}
	if r := DiskCheck("/definitely/not/here", th)(ctx); r.State != health.Unavailable {
		t.Fatalf("missing path: %+v", r)
	}
}
