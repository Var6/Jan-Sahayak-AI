// Package sdnotify implements the small part of the systemd notification
// protocol the agent needs (READY=1, WATCHDOG=1, STOPPING=1, STATUS=…)
// without a dependency: one datagram per message to $NOTIFY_SOCKET.
//
// When the agent is not started by systemd (NOTIFY_SOCKET unset, e.g. on a
// developer Mac) every call is a cheap no-op.
package sdnotify

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Notifier sends notifications to one socket.
type Notifier struct {
	addr *net.UnixAddr // nil = disabled
}

// FromEnv builds a Notifier from NOTIFY_SOCKET. An abstract socket is given
// with a leading '@'.
func FromEnv(getenv func(string) string) *Notifier {
	if getenv == nil {
		getenv = os.Getenv
	}
	s := getenv("NOTIFY_SOCKET")
	if s == "" {
		return &Notifier{}
	}
	if s[0] == '@' {
		s = "\x00" + s[1:]
	} else if s[0] != '/' {
		return &Notifier{} // unsupported (e.g. vsock); stay silent
	}
	return &Notifier{addr: &net.UnixAddr{Name: s, Net: "unixgram"}}
}

// Enabled reports whether notifications go anywhere.
func (n *Notifier) Enabled() bool { return n != nil && n.addr != nil }

// Send writes one notification (lines joined by '\n').
func (n *Notifier) Send(lines ...string) error {
	if !n.Enabled() {
		return nil
	}
	msg := strings.Join(lines, "\n")
	if len(msg) > 4096 {
		return errors.New("sdnotify: message too long")
	}
	c, err := net.DialUnix("unixgram", nil, n.addr)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetWriteDeadline(time.Now().Add(time.Second))
	_, err = c.Write([]byte(msg))
	return err
}

// Ready tells systemd start-up finished.
func (n *Notifier) Ready() error { return n.Send("READY=1", "STATUS=serving") }

// Watchdog pings the systemd watchdog.
func (n *Notifier) Watchdog() error { return n.Send("WATCHDOG=1") }

// Stopping tells systemd shutdown began.
func (n *Notifier) Stopping() error { return n.Send("STOPPING=1", "STATUS=stopping") }

// Status sets the free-form unit status line (no citizen data, ever).
func (n *Notifier) Status(s string) error {
	return n.Send("STATUS=" + strings.ReplaceAll(s, "\n", " "))
}

// WatchdogInterval returns how often to ping: half of WATCHDOG_USEC, or 0
// when the watchdog is not enabled for this process (WATCHDOG_PID, when
// set, must be our pid).
func WatchdogInterval(getenv func(string) string) time.Duration {
	if getenv == nil {
		getenv = os.Getenv
	}
	us, err := strconv.ParseInt(getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || us <= 0 {
		return 0
	}
	if p := getenv("WATCHDOG_PID"); p != "" && p != strconv.Itoa(os.Getpid()) {
		return 0
	}
	return time.Duration(us) * time.Microsecond / 2
}
