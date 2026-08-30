package nexus

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func init() {
	// Make polling instant in all tests.
	pollSleep = func(time.Duration) {}
}

func TestResolveBase(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", DefaultAPIBase},
		{"  ", DefaultAPIBase},
		{"https://x.example.com", "https://x.example.com"},
		{"https://x.example.com/", "https://x.example.com"},
		{"https://x.example.com///", "https://x.example.com"},
	}
	for _, c := range cases {
		if got := ResolveBase(c.in); got != c.want {
			t.Errorf("ResolveBase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDeviceFlowSuccess(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing Content-Type header")
		}
		switch r.URL.Path {
		case "/api/device/code":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"user_code":        "WXYZ-1234",
				"device_code":      "dev-abc",
				"verification_uri": "https://verify.example.com",
				"interval":         1,
				"expires_in":       60,
			})
		case "/api/device/token":
			// First poll pending, second poll complete.
			if atomic.AddInt32(&polls, 1) < 2 {
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "complete",
				"tokens": map[string]string{"id_token": "ID_TOK", "access_token": "ACC_TOK"},
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	var opened string
	res, err := DeviceFlow(srv.URL, func(u string) error { opened = u; return nil }, nil)
	if err != nil {
		t.Fatalf("DeviceFlow error: %v", err)
	}
	if res.IDToken != "ID_TOK" || res.AccessToken != "ACC_TOK" {
		t.Errorf("got tokens %+v", res)
	}
	if atomic.LoadInt32(&polls) < 2 {
		t.Errorf("expected at least 2 polls, got %d", polls)
	}
	if opened == "" {
		t.Errorf("expected verification page to be opened")
	}
}

func TestDeviceFlowExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/device/code":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"device_code": "dev-abc", "verification_uri": "https://v", "interval": 1, "expires_in": 60,
			})
		case "/api/device/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "expired_token"})
		}
	}))
	defer srv.Close()

	_, err := DeviceFlow(srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected expired-token error, got nil")
	}
}

func TestDeviceFlowCodeRequestFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := DeviceFlow(srv.URL, nil, nil); err == nil {
		t.Fatal("expected error when device-code request fails")
	}
}

func TestReportPlatform(t *testing.T) {
	var gotBody map[string]string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/users/platform" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := ReportPlatform(srv.URL, "user@example.com", "TOK123", "claude-code"); err != nil {
		t.Fatalf("ReportPlatform error: %v", err)
	}
	if gotAuth != "Bearer TOK123" {
		t.Errorf("Authorization = %q, want Bearer TOK123", gotAuth)
	}
	if gotBody["email"] != "user@example.com" {
		t.Errorf("email = %q", gotBody["email"])
	}
	if gotBody["platform"] == "" || gotBody["arch"] == "" {
		t.Errorf("expected platform/arch in body, got %+v", gotBody)
	}
}

func TestReportPlatformNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if err := ReportPlatform(srv.URL, "e", "t"); err == nil {
		t.Fatal("expected error on non-2xx response")
	}
}
