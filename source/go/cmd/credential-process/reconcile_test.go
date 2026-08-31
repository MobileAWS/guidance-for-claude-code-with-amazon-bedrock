package main

import "testing"

// Test the core promise: what the admin has enabled == what ends up in the user's config,
// while user-added MCPs are preserved and admin-disabled MCPs are removed.
func TestReconcileMcpServers_AdminConfigIsSourceOfTruth(t *testing.T) {
	// The user currently has: two Nexus-managed MCPs (github, slack) and one they added
	// themselves (my-personal-mcp). The admin has since disabled slack and added hubspot.
	existing := map[string]interface{}{
		"github":          map[string]interface{}{"command": "npx", "args": []interface{}{"-y", "@modelcontextprotocol/server-github"}},
		"slack":           map[string]interface{}{"command": "npx", "args": []interface{}{"-y", "@modelcontextprotocol/server-slack"}},
		"my-personal-mcp": map[string]interface{}{"command": "npx", "args": []interface{}{"-y", "some-personal-thing"}},
	}
	// Current admin-enabled set (from S3): github (kept) + hubspot (new). slack is gone.
	managed := map[string]interface{}{
		"github":  map[string]interface{}{"command": "npx", "args": []interface{}{"-y", "@modelcontextprotocol/server-github"}},
		"hubspot": map[string]interface{}{"command": "npx", "args": []interface{}{"-y", "@hubspot/mcp-server"}},
	}
	// prevManaged = what Nexus managed last sync (github, slack). newManaged = current (github, hubspot).
	prevManaged := map[string]bool{"github": true, "slack": true}
	newManaged := map[string]bool{"github": true, "hubspot": true}

	toRemove := computeMcpRemovals(prevManaged, newManaged)
	if !toRemove["slack"] {
		t.Fatalf("expected slack to be removed (admin disabled it), got %v", toRemove)
	}
	if toRemove["github"] || toRemove["hubspot"] || toRemove["my-personal-mcp"] {
		t.Fatalf("only slack should be removed, got %v", toRemove)
	}

	result := reconcileMcpServers(existing, managed, toRemove, false)

	// github: still present (admin keeps it)
	if _, ok := result["github"]; !ok {
		t.Error("github should remain (admin-enabled)")
	}
	// hubspot: now present (admin added it)
	if _, ok := result["hubspot"]; !ok {
		t.Error("hubspot should be added (admin-enabled)")
	}
	// slack: removed (admin disabled it)
	if _, ok := result["slack"]; ok {
		t.Error("slack should be removed (admin disabled it)")
	}
	// my-personal-mcp: preserved (user-added, never managed by Nexus)
	if _, ok := result["my-personal-mcp"]; !ok {
		t.Error("user-added MCP must be preserved")
	}
	if len(result) != 3 {
		t.Errorf("expected exactly {github, hubspot, my-personal-mcp}, got %d keys: %v", len(result), keysOf(result))
	}
}

// Injected env vars (e.g. per-user OAuth tokens) must survive re-sync.
func TestReconcileMcpServers_PreservesInjectedEnv(t *testing.T) {
	existing := map[string]interface{}{
		"hubspot": map[string]interface{}{
			"command": "npx",
			"args":    []interface{}{"-y", "@hubspot/mcp-server"},
			"env":     map[string]interface{}{"PRIVATE_APP_ACCESS_TOKEN": "user-token-123"},
		},
	}
	// The managed config from S3 has an empty env placeholder.
	managed := map[string]interface{}{
		"hubspot": map[string]interface{}{
			"command": "npx",
			"args":    []interface{}{"-y", "@hubspot/mcp-server"},
			"env":     map[string]interface{}{},
		},
	}
	result := reconcileMcpServers(existing, managed, map[string]bool{}, false)
	hs, _ := result["hubspot"].(map[string]interface{})
	env, _ := hs["env"].(map[string]interface{})
	if env["PRIVATE_APP_ACCESS_TOKEN"] != "user-token-123" {
		t.Errorf("injected token must be preserved across sync, got %v", env)
	}
}

// __http__ MCPs are skipped for settings.json (skipHTTP=true) but kept for .claude.json.
func TestReconcileMcpServers_SkipHTTP(t *testing.T) {
	managed := map[string]interface{}{
		"partner-central": map[string]interface{}{"command": "__http__", "args": "https://x/mcp"},
		"github":          map[string]interface{}{"command": "npx", "args": []interface{}{"-y", "gh"}},
	}
	withSkip := reconcileMcpServers(map[string]interface{}{}, managed, map[string]bool{}, true)
	if _, ok := withSkip["partner-central"]; ok {
		t.Error("__http__ MCP should be skipped when skipHTTP=true (settings.json)")
	}
	if _, ok := withSkip["github"]; !ok {
		t.Error("stdio MCP should be present")
	}
	withoutSkip := reconcileMcpServers(map[string]interface{}{}, managed, map[string]bool{}, false)
	if _, ok := withoutSkip["partner-central"]; !ok {
		t.Error("__http__ MCP should be kept when skipHTTP=false (.claude.json)")
	}
}

func keysOf(m map[string]interface{}) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
