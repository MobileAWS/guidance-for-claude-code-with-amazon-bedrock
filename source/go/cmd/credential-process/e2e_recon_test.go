package main

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// End-to-end: use the REAL S3 managed set + a simulated employee config with drift,
// run the actual reconciliation, and assert the result == exactly the admin-enabled set
// plus the user's own MCP.
func TestReconcile_E2E_RealS3(t *testing.T) {
	s3data, err := os.ReadFile("/tmp/real-s3-mcps.json")
	if err != nil {
		t.Skip("no /tmp/real-s3-mcps.json (run the aws s3 cp first)")
	}
	var managed map[string]interface{}
	if err := json.Unmarshal(s3data, &managed); err != nil {
		t.Fatalf("bad S3 json: %v", err)
	}

	// Employee's stale config
	existing := map[string]interface{}{
		"github":          map[string]interface{}{"command": "npx"},
		"activecampaign":  map[string]interface{}{"command": "npx"}, // stale (admin disabled)
		"atlassian":       map[string]interface{}{"command": "npx"}, // stale (admin disabled)
		"my-personal-mcp": map[string]interface{}{"command": "npx"}, // user-added
	}

	// First-run seed (matches the binary's knownNexusManaged)
	prevManaged := map[string]bool{}
	for _, n := range []string{
		"github", "slack", "hubspot", "activecampaign", "zapier", "nexus-factory",
		"web-search", "partner-central", "atlassian", "jira",
		"google-drive", "google-docs", "google-slides", "google-workspace", "google-docs-&-slides",
	} {
		prevManaged[n] = true
	}
	newManaged := map[string]bool{}
	for k := range managed {
		newManaged[k] = true
	}

	toRemove := computeMcpRemovals(prevManaged, newManaged)
	result := reconcileMcpServers(existing, managed, toRemove, false)

	got := keysOf(result)
	sort.Strings(got)

	// Expected: exactly the admin-enabled set + the user's personal MCP.
	expected := map[string]bool{"my-personal-mcp": true}
	for k := range managed {
		expected[k] = true
	}

	// Every result key must be either admin-enabled or the user's own.
	for _, k := range got {
		if !expected[k] {
			t.Errorf("unexpected MCP in result (should have been removed): %s", k)
		}
	}
	// Every admin-enabled MCP must be present.
	for k := range managed {
		if _, ok := result[k]; !ok {
			t.Errorf("admin-enabled MCP missing from result: %s", k)
		}
	}
	// The user's personal MCP must survive.
	if _, ok := result["my-personal-mcp"]; !ok {
		t.Error("user-added MCP was wrongly removed")
	}
	// Stale ones must be gone.
	for _, stale := range []string{"activecampaign", "atlassian"} {
		if _, ok := result[stale]; ok {
			t.Errorf("stale admin-disabled MCP %s should have been removed", stale)
		}
	}

	t.Logf("Employee config reconciled to: %v", got)
}
