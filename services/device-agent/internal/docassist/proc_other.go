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

// limitMemory is a no-op outside Linux: macOS does not enforce RLIMIT_AS.
// The kiosk (Linux) applies the cap.
func limitMemory(int, uint64) error { return nil }
