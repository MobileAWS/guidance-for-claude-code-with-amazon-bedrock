package otel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCollectorRunningNoOpWhenNotInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// No otelcol binary / collector-config.yaml present.
	EnsureCollectorRunning("ClaudeCode", nil)

	// It must not create a PID file when the collector isn't installed.
	if _, err := os.Stat(filepath.Join(home, "claude-code-with-bedrock", "collector.pid")); !os.IsNotExist(err) {
		t.Errorf("expected no collector.pid when collector is not installed")
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Errorf("processAlive(self) = false, want true")
	}
	if processAlive(-1) {
		t.Errorf("processAlive(-1) = true, want false")
	}
	if processAlive(0) {
		t.Errorf("processAlive(0) = true, want false")
	}
}
