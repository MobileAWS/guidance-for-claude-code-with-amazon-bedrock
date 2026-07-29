package otel

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// EnsureCollectorRunning starts the OTel collector sidecar if it is installed
// and not already running. It is a no-op when the collector binary or config is
// absent (i.e. monitoring not deployed). The collector is launched under the
// "<profile>-collector" AWS profile so its Go SDK resolves credentials via
// credential_process (which can auto-refresh) rather than static credentials.
//
// This is the Go port of the fork's Python otel_helper.ensure_collector_running.
// logf, if non-nil, receives debug messages.
func EnsureCollectorRunning(profile string, logf func(string, ...interface{})) {
	debug := func(format string, args ...interface{}) {
		if logf != nil {
			logf(format, args...)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	installDir := filepath.Join(home, "claude-code-with-bedrock")
	binName := "otelcol"
	if runtime.GOOS == "windows" {
		binName = "otelcol.exe"
	}
	collectorBinary := filepath.Join(installDir, binName)
	collectorConfig := filepath.Join(installDir, "collector-config.yaml")
	pidFile := filepath.Join(installDir, "collector.pid")

	if !fileExists(collectorBinary) || !fileExists(collectorConfig) {
		return // collector not installed — nothing to start
	}

	// Already running? (stale PID files fall through to a relaunch.)
	if data, readErr := os.ReadFile(pidFile); readErr == nil {
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && processAlive(pid) {
			return
		}
	}

	if profile == "" {
		profile = "ClaudeCode"
	}
	sessionDir := filepath.Join(home, ".claude-code-session")
	_ = os.MkdirAll(sessionDir, 0o755)
	logFile := filepath.Join(sessionDir, "collector.log")
	lf, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		debug("Failed to open collector log: %v", err)
		return
	}
	defer lf.Close()

	cmd := exec.Command(collectorBinary, "--config", collectorConfig) // #nosec G204 -- fixed install path
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.Env = append(os.Environ(), "AWS_PROFILE="+profile+"-collector")
	detach(cmd) // platform-specific: new session (unix) / new process group (windows)

	if err := cmd.Start(); err != nil {
		debug("Failed to start collector sidecar: %v", err)
		return
	}
	if writeErr := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); writeErr != nil {
		debug("Failed to write collector PID file: %v", writeErr)
	}
	debug("Started collector sidecar (PID %d)", cmd.Process.Pid)
	// Release so the detached collector keeps running after this helper exits.
	_ = cmd.Process.Release()
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
