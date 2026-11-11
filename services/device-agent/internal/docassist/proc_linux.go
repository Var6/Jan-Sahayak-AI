//go:build linux

package docassist

import (
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

// limitMemory sets RLIMIT_AS of process pid with prlimit(2). Lowering the
// limit of a process with the same credentials needs no privileges; the
// agent's seccomp filter (@system-service) allows prlimit64.
func limitMemory(pid int, bytes uint64) error {
	lim := syscall.Rlimit{Cur: bytes, Max: bytes}
	_, _, e := syscall.RawSyscall6(syscall.SYS_PRLIMIT64, uintptr(pid), uintptr(syscall.RLIMIT_AS),
		uintptr(unsafe.Pointer(&lim)), 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
