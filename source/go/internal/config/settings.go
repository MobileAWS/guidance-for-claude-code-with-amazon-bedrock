package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// claudeSettingsPath returns the path to Claude Code's settings.json
// (~/.claude/settings.json) — distinct from ccwb's own config.json.
func claudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// EnforceEnabledModels rewrites ~/.claude/settings.json so that ANTHROPIC_MODEL
// is one of the admin-enabled models. If the current model is set and not in the
// enabled list, it is switched to enabledModels[0]. It is a no-op (returns nil)
// when enabledModels is empty, the settings file is absent, there is no env
// block, ANTHROPIC_MODEL is unset, or the current model is already allowed.
//
// This is the Go port of the Python credential_provider._enforce_enabled_models.
// Other settings keys are preserved (their values pass through unchanged);
// JSON key ordering is not preserved (Go re-serializes map keys sorted), which
// is cosmetic for a settings file.
func EnforceEnabledModels(enabledModels []string) error {
	if len(enabledModels) == 0 {
		return nil
	}
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no settings file — nothing to enforce
		}
		return err
	}

	// Preserve unrelated top-level keys as raw JSON; only the env block is rewritten.
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return err
	}
	raw, ok := settings["env"]
	if !ok {
		return nil // no env block — nothing to enforce
	}
	var env map[string]interface{}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}

	current, _ := env["ANTHROPIC_MODEL"].(string)
	if current == "" || containsString(enabledModels, current) {
		return nil // unset or already allowed
	}

	env["ANTHROPIC_MODEL"] = enabledModels[0]
	envRaw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	settings["env"] = envRaw

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
