//go:build windows

package otel

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// processAlive reports whether a process with the given PID is running.
// os.FindProcess always succeeds on Windows, so query tasklist instead
// (the same approach the Python helper uses).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), strconv.Itoa(pid))
}

// detach starts the collector in a new process group so it survives the
// helper's exit (equivalent to the Python CREATE_NEW_PROCESS_GROUP).
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
