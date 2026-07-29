package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeSettings creates ~/.claude/settings.json under a temp HOME and returns
// the settings path. Pass nil env to write a settings file with no env block.
func writeSettings(t *testing.T, env map[string]string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	settings := map[string]interface{}{"otherKey": "preserved"}
	if env != nil {
		envIface := map[string]interface{}{}
		for k, v := range env {
			envIface[k] = v
		}
		settings["env"] = envIface
	}
	data, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readModel(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]json.RawMessage
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	// Confirm unrelated keys survive.
	if _, ok := s["otherKey"]; !ok {
		t.Errorf("unrelated key 'otherKey' was dropped")
	}
	var env map[string]string
	if raw, ok := s["env"]; ok {
		_ = json.Unmarshal(raw, &env)
	}
	return env["ANTHROPIC_MODEL"]
}

func TestEnforceEnabledModels(t *testing.T) {
	t.Run("switches disallowed model to first enabled", func(t *testing.T) {
		path := writeSettings(t, map[string]string{"ANTHROPIC_MODEL": "blocked-model"})
		if err := EnforceEnabledModels([]string{"allowed-a", "allowed-b"}); err != nil {
			t.Fatal(err)
		}
		if got := readModel(t, path); got != "allowed-a" {
			t.Errorf("ANTHROPIC_MODEL = %q, want allowed-a", got)
		}
	})

	t.Run("leaves allowed model unchanged", func(t *testing.T) {
		path := writeSettings(t, map[string]string{"ANTHROPIC_MODEL": "allowed-b"})
		if err := EnforceEnabledModels([]string{"allowed-a", "allowed-b"}); err != nil {
			t.Fatal(err)
		}
		if got := readModel(t, path); got != "allowed-b" {
			t.Errorf("ANTHROPIC_MODEL = %q, want allowed-b (unchanged)", got)
		}
	})

	t.Run("empty enabled list is a no-op", func(t *testing.T) {
		path := writeSettings(t, map[string]string{"ANTHROPIC_MODEL": "anything"})
		if err := EnforceEnabledModels(nil); err != nil {
			t.Fatal(err)
		}
		if got := readModel(t, path); got != "anything" {
			t.Errorf("ANTHROPIC_MODEL = %q, want unchanged", got)
		}
	})

	t.Run("unset model is a no-op", func(t *testing.T) {
		path := writeSettings(t, map[string]string{"OTHER": "x"})
		if err := EnforceEnabledModels([]string{"allowed-a"}); err != nil {
			t.Fatal(err)
		}
		if got := readModel(t, path); got != "" {
			t.Errorf("ANTHROPIC_MODEL = %q, want empty (untouched)", got)
		}
	})

	t.Run("no env block is a no-op", func(t *testing.T) {
		writeSettings(t, nil)
		if err := EnforceEnabledModels([]string{"allowed-a"}); err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
	})

	t.Run("missing settings file is a no-op", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home) // no ~/.claude/settings.json created
		if err := EnforceEnabledModels([]string{"allowed-a"}); err != nil {
			t.Fatalf("expected nil error for missing file, got %v", err)
		}
	})
}
