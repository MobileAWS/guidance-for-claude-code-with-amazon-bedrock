package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"ccwb-go/internal/config"
	"ccwb-go/internal/federation"
	"ccwb-go/internal/jwt"
	"ccwb-go/internal/oidc"
	"ccwb-go/internal/otel"
	"ccwb-go/internal/portlock"
	"ccwb-go/internal/provider"
	"ccwb-go/internal/quota"
	"ccwb-go/internal/storage"
	"ccwb-go/internal/version"

	_ "ccwb-go/internal/browser" // available for future integration flows
)

var debug bool

func debugPrint(format string, args ...interface{}) {
	if debug {
		fmt.Fprintf(os.Stderr, "Debug: "+format+"\n", args...)
	}
}

func main() {
	defaultProfile := os.Getenv("CCWB_PROFILE")
	if defaultProfile == "" {
		defaultProfile = "ClaudeCode"
	}

	profileFlag := flag.String("profile", defaultProfile, "Configuration profile to use")
	shortProfile := flag.String("p", "", "Configuration profile to use (short)")
	orgFlag := flag.String("org", "", "Select org for multi-org users (e.g. --org skematic)")
	versionFlag := flag.Bool("version", false, "Show version")
	shortVersion := flag.Bool("v", false, "Show version (short)")
	getMonitoring := flag.Bool("get-monitoring-token", false, "Get cached monitoring token")
	clearCache := flag.Bool("clear-cache", false, "Clear cached credentials")
	checkExpiration := flag.Bool("check-expiration", false, "Check if credentials are expired")
	refreshIfNeeded := flag.Bool("refresh-if-needed", false, "Refresh credentials if expired")
	showTags := flag.Bool("show-tags", false, "Print the https://aws.amazon.com/tags claim from the cached ID token (debug)")
	getTag := flag.String("get-tag", "", "Print the value of a single principal tag from the cached ID token (e.g. --get-tag Zone). Exit codes: 0 hit, 2 absent, 4 expired.")
	flag.Parse()

	if *versionFlag || *shortVersion {
		fmt.Printf("credential-process %s\n", version.Version)
		os.Exit(0)
	}

	profile := *profileFlag
	if *shortProfile != "" {
		profile = *shortProfile
	}
	if profile == defaultProfile {
		// Try auto-detect if using default
		if detected := config.AutoDetectProfile(); detected != "" {
			profile = detected
		}
	}

	debug = os.Getenv("COGNITO_AUTH_DEBUG") == "1" || os.Getenv("COGNITO_AUTH_DEBUG") == "true" || os.Getenv("COGNITO_AUTH_DEBUG") == "yes"

	cfg, err := config.LoadProfile(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// SSO-disabled (passthrough) mode. Resolve before provider-type detection
	// because cfg.ProviderDomain is intentionally "none" for these profiles
	// and would trip the auto-detect error otherwise. Mirrors Python PR #303.
	if !cfg.IsSsoEnabled() {
		// Build a minimal app — we don't need providerType or redirectPort for
		// passthrough, only the ambient AWS chain.
		app := &credentialApp{
			profile: profile,
			cfg:     cfg,
		}
		// Honor the small-but-useful flag set that doesn't depend on OIDC.
		if *clearCache {
			app.clearCache()
			os.Exit(0)
		}
		os.Exit(app.runPassthrough())
	}

	// Resolve provider type
	providerType := resolveProviderType(cfg)

	// Resolve redirect port: REDIRECT_PORT env > config.json > 8400
	redirectPort := 8400
	if envPort := os.Getenv("REDIRECT_PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil && p > 0 {
			redirectPort = p
		}
	} else if cfg.RedirectPort > 0 {
		redirectPort = cfg.RedirectPort
	}

	app := &credentialApp{
		profile:      profile,
		orgFlag:      *orgFlag,
		cfg:          cfg,
		providerType: providerType,
		redirectPort: redirectPort,
	}

	if *clearCache {
		app.clearCache()
		os.Exit(0)
	}

	if *showTags {
		os.Exit(app.showTags())
	}

	if *getTag != "" {
		os.Exit(app.getTag(*getTag))
	}

	if *getMonitoring {
		os.Exit(app.getMonitoringToken())
	}

	if *checkExpiration {
		os.Exit(app.checkExpiration())
	}

	if *refreshIfNeeded {
		if cfg.CredentialStorage != "session" {
			fmt.Fprintln(os.Stderr, "Error: --refresh-if-needed only works with session storage mode")
			os.Exit(1)
		}
		creds, err := storage.ReadFromCredentialsFile(profile)
		if err == nil && creds != nil && !storage.IsExpiredDummy(creds) {
			remaining := storage.ParseExpirationSeconds(creds.Expiration)
			if remaining > 30 {
				debugPrint("Credentials still valid for profile '%s', no refresh needed", profile)
				os.Exit(0)
			}
		}
		// Fall through to normal auth flow
	}

	os.Exit(app.run())
}

type credentialApp struct {
	profile      string
	orgFlag      string
	cfg          *config.ProfileConfig
	providerType string
	redirectPort int
}

func resolveProviderType(cfg *config.ProfileConfig) string {
	if provider.IsKnown(cfg.ProviderType) {
		return cfg.ProviderType
	}
	detected := provider.Detect(cfg.ProviderDomain)
	if detected == "oidc" {
		fmt.Fprintf(os.Stderr, "Error: Unable to auto-detect provider type for domain '%s'.\n", cfg.ProviderDomain)
		fmt.Fprintln(os.Stderr, "Known providers: Okta, Auth0, Microsoft/Azure, AWS Cognito User Pool, Generic OIDC.")
		fmt.Fprintln(os.Stderr, "Set provider_type to \"generic\" in config.json for custom OIDC providers.")
		os.Exit(1)
	}
	return detected
}

func (a *credentialApp) getCachedCredentials() *federation.AWSCredentials {
	var creds *federation.AWSCredentials
	var err error

	if a.cfg.CredentialStorage == "keyring" {
		creds, err = storage.ReadFromKeyring(a.profile)
	} else {
		// Read from session cache (not ~/.aws/credentials)
		creds, err = storage.ReadFromSessionCache(a.profile)
	}
	if err != nil || creds == nil || storage.IsExpiredDummy(creds) {
		return nil
	}

	remaining := storage.ParseExpirationSeconds(creds.Expiration)
	if remaining <= 30 {
		return nil
	}
	return creds
}

func (a *credentialApp) saveCredentials(creds *federation.AWSCredentials) error {
	if a.cfg.CredentialStorage == "keyring" {
		return storage.SaveToKeyring(creds, a.profile)
	}
	// Save to session cache (NOT ~/.aws/credentials) so the AWS SDK
	// always calls credential_process and we can handle refresh silently.
	return storage.SaveToSessionCache(creds, a.profile)
}

func (a *credentialApp) clearCache() {
	if a.cfg.CredentialStorage == "keyring" {
		_ = storage.ClearKeyring(a.profile)
	}
	// Clear session cache
	expired := &federation.AWSCredentials{
		Version: 1, AccessKeyID: "EXPIRED", SecretAccessKey: "EXPIRED",
		SessionToken: "EXPIRED", Expiration: "2000-01-01T00:00:00Z",
	}
	_ = storage.SaveToSessionCache(expired, a.profile)
	// Clear refresh token
	storage.ClearRefreshToken(a.profile)
	fmt.Fprintf(os.Stderr, "Cleared cached credentials for profile '%s'\n", a.profile)
}

func (a *credentialApp) getMonitoringToken() int {
	token, err := storage.GetMonitoringToken(a.profile, a.cfg.CredentialStorage)
	if err == nil && token != "" {
		fmt.Println(token)
		return 0
	}

	// No cached token — trigger authentication
	debugPrint("No valid monitoring token found, triggering authentication...")
	authResult, err := a.authenticate()
	if err != nil {
		debugPrint("Authentication failed: %v", err)
		return 1
	}

	// Get AWS creds (needed to complete the flow)
	awsCreds, err := a.getAWSCredentials(authResult)
	if err != nil {
		debugPrint("Failed to get AWS credentials: %v", err)
		return 1
	}
	_ = a.saveCredentials(awsCreds)

	// Save monitoring token
	_ = storage.SaveMonitoringToken(a.profile, a.cfg.CredentialStorage,
		authResult.IDToken, map[string]interface{}(authResult.TokenClaims))

	// Report platform/OS to Nexus API (best-effort, non-blocking)
	go reportPlatform(authResult.IDToken)

	fmt.Println(authResult.IDToken)
	return 0
}

func (a *credentialApp) checkExpiration() int {
	creds, err := storage.ReadFromCredentialsFile(a.profile)
	if err != nil || creds == nil || storage.IsExpiredDummy(creds) {
		fmt.Fprintf(os.Stderr, "Credentials expired or missing for profile '%s'\n", a.profile)
		return 1
	}
	remaining := storage.ParseExpirationSeconds(creds.Expiration)
	if remaining <= 30 {
		fmt.Fprintf(os.Stderr, "Credentials expired or missing for profile '%s'\n", a.profile)
		return 1
	}
	fmt.Fprintf(os.Stderr, "Credentials valid for profile '%s'\n", a.profile)
	return 0
}

// showTags prints the contents of the `https://aws.amazon.com/tags` claim
// from the cached monitoring token. This is a diagnostic for customers
// setting up session-tag-based cost attribution -- it answers "is my IdP
// actually emitting the tags I expect?" without needing to decode JWTs
// by hand. Triggers a fresh OIDC flow if no cached token is available.
func (a *credentialApp) showTags() int {
	token, _ := storage.GetMonitoringToken(a.profile, a.cfg.CredentialStorage)
	var claims jwt.Claims
	if token != "" {
		if c, err := jwt.DecodePayload(token); err == nil {
			claims = c
		}
	}
	if claims == nil {
		debugPrint("No cached monitoring token; running OIDC flow to read tags claim")
		authResult, err := a.authenticate()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		claims = authResult.TokenClaims
		_ = storage.SaveMonitoringToken(a.profile, a.cfg.CredentialStorage,
			authResult.IDToken, map[string]interface{}(claims))
	}

	// Accept both claim shapes that STS itself accepts:
	//   flat:   claims["https://aws.amazon.com/tags/principal_tags/<Key>"]
	//   nested: claims["https://aws.amazon.com/tags"].principal_tags.<Key>
	// Gather anything we can find, report nothing only when both shapes are absent.
	summary := map[string]interface{}{}
	if nested, ok := claims["https://aws.amazon.com/tags"]; ok {
		summary["https://aws.amazon.com/tags"] = nested
	}
	flat := map[string]string{}
	for k, v := range claims {
		const prefix = "https://aws.amazon.com/tags/principal_tags/"
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			if s, ok := v.(string); ok {
				flat[k[len(prefix):]] = s
			}
		}
	}
	if len(flat) > 0 {
		summary["principal_tags (flat)"] = flat
	}
	if len(summary) == 0 {
		fmt.Fprintln(os.Stderr, "No `https://aws.amazon.com/tags` claim present in the ID token.")
		fmt.Fprintln(os.Stderr, "Your IdP is not configured to emit session tags. See assets/docs/COST_ATTRIBUTION.md section 3.")
		return 1
	}
	// Surface the resolved value of the cost-attribution tag regardless of
	// which shape produced it -- this is the exact value the OTel pipeline
	// emits as x-project. Key name comes from config (default "Project") so
	// customers using CostCenter/BillingCode see the same diagnostic.
	costTagKey := a.cfg.CostAttributionTagKey
	if costTagKey == "" {
		costTagKey = "Project"
	}
	if p := otel.ExtractPrincipalTag(claims, costTagKey); p != "" {
		summary[fmt.Sprintf("%s (resolved)", costTagKey)] = p
	}
	pretty, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not format tags claim: %v\n", err)
		return 1
	}
	fmt.Println(string(pretty))
	return 0
}

// getTag prints a single principal-tag value from the cached ID token.
// This backs the install-time shell function that sets ANTHROPIC_MODEL
// from the user's Zone tag on every `claude` launch. It is purely local
// (no OIDC flow, no network) so it's safe to call from a non-interactive
// shell function; missing/expired tokens bubble up as distinct exit codes
// the shell function can translate into a user-readable message.
//
// Exit codes:
//
//	0 -- tag present, value printed to stdout
//	2 -- no cached token, or token has no such tag
//	4 -- token is expired (user needs to re-auth)
func (a *credentialApp) getTag(key string) int {
	token, _ := storage.GetMonitoringToken(a.profile, a.cfg.CredentialStorage)
	if token == "" {
		return 2
	}
	claims, err := jwt.DecodePayload(token)
	if err != nil {
		return 2
	}
	if exp := claims.GetFloat("exp"); exp > 0 && int64(exp) < time.Now().Unix() {
		return 4
	}
	value := otel.ExtractPrincipalTag(claims, key)
	if value == "" {
		return 2
	}
	fmt.Println(value)
	return 0
}

func (a *credentialApp) run() int {
	// Save org selection if explicitly passed
	if a.orgFlag != "" {
		saveActiveOrg(a.profile, a.orgFlag)
	}

	// Check for self-update (background, non-blocking)
	go checkForUpdate()

	// Sync MCP servers from Nexus (quick, with timeout)
	syncMcpServers(a.profile)

	// Inject per-user integration tokens (HubSpot OAuth, etc.)
	syncIntegrationTokens(a.profile)

	// Sync Codex config from Nexus (quick, best-effort)
	syncCodexConfig(a.profile)

	// Sync Skills from Nexus (quick, with timeout)
	syncSkills()

	// Sync managed config for Claude Desktop (quick, with timeout)
	syncManagedConfig(a.profile)

	// Clear AWS CLI cache if our credentials are expired (prevents stale credential loops)
	clearAwsCliCacheIfExpired(a.profile)

	// Check cache first
	if cached := a.getCachedCredentials(); cached != nil {
		// Periodic quota re-check
		if a.shouldRecheckQuota() {
			a.performQuotaRecheck()
		}
		outputJSON(chainAssumeRole(cached, a.cfg))
		return 0
	}

	// Try to acquire port lock
	ln, err := portlock.TryAcquire(a.redirectPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	if ln == nil {
		// Port busy — another auth in progress
		debugPrint("Another authentication is in progress, waiting...")
		if portlock.WaitForRelease(a.redirectPort, 60*time.Second) {
			if cached := a.getCachedCredentials(); cached != nil {
				outputJSON(chainAssumeRole(cached, a.cfg))
				return 0
			}
		}
		debugPrint("Authentication timeout or failed in another process")
		return 1
	}
	// Release the port lock so the callback server can use it
	ln.Close()

	// Check cache again (race condition guard)
	if cached := a.getCachedCredentials(); cached != nil {
		outputJSON(chainAssumeRole(cached, a.cfg))
		return 0
	}

	// Try silent refresh using cached id_token before opening browser
	if creds := a.trySilentRefresh(); creds != nil {
		if a.cfg.QuotaAPIEndpoint != "" {
			token, _ := storage.GetMonitoringToken(a.profile, a.cfg.CredentialStorage)
			if token != "" {
				qr := quota.Check(a.cfg.QuotaAPIEndpoint, token, a.cfg.QuotaCheckTimeout, a.cfg.QuotaFailMode)
				if !qr.Allowed {
					printQuotaBlocked(qr)
					return 1
				}
			}
		}
		outputJSON(chainAssumeRole(creds, a.cfg))
		return 0
	}

	// Try refresh_token exchange before falling back to browser auth.
	// This enables Cowork 3P (Claude Desktop) to refresh silently even after
	// the id_token expires, since Claude Desktop cannot open a browser popup.
	if creds := a.tryRefreshToken(); creds != nil {
		if a.cfg.QuotaAPIEndpoint != "" {
			token, _ := storage.GetMonitoringToken(a.profile, a.cfg.CredentialStorage)
			if token != "" {
				qr := quota.Check(a.cfg.QuotaAPIEndpoint, token, a.cfg.QuotaCheckTimeout, a.cfg.QuotaFailMode)
				if !qr.Allowed {
					printQuotaBlocked(qr)
					return 1
				}
			}
		}
		outputJSON(chainAssumeRole(creds, a.cfg))
		return 0
	}

	// Authenticate with OIDC provider (browser popup)
	debugPrint("Authenticating with %s for profile '%s'...", a.providerType, a.profile)
	authResult, err := a.authenticate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// Quota check before issuing credentials
	if a.cfg.QuotaAPIEndpoint != "" {
		qr := quota.Check(a.cfg.QuotaAPIEndpoint, authResult.IDToken, a.cfg.QuotaCheckTimeout, a.cfg.QuotaFailMode)
		if !qr.Allowed {
			printQuotaBlocked(qr)
			return 1
		}
	}

	// Get AWS credentials
	debugPrint("Exchanging token for AWS credentials...")
	awsCreds, err := a.getAWSCredentials(authResult)
	if err != nil {
		if federation.IsRetryableAuthError(err) {
			a.clearCache()
			fmt.Fprintf(os.Stderr, "Authentication failed - cached credentials were invalid and have been cleared.\nPlease try again to re-authenticate.\n")
		} else {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return 1
	}

	// Cache credentials
	if err := a.saveCredentials(awsCreds); err != nil {
		debugPrint("Failed to save credentials: %v", err)
	}

	// Save monitoring token (non-blocking)
	_ = storage.SaveMonitoringToken(a.profile, a.cfg.CredentialStorage,
		authResult.IDToken, map[string]interface{}(authResult.TokenClaims))

	// Persist refresh_token for silent renewal (Cowork 3P support)
	_ = storage.SaveRefreshToken(a.profile, a.cfg.CredentialStorage, authResult.RefreshToken)

	// Org selection for multi-org users
	if groups, ok := authResult.TokenClaims["cognito:groups"]; ok {
		if groupList, ok := groups.([]interface{}); ok {
			var orgs []string
			for _, g := range groupList {
				if s, ok := g.(string); ok && len(s) > 4 && s[:4] == "org-" && !strings.HasSuffix(s, "-admins") {
					orgs = append(orgs, s[4:])
				}
			}
			if len(orgs) > 0 {
				selectedOrg := ""
				if a.orgFlag != "" {
					selectedOrg = a.orgFlag
				} else if len(orgs) == 1 {
					selectedOrg = orgs[0]
				} else {
					// Multiple orgs — check saved preference or prompt
					savedOrg := readActiveOrg(a.profile)
					if savedOrg != "" {
						selectedOrg = savedOrg
					} else {
						fmt.Fprintf(os.Stderr, "\nYou belong to multiple organizations:\n")
						for i, org := range orgs {
							fmt.Fprintf(os.Stderr, "  [%d] %s\n", i+1, org)
						}
						fmt.Fprintf(os.Stderr, "Select org (1-%d): ", len(orgs))
						var choice int
						fmt.Scanln(&choice)
						if choice >= 1 && choice <= len(orgs) {
							selectedOrg = orgs[choice-1]
						} else {
							selectedOrg = orgs[0]
						}
					}
				}
				if selectedOrg != "" {
					saveActiveOrg(a.profile, selectedOrg)
				}
			}
		}
	}

	outputJSON(awsCreds)
	return 0
}

func (a *credentialApp) authenticate() (*oidc.AuthResult, error) {
	confidential, err := a.resolveConfidentialAuth()
	if err != nil {
		return nil, err
	}
	var generic *oidc.GenericEndpoints
	if a.providerType == "generic" {
		generic = &oidc.GenericEndpoints{
			AuthorizeURL: a.cfg.OIDCAuthorizationEndpoint,
			TokenURL:     a.cfg.OIDCTokenEndpoint,
		}
	}
	return oidc.Authenticate(
		a.cfg.ProviderDomain,
		a.cfg.ClientID,
		a.providerType,
		a.cfg.OktaAuthServerID, // "" or "default" -> default CAS; anything else rewrites endpoints
		a.redirectPort,
		confidential,
		generic,
	)
}

// resolveConfidentialAuth loads Azure confidential-client material -- either a
// client secret from the OS keyring, or a certificate + private-key pair from
// disk. Env-var overrides (AZURE_CLIENT_CERTIFICATE_PATH,
// AZURE_CLIENT_CERTIFICATE_KEY_PATH) take precedence over config.json so
// installs stay portable across machines. Returns nil for public-client flows.
func (a *credentialApp) resolveConfidentialAuth() (*oidc.ConfidentialAuth, error) {
	if a.providerType != "azure" {
		return nil, nil
	}
	mode := a.cfg.AzureAuthMode
	if mode == "" || mode == "public" {
		return nil, nil
	}
	switch mode {
	case "secret":
		secret, err := storage.ReadClientSecret(a.profile)
		if err != nil {
			return nil, fmt.Errorf("reading client secret from keyring: %w", err)
		}
		if secret == "" {
			return nil, fmt.Errorf(
				"azure_auth_mode is 'secret' but no client secret is stored.\n"+
					"Run: ccwb init --profile %s (re-run the Azure step) to store one in the OS keyring.",
				a.profile)
		}
		return &oidc.ConfidentialAuth{ClientSecret: secret}, nil
	case "certificate":
		certPath := os.Getenv("AZURE_CLIENT_CERTIFICATE_PATH")
		if certPath == "" {
			certPath = a.cfg.ClientCertificatePath
		}
		keyPath := os.Getenv("AZURE_CLIENT_CERTIFICATE_KEY_PATH")
		if keyPath == "" {
			keyPath = a.cfg.ClientCertificateKeyPath
		}
		if certPath == "" || keyPath == "" {
			return nil, fmt.Errorf(
				"azure_auth_mode is 'certificate' but no certificate paths are configured.\n" +
					"Set AZURE_CLIENT_CERTIFICATE_PATH and AZURE_CLIENT_CERTIFICATE_KEY_PATH, " +
					"or update 'client_certificate_path' and 'client_certificate_key_path' in config.json.")
		}
		return &oidc.ConfidentialAuth{CertificatePath: certPath, PrivateKeyPath: keyPath}, nil
	default:
		return nil, fmt.Errorf("unknown azure_auth_mode %q (expected public, secret, or certificate)", mode)
	}
}

func (a *credentialApp) getAWSCredentials(auth *oidc.AuthResult) (*federation.AWSCredentials, error) {
	if a.cfg.FederationType == "direct" {
		return federation.AssumeRoleWithWebIdentity(
			a.cfg.AWSRegion, a.cfg.FederatedRoleARN, auth.IDToken,
			auth.TokenClaims, a.cfg.MaxSessionDuration,
		)
	}
	return federation.GetCredentialsViaCognito(
		a.cfg.AWSRegion, a.cfg.IdentityPoolID, a.cfg.ProviderDomain,
		a.providerType, auth.IDToken, auth.TokenClaims,
	)
}

func (a *credentialApp) trySilentRefresh() *federation.AWSCredentials {
	token, err := storage.GetMonitoringToken(a.profile, a.cfg.CredentialStorage)
	if err != nil || token == "" {
		debugPrint("No valid cached id_token for silent refresh")
		return nil
	}
	debugPrint("Found valid cached id_token, attempting silent credential refresh...")
	claims, err := jwt.DecodePayload(token)
	if err != nil {
		debugPrint("Failed to decode cached id_token: %v", err)
		return nil
	}
	// Check if the id_token itself is expired
	if exp := claims.GetFloat("exp"); exp > 0 && int64(exp) < time.Now().Unix() {
		debugPrint("Cached id_token is expired, silent refresh not possible")
		return nil
	}
	authResult := &oidc.AuthResult{IDToken: token, TokenClaims: claims}
	creds, err := a.getAWSCredentials(authResult)
	if err != nil {
		debugPrint("Silent refresh failed, will require browser auth: %v", err)
		return nil
	}
	if saveErr := a.saveCredentials(creds); saveErr != nil {
		debugPrint("Failed to save silently-refreshed credentials: %v", saveErr)
	}
	// Re-save monitoring token to refresh its expiry tracking
	_ = storage.SaveMonitoringToken(a.profile, a.cfg.CredentialStorage,
		token, map[string]interface{}(claims))
	debugPrint("Silent credential refresh succeeded")
	return creds
}

// tryRefreshToken attempts to use a stored OIDC refresh_token to obtain a
// fresh id_token without browser interaction. This is the key enabler for
// Cowork 3P (Claude Desktop): after the id_token expires, credential-process
// can still silently refresh credentials as long as the refresh_token is valid
// (typically 7-30 days depending on IdP configuration).
func (a *credentialApp) tryRefreshToken() *federation.AWSCredentials {
	refreshToken := storage.LoadRefreshToken(a.profile, a.cfg.CredentialStorage)
	if refreshToken == "" {
		debugPrint("No cached refresh_token, cannot refresh silently")
		return nil
	}

	debugPrint("Found cached refresh_token, attempting token exchange...")

	// Resolve token endpoint URL
	var tokenURL string
	if a.providerType == "generic" {
		tokenURL = a.cfg.OIDCTokenEndpoint
	} else {
		provCfg := provider.ConfigFor(a.providerType, a.cfg.OktaAuthServerID)
		domain := a.cfg.ProviderDomain
		tokenURL = "https://" + domain + provCfg.TokenEndpoint
	}

	// Resolve confidential client auth (Azure secret/cert)
	confidential, err := a.resolveConfidentialAuth()
	if err != nil {
		debugPrint("Failed to resolve confidential auth for refresh: %v", err)
		return nil
	}

	// Exchange refresh_token for fresh tokens
	tokenResp, err := oidc.RefreshTokenExchange(tokenURL, refreshToken, a.cfg.ClientID, confidential)
	if err != nil {
		debugPrint("Refresh token exchange failed: %v", err)
		// Token may be revoked/expired — clear it so we don't retry next time
		storage.ClearRefreshToken(a.profile)
		return nil
	}

	if tokenResp.IDToken == "" {
		debugPrint("Refresh response did not contain an id_token")
		return nil
	}

	// Decode fresh id_token
	claims, err := jwt.DecodePayload(tokenResp.IDToken)
	if err != nil {
		debugPrint("Failed to decode refreshed id_token: %v", err)
		return nil
	}

	// Exchange for AWS credentials
	authResult := &oidc.AuthResult{
		IDToken:      tokenResp.IDToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenClaims:  claims,
	}
	creds, err := a.getAWSCredentials(authResult)
	if err != nil {
		debugPrint("AWS credential exchange after refresh failed: %v", err)
		return nil
	}

	// Save refreshed credentials
	if saveErr := a.saveCredentials(creds); saveErr != nil {
		debugPrint("Failed to save refresh-derived credentials: %v", saveErr)
	}

	// Update monitoring token with fresh id_token
	_ = storage.SaveMonitoringToken(a.profile, a.cfg.CredentialStorage,
		tokenResp.IDToken, map[string]interface{}(claims))

	// Persist rotated refresh_token (some IdPs rotate on every use)
	if tokenResp.RefreshToken != "" {
		_ = storage.SaveRefreshToken(a.profile, a.cfg.CredentialStorage, tokenResp.RefreshToken)
	}

	debugPrint("Refresh token exchange succeeded — credentials renewed without browser")
	return creds
}

func (a *credentialApp) shouldRecheckQuota() bool {
	if a.cfg.QuotaAPIEndpoint == "" {
		return false
	}
	// Simple interval check - omitting full persistence for now
	return false
}

func (a *credentialApp) performQuotaRecheck() {
	token, _ := storage.GetMonitoringToken(a.profile, a.cfg.CredentialStorage)
	if token == "" {
		return
	}
	claims, err := jwt.DecodePayload(token)
	if err != nil {
		return
	}
	qr := quota.Check(a.cfg.QuotaAPIEndpoint, token, a.cfg.QuotaCheckTimeout, a.cfg.QuotaFailMode)
	_ = claims // suppress unused
	if !qr.Allowed {
		printQuotaBlocked(qr)
	}
}

func printQuotaBlocked(qr *quota.Result) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "============================================================")
	fmt.Fprintln(os.Stderr, "ACCESS BLOCKED - QUOTA EXCEEDED")
	fmt.Fprintln(os.Stderr, "============================================================")
	fmt.Fprintf(os.Stderr, "\n%s\n", qr.Message)
	fmt.Fprintln(os.Stderr, "\nTo request an unblock, contact your administrator.")
	fmt.Fprintln(os.Stderr, "============================================================")
}

func outputJSON(v interface{}) {
	data, _ := json.Marshal(v)
	fmt.Println(string(data))
}

// chainAssumeRole takes existing AWS credentials and assumes a cross-account role
// if bedrock_role_arn is configured. Returns the chained credentials or original.
func chainAssumeRole(creds *federation.AWSCredentials, cfg *config.ProfileConfig) *federation.AWSCredentials {
	if cfg == nil || cfg.BedrockRoleArn == "" {
		return creds
	}

	// Use the existing creds to assume the cross-account role
	client := &http.Client{Timeout: 5 * time.Second}
	// Build STS AssumeRole request using existing credentials
	stsEndpoint := fmt.Sprintf("https://sts.%s.amazonaws.com/", cfg.AWSRegion)
	if cfg.AWSRegion == "" {
		stsEndpoint = "https://sts.us-east-1.amazonaws.com/"
	}

	params := fmt.Sprintf("Action=AssumeRole&Version=2011-06-15&RoleArn=%s&RoleSessionName=nexus-bedrock&DurationSeconds=3600",
		cfg.BedrockRoleArn)

	req, err := http.NewRequest("POST", stsEndpoint, strings.NewReader(params))
	if err != nil {
		return creds
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Sign the request with existing credentials (use AWS SDK-style signing)
	// For simplicity, use the aws CLI via exec
	tmpFile := filepath.Join(os.TempDir(), "nexus-chain-assume.json")
	cmd := exec.Command("aws", "sts", "assume-role",
		"--role-arn", cfg.BedrockRoleArn,
		"--role-session-name", "nexus-bedrock",
		"--region", cfg.AWSRegion,
		"--output", "json")
	cmd.Env = append(os.Environ(),
		"AWS_ACCESS_KEY_ID="+creds.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY="+creds.SecretAccessKey,
		"AWS_SESSION_TOKEN="+creds.SessionToken,
	)
	output, err := cmd.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[nexus] chain-assume failed, using direct credentials")
		return creds
	}
	_ = client
	_ = tmpFile

	var stsResp struct {
		Credentials struct {
			AccessKeyId     string `json:"AccessKeyId"`
			SecretAccessKey string `json:"SecretAccessKey"`
			SessionToken    string `json:"SessionToken"`
			Expiration      string `json:"Expiration"`
		} `json:"Credentials"`
	}
	if err := json.Unmarshal(output, &stsResp); err != nil {
		return creds
	}

	return &federation.AWSCredentials{
		Version:         1,
		AccessKeyID:     stsResp.Credentials.AccessKeyId,
		SecretAccessKey:  stsResp.Credentials.SecretAccessKey,
		SessionToken:    stsResp.Credentials.SessionToken,
		Expiration:      stsResp.Credentials.Expiration,
	}
}

func readActiveOrg(profile string) string {
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".claude-code-session", profile+"-active-org"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveActiveOrg(profile, org string) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".claude-code-session")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, profile+"-active-org"), []byte(org), 0600)
}

// syncCodexConfig fetches the org's Codex configuration from the Nexus API and
// writes ~/.codex/config.toml when codex_enabled is true. It also ensures the
// AWS_BEARER_TOKEN_BEDROCK variable is exported from the user's shell profile.
// The function is non-blocking and best-effort — all errors are silently
// ignored so that a misconfigured or unreachable API never blocks credential
// issuance. A 5-minute cache timestamp at ~/.claude-code-session/codex-sync-ts
// prevents hammering the API on every invocation.
func syncCodexConfig(profile string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	// Check if we synced recently (skip if < 5 min ago)
	cachePath := filepath.Join(home, ".claude-code-session", "codex-sync-ts")
	if data, err := os.ReadFile(cachePath); err == nil {
		if ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			if time.Now().Unix()-ts < 300 {
				return // Synced less than 5 min ago
			}
		}
	}

	// Determine the active org for this profile
	activeOrg := readActiveOrg(profile)
	if activeOrg == "" {
		return
	}

	// Fetch codex config from the Nexus API
	apiBase := nexusAPIBase()
	endpoint := apiBase + "/api/orgs/" + activeOrg + "/codex-config"
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil || resp.StatusCode != 200 {
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || len(body) < 2 {
		return
	}

	// Parse response
	var result struct {
		CodexEnabled bool   `json:"codex_enabled"`
		CodexAPIKey  string `json:"codex_api_key"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return
	}
	if !result.CodexEnabled || result.CodexAPIKey == "" {
		return
	}

	// Create ~/.codex/ if it doesn't exist
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		return
	}

	// Write ~/.codex/config.toml
	configTOML := fmt.Sprintf(`model_provider = "amazon-bedrock"
bedrock_api_key = "%s"
`, result.CodexAPIKey)
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(configTOML), 0600); err != nil {
		return
	}

	// Update the shell profile with AWS_BEARER_TOKEN_BEDROCK
	shellProfile := resolveShellProfile(home)
	if shellProfile != "" {
		updateShellProfileEnv(shellProfile, "AWS_BEARER_TOKEN_BEDROCK", result.CodexAPIKey)
	}

	// Update sync timestamp
	os.MkdirAll(filepath.Dir(cachePath), 0700)
	os.WriteFile(cachePath, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0600)
}

// nexusAPIBase returns the Nexus hub base URL. It respects the
// CCWB_NEXUS_API_BASE env-var override used in tests; otherwise falls back to
// the well-known default from the nexus package.
func nexusAPIBase() string {
	if override := strings.TrimRight(strings.TrimSpace(os.Getenv("CCWB_NEXUS_API_BASE")), "/"); override != "" {
		return override
	}
	// Dev installs hit the dev API Gateway; prod hits the prod one.
	if strings.EqualFold(strings.TrimSpace(os.Getenv("NEXUS_ENV")), "dev") {
		return "https://5ws93rfch3.execute-api.us-east-1.amazonaws.com"
	}
	// Inline the default so this file has no import cycle with internal/nexus.
	return "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com"
}

// nexusCoworkPrefix returns the S3 key prefix for config artifacts (MCP lists, cowork
// configs, skills, version file, binaries). When NEXUS_ENV=dev, dev artifacts live under
// "cowork/dev/" so a dev install never reads or overwrites the prod files that real users
// download. Defaults to prod ("cowork/").
func nexusCoworkPrefix() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("NEXUS_ENV")), "dev") {
		return "cowork/dev/"
	}
	return "cowork/"
}

// nexusDistBase returns the base URL of the S3 distribution bucket (without trailing slash).
func nexusDistBase() string {
	return "https://claude-code-auth-distribution-916587687563.s3.amazonaws.com"
}

// resolveNpxPath returns an absolute path to `npx`, searching the common install
// locations (Homebrew, Volta, nvm, system). Claude Code may spawn MCP servers with a
// minimal PATH that lacks Homebrew, so bare "npx" in an MCP config fails to launch and
// the server shows as disconnected. Writing the absolute path makes MCPs PATH-independent.
// Returns "npx" as a last resort so behavior is never worse than before.
func resolveNpxPath() string {
	// 1. Already on PATH?
	if p, err := exec.LookPath("npx"); err == nil && p != "" {
		return p
	}
	// 2. Common absolute locations.
	home, _ := os.UserHomeDir()
	candidates := []string{
		"/opt/homebrew/bin/npx",             // Apple Silicon Homebrew
		"/usr/local/bin/npx",                // Intel Homebrew / system
		filepath.Join(home, ".volta/bin/npx"),
		filepath.Join(home, ".local/bin/npx"),
		"/usr/bin/npx",
	}
	// nvm: newest version dir under ~/.nvm/versions/node/*/bin/npx
	if matches, _ := filepath.Glob(filepath.Join(home, ".nvm/versions/node/*/bin/npx")); len(matches) > 0 {
		candidates = append(candidates, matches[len(matches)-1])
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return "npx"
}

// mcpLaunchPath returns a PATH value that includes the directory containing node/npx plus
// the common bin dirs. Claude Code may spawn MCP servers with a minimal PATH (e.g. just
// /usr/bin:/bin), and npx is a node script — so without node's dir on PATH the server
// fails with "env: node: No such file or directory". Injecting this PATH into each MCP's
// env makes stdio MCP servers launch reliably regardless of how Claude Code was started.
func mcpLaunchPath() string {
	dirs := []string{}
	seen := map[string]bool{}
	add := func(d string) {
		if d != "" && !seen[d] {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				dirs = append(dirs, d)
				seen[d] = true
			}
		}
	}
	// Directory of the resolved npx (where node usually lives too).
	if p := resolveNpxPath(); p != "npx" {
		add(filepath.Dir(p))
	}
	home, _ := os.UserHomeDir()
	add("/opt/homebrew/bin")
	add("/usr/local/bin")
	add(filepath.Join(home, ".volta/bin"))
	add(filepath.Join(home, ".local/bin"))
	if matches, _ := filepath.Glob(filepath.Join(home, ".nvm/versions/node/*/bin")); len(matches) > 0 {
		add(matches[len(matches)-1])
	}
	// Always keep the base system dirs.
	dirs = append(dirs, "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	return strings.Join(dirs, ":")
}

// resolveShellProfile returns the path of the shell profile file to update.
// Preference order: ~/.zshrc if it exists, ~/.bashrc if it exists, and on
// macOS create ~/.zshrc as the platform default when neither is present.
func resolveShellProfile(home string) string {
	zshrc := filepath.Join(home, ".zshrc")
	bashrc := filepath.Join(home, ".bashrc")

	if _, err := os.Stat(zshrc); err == nil {
		return zshrc
	}
	if _, err := os.Stat(bashrc); err == nil {
		return bashrc
	}
	// Neither exists — create ~/.zshrc on macOS, otherwise ~/.bashrc
	if runtime.GOOS == "darwin" {
		return zshrc
	}
	return bashrc
}

// updateShellProfileEnv appends or replaces an `export KEY=VALUE` line in the
// given shell profile file. If the key is already exported (with any value),
// the existing line is replaced in-place so the file stays idempotent.
func updateShellProfileEnv(profilePath, key, value string) {
	exportLine := fmt.Sprintf("export %s=%s", key, value)
	prefix := "export " + key + "="

	data, err := os.ReadFile(profilePath)
	if err != nil {
		// File may not exist yet — write a new one with just this export
		os.WriteFile(profilePath, []byte(exportLine+"\n"), 0644)
		return
	}

	lines := strings.Split(string(data), "\n")
	updated := false
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			if line == exportLine {
				// Already correct — nothing to do
				return
			}
			lines[i] = exportLine
			updated = true
			break
		}
	}

	var newContent string
	if updated {
		newContent = strings.Join(lines, "\n")
	} else {
		// Key not present — append it
		content := string(data)
		if len(content) > 0 && content[len(content)-1] != '\n' {
			content += "\n"
		}
		content += exportLine + "\n"
		newContent = content
	}
	os.WriteFile(profilePath, []byte(newContent), 0644)
}

// computeMcpRemovals returns the set of MCP keys to remove from a user's config:
// those that Nexus previously managed but that are no longer in the current managed set
// (i.e. the admin disabled or deleted them). User-added MCPs are never in prevManaged,
// so they are never returned here.
func computeMcpRemovals(prevManaged, newManaged map[string]bool) map[string]bool {
	toRemove := map[string]bool{}
	for name := range prevManaged {
		if !newManaged[name] {
			toRemove[name] = true
		}
	}
	return toRemove
}

// reconcileMcpServers applies the managed set to an existing mcpServers map:
//   - adds/updates every MCP in `managed` (preserving existing env vars, e.g. injected tokens)
//   - removes every key in `toRemove` (admin-disabled managed MCPs)
//   - leaves any other key untouched (user-added MCPs)
//
// skipHTTP=true drops __http__ MCPs (used for settings.json, which can't host HTTP MCPs).
// It returns the updated map. This is pure (no I/O) so it can be unit-tested.
func reconcileMcpServers(existing, managed map[string]interface{}, toRemove map[string]bool, skipHTTP bool) map[string]interface{} {
	if existing == nil {
		existing = make(map[string]interface{})
	}
	for name, config := range managed {
		newConfig, _ := config.(map[string]interface{})
		if newConfig != nil && skipHTTP {
			if cmd, _ := newConfig["command"].(string); cmd == "__http__" {
				continue
			}
		}
		// Preserve env vars from the existing config that aren't in the new config
		// (integration tokens get injected into env between syncs).
		if existingConfig, ok := existing[name].(map[string]interface{}); ok && newConfig != nil {
			if existingEnv, ok := existingConfig["env"].(map[string]interface{}); ok && len(existingEnv) > 0 {
				newEnv, _ := newConfig["env"].(map[string]interface{})
				if newEnv == nil {
					newEnv = make(map[string]interface{})
				}
				for k, v := range existingEnv {
					if _, exists := newEnv[k]; !exists {
						newEnv[k] = v
					}
				}
				newConfig["env"] = newEnv
			}
		}
		existing[name] = config
	}
	for name := range toRemove {
		delete(existing, name)
	}
	return existing
}

func syncMcpServers(profile string) {
	// Fetch MCP config from S3 and merge into ~/.claude/settings.json AND ~/.claude.json
	// Non-blocking, best effort — failures are silent
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	claudeJsonPath := filepath.Join(home, ".claude.json")

	// Check if we synced recently (skip if < 5 min ago)
	cachePath := filepath.Join(home, ".claude-code-session", "mcp-sync-ts")
	if data, err := os.ReadFile(cachePath); err == nil {
		if ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			if time.Now().Unix()-ts < 300 {
				return // Synced less than 5 min ago
			}
		}
	}

	// Fetch MCPs from S3
	// Determine active org for org-specific config
	orgID := "allcode"
	orgFiles, _ := filepath.Glob(filepath.Join(home, ".claude-code-session", "*-active-org"))
	for _, f := range orgFiles {
		if data, err := os.ReadFile(f); err == nil && len(data) > 0 {
			orgID = strings.TrimSpace(string(data))
			break
		}
	}

	// Fetch MCPs from S3 (org-specific, falls back to allcode). Env-aware prefix so a
	// dev install reads cowork/dev/... and never the prod files real users download.
	client := &http.Client{Timeout: 3 * time.Second}
	prefix := nexusCoworkPrefix()
	mcpURL := fmt.Sprintf("%s/%sorg-%s-mcps.json", nexusDistBase(), prefix, orgID)
	resp, err := client.Get(mcpURL)
	if err != nil || resp.StatusCode != 200 {
		// Fall back to default
		if resp != nil {
			resp.Body.Close()
		}
		resp, err = client.Get(fmt.Sprintf("%s/%sclaude-code-mcps.json", nexusDistBase(), prefix))
		if err != nil || resp.StatusCode != 200 {
			return
		}
	}
	defer resp.Body.Close()
	mcpData, err := io.ReadAll(resp.Body)
	if err != nil || len(mcpData) < 3 {
		return
	}

	// Parse MCPs
	var mcps map[string]interface{}
	if err := json.Unmarshal(mcpData, &mcps); err != nil {
		return
	}

	// Reconciliation: the S3 file is the authoritative list of MANAGED MCPs for this org.
	// We track which MCP keys Nexus previously managed so we can REMOVE ones the admin has
	// since disabled/deleted, while never touching MCPs the user added themselves.
	managedStatePath := filepath.Join(home, ".claude-code-session", "nexus-managed-mcps.json")
	prevManaged := map[string]bool{}
	if data, err := os.ReadFile(managedStatePath); err == nil {
		var names []string
		if json.Unmarshal(data, &names) == nil {
			for _, n := range names {
				prevManaged[n] = true
			}
		}
	} else {
		// First run after upgrade (no state file yet): seed the managed set with every
		// MCP key Nexus has ever shipped. This lets reconciliation clean up stale entries
		// that accumulated before reconciliation existed (e.g. an MCP the admin later
		// disabled). User-added MCPs are NOT in this list, so they are preserved.
		knownNexusManaged := []string{
			"github", "slack", "hubspot", "activecampaign", "zapier", "nexus-factory",
			"web-search", "partner-central", "atlassian", "jira",
			"google-drive", "google-docs", "google-slides", "google-workspace", "google-docs-&-slides",
		}
		for _, n := range knownNexusManaged {
			prevManaged[n] = true
		}
	}
	// The new managed set = keys currently in the S3 file.
	newManaged := map[string]bool{}
	newManagedList := []string{}
	for name := range mcps {
		newManaged[name] = true
		newManagedList = append(newManagedList, name)
	}
	// MCPs to remove = previously managed but no longer in the S3 file (admin disabled/deleted).
	toRemove := computeMcpRemovals(prevManaged, newManaged)

	// Read current settings
	settingsData, err := os.ReadFile(settingsPath)
	if err != nil {
		// Fresh install: ~/.claude/settings.json may not exist yet (Claude Code hasn't
		// created it). Start from an empty object instead of aborting — otherwise the
		// MCP sync would leave .claude.json empty on first run ("No MCP servers").
		settingsData = []byte("{}")
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(settingsData, &settings); err != nil {
		settings = make(map[string]interface{})
	}
	os.MkdirAll(filepath.Dir(settingsPath), 0700)

	// Reconcile managed MCPs into settings.json (skip __http__ — only .claude.json hosts those).
	existing, _ := settings["mcpServers"].(map[string]interface{})
	existing = reconcileMcpServers(existing, mcps, toRemove, true)
	settings["mcpServers"] = existing
	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(settingsPath, newData, 0600)

	// Also write to ~/.claude.json for Claude Code 2.1+ (reads MCPs from here)
	var claudeJson map[string]interface{}
	if data, err := os.ReadFile(claudeJsonPath); err == nil {
		json.Unmarshal(data, &claudeJson)
	}
	if claudeJson == nil {
		claudeJson = make(map[string]interface{})
	}
	// Convert mcps to the format Claude Code 2.1+ expects.
	// Resolve npx to an absolute path so MCP servers launch even when Claude Code has a
	// minimal PATH (a common cause of "all MCPs disconnected").
	npxPath := resolveNpxPath()
	launchPath := mcpLaunchPath()
	claudeMcps := make(map[string]interface{})
	for name, config := range mcps {
		cfgMap, _ := config.(map[string]interface{})
		if cfgMap == nil {
			continue
		}
		cmd, _ := cfgMap["command"].(string)
		if cmd == "npx" {
			cmd = npxPath
		}

		// HTTP-type MCPs (e.g., Partner Central) — use native HTTP transport
		if cmd == "__http__" {
			url, _ := cfgMap["args"].(string)
			if url == "" {
				// args might be a []interface{} with URL as first element
				if argsArr, ok := cfgMap["args"].([]interface{}); ok && len(argsArr) > 0 {
					url, _ = argsArr[0].(string)
				}
			}
			entry := map[string]interface{}{
				"type": "http",
				"url":  url,
			}
			// If this is our own AgentCore gateway, inject the auth header
			if strings.Contains(url, "gateway.bedrock-agentcore") {
				monToken, _ := storage.GetMonitoringToken(profile, "keyring")
				if monToken != "" {
					entry["headers"] = map[string]interface{}{
						"Authorization": "Bearer " + monToken,
					}
				}
			}
			claudeMcps[name] = entry
			continue
		}

		// Standard stdio MCPs
		entry := map[string]interface{}{
			"type":    "stdio",
			"command": cmd,
			"args":    cfgMap["args"],
		}
		var envMap map[string]interface{}
		if env, ok := cfgMap["env"].(map[string]interface{}); ok {
			envMap = env
		} else {
			envMap = map[string]interface{}{}
		}
		// Inject PATH so node/npx are found even when Claude Code spawns with a minimal PATH
		// (root cause of stdio MCPs showing "failed"). Only set if not already provided.
		if _, has := envMap["PATH"]; !has {
			envMap["PATH"] = launchPath
		}
		entry["env"] = envMap
		claudeMcps[name] = entry
	}
	// Merge: preserve existing user-added MCPs
	existingMcps, _ := claudeJson["mcpServers"].(map[string]interface{})
	if existingMcps == nil {
		existingMcps = make(map[string]interface{})
	}
	for name, config := range claudeMcps {
		// Preserve existing env vars (integration tokens)
		newConfig, _ := config.(map[string]interface{})
		if existingConfig, ok := existingMcps[name].(map[string]interface{}); ok && newConfig != nil {
			if existingEnv, ok := existingConfig["env"].(map[string]interface{}); ok && len(existingEnv) > 0 {
				newEnv, _ := newConfig["env"].(map[string]interface{})
				if newEnv == nil {
					newEnv = make(map[string]interface{})
				}
				for k, v := range existingEnv {
					if _, exists := newEnv[k]; !exists {
						newEnv[k] = v
					}
				}
				newConfig["env"] = newEnv
			}
		}
		existingMcps[name] = config
	}
	// Remove MCPs the admin has disabled/deleted (previously managed, now gone from S3).
	for name := range toRemove {
		delete(existingMcps, name)
	}
	claudeJson["mcpServers"] = existingMcps
	if claudeData, err := json.MarshalIndent(claudeJson, "", "  "); err == nil {
		os.WriteFile(claudeJsonPath, claudeData, 0600)
	}

	// Persist the current managed set so the next sync can compute removals.
	if stateData, err := json.Marshal(newManagedList); err == nil {
		os.MkdirAll(filepath.Dir(managedStatePath), 0700)
		os.WriteFile(managedStatePath, stateData, 0600)
	}

	// Update sync timestamp — but ONLY if we actually populated MCPs. If the config ended
	// up empty (S3 hiccup, first-run race, or genuinely empty catalog), do NOT cache the
	// timestamp so the next run retries instead of leaving "No MCP servers" for 5 minutes.
	if len(existingMcps) > 0 {
		os.MkdirAll(filepath.Dir(cachePath), 0700)
		os.WriteFile(cachePath, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0600)
	}
}


// syncIntegrationTokens fetches per-user OAuth tokens for MCP servers that need them
// (e.g., HubSpot) and injects them as env vars in settings.json.
// hasUnconnectedIntegration reports whether any token-based MCP is present in the user's
// config but still missing its credential env var (i.e. enabled by the admin but not yet
// connected by the user on /me). When true, syncIntegrationTokens bypasses its 10-min
// throttle and checks every run, so a token the user just pasted on /me is picked up almost
// immediately — no commands, no manual restart required.
func hasUnconnectedIntegration(home string) bool {
	// (mcpKey, credentialEnvVar) pairs for token-injected integrations.
	checks := map[string]string{
		"hubspot":        "PRIVATE_APP_ACCESS_TOKEN",
		"activecampaign": "ACTIVECAMPAIGN_API_KEY",
		"zapier":         "ZAPIER_MCP_TOKEN",
		"nexus-factory":  "NEXUS_FACTORY_API_KEY",
		"google-drive":   "GOOGLE_OAUTH_ACCESS_TOKEN",
		"jira":           "JIRA_API_TOKEN",
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return false
	}
	var cfg map[string]interface{}
	if json.Unmarshal(data, &cfg) != nil {
		return false
	}
	servers, _ := cfg["mcpServers"].(map[string]interface{})
	for key, envVar := range checks {
		srv, ok := servers[key].(map[string]interface{})
		if !ok {
			continue // MCP not enabled for this user's org
		}
		env, _ := srv["env"].(map[string]interface{})
		val, _ := env[envVar].(string)
		if val == "" {
			return true // enabled but no token yet → keep checking
		}
	}
	return false
}

// If the user hasn't authenticated with a service yet, opens browser for OAuth.
func syncIntegrationTokens(profile string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	debugPrint("syncIntegrationTokens: starting for profile %s", profile)

	// Check if we synced recently. We normally throttle to every 10 min, BUT if the user
	// has an MCP enabled that is not yet connected (no token), we keep checking every run
	// so a freshly-connected token (pasted on /me) is picked up almost immediately with
	// zero action from the user — no commands, no forced restart.
	cachePath := filepath.Join(home, ".claude-code-session", "integration-token-sync-ts")
	if !hasUnconnectedIntegration(home) {
		if data, err := os.ReadFile(cachePath); err == nil {
			if ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
				if time.Now().Unix()-ts < 600 {
					debugPrint("syncIntegrationTokens: skipping, synced recently")
					return
				}
			}
		}
	}

	// Get monitoring token for API auth
	monToken, _ := storage.GetMonitoringToken(profile, "keyring")
	if monToken == "" {
		debugPrint("syncIntegrationTokens: no monitoring token, skipping")
		return
	}

	debugPrint("syncIntegrationTokens: got monitoring token, checking integrations")

	// integrations that need per-user tokens
	apiBase := nexusAPIBase()
	integrations := []struct {
		name       string
		mcpKey     string
		mcpKeys    []string // if set, inject into all these MCP keys (e.g. the 4 Google MCPs); overrides mcpKey for injection
		envVar     string
		extraEnvs  map[string]string // additional env vars to inject from token response
		tokenURL   string
		connectURL string
	}{
		{
			name:       "hubspot",
			mcpKey:     "hubspot",
			envVar:     "PRIVATE_APP_ACCESS_TOKEN",
			tokenURL:   "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/integrations/hubspot/token",
			connectURL: "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/integrations/hubspot/connect",
		},
		{
			name:       "activecampaign",
			mcpKey:     "activecampaign",
			envVar:     "ACTIVECAMPAIGN_API_KEY",
			extraEnvs:  map[string]string{"account_url": "ACTIVECAMPAIGN_API_URL"},
			tokenURL:   "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/integrations/activecampaign/token",
			connectURL: "https://nexus.allcode.com/me",
		},
		{
			name:       "zapier",
			mcpKey:     "zapier",
			envVar:     "ZAPIER_MCP_TOKEN",
			tokenURL:   "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/integrations/zapier/token",
			connectURL: "https://nexus.allcode.com/me",
		},
		{
			name:       "nexus-factory",
			mcpKey:     "nexus-factory",
			envVar:     "NEXUS_FACTORY_API_KEY",
			tokenURL:   "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/integrations/nexus-factory/token",
			connectURL: "https://nexus.allcode.com/me",
		},
		{
			name:       "google",
			mcpKey:     "google-drive",
			mcpKeys:    []string{"google-drive"},
			envVar:     "GOOGLE_OAUTH_ACCESS_TOKEN",
			// The Workspace MCP needs the user's Google email + OAuth client to load the
			// credential file our file-bridge writes. Inject them from the token response.
			extraEnvs: map[string]string{
				"google_email":  "USER_GOOGLE_EMAIL",
				"client_id":     "GOOGLE_OAUTH_CLIENT_ID",
				"client_secret": "GOOGLE_OAUTH_CLIENT_SECRET",
			},
			tokenURL:   "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/integrations/google/token",
			connectURL: "https://nexus.allcode.com/me",
		},
		{
			name:       "jira",
			mcpKey:     "jira",
			envVar:     "JIRA_API_TOKEN",
			extraEnvs:  map[string]string{"atlassian_url": "JIRA_HOST", "atlassian_email": "JIRA_EMAIL"},
			tokenURL:   "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/integrations/jira/token",
			connectURL: "https://nexus.allcode.com/me",
		},
		{
			// Separate, READ-ONLY Gmail. Its own gmail.readonly-scoped token, written to a
			// dedicated credential dir so it never mixes with the broad google-drive token.
			name:       "gmail",
			mcpKey:     "gmail",
			envVar:     "GMAIL_OAUTH_ACCESS_TOKEN",
			extraEnvs: map[string]string{
				"google_email":  "USER_GOOGLE_EMAIL",
				"client_id":     "GOOGLE_OAUTH_CLIENT_ID",
				"client_secret": "GOOGLE_OAUTH_CLIENT_SECRET",
			},
			tokenURL:   "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/integrations/gmail/token",
			connectURL: "https://nexus.allcode.com/me",
		},
	}

	// Rewrite the hardcoded prod API base to the resolved base (dev when NEXUS_ENV=dev).
	// The literals above use the prod URL as the canonical form; this makes dev installs
	// hit the dev API without duplicating the slice.
	const prodBase = "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com"
	if apiBase != prodBase {
		for i := range integrations {
			integrations[i].tokenURL = strings.Replace(integrations[i].tokenURL, prodBase, apiBase, 1)
			integrations[i].connectURL = strings.Replace(integrations[i].connectURL, prodBase, apiBase, 1)
		}
	}

	settingsPath := filepath.Join(home, ".claude", "settings.json")
	claudeJsonPath := filepath.Join(home, ".claude.json")
	// Cowork (Claude Desktop on Bedrock) reads MCPs from this file. Inject the same per-user
	// tokens here so integrations work in Cowork too, not just Claude Code.
	coworkDesktopPath := filepath.Join(home, "Library", "Application Support", "Claude-3p", "claude_desktop_config.json")

	for _, integ := range integrations {
		// Fetch token from Nexus API
		client := &http.Client{Timeout: 5 * time.Second}
		req, err := http.NewRequest("GET", integ.tokenURL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+monToken)
		resp, err := client.Do(req)
		if err != nil || resp == nil {
			debugPrint("syncIntegrationTokens: %s token fetch failed: %v", integ.name, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		debugPrint("syncIntegrationTokens: %s token response status=%d", integ.name, resp.StatusCode)

		var tokenResp map[string]interface{}
		if err := json.Unmarshal(body, &tokenResp); err != nil {
			continue
		}

		accessToken, _ := tokenResp["access_token"].(string)
		if accessToken == "" {
			// User hasn't connected this integration yet
			// Check if the MCP is even configured before prompting
			settingsData, err := os.ReadFile(settingsPath)
			if err != nil {
				continue
			}
			var settings map[string]interface{}
			if err := json.Unmarshal(settingsData, &settings); err != nil {
				continue
			}
			mcpServers, _ := settings["mcpServers"].(map[string]interface{})
			if mcpServers == nil {
				continue
			}
			// Check if any of this integration's MCP keys is configured.
			checkKeys := integ.mcpKeys
			if len(checkKeys) == 0 {
				checkKeys = []string{integ.mcpKey}
			}
			anyConfigured := false
			for _, mk := range checkKeys {
				if _, exists := mcpServers[mk]; exists {
					anyConfigured = true
					break
				}
			}
			if !anyConfigured {
				continue // MCP not configured, skip
			}
			// Not connected in Nexus — clear any STALE token previously injected into the
			// config so a revoked/removed integration stops working locally (reconciliation).
			for _, mk := range checkKeys {
				clearMcpEnvVar(settingsPath, mk, integ.envVar)
				clearMcpEnvVar(claudeJsonPath, mk, integ.envVar)
				clearMcpEnvVar(coworkDesktopPath, mk, integ.envVar)
				if integ.extraEnvs != nil {
					for _, envVarName := range integ.extraEnvs {
						clearMcpEnvVar(settingsPath, mk, envVarName)
						clearMcpEnvVar(claudeJsonPath, mk, envVarName)
						clearMcpEnvVar(coworkDesktopPath, mk, envVarName)
					}
				}
			}
			// Jira uses a header, not env — clear the stale Authorization header too.
			if integ.name == "jira" {
				clearMcpHeader(claudeJsonPath, "jira", "Authorization")
			}
			// MCP is configured but no token — show user how to connect
			connectCachePath := filepath.Join(home, ".claude-code-session", integ.name+"-connect-prompted")
			if _, err := os.Stat(connectCachePath); err != nil {
				// First time seeing this - show the message
				fmt.Fprintf(os.Stderr, "\n⚠ %s MCP needs authentication.\n  Open this URL in your browser to connect:\n  %s\n  Then restart claude.\n\n", integ.name, integ.connectURL)
				os.MkdirAll(filepath.Dir(connectCachePath), 0700)
				os.WriteFile(connectCachePath, []byte("1"), 0600)
			}
			debugPrint("syncIntegrationTokens: %s not connected, cleared stale token", integ.name)
			continue
		}

		// Got a token — inject into MCP env vars in settings.json.
		// If mcpKeys is set (e.g. the 4 Google MCPs share one token), inject into each.
		targetKeys := integ.mcpKeys
		if len(targetKeys) == 0 {
			targetKeys = []string{integ.mcpKey}
		}
		for _, mk := range targetKeys {
			injectMcpEnvVar(settingsPath, mk, integ.envVar, accessToken)
			injectMcpEnvVar(claudeJsonPath, mk, integ.envVar, accessToken)
			injectMcpEnvVar(coworkDesktopPath, mk, integ.envVar, accessToken)

			// Inject additional env vars from the token response
			// (e.g., account_url -> ACTIVECAMPAIGN_API_URL, atlassian_url -> ATLASSIAN_URL)
			if integ.extraEnvs != nil {
				for responseKey, envVarName := range integ.extraEnvs {
					if val, ok := tokenResp[responseKey].(string); ok && val != "" {
						injectMcpEnvVar(settingsPath, mk, envVarName, val)
						injectMcpEnvVar(claudeJsonPath, mk, envVarName, val)
						injectMcpEnvVar(coworkDesktopPath, mk, envVarName, val)
					}
				}
			}
		}

		// Google file-bridge: the common Google Drive/Docs MCP packages read OAuth tokens
		// from an on-disk credentials file, not an env var. Nexus still owns the token
		// (fetched above, auto-refreshed server-side); we just write it to the file the
		// package expects so the user never has to run a browser OAuth flow themselves.
		if integ.name == "google" {
			writeGoogleCredentialsFile(home, tokenResp)
		}
		if integ.name == "gmail" {
			writeGmailCredentialsFile(home, tokenResp)
			// Point the gmail MCP at its dedicated (read-only) credential dir.
			gmailDir := filepath.Join(home, ".google_workspace_mcp_gmail", "credentials")
			injectMcpEnvVar(settingsPath, "gmail", "GOOGLE_MCP_CREDENTIALS_DIR", gmailDir)
			injectMcpEnvVar(claudeJsonPath, "gmail", "GOOGLE_MCP_CREDENTIALS_DIR", gmailDir)
			injectMcpEnvVar(coworkDesktopPath, "gmail", "GOOGLE_MCP_CREDENTIALS_DIR", gmailDir)
		}
		// Jira uses Atlassian's official Rovo MCP over HTTP with the authv2 OAuth endpoint
		// (matching prod — full tool set). Atlassian handles auth via its own OAuth flow;
		// we do NOT inject an Authorization header (API-token auth limits Atlassian to ~3
		// tools). Ensure any stale Basic-auth header/env from a prior approach is removed.
		if integ.name == "jira" {
			clearMcpHeader(claudeJsonPath, "jira", "Authorization")
			for _, ev := range []string{"JIRA_API_TOKEN", "JIRA_EMAIL", "JIRA_HOST"} {
				clearMcpEnvVar(claudeJsonPath, "jira", ev)
				clearMcpEnvVar(settingsPath, "jira", ev)
			}
		}
	}

	// Update sync timestamp
	os.MkdirAll(filepath.Dir(cachePath), 0700)
	os.WriteFile(cachePath, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0600)
}

// injectMcpHeader adds/updates a header on a specific HTTP MCP server's config in the given
// file (.claude.json). Used for the Jira/Atlassian official HTTP MCP (Authorization: Basic ...).
func injectMcpHeader(claudeJsonPath, mcpKey, headerName, value string) {
	data, err := os.ReadFile(claudeJsonPath)
	if err != nil {
		return
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return
	}
	servers, _ := cfg["mcpServers"].(map[string]interface{})
	if servers == nil {
		return
	}
	mcp, ok := servers[mcpKey].(map[string]interface{})
	if !ok {
		return
	}
	headers, _ := mcp["headers"].(map[string]interface{})
	if headers == nil {
		headers = map[string]interface{}{}
	}
	headers[headerName] = value
	mcp["headers"] = headers
	// Ensure it's marked as an http MCP.
	if _, has := mcp["type"]; !has {
		mcp["type"] = "http"
	}
	servers[mcpKey] = mcp
	cfg["mcpServers"] = servers
	if out, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		os.WriteFile(claudeJsonPath, out, 0600)
	}
}

// clearMcpEnvVar removes an env var from an MCP server's config (used to purge a stale token
// when Nexus no longer has that integration connected — reconciliation).
func clearMcpEnvVar(cfgPath, mcpKey, envVar string) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return
	}
	var cfg map[string]interface{}
	if json.Unmarshal(data, &cfg) != nil {
		return
	}
	servers, _ := cfg["mcpServers"].(map[string]interface{})
	if servers == nil {
		return
	}
	mcp, ok := servers[mcpKey].(map[string]interface{})
	if !ok {
		return
	}
	env, _ := mcp["env"].(map[string]interface{})
	if env == nil {
		return
	}
	if _, present := env[envVar]; !present {
		return
	}
	delete(env, envVar)
	mcp["env"] = env
	servers[mcpKey] = mcp
	cfg["mcpServers"] = servers
	if out, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		os.WriteFile(cfgPath, out, 0600)
	}
}

// clearMcpHeader removes a header from an HTTP MCP server's config (stale Jira Authorization).
func clearMcpHeader(cfgPath, mcpKey, headerName string) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return
	}
	var cfg map[string]interface{}
	if json.Unmarshal(data, &cfg) != nil {
		return
	}
	servers, _ := cfg["mcpServers"].(map[string]interface{})
	if servers == nil {
		return
	}
	mcp, ok := servers[mcpKey].(map[string]interface{})
	if !ok {
		return
	}
	headers, _ := mcp["headers"].(map[string]interface{})
	if headers == nil {
		return
	}
	if _, present := headers[headerName]; !present {
		return
	}
	delete(headers, headerName)
	mcp["headers"] = headers
	servers[mcpKey] = mcp
	cfg["mcpServers"] = servers
	if out, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		os.WriteFile(cfgPath, out, 0600)
	}
}

// injectMcpEnvVar adds/updates an env var in a specific MCP server's config within settings.json
func injectMcpEnvVar(settingsPath, mcpKey, envVar, value string) {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return
	}
	mcpServers, _ := settings["mcpServers"].(map[string]interface{})
	if mcpServers == nil {
		return
	}
	mcpConfig, ok := mcpServers[mcpKey].(map[string]interface{})
	if !ok {
		return
	}
	env, _ := mcpConfig["env"].(map[string]interface{})
	if env == nil {
		env = make(map[string]interface{})
	}
	env[envVar] = value
	mcpConfig["env"] = env
	mcpServers[mcpKey] = mcpConfig
	settings["mcpServers"] = mcpServers
	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(settingsPath, newData, 0600)
}

// writeGoogleCredentialsFile bridges Nexus's per-user Google OAuth token to the on-disk
// credentials file that the Google Drive/Docs/Slides MCP packages expect. Nexus owns and
// refreshes the token server-side; this just materializes the current token in the format
// and locations those packages read, so the user never runs a browser OAuth flow locally.
func writeGoogleCredentialsFile(home string, tokenResp map[string]interface{}) {
	accessToken, _ := tokenResp["access_token"].(string)
	if accessToken == "" {
		return
	}
	refreshToken, _ := tokenResp["refresh_token"].(string)
	clientID, _ := tokenResp["client_id"].(string)
	clientSecret, _ := tokenResp["client_secret"].(string)
	googleEmail, _ := tokenResp["google_email"].(string)
	tokenURI, _ := tokenResp["token_uri"].(string)
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	scopesStr, _ := tokenResp["scopes"].(string)

	// ISO expiry (workspace MCP) + ms epoch (legacy packages).
	var expiryUnix int64
	if ea, ok := tokenResp["expires_at"].(float64); ok && ea > 0 {
		expiryUnix = int64(ea)
	} else if ei, ok := tokenResp["expires_in"].(float64); ok && ei > 0 {
		expiryUnix = time.Now().Unix() + int64(ei)
	} else {
		expiryUnix = time.Now().Unix() + 3600
	}
	expiryISO := time.Unix(expiryUnix, 0).UTC().Format("2006-01-02T15:04:05")

	// ---- taylorwilsdon/google_workspace_mcp format (Gmail+Calendar+Drive+Docs+Sheets+Slides).
	// It reads <WORKSPACE_MCP_CREDENTIALS_DIR>/<url-encoded-email>.json indexed by Google email.
	if googleEmail != "" {
		scopeList := []string{}
		for _, s := range strings.Fields(scopesStr) {
			scopeList = append(scopeList, s)
		}
		wsCreds := map[string]interface{}{
			"token":         accessToken,
			"refresh_token": refreshToken,
			"token_uri":     tokenURI,
			"client_id":     clientID,
			"client_secret": clientSecret,
			"scopes":        scopeList,
			"expiry":        expiryISO,
		}
		wsDir := filepath.Join(home, ".google_workspace_mcp", "credentials")
		os.MkdirAll(wsDir, 0700)
		if wd, err := json.MarshalIndent(wsCreds, "", "  "); err == nil {
			// url-encode the email the same way the MCP does (safe chars @._-).
			safe := urlEncodeEmail(googleEmail)
			os.WriteFile(filepath.Join(wsDir, safe+".json"), wd, 0600)
		}
	}

	// ---- Legacy @piotr-agier / server-gdrive format (kept for backward compat).
	var expiryMs int64 = expiryUnix * 1000
	creds := map[string]interface{}{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expiry_date":  expiryMs,
	}
	if refreshToken != "" {
		creds["refresh_token"] = refreshToken
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return
	}
	gdriveDir := filepath.Join(home, ".config", "google-drive-mcp")
	os.MkdirAll(gdriveDir, 0700)
	for _, p := range []string{
		filepath.Join(gdriveDir, "tokens.json"),
		filepath.Join(gdriveDir, "credentials.json"),
		filepath.Join(home, ".gdrive-server-credentials.json"),
	} {
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, data, 0600)
	}
	if clientID != "" {
		keys := map[string]interface{}{
			"installed": map[string]interface{}{
				"client_id":     clientID,
				"client_secret": clientSecret,
				"redirect_uris": []string{"http://localhost"},
				"auth_uri":      "https://accounts.google.com/o/oauth2/auth",
				"token_uri":     tokenURI,
			},
		}
		if kd, err := json.MarshalIndent(keys, "", "  "); err == nil {
			os.WriteFile(filepath.Join(gdriveDir, "gcp-oauth.keys.json"), kd, 0600)
		}
	}
	debugPrint("writeGoogleCredentialsFile: wrote Google token for %s (workspace + legacy)", googleEmail)
}

// writeGmailCredentialsFile writes the READ-ONLY Gmail token to a SEPARATE workspace-mcp
// credential dir (~/.google_workspace_mcp_gmail/credentials) so it never mixes with the broad
// google-drive token. The gmail MCP entry points GOOGLE_MCP_CREDENTIALS_DIR here.
func writeGmailCredentialsFile(home string, tokenResp map[string]interface{}) {
	accessToken, _ := tokenResp["access_token"].(string)
	if accessToken == "" {
		return
	}
	googleEmail, _ := tokenResp["google_email"].(string)
	if googleEmail == "" {
		return
	}
	tokenURI, _ := tokenResp["token_uri"].(string)
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	scopeList := []string{}
	if s, _ := tokenResp["scopes"].(string); s != "" {
		scopeList = strings.Fields(s)
	}
	var expiryUnix int64
	if ea, ok := tokenResp["expires_at"].(float64); ok && ea > 0 {
		expiryUnix = int64(ea)
	} else {
		expiryUnix = time.Now().Unix() + 3600
	}
	creds := map[string]interface{}{
		"token":         accessToken,
		"refresh_token": tokenResp["refresh_token"],
		"token_uri":     tokenURI,
		"client_id":     tokenResp["client_id"],
		"client_secret": tokenResp["client_secret"],
		"scopes":        scopeList,
		"expiry":        time.Unix(expiryUnix, 0).UTC().Format("2006-01-02T15:04:05"),
	}
	dir := filepath.Join(home, ".google_workspace_mcp_gmail", "credentials")
	os.MkdirAll(dir, 0700)
	if d, err := json.MarshalIndent(creds, "", "  "); err == nil {
		os.WriteFile(filepath.Join(dir, urlEncodeEmail(googleEmail)+".json"), d, 0600)
	}
	debugPrint("writeGmailCredentialsFile: wrote gmail.readonly token for %s", googleEmail)
}

// urlEncodeEmail mirrors Python's urllib.parse.quote(email, safe="@._-") used by the
// Workspace MCP to name its per-user credential file.
func urlEncodeEmail(email string) string {
	var b strings.Builder
	for _, r := range email {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			r == '@' || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteString(fmt.Sprintf("%%%02X", r))
		}
	}
	return b.String()
}

func syncSkills() {
	// Fetch skills from S3 and write to ~/.claude/CLAUDE.md
	// Non-blocking, best effort — failures are silent
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	// Check if we synced recently (skip if < 5 min ago)
	cachePath := filepath.Join(home, ".claude-code-session", "skills-sync-ts")
	if data, err := os.ReadFile(cachePath); err == nil {
		if ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			if time.Now().Unix()-ts < 300 {
				return
			}
		}
	}

	// Determine active org
	orgID := "allcode"
	orgFiles, _ := filepath.Glob(filepath.Join(home, ".claude-code-session", "*-active-org"))
	for _, f := range orgFiles {
		if data, err := os.ReadFile(f); err == nil && len(data) > 0 {
			orgID = strings.TrimSpace(string(data))
			break
		}
	}

	// Fetch skills from S3 (org-specific, falls back to allcode)
	client := &http.Client{Timeout: 3 * time.Second}
	skillsURL := fmt.Sprintf("https://claude-code-auth-distribution-916587687563.s3.amazonaws.com/cowork/org-%s-skills.json", orgID)
	resp, err := client.Get(skillsURL)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		resp, err = client.Get("https://claude-code-auth-distribution-916587687563.s3.amazonaws.com/cowork/claude-code-skills.json")
		if err != nil || resp.StatusCode != 200 {
			return
		}
	}
	defer resp.Body.Close()
	skillsData, err := io.ReadAll(resp.Body)
	if err != nil || len(skillsData) < 3 {
		return
	}

	// Parse skills
	var skills []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
		Prompt      string `json:"prompt"`
	}
	if err := json.Unmarshal(skillsData, &skills); err != nil {
		return
	}
	if len(skills) == 0 {
		return
	}

	// Generate CLAUDE.md content from skills
	var content strings.Builder
	content.WriteString("# Organization Skills (Managed by AllCode Nexus)\n\n")
	content.WriteString("The following skills are provided by your organization:\n\n")
	for _, skill := range skills {
		content.WriteString("## " + skill.Name + "\n")
		if skill.Description != "" {
			content.WriteString(skill.Description + "\n")
		}
		content.WriteString("\n" + skill.Prompt + "\n\n")
	}

	// Write to ~/.claude/CLAUDE.md (for Claude Code)
	claudeDir := filepath.Join(home, ".claude")
	os.MkdirAll(claudeDir, 0700)
	claudeMdPath := filepath.Join(claudeDir, "CLAUDE.md")
	os.WriteFile(claudeMdPath, []byte(content.String()), 0600)

	// Write to org-plugins directory (for Claude Desktop)
	// macOS: /Library/Application Support/Claude/org-plugins/nexus-skills/
	// Windows: C:\Program Files\Claude\org-plugins\nexus-skills\
	var pluginDir string
	if runtime.GOOS == "darwin" {
		pluginDir = "/Library/Application Support/Claude/org-plugins/nexus-skills"
	} else if runtime.GOOS == "windows" {
		pluginDir = filepath.Join(os.Getenv("ProgramFiles"), "Claude", "org-plugins", "nexus-skills")
	}
	if pluginDir != "" {
		skillsDir := filepath.Join(pluginDir, "skills")
		pluginJsonDir := filepath.Join(pluginDir, ".claude-plugin")

		// Only write if directory exists (created by install script with admin perms)
		if _, err := os.Stat(pluginDir); err == nil {
			// Write plugin.json
			os.MkdirAll(pluginJsonDir, 0755)
			os.WriteFile(filepath.Join(pluginJsonDir, "plugin.json"), []byte(`{"name":"nexus-skills","version":"1.0.0","description":"Organization skills managed by AllCode Nexus","installationPreference":"required"}`), 0644)

			// Write version.json (use timestamp to trigger re-sync)
			os.WriteFile(filepath.Join(pluginDir, "version.json"), []byte(`{"version":"`+strconv.FormatInt(time.Now().Unix(), 10)+`"}`), 0644)

			// Write each skill as a SKILL.md
			for _, skill := range skills {
				skillDir := filepath.Join(skillsDir, strings.ToLower(strings.ReplaceAll(skill.Name, " ", "-")))
				os.MkdirAll(skillDir, 0755)
				skillContent := "---\nname: " + skill.Name + "\ndescription: " + skill.Description + "\n---\n\n" + skill.Prompt + "\n"
				os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644)
			}
		}
	}

	// Update sync timestamp
	os.MkdirAll(filepath.Dir(cachePath), 0700)
	os.WriteFile(cachePath, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0600)
}

func syncManagedConfig(profile string) {
	// Fetch the latest cowork config from S3 and update managed_config.json
	// This keeps Claude Desktop's managed MCPs in sync with the Nexus portal
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	// Check if we synced recently (skip if < 5 min ago)
	cachePath := filepath.Join(home, ".claude-code-session", "cowork-sync-ts")
	if data, err := os.ReadFile(cachePath); err == nil {
		if ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			if time.Now().Unix()-ts < 300 {
				return
			}
		}
	}

	// Fetch cowork config from S3 (org-specific based on profile)
	client := &http.Client{Timeout: 3 * time.Second}

	// Determine org from profile name (e.g., "lets-play-us-east-2" → "lets-play")
	orgID := "allcode"
	parts := strings.Split(profile, "-")
	if len(parts) >= 3 {
		// Remove the last 2 parts (region like "us-east-2") to get org slug
		regionParts := 0
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] == "east" || parts[i] == "west" || parts[i] == "us" || len(parts[i]) <= 2 {
				regionParts++
			} else {
				break
			}
		}
		if regionParts > 0 && regionParts < len(parts) {
			orgID = strings.Join(parts[:len(parts)-regionParts], "-")
		}
	}
	if profile == "allcode-dev-us-east-1" {
		orgID = "allcode"
	}

	// Try org-specific config first, fall back to default
	coworkURL := fmt.Sprintf("https://claude-code-auth-distribution-916587687563.s3.amazonaws.com/cowork/org-%s-cowork-3p-config.json", orgID)
	resp, err := client.Get(coworkURL)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		resp, err = client.Get("https://claude-code-auth-distribution-916587687563.s3.amazonaws.com/cowork/cowork-3p-config.json")
	}
	if err != nil || resp.StatusCode != 200 {
		return
	}
	defer resp.Body.Close()
	configData, err := io.ReadAll(resp.Body)
	if err != nil || len(configData) < 10 {
		return
	}

	// Parse the config
	var remoteConfig map[string]interface{}
	if err := json.Unmarshal(configData, &remoteConfig); err != nil {
		return
	}

	// Read current managed_config.json (if exists)
	managedConfigPath := filepath.Join(home, "Library", "Application Support", "Claude", "managed_config.json")
	managedConfigPath3P := filepath.Join(home, "Library", "Application Support", "Claude-3p", "managed_config.json")
	var localConfig map[string]interface{}
	if data, err := os.ReadFile(managedConfigPath); err == nil {
		json.Unmarshal(data, &localConfig)
	}
	if localConfig == nil {
		localConfig = make(map[string]interface{})
	}

	// Update managedMcpServers from remote config
	if mcps, ok := remoteConfig["managedMcpServers"]; ok {
		localConfig["managedMcpServers"] = mcps
	}

	// Inject per-user integration tokens into managed MCP env vars (HubSpot, ActiveCampaign, Zapier, Nexus Factory)
	if mcps, ok := localConfig["managedMcpServers"].([]interface{}); ok {
		monToken, _ := storage.GetMonitoringToken(profile, "keyring")
		if monToken != "" {
			client := &http.Client{Timeout: 5 * time.Second}
			type mcpTokenConfig struct {
				mcpName  string
				tokenURL string
				envMap   map[string]string // response field → env var name
			}
			tokenConfigs := []mcpTokenConfig{
				{"HubSpot", "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/integrations/hubspot/token", map[string]string{"access_token": "PRIVATE_APP_ACCESS_TOKEN"}},
				{"ActiveCampaign", "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/integrations/activecampaign/token", map[string]string{"access_token": "ACTIVECAMPAIGN_API_KEY", "account_url": "ACTIVECAMPAIGN_API_URL"}},
				{"Zapier", "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/integrations/zapier/token", map[string]string{"access_token": "ZAPIER_MCP_TOKEN"}},
				{"Nexus Factory", "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/integrations/nexus-factory/token", map[string]string{"access_token": "NEXUS_FACTORY_API_KEY"}},
			}

			for i, mcp := range mcps {
				mcpMap, _ := mcp.(map[string]interface{})
				if mcpMap == nil {
					continue
				}
				name, _ := mcpMap["name"].(string)
				for _, tc := range tokenConfigs {
					if name == tc.mcpName {
						req, _ := http.NewRequest("GET", tc.tokenURL, nil)
						if req == nil {
							break
						}
						req.Header.Set("Authorization", "Bearer "+monToken)
						resp, err := client.Do(req)
						if err != nil || resp == nil {
							break
						}
						body, _ := io.ReadAll(resp.Body)
						resp.Body.Close()
						var tokenResp map[string]interface{}
						json.Unmarshal(body, &tokenResp)
						env, _ := mcpMap["env"].(map[string]interface{})
						if env == nil {
							env = make(map[string]interface{})
						}
						injected := false
						for respField, envVar := range tc.envMap {
							if val, ok := tokenResp[respField].(string); ok && val != "" {
								env[envVar] = val
								injected = true
							}
						}
						if injected {
							mcpMap["env"] = env
							mcps[i] = mcpMap
						}
						break
					}
				}
			}
			localConfig["managedMcpServers"] = mcps
		}
	}

	// Also sync other config fields (but preserve inferenceCredentialHelper which is local-path specific)
	for _, key := range []string{"isDesktopExtensionEnabled", "isDesktopExtensionDirectoryEnabled", "isLocalDevMcpEnabled", "isClaudeCodeForDesktopEnabled", "inferenceProvider", "inferenceBedrockRegion", "inferenceBedrockProfile", "inferenceCredentialHelper", "inferenceCredentialHelperTtlSec", "inferenceModels", "otlpEndpoint", "otlpProtocol", "coworkEgressAllowedHosts"} {
		if v, ok := remoteConfig[key]; ok {
			localConfig[key] = v
		}
	}

	// Write updated managed_config.json
	// Inject per-user otlpHeaders for Cowork telemetry attribution
	otelFiles, _ := filepath.Glob(filepath.Join(home, ".claude-code-session", "*-otel-headers.raw"))
	for _, of := range otelFiles {
		if data, err := os.ReadFile(of); err == nil {
			var otelHeaders map[string]interface{}
			if json.Unmarshal(data, &otelHeaders) == nil {
				if email, ok := otelHeaders["x-user-email"].(string); ok && email != "" {
					localConfig["otlpHeaders"] = fmt.Sprintf("x-user-email=%s", email)
					break
				}
			}
		}
	}

	newData, err := json.MarshalIndent(localConfig, "", "  ")
	if err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(managedConfigPath), 0700)
	os.WriteFile(managedConfigPath, newData, 0600)

	// Also write to Claude-3p directory (3rd-party/Bedrock variant)
	os.MkdirAll(filepath.Dir(managedConfigPath3P), 0700)
	os.WriteFile(managedConfigPath3P, newData, 0600)

	// Write mcpServers to Claude-3p/claude_desktop_config.json (Cowork on Bedrock reads from here)
	desktopConfigPath := filepath.Join(home, "Library", "Application Support", "Claude-3p", "claude_desktop_config.json")
	var desktopConfig map[string]interface{}
	if data, err := os.ReadFile(desktopConfigPath); err == nil {
		json.Unmarshal(data, &desktopConfig)
	}
	if desktopConfig == nil {
		desktopConfig = make(map[string]interface{})
	}
	if mcps, ok := localConfig["managedMcpServers"].([]interface{}); ok {
		mcpServers, _ := desktopConfig["mcpServers"].(map[string]interface{})
		if mcpServers == nil {
			mcpServers = make(map[string]interface{})
		}
		for _, mcp := range mcps {
			mcpMap, _ := mcp.(map[string]interface{})
			if mcpMap == nil {
				continue
			}
			name, _ := mcpMap["name"].(string)
			if name == "" {
				continue
			}
			key := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, " ", "-"), "&", "and"))
			entry := map[string]interface{}{
				"command": mcpMap["command"],
				"args":    mcpMap["args"],
			}
			if env, ok := mcpMap["env"].(map[string]interface{}); ok && len(env) > 0 {
				entry["env"] = env
			}
			mcpServers[key] = entry
		}
		desktopConfig["mcpServers"] = mcpServers
	}
	if desktopData, err := json.MarshalIndent(desktopConfig, "", "  "); err == nil {
		os.WriteFile(desktopConfigPath, desktopData, 0600)
	}

	// Update Claude Desktop managed preferences plist (if writable)
	// This is the file Claude Desktop actually reads for managed MCPs
	usr, _ := user.Current()
	if usr != nil {
		plistPath := filepath.Join("/Library/Managed Preferences", usr.Username, "com.anthropic.claudefordesktop.plist")
		if _, err := os.Stat(plistPath); err == nil {
			// Update managedMcpServers
			if mcps, ok := remoteConfig["managedMcpServers"]; ok {
				mcpJSON, _ := json.Marshal(mcps)
				exec.Command("plutil", "-replace", "managedMcpServers", "-string", string(mcpJSON), plistPath).Run()
			}

			// Update OTel config for Desktop telemetry
			if endpoint, ok := remoteConfig["otlpEndpoint"].(string); ok && endpoint != "" {
				exec.Command("plutil", "-replace", "otlpEndpoint", "-string", endpoint, plistPath).Run()
			}
			if protocol, ok := remoteConfig["otlpProtocol"].(string); ok && protocol != "" {
				exec.Command("plutil", "-replace", "otlpProtocol", "-string", protocol, plistPath).Run()
			}

			// Set per-user otlpHeaders from cached otel-headers (for per-user attribution)
			userEmail := ""
			otelFiles, _ := filepath.Glob(filepath.Join(home, ".claude-code-session", "*-otel-headers.raw"))
			for _, of := range otelFiles {
				if data, err := os.ReadFile(of); err == nil {
					var headers map[string]interface{}
					if json.Unmarshal(data, &headers) == nil {
						if email, ok := headers["x-user-email"].(string); ok && email != "" {
							userEmail = email
							break
						}
					}
				}
			}
			if userEmail != "" {
				headers := fmt.Sprintf("x-user-email=%s", userEmail)
				exec.Command("plutil", "-replace", "otlpHeaders", "-string", headers, plistPath).Run()
			}
		}
	}

	// Windows: Update registry for Claude Desktop managed config
	if runtime.GOOS == "windows" {
		regPath := `HKLM\SOFTWARE\Policies\Anthropic\ClaudeForDesktop`
		// Update managedMcpServers
		if mcps, ok := remoteConfig["managedMcpServers"]; ok {
			mcpJSON, _ := json.Marshal(mcps)
			exec.Command("reg", "add", regPath, "/v", "managedMcpServers", "/t", "REG_SZ", "/d", string(mcpJSON), "/f").Run()
		}
		// Update OTel config
		if endpoint, ok := remoteConfig["otlpEndpoint"].(string); ok && endpoint != "" {
			exec.Command("reg", "add", regPath, "/v", "otlpEndpoint", "/t", "REG_SZ", "/d", endpoint, "/f").Run()
		}
		if protocol, ok := remoteConfig["otlpProtocol"].(string); ok && protocol != "" {
			exec.Command("reg", "add", regPath, "/v", "otlpProtocol", "/t", "REG_SZ", "/d", protocol, "/f").Run()
		}
		// Set per-user otlpHeaders
		userEmail := ""
		otelFiles, _ := filepath.Glob(filepath.Join(home, ".claude-code-session", "*-otel-headers.raw"))
		for _, of := range otelFiles {
			if data, err := os.ReadFile(of); err == nil {
				var headers map[string]interface{}
				if json.Unmarshal(data, &headers) == nil {
					if email, ok := headers["x-user-email"].(string); ok && email != "" {
						userEmail = email
						break
					}
				}
			}
		}
		if userEmail != "" {
			hdrs := fmt.Sprintf("x-user-email=%s", userEmail)
			exec.Command("reg", "add", regPath, "/v", "otlpHeaders", "/t", "REG_SZ", "/d", hdrs, "/f").Run()
		}
	}

	// Update sync timestamp
	os.MkdirAll(filepath.Dir(cachePath), 0700)
	os.WriteFile(cachePath, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0600)
}


// reportPlatform sends the user's OS/arch/tool info to the Nexus API for the Users page Platform column.
func reportPlatform(idToken string) {
	// Extract email from ID token
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return
	}
	email, _ := claims["email"].(string)
	if email == "" {
		return
	}

	body, _ := json.Marshal(map[string]string{
		"email":    email,
		"platform": runtime.GOOS,
		"arch":     runtime.GOARCH,
		"tool":     "claude-code",
	})

	client := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest("POST", "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/users/platform", bytes.NewReader(body))
	if req != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+idToken)
		client.Do(req)
	}
}

func checkForUpdate() {
	// Self-update: check S3 for newer binary version, download and replace if available
	// Throttled to once per 24 hours
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	cachePath := filepath.Join(home, ".claude-code-session", "update-check-ts")
	if data, err := os.ReadFile(cachePath); err == nil {
		if ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			if time.Now().Unix()-ts < 3600 { // 1 hour
				return
			}
		}
	}

	// Determine platform binary name
	var binaryName string
	switch runtime.GOOS + "_" + runtime.GOARCH {
	case "darwin_arm64":
		binaryName = "credential-process-darwin-arm64"
	case "darwin_amd64":
		binaryName = "credential-process-darwin-amd64"
	case "linux_amd64":
		binaryName = "credential-process-linux-amd64"
	case "linux_arm64":
		binaryName = "credential-process-linux-arm64"
	case "windows_amd64":
		binaryName = "credential-process-windows-amd64.exe"
	default:
		return
	}

	// Fetch version file (env-aware: dev installs check cowork/dev/, prod checks cowork/)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("%s/%scredential-process-version.json", nexusDistBase(), nexusCoworkPrefix()))
	if err != nil || resp.StatusCode != 200 {
		return
	}
	defer resp.Body.Close()
	versionData, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	var versionInfo map[string]interface{}
	if err := json.Unmarshal(versionData, &versionInfo); err != nil {
		return
	}

	// Get remote hash for our platform
	hashKey := "sha256_" + runtime.GOOS + "_" + runtime.GOARCH
	remoteHash, _ := versionInfo[hashKey].(string)
	if remoteHash == "" {
		// Save timestamp so we don't check again for 24h
		os.MkdirAll(filepath.Dir(cachePath), 0700)
		os.WriteFile(cachePath, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0600)
		return
	}

	// Get current binary hash
	execPath, err := os.Executable()
	if err != nil {
		return
	}
	execPath, _ = filepath.EvalSymlinks(execPath)
	currentData, err := os.ReadFile(execPath)
	if err != nil {
		return
	}

	// Calculate SHA256
	currentHash := fmt.Sprintf("%x", sha256Sum(currentData))
	if currentHash == remoteHash {
		// Already up to date
		os.MkdirAll(filepath.Dir(cachePath), 0700)
		os.WriteFile(cachePath, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0600)
		return
	}

	// Download new binary (env-aware prefix)
	dlResp, err := client.Get(nexusDistBase() + "/" + nexusCoworkPrefix() + binaryName)
	if err != nil || dlResp.StatusCode != 200 {
		return
	}
	defer dlResp.Body.Close()
	newBinary, err := io.ReadAll(dlResp.Body)
	if err != nil || len(newBinary) < 1000 {
		return
	}

	// Verify hash of downloaded binary
	dlHash := fmt.Sprintf("%x", sha256Sum(newBinary))
	if dlHash != remoteHash {
		return // Hash mismatch — don't install
	}

	// Replace self: write to temp file, then rename
	tmpPath := execPath + ".update"
	if err := os.WriteFile(tmpPath, newBinary, 0755); err != nil {
		return
	}

	// Atomic rename
	if err := os.Rename(tmpPath, execPath); err != nil {
		os.Remove(tmpPath)
		return
	}

	fmt.Fprintln(os.Stderr, "[nexus] Updated credential-process to latest version")

	// Save timestamp
	os.MkdirAll(filepath.Dir(cachePath), 0700)
	os.WriteFile(cachePath, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0600)
}

func sha256Sum(data []byte) [32]byte {
	return sha256.Sum256(data)
}

func clearAwsCliCacheIfExpired(profile string) {
	// The AWS CLI/SDK caches credential_process results at ~/.aws/cli/cache/
	// If our credentials expired, this stale cache causes 403 errors even after
	// the credential-process would return fresh ones (because the CLI doesn't re-call us).
	// Fix: proactively clear the CLI cache when our creds are expired.
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	// Check if our cached credentials are expired
	creds, err := storage.ReadFromCredentialsFile(profile)
	if err != nil || creds == nil {
		return
	}
	remaining := storage.ParseExpirationSeconds(creds.Expiration)
	if remaining > 60 {
		return // Not expired yet, no need to clear
	}

	// Credentials expired or about to expire — clear the AWS CLI cache
	cacheDir := filepath.Join(home, ".aws", "cli", "cache")
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			os.Remove(filepath.Join(cacheDir, entry.Name()))
		}
	}
}
