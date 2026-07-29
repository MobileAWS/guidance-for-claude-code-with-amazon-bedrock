package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTempHome creates a temporary directory, sets HOME to it for the duration
// of the test, and returns both the path and a cleanup function. Using a
// private HOME keeps every test hermetic: no real ~/.codex or
// ~/.claude-code-session is touched.
func newTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	origHome := os.Getenv("HOME")
	t.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	// os.UserHomeDir() on Linux/macOS resolves $HOME, so the in-process
	// override is sufficient. On Windows it uses USERPROFILE; set that too.
	origUP := os.Getenv("USERPROFILE")
	t.Setenv("USERPROFILE", dir)
	t.Cleanup(func() { os.Setenv("USERPROFILE", origUP) })

	return dir
}

// writeTimestamp writes a Unix timestamp into the sync-timestamp file at
// tsPath, creating parent directories as needed.
func writeTimestamp(t *testing.T, tsPath string, ts int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(tsPath), 0700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(tsPath), err)
	}
	if err := os.WriteFile(tsPath, []byte(strconv.FormatInt(ts, 10)), 0600); err != nil {
		t.Fatalf("WriteFile(%s): %v", tsPath, err)
	}
}

// ---------------------------------------------------------------------------
// isSyncFresh
// ---------------------------------------------------------------------------

func TestIsSyncFresh_Missing(t *testing.T) {
	home := newTempHome(t)
	tsPath := filepath.Join(home, ".claude-code-session", "test-sync-ts")
	if isSyncFresh(tsPath, 300) {
		t.Error("expected false for missing timestamp file, got true")
	}
}

func TestIsSyncFresh_RecentTimestamp(t *testing.T) {
	home := newTempHome(t)
	tsPath := filepath.Join(home, ".claude-code-session", "test-sync-ts")
	writeTimestamp(t, tsPath, time.Now().Unix()-60) // 60 s ago → fresh
	if !isSyncFresh(tsPath, 300) {
		t.Error("expected true for 60-s-old timestamp with 300-s TTL, got false")
	}
}

func TestIsSyncFresh_StaleTimestamp(t *testing.T) {
	home := newTempHome(t)
	tsPath := filepath.Join(home, ".claude-code-session", "test-sync-ts")
	writeTimestamp(t, tsPath, time.Now().Unix()-400) // 400 s ago → stale
	if isSyncFresh(tsPath, 300) {
		t.Error("expected false for 400-s-old timestamp with 300-s TTL, got true")
	}
}

func TestIsSyncFresh_Malformed(t *testing.T) {
	home := newTempHome(t)
	tsPath := filepath.Join(home, ".claude-code-session", "test-sync-ts")
	os.MkdirAll(filepath.Dir(tsPath), 0700)
	os.WriteFile(tsPath, []byte("not-a-number"), 0600)
	if isSyncFresh(tsPath, 300) {
		t.Error("expected false for malformed timestamp file, got true")
	}
}

func TestIsSyncFresh_ExactBoundary(t *testing.T) {
	home := newTempHome(t)
	tsPath := filepath.Join(home, ".claude-code-session", "test-sync-ts")
	// Exactly at TTL — the condition is < ttl, so exactly 300 s old is stale.
	writeTimestamp(t, tsPath, time.Now().Unix()-300)
	if isSyncFresh(tsPath, 300) {
		t.Error("expected false when age == TTL (boundary), got true")
	}
}

// ---------------------------------------------------------------------------
// writeSyncTimestamp
// ---------------------------------------------------------------------------

func TestWriteSyncTimestamp_CreatesFileAndParentDirs(t *testing.T) {
	home := newTempHome(t)
	tsPath := filepath.Join(home, ".claude-code-session", "new-dir", "ts")
	before := time.Now().Unix()
	writeSyncTimestamp(tsPath)
	after := time.Now().Unix()

	data, err := os.ReadFile(tsPath)
	if err != nil {
		t.Fatalf("timestamp file not created: %v", err)
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		t.Fatalf("timestamp not parseable: %v", err)
	}
	if ts < before || ts > after {
		t.Errorf("timestamp %d not in range [%d, %d]", ts, before, after)
	}
}

// ---------------------------------------------------------------------------
// tomlQuote
// ---------------------------------------------------------------------------

func TestTomlQuote(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"bedrock", `"bedrock"`},
		{"us-east-1", `"us-east-1"`},
		{`has"quote`, `"has\"quote"`},
		{`has\back`, `"has\\back"`},
		{"", `""`},
	}
	for _, c := range cases {
		got := tomlQuote(c.input)
		if got != c.want {
			t.Errorf("tomlQuote(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// syncCodexConfig — happy path
// ---------------------------------------------------------------------------

func TestSyncCodexConfig_WritesConfigToml(t *testing.T) {
	home := newTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"model_provider":"bedrock","region":"us-east-1"}`)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "my-org")

	tomlPath := filepath.Join(home, ".codex", "config.toml")
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("config.toml not created: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, `provider = "bedrock"`) {
		t.Errorf("expected provider = \"bedrock\" in config.toml, got:\n%s", got)
	}
	if !strings.Contains(got, `region = "us-east-1"`) {
		t.Errorf("expected region = \"us-east-1\" in config.toml, got:\n%s", got)
	}
	if strings.Contains(got, "BEARER") || strings.Contains(got, "TOKEN") || strings.Contains(got, "SECRET") {
		t.Errorf("config.toml must not contain secrets, got:\n%s", got)
	}
}

func TestSyncCodexConfig_UpdatesTimestamp(t *testing.T) {
	home := newTempHome(t)
	before := time.Now().Unix()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"model_provider":"bedrock","region":"eu-west-1"}`)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "org-x")

	tsPath := filepath.Join(home, ".claude-code-session", "codex-sync-ts")
	data, err := os.ReadFile(tsPath)
	if err != nil {
		t.Fatalf("sync timestamp not written: %v", err)
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		t.Fatalf("timestamp not parseable: %v", err)
	}
	after := time.Now().Unix()
	if ts < before || ts > after {
		t.Errorf("timestamp %d out of range [%d, %d]", ts, before, after)
	}
}

func TestSyncCodexConfig_UsesOrgIDInURL(t *testing.T) {
	home := newTempHome(t)
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprintln(w, `{"model_provider":"bedrock","region":"us-west-2"}`)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "skematic")

	if !strings.Contains(gotPath, "skematic") {
		t.Errorf("expected org ID in request path, got %q", gotPath)
	}
	if !strings.Contains(gotPath, "codex-config.json") {
		t.Errorf("expected codex-config.json in request path, got %q", gotPath)
	}
}

func TestSyncCodexConfig_FallbackPathWhenNoOrg(t *testing.T) {
	home := newTempHome(t)
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprintln(w, `{"model_provider":"bedrock","region":"ap-northeast-1"}`)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "") // empty orgID

	// Fallback path must not include an org-specific segment
	if strings.Count(gotPath, "/") > 2 {
		t.Errorf("expected flat fallback path, got %q", gotPath)
	}
	if !strings.Contains(gotPath, "codex-config.json") {
		t.Errorf("expected codex-config.json in fallback path, got %q", gotPath)
	}
}

// ---------------------------------------------------------------------------
// syncCodexConfig — skip / no-op conditions
// ---------------------------------------------------------------------------

func TestSyncCodexConfig_SkipsWhenFreshTimestamp(t *testing.T) {
	home := newTempHome(t)

	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		fmt.Fprintln(w, `{"model_provider":"bedrock","region":"us-east-1"}`)
	}))
	defer srv.Close()

	// Write a recent timestamp (30 s ago → well within 5-min TTL)
	tsPath := filepath.Join(home, ".claude-code-session", "codex-sync-ts")
	writeTimestamp(t, tsPath, time.Now().Unix()-30)

	syncCodexConfigWithURL(home, srv.URL+"/", "my-org")

	if requestCount != 0 {
		t.Errorf("expected no HTTP request when timestamp is fresh, got %d", requestCount)
	}
}

func TestSyncCodexConfig_SkipsOn404(t *testing.T) {
	home := newTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "no-such-org")

	tomlPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := os.Stat(tomlPath); !os.IsNotExist(err) {
		t.Error("expected no config.toml when S3 returns 404")
	}
	tsPath := filepath.Join(home, ".claude-code-session", "codex-sync-ts")
	if _, err := os.Stat(tsPath); !os.IsNotExist(err) {
		t.Error("expected no sync timestamp when S3 returns 404")
	}
}

func TestSyncCodexConfig_SkipsWhenCodexDisabled(t *testing.T) {
	home := newTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"codex_enabled":false,"model_provider":"bedrock","region":"us-east-1"}`)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "opt-out-org")

	tomlPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := os.Stat(tomlPath); !os.IsNotExist(err) {
		t.Error("expected no config.toml when codex_enabled=false")
	}
}

func TestSyncCodexConfig_ProceedsWhenCodexEnabledTrue(t *testing.T) {
	home := newTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"codex_enabled":true,"model_provider":"bedrock","region":"us-east-1"}`)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "opt-in-org")

	tomlPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := os.Stat(tomlPath); err != nil {
		t.Errorf("expected config.toml when codex_enabled=true, but stat failed: %v", err)
	}
}

func TestSyncCodexConfig_SkipsWhenNoModelProvider(t *testing.T) {
	home := newTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// model_provider is absent — not actionable
		fmt.Fprintln(w, `{"region":"us-east-1"}`)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "incomplete-org")

	tomlPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := os.Stat(tomlPath); !os.IsNotExist(err) {
		t.Error("expected no config.toml when model_provider is absent")
	}
}

func TestSyncCodexConfig_SkipsOnNetworkError(t *testing.T) {
	home := newTempHome(t)

	// Point at a port that refuses connections
	syncCodexConfigWithURL(home, "http://127.0.0.1:1/", "any-org")

	tomlPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := os.Stat(tomlPath); !os.IsNotExist(err) {
		t.Error("expected no config.toml on network error")
	}
}

func TestSyncCodexConfig_SkipsOnNon200NonError(t *testing.T) {
	home := newTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "broken-org")

	tomlPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := os.Stat(tomlPath); !os.IsNotExist(err) {
		t.Error("expected no config.toml on 500 response")
	}
}

func TestSyncCodexConfig_SkipsOnInvalidJSON(t *testing.T) {
	home := newTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `not json at all`)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "json-broken")

	tomlPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := os.Stat(tomlPath); !os.IsNotExist(err) {
		t.Error("expected no config.toml on invalid JSON response")
	}
}

// ---------------------------------------------------------------------------
// syncCodexConfig — TOML content correctness
// ---------------------------------------------------------------------------

func TestSyncCodexConfig_RegionOmittedWhenEmpty(t *testing.T) {
	home := newTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// region is absent
		fmt.Fprintln(w, `{"model_provider":"openai"}`)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "org-noregion")

	tomlPath := filepath.Join(home, ".codex", "config.toml")
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("config.toml not created: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "region") {
		t.Errorf("expected no region key when region is absent, got:\n%s", got)
	}
	if !strings.Contains(got, `provider = "openai"`) {
		t.Errorf("expected provider = \"openai\", got:\n%s", got)
	}
}

func TestSyncCodexConfig_TomlHasModelSection(t *testing.T) {
	home := newTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"model_provider":"bedrock","region":"us-west-2"}`)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "my-org")

	tomlPath := filepath.Join(home, ".codex", "config.toml")
	data, _ := os.ReadFile(tomlPath)
	got := string(data)

	if !strings.Contains(got, "[model]") {
		t.Errorf("expected [model] section in TOML, got:\n%s", got)
	}
}

func TestSyncCodexConfig_NoSecretsInToml(t *testing.T) {
	home := newTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Even if the server were to accidentally include a bearer token
		// field, we must not write it to disk.
		fmt.Fprintln(w, `{
			"model_provider":"bedrock",
			"region":"us-east-1",
			"aws_bearer_token":"super-secret",
			"AWS_BEARER_TOKEN_BEDROCK":"also-secret"
		}`)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "secret-test-org")

	tomlPath := filepath.Join(home, ".codex", "config.toml")
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("config.toml not created: %v", err)
	}
	got := string(data)
	for _, forbidden := range []string{"super-secret", "also-secret", "aws_bearer_token", "AWS_BEARER_TOKEN"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("config.toml must not contain %q:\n%s", forbidden, got)
		}
	}
}

func TestSyncCodexConfig_OverwritesExistingConfig(t *testing.T) {
	home := newTempHome(t)

	// Pre-populate an existing config
	codexDir := filepath.Join(home, ".codex")
	os.MkdirAll(codexDir, 0700)
	os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte("[model]\nprovider = \"oldprovider\"\n"), 0600)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"model_provider":"bedrock","region":"eu-central-1"}`)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "update-org")

	data, _ := os.ReadFile(filepath.Join(codexDir, "config.toml"))
	got := string(data)
	if strings.Contains(got, "oldprovider") {
		t.Errorf("expected config.toml to be overwritten, still contains 'oldprovider':\n%s", got)
	}
	if !strings.Contains(got, `provider = "bedrock"`) {
		t.Errorf("expected new provider in config.toml:\n%s", got)
	}
}

func TestSyncCodexConfig_FilePermissions(t *testing.T) {
	if os.Getenv("GOOS") == "windows" {
		t.Skip("permission bits not meaningful on Windows")
	}
	home := newTempHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"model_provider":"bedrock","region":"us-east-1"}`)
	}))
	defer srv.Close()

	syncCodexConfigWithURL(home, srv.URL+"/", "perm-org")

	tomlPath := filepath.Join(home, ".codex", "config.toml")
	info, err := os.Stat(tomlPath)
	if err != nil {
		t.Fatalf("config.toml not created: %v", err)
	}
	mode := info.Mode().Perm()
	if mode != 0600 {
		t.Errorf("expected config.toml permissions 0600, got %o", mode)
	}
}

// ---------------------------------------------------------------------------
// syncCodexConfig — idempotency
// ---------------------------------------------------------------------------

func TestSyncCodexConfig_IdempotentOnRepeatedCalls(t *testing.T) {
	home := newTempHome(t)
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		fmt.Fprintln(w, `{"model_provider":"bedrock","region":"us-east-1"}`)
	}))
	defer srv.Close()

	// First call should hit the server and write the file
	syncCodexConfigWithURL(home, srv.URL+"/", "idem-org")
	if callCount != 1 {
		t.Errorf("expected 1 request on first call, got %d", callCount)
	}

	// Second call should be skipped due to fresh timestamp
	syncCodexConfigWithURL(home, srv.URL+"/", "idem-org")
	if callCount != 1 {
		t.Errorf("expected no additional requests on second call (throttled), got %d total", callCount)
	}
}
