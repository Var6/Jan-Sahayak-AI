// Package resources samples the device's resource usage — process and
// system memory, goroutines, free disk, SoC temperature and Raspberry Pi
// throttling flags — and turns the samples into health checks with
// configurable thresholds.
//
// Health details are fixed reason codes (disk_low, temperature_high …):
// they may be sent to the central server, so measured values go in Data
// and never into free text.
//
// Platform support (never faked: an unsupported reading is reported as
// "not available on this platform", not as a number):
//
//	                 linux (Pi)                         darwin (dev Mac)
//	process RSS      /proc/self/status VmRSS            `ps -o rss=` (dev only)
//	system memory    /proc/meminfo MemAvailable         not available
//	disk             statfs                             statfs
//	temperature      /sys/class/thermal/thermal_zone0   not available
//	throttling       `vcgencmd get_throttled` if found  not available
package resources

import (
	"context"
	"fmt"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"

	"github.com/janmanindia/jan-sahayak/services/device-agent/internal/health"
)

// Thresholds for the health checks.
type Thresholds struct {
	MemWarnMB, MemCritMB         int
	SysMemWarnPct, SysMemCritPct float64
	DiskWarnMB, DiskCritMB       int
	TempWarnC, TempCritC         float64
	GoroutineWarn, GoroutineCrit int
}

// Memory is a memory sample. Zero means "unknown".
type Memory struct {
	ProcRSS   uint64 `json:"proc_rss_bytes,omitempty"`
	GoHeap    uint64 `json:"go_heap_bytes"`
	GoTotal   uint64 `json:"go_total_bytes"` // all memory mapped by the Go runtime
	SysTotal  uint64 `json:"sys_total_bytes,omitempty"`
	SysAvail  uint64 `json:"sys_available_bytes,omitempty"`
	RSSSource string `json:"rss_source,omitempty"`
}

var goMetrics = []metrics.Sample{
	{Name: "/memory/classes/heap/objects:bytes"},
	{Name: "/memory/classes/total:bytes"},
}

// ReadMemory samples process and system memory.
func ReadMemory(ctx context.Context) Memory {
	s := make([]metrics.Sample, len(goMetrics))
	copy(s, goMetrics)
	metrics.Read(s)
	var m Memory
	if s[0].Value.Kind() == metrics.KindUint64 {
		m.GoHeap = s[0].Value.Uint64()
	}
	if s[1].Value.Kind() == metrics.KindUint64 {
		m.GoTotal = s[1].Value.Uint64()
	}
	m.ProcRSS, m.RSSSource = procRSS(ctx)
	m.SysTotal, m.SysAvail = sysMemory()
	return m
}

// Disk is a filesystem sample.
type Disk struct {
	Path  string `json:"path"`
	Total uint64 `json:"total_bytes"`
	Free  uint64 `json:"free_bytes"` // available to unprivileged users
}

// ReadDisk samples the filesystem holding path.
func ReadDisk(path string) (Disk, error) {
	total, free, err := statfs(path)
	return Disk{Path: path, Total: total, Free: free}, err
}

// ReadTemperature returns the SoC temperature in °C; ok=false when the
// platform has no readable sensor.
func ReadTemperature() (c float64, ok bool, err error) { return cpuTemp() }

// Throttle is the decoded `vcgencmd get_throttled` bit field.
type Throttle struct {
	Raw               uint64 `json:"raw"`
	UnderVoltageNow   bool   `json:"under_voltage_now"`
	FreqCappedNow     bool   `json:"freq_capped_now"`
	ThrottledNow      bool   `json:"throttled_now"`
	SoftTempLimitNow  bool   `json:"soft_temp_limit_now"`
	UnderVoltageSince bool   `json:"under_voltage_since_boot"`
	ThrottledSince    bool   `json:"throttled_since_boot"`
}

// ParseThrottled parses "throttled=0x50005".
func ParseThrottled(out string) (Throttle, error) {
	s := strings.TrimSpace(out)
	_, v, ok := strings.Cut(s, "=")
	if !ok {
		return Throttle{}, fmt.Errorf("resources: unexpected vcgencmd output %q", s)
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(v), "0x"), 16, 64)
	if err != nil {
		return Throttle{}, fmt.Errorf("resources: bad throttled value %q", v)
	}
	return Throttle{
		Raw: n, UnderVoltageNow: n&0x1 != 0, FreqCappedNow: n&0x2 != 0, ThrottledNow: n&0x4 != 0,
		SoftTempLimitNow: n&0x8 != 0, UnderVoltageSince: n&0x10000 != 0, ThrottledSince: n&0x40000 != 0,
	}, nil
}

// ReadThrottle runs vcgencmd if present; ok=false when it is not.
func ReadThrottle(ctx context.Context) (Throttle, bool, error) { return throttled(ctx) }

const mb = 1 << 20

// MemoryCheck reports process RSS (or Go-mapped memory when RSS is unknown)
// and system available memory against the thresholds.
func MemoryCheck(t Thresholds) health.CheckFunc {
	return func(ctx context.Context) health.Result {
		m := ReadMemory(ctx)
		data := map[string]any{"go_heap_mb": m.GoHeap / mb, "go_total_mb": m.GoTotal / mb}
		used, src := m.ProcRSS, "rss"
		if used == 0 {
			used, src = m.GoTotal, "go_runtime"
		}
		data["process_mb"] = used / mb
		data["process_source"] = src
		st, detail := health.Healthy, ""
		switch {
		case used >= uint64(t.MemCritMB)*mb:
			st, detail = health.Unavailable, "process_memory_critical"
		case used >= uint64(t.MemWarnMB)*mb:
			st, detail = health.Degraded, "process_memory_high"
		}
		if m.SysTotal > 0 {
			pct := float64(m.SysAvail) * 100 / float64(m.SysTotal)
			data["sys_total_mb"] = m.SysTotal / mb
			data["sys_available_mb"] = m.SysAvail / mb
			data["sys_available_pct"] = int(pct)
			switch {
			case pct < t.SysMemCritPct:
				st, detail = worse(st, health.Unavailable), joinDetail(detail, "system_memory_critical")
			case pct < t.SysMemWarnPct:
				st, detail = worse(st, health.Degraded), joinDetail(detail, "system_memory_low")
			}
		} else {
			data["sys_memory"] = "not available on this platform"
		}
		return health.Result{State: st, Detail: detail, Data: data}
	}
}

// GoroutineCheck reports the goroutine count (leak detector).
func GoroutineCheck(t Thresholds) health.CheckFunc {
	return func(context.Context) health.Result {
		n := runtime.NumGoroutine()
		r := health.Result{State: health.Healthy, Data: map[string]any{"goroutines": n}}
		switch {
		case n >= t.GoroutineCrit:
			r.State, r.Detail = health.Unavailable, "goroutines_critical"
		case n >= t.GoroutineWarn:
			r.State, r.Detail = health.Degraded, "goroutines_high"
		}
		return r
	}
}

// DiskCheck reports free space on the filesystem holding path.
func DiskCheck(path string, t Thresholds) health.CheckFunc {
	return func(context.Context) health.Result {
		d, err := ReadDisk(path)
		if err != nil {
			return health.Result{State: health.Unavailable, Detail: "disk_unreadable"}
		}
		r := health.Result{State: health.Healthy, Data: map[string]any{"free_mb": d.Free / mb, "total_mb": d.Total / mb}}
		switch {
		case d.Free < uint64(t.DiskCritMB)*mb:
			r.State, r.Detail = health.Unavailable, "disk_critical"
		case d.Free < uint64(t.DiskWarnMB)*mb:
			r.State, r.Detail = health.Degraded, "disk_low"
		}
		return r
	}
}

// TemperatureCheck reports SoC temperature and Pi throttling flags.
func TemperatureCheck(t Thresholds) health.CheckFunc {
	return func(ctx context.Context) health.Result {
		r := health.Result{State: health.Healthy, Data: map[string]any{}}
		c, ok, err := ReadTemperature()
		switch {
		case err != nil:
			r.State, r.Detail = health.Degraded, "temperature_unreadable"
		case !ok:
			r.Data["temperature"] = "not available on this platform"
		default:
			r.Data["temp_c"] = c
			switch {
			case c >= t.TempCritC:
				r.State, r.Detail = health.Unavailable, "temperature_critical"
			case c >= t.TempWarnC:
				r.State, r.Detail = health.Degraded, "temperature_high"
			}
		}
		th, ok, err := ReadThrottle(ctx)
		switch {
		case err != nil:
			r.Data["throttle_error"] = err.Error()
		case ok:
			r.Data["throttle"] = th
			if th.UnderVoltageNow || th.ThrottledNow || th.FreqCappedNow {
				r.State = worse(r.State, health.Degraded)
				r.Detail = joinDetail(r.Detail, "throttled")
			}
		}
		return r
	}
}

func worse(a, b health.State) health.State {
	rank := map[health.State]int{health.Healthy: 0, health.Recovering: 1, health.Degraded: 2, health.Unavailable: 3, health.RequiresAttention: 4}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func joinDetail(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
