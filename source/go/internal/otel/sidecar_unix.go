//go:build !windows

package otel

import (
	"os/exec"
	"syscall"
)

// processAlive reports whether a process with the given PID is running.
// Signal 0 performs existence/permission checking without delivering a signal.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// detach puts the collector in its own session so it survives the helper's exit
// (equivalent to the Python start_new_session=True).
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
