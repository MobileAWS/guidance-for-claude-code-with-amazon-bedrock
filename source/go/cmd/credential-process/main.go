package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ccwb-go/internal/config"
	"ccwb-go/internal/federation"
	"ccwb-go/internal/jwt"
	"ccwb-go/internal/nexus"
	"ccwb-go/internal/oidc"
	"ccwb-go/internal/otel"
	"ccwb-go/internal/portlock"
	"ccwb-go/internal/provider"
	"ccwb-go/internal/quota"
	"ccwb-go/internal/storage"
	"ccwb-go/internal/version"
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
		creds, err = storage.ReadFromCredentialsFile(a.profile)
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
	return storage.SaveToCredentialsFile(creds, a.profile)
}

func (a *credentialApp) clearCache() {
	if a.cfg.CredentialStorage == "keyring" {
		_ = storage.ClearKeyring(a.profile)
	}
	// Also clear session file
	expired := &federation.AWSCredentials{
		Version: 1, AccessKeyID: "EXPIRED", SecretAccessKey: "EXPIRED",
		SessionToken: "EXPIRED", Expiration: "2000-01-01T00:00:00Z",
	}
	_ = storage.SaveToCredentialsFile(expired, a.profile)
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

	// Sync MCP servers from Nexus (quick, with timeout)
	syncMcpServers()

	// Sync Codex config from Nexus (quick, with timeout)
	syncCodexConfig(a.profile, a.cfg)

	// Check cache first
	if cached := a.getCachedCredentials(); cached != nil {
		// Periodic quota re-check
		if a.shouldRecheckQuota() {
			a.performQuotaRecheck()
		}
		outputJSON(cached)
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
				outputJSON(cached)
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
		outputJSON(cached)
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
		outputJSON(creds)
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
		outputJSON(creds)
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

// syncCodexConfig fetches Codex configuration from the Nexus API and writes
// ~/.codex/config.toml when codex is enabled for the active org. It also
// injects AWS_BEARER_TOKEN_BEDROCK into the user's shell profile so the
// Codex CLI can authenticate against Amazon Bedrock. Non-blocking, best-effort
// — all errors are silent, matching the syncMcpServers() pattern.
func syncCodexConfig(profile string, cfg *config.ProfileConfig) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	// Check timestamp cache — skip if synced within the last 5 minutes.
	cachePath := filepath.Join(home, ".claude-code-session", "codex-sync-ts")
	if data, err := os.ReadFile(cachePath); err == nil {
		if ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			if time.Now().Unix()-ts < 300 {
				return // Synced less than 5 minutes ago.
			}
		}
	}

	// Determine the active org — skip silently if none is set.
	org := readActiveOrg(profile)
	if org == "" {
		return
	}

	// Derive the Nexus API base URL from QuotaAPIEndpoint by stripping the
	// trailing "/quota" path component. Fall back to nexus.DefaultAPIBase.
	apiBase := ""
	if cfg != nil && cfg.QuotaAPIEndpoint != "" {
		apiBase = strings.TrimSuffix(
			strings.TrimRight(cfg.QuotaAPIEndpoint, "/"),
			"/quota",
		)
		apiBase = strings.TrimRight(apiBase, "/")
	}
	if apiBase == "" {
		apiBase = nexus.ResolveBase("")
	}

	// Retrieve the cached monitoring token for Bearer auth.
	storageType := ""
	if cfg != nil {
		storageType = cfg.CredentialStorage
	}
	monToken, err := storage.GetMonitoringToken(profile, storageType)
	if err != nil || monToken == "" {
		return
	}

	// Fetch org Codex config: GET {apiBase}/api/orgs/{org}/codex-config
	endpoint := apiBase + "/api/orgs/" + org + "/codex-config"
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+monToken)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil || len(body) < 2 {
		return
	}

	var codexResp struct {
		CodexEnabled bool   `json:"codex_enabled"`
		CodexAPIKey  string `json:"codex_api_key"`
	}
	if err := json.Unmarshal(body, &codexResp); err != nil {
		return
	}

	// Only proceed when Codex is enabled and an API key is present.
	if !codexResp.CodexEnabled || codexResp.CodexAPIKey == "" {
		return
	}

	// Create ~/.codex/ directory if it doesn't exist.
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		return
	}

	// Write ~/.codex/config.toml using simple string formatting — no external
	// TOML library needed for this minimal two-key file.
	const configTOML = "model_provider = \"amazon-bedrock\"\nmodel = \"anthropic.claude-opus-4-5\"\n"
	codexConfigPath := filepath.Join(codexDir, "config.toml")
	if err := os.WriteFile(codexConfigPath, []byte(configTOML), 0600); err != nil {
		return
	}

	// Inject / update AWS_BEARER_TOKEN_BEDROCK in the user's shell profile.
	updateShellProfileCodex(home, codexResp.CodexAPIKey)

	// Update the sync timestamp.
	os.MkdirAll(filepath.Dir(cachePath), 0700)
	os.WriteFile(cachePath, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0600)
}

// updateShellProfileCodex writes / replaces the AWS_BEARER_TOKEN_BEDROCK
// export line in the user's preferred shell profile file. It uses a
// "# Codex Bedrock" marker comment to locate and replace any previous entry
// so repeated runs stay idempotent. Non-blocking — silent on error.
func updateShellProfileCodex(home, apiKey string) {
	// Choose the shell profile file: ~/.zshrc > ~/.bashrc > ~/.profile
	shellProfile := ""
	for _, candidate := range []string{
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".profile"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			shellProfile = candidate
			break
		}
	}
	if shellProfile == "" {
		// None exists — create ~/.profile as a safe universal fallback.
		shellProfile = filepath.Join(home, ".profile")
	}

	const marker = "# Codex Bedrock"
	exportLine := "export AWS_BEARER_TOKEN_BEDROCK=" + apiKey

	// Read existing file content (tolerate a missing file gracefully).
	existingData, _ := os.ReadFile(shellProfile)

	// Scan for an existing marker block and replace it in-place; otherwise
	// append the marker + export at the end of the file.
	var newLines []string
	replaced := false
	scanner := bufio.NewScanner(strings.NewReader(string(existingData)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == marker {
			// Emit the refreshed marker + export pair.
			newLines = append(newLines, marker)
			newLines = append(newLines, exportLine)
			replaced = true
			// Consume the immediately-following line if it is the old export so
			// we don't leave a stale duplicate behind.
			if scanner.Scan() {
				next := scanner.Text()
				if !strings.HasPrefix(strings.TrimSpace(next), "export AWS_BEARER_TOKEN_BEDROCK=") {
					newLines = append(newLines, next)
				}
			}
			continue
		}
		newLines = append(newLines, line)
	}

	if !replaced {
		// Append a blank separator when the file is non-empty and does not
		// already end with a blank line, then add the marker + export.
		if len(existingData) > 0 && !strings.HasSuffix(string(existingData), "\n\n") {
			newLines = append(newLines, "")
		}
		newLines = append(newLines, marker)
		newLines = append(newLines, exportLine)
	}

	// Ensure the file ends with exactly one newline.
	result := strings.Join(newLines, "\n")
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}

	os.WriteFile(shellProfile, []byte(result), 0600)
}

func syncMcpServers() {
	// Fetch MCP config from S3 and merge into ~/.claude/settings.json
	// Non-blocking, best effort — failures are silent
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")

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
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("https://claude-code-auth-distribution-916587687563.s3.amazonaws.com/cowork/claude-code-mcps.json")
	if err != nil || resp.StatusCode != 200 {
		return
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

	// Read current settings
	settingsData, err := os.ReadFile(settingsPath)
	if err != nil {
		return
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(settingsData, &settings); err != nil {
		return
	}

	// Merge MCPs
	settings["mcpServers"] = mcps
	newData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(settingsPath, newData, 0600)

	// Update sync timestamp
	os.MkdirAll(filepath.Dir(cachePath), 0700)
	os.WriteFile(cachePath, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0600)
}
