package sdnotify

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestDisabledIsNoop(t *testing.T) {
	n := FromEnv(func(string) string { return "" })
	if n.Enabled() {
		t.Fatal("enabled without NOTIFY_SOCKET")
	}
	if err := n.Ready(); err != nil {
		t.Fatal(err)
	}
}

func TestSendToSocket(t *testing.T) {
	// Unix socket paths are limited to ~104 bytes on macOS; t.TempDir can
	// be longer, so use a short dir under os.TempDir.
	dir, err := os.MkdirTemp("", "sdn")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "n.sock")
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	n := FromEnv(func(k string) string {
		if k == "NOTIFY_SOCKET" {
			return path
		}
		return ""
	})
	for _, f := range []func() error{n.Ready, n.Watchdog, n.Stopping} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"READY=1\nSTATUS=serving", "WATCHDOG=1", "STOPPING=1\nSTATUS=stopping"}
	buf := make([]byte, 512)
	for _, w := range want {
		_ = l.SetReadDeadline(time.Now().Add(2 * time.Second))
		k, _, err := l.ReadFromUnix(buf)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(buf[:k]); got != w {
			t.Fatalf("got %q want %q", got, w)
		}
	}
}

func TestWatchdogInterval(t *testing.T) {
	g := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if d := WatchdogInterval(g(map[string]string{"WATCHDOG_USEC": "30000000"})); d != 15*time.Second {
		t.Fatalf("got %s", d)
	}
	if d := WatchdogInterval(g(map[string]string{"WATCHDOG_USEC": "30000000", "WATCHDOG_PID": "1"})); d != 0 && os.Getpid() != 1 {
		t.Fatalf("other pid: got %s", d)
	}
	if d := WatchdogInterval(g(map[string]string{"WATCHDOG_USEC": "30000000", "WATCHDOG_PID": strconv.Itoa(os.Getpid())})); d != 15*time.Second {
		t.Fatalf("own pid: got %s", d)
	}
	if d := WatchdogInterval(g(nil)); d != 0 {
		t.Fatal("unset")
	}
}
