//go:build linux

package resources

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// readSmall reads a small /proc or /sys file with a hard size cap.
func readSmall(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, limit)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, err
	}
	return buf[:n], nil
}

// kbField finds "Name:   12345 kB" in /proc files.
func kbField(b []byte, name string) uint64 {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name+":") {
			continue
		}
		f := strings.Fields(line[len(name)+1:])
		if len(f) == 0 {
			return 0
		}
		v, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return 0
		}
		return v * 1024
	}
	return 0
}

func procRSS(context.Context) (uint64, string) {
	b, err := readSmall("/proc/self/status", 8<<10)
	if err != nil {
		return 0, ""
	}
	return kbField(b, "VmRSS"), "/proc/self/status"
}

func sysMemory() (total, avail uint64) {
	b, err := readSmall("/proc/meminfo", 8<<10)
	if err != nil {
		return 0, 0
	}
	return kbField(b, "MemTotal"), kbField(b, "MemAvailable")
}

func cpuTemp() (float64, bool, error) {
	b, err := readSmall("/sys/class/thermal/thermal_zone0/temp", 64)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	milli, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false, errors.New("unparseable thermal_zone0/temp")
	}
	return float64(milli) / 1000, true, nil
}

func throttled(ctx context.Context) (Throttle, bool, error) {
	path, err := exec.LookPath("vcgencmd")
	if err != nil {
		return Throttle{}, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "get_throttled").Output()
	if err != nil {
		return Throttle{}, false, err
	}
	if len(out) > 256 {
		out = out[:256]
	}
	t, err := ParseThrottled(string(out))
	return t, err == nil, err
}
