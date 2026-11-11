//go:build unix && !linux

package docassist

import (
	"os/exec"
	"syscall"
)

// setProcAttrs runs tesseract in its own process group and kills the whole
// group on timeout. (No parent-death signal outside Linux: dev machines.)
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return cmd.Process.Kill()
	}
}

// limitChild is a no-op outside Linux: macOS does not enforce RLIMIT_AS
// and has no prlimit; the child inherits the agent's RLIMIT_CORE=0 (set at
// startup). The kiosk (Linux) applies every limit per process.
func limitChild(int, uint64) error { return nil }
