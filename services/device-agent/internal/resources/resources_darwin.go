//go:build darwin

package resources

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// procRSS on macOS (development only) asks ps: reading another task's RSS
// without cgo is not possible, and this runs at most every few seconds.
func procRSS(ctx context.Context) (uint64, string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, ""
	}
	kb, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, ""
	}
	return kb * 1024, "ps"
}

func sysMemory() (total, avail uint64) { return 0, 0 }

func cpuTemp() (float64, bool, error) { return 0, false, nil }

func throttled(context.Context) (Throttle, bool, error) { return Throttle{}, false, nil }
