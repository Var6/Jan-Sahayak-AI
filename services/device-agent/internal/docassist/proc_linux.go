//go:build linux

package docassist

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// setProcAttrs runs tesseract in its own process group, kills the whole
// group on timeout, and kills it if the agent dies (no orphaned OCR
// process holding a citizen's image).
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return killGroup(cmd) }
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return cmd.Process.Kill()
}

// limitChild prepares process pid before it receives the photo:
//   - RLIMIT_CORE = 0 always: a crash (e.g. on a crafted JPEG) never
//     writes the photo's memory to a core file;
//   - RLIMIT_AS = memBytes when memBytes > 0;
//   - oom_score_adj = 1000 (best effort): inside the agent's cgroup the
//     OOM killer picks tesseract, not the agent.
//
// prlimit(2) on a process with the same credentials needs no privileges;
// the agent's seccomp filter (@system-service) allows prlimit64. Raising
// one's own child's oom_score_adj needs no privileges either.
func limitChild(pid int, memBytes uint64) error {
	if err := prlimit(pid, syscall.RLIMIT_CORE, 0); err != nil {
		return fmt.Errorf("core limit: %w", err)
	}
	if memBytes > 0 {
		if err := prlimit(pid, syscall.RLIMIT_AS, memBytes); err != nil {
			return fmt.Errorf("memory limit: %w", err)
		}
	}
	_ = os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), []byte("1000"), 0)
	return nil
}

func prlimit(pid, resource int, v uint64) error {
	lim := syscall.Rlimit{Cur: v, Max: v}
	_, _, e := syscall.RawSyscall6(syscall.SYS_PRLIMIT64, uintptr(pid), uintptr(resource),
		uintptr(unsafe.Pointer(&lim)), 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
