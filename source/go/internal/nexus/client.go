// Package nexus implements the Nexus-hub HTTP integrations used by Nexus-managed
// deployments: the OAuth device-code flow (portless authentication) and
// best-effort platform reporting. These have no upstream equivalent — they are
// the Go port of the fork's Python credential_provider Nexus features.
package nexus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

// DefaultAPIBase is the Nexus hub API Gateway used when a profile opts into
// device flow (via quota_api_endpoint) but does not set device_auth_endpoint.
//
// TODO(phase3): externalize this. The Nexus extraction should have package-gen
// write device_auth_endpoint into config.json so no deployment identifier is
// embedded in the shipped binary. Tracked alongside the other identifier
// externalization in UPGRADE_LOG.html.
const DefaultAPIBase = "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com"

// ResolveBase returns the Nexus API base URL: the configured device_auth_endpoint
// if non-empty, otherwise DefaultAPIBase. Trailing slashes are trimmed.
func ResolveBase(configured string) string {
	b := strings.TrimSpace(configured)
	if b == "" {
		b = DefaultAPIBase
	}
	return strings.TrimRight(b, "/")
}

type deviceCodeResp struct {
	UserCode        string `json:"user_code"`
	DeviceCode      string `json:"device_code"`
	VerificationURI string `json:"verification_uri"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

type deviceTokenResp struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Tokens struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
	} `json:"tokens"`
}

// pollSleep is the inter-poll delay; a package var so tests can make polling
// instant without changing flow behavior.
var pollSleep = time.Sleep

// DeviceFlowResult holds the tokens returned by a completed device-code flow.
type DeviceFlowResult struct {
	IDToken     string
	AccessToken string
}

// DeviceFlow performs the OAuth device authorization flow against the Nexus hub:
// it requests a device code, shows the code to the user (and opens the
// verification page via openBrowser, if provided), then polls until the hub
// reports completion and returns the tokens. No local port or redirect server is
// used — this is the "no ports, no redirects" path. baseURL is resolved via
// ResolveBase. logf, if non-nil, receives debug messages.
func DeviceFlow(baseURL string, openBrowser func(string) error, logf func(string, ...interface{})) (*DeviceFlowResult, error) {
	base := ResolveBase(baseURL)
	client := &http.Client{Timeout: 15 * time.Second}
	debug := func(format string, args ...interface{}) {
		if logf != nil {
			logf(format, args...)
		}
	}

	// Step 1: request a device code (includes platform info, no auth).
	codeBody, _ := json.Marshal(map[string]string{
		"platform": runtime.GOOS,
		"arch":     runtime.GOARCH,
	})
	var code deviceCodeResp
	if err := postJSON(client, base+"/api/device/code", "", codeBody, &code); err != nil {
		return nil, fmt.Errorf("requesting device code: %w", err)
	}
	interval := code.Interval
	if interval <= 0 {
		interval = 3
	}
	expiresIn := code.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}

	// Step 2: show the code and open the verification page.
	fmt.Fprintf(os.Stderr, "\n  To authenticate, visit: %s\n", code.VerificationURI)
	fmt.Fprintf(os.Stderr, "  and enter code: %s\n\n", code.UserCode)
	if openBrowser != nil && code.VerificationURI != "" {
		_ = openBrowser(fmt.Sprintf("%s?code=%s", code.VerificationURI, url.QueryEscape(code.UserCode)))
	}

	// Step 3: poll for completion.
	pollBody, _ := json.Marshal(map[string]string{"device_code": code.DeviceCode})
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	for time.Now().Before(deadline) {
		pollSleep(time.Duration(interval) * time.Second)
		var pr deviceTokenResp
		if err := postJSON(client, base+"/api/device/token", "", pollBody, &pr); err != nil {
			debug("device token poll error (continuing): %v", err)
			continue // transient or pending-as-HTTP-error; keep polling
		}
		switch pr.Status {
		case "complete":
			if pr.Tokens.IDToken != "" {
				return &DeviceFlowResult{IDToken: pr.Tokens.IDToken, AccessToken: pr.Tokens.AccessToken}, nil
			}
			// Authorized but tokens not posted yet — keep polling.
		case "authorization_pending":
			// Keep polling.
		}
		if pr.Error == "expired_token" || pr.Error == "invalid_device_code" {
			return nil, fmt.Errorf("device code expired; please try again")
		}
	}
	return nil, fmt.Errorf("device authorization timed out")
}

// ReportPlatform sends a best-effort platform report to the Nexus hub
// (POST /api/users/platform). It is non-blocking by contract: callers ignore the
// error. A short timeout keeps it off the credential-issuance hot path.
func ReportPlatform(baseURL, email, idToken, tool string) error {
	base := ResolveBase(baseURL)
	client := &http.Client{Timeout: 5 * time.Second}
	if tool == "" {
		tool = "claude-code"
	}
	body, _ := json.Marshal(map[string]string{
		"email":    email,
		"platform": runtime.GOOS,
		"arch":     runtime.GOARCH,
		"tool":     tool,
	})
	return postJSON(client, base+"/api/users/platform", idToken, body, nil)
}

// postJSON POSTs a JSON body and, when out is non-nil, decodes a JSON response.
// A non-2xx status is returned as an error (so the device-token poll treats it
// like the Python urllib HTTPError path and keeps polling).
func postJSON(client *http.Client, endpoint, bearer string, body []byte, out interface{}) error {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s returned status %d", endpoint, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decoding response from %s: %w", endpoint, err)
		}
	}
	return nil
}
