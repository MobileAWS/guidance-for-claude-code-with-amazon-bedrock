# Credential Process Binary - System Permission Requirements Analysis

## Executive Summary

The `credential-process` Go binary (`source/go/cmd/credential-process/main.go`) requires several system permissions that can trigger macOS permission prompts. This analysis documents the specific capabilities needed and explains why each permission is required.

## Detailed Permission Requirements

### 1. Keychain Access (macOS System Keychain)
**Triggers**: "credential-process wants to access keychain" prompts

**Code Location**: `source/go/internal/storage/keyring.go`
```go
func openKeyring() (keyring.Keyring, error) {
    return keyring.Open(keyring.Config{
        ServiceName: serviceName,
        KeychainName: "login",
        KeychainTrustApplication: true,
    })
}
```

**Operations**:
- **Read**: AWS credentials, Azure client secrets, monitoring tokens, refresh tokens
- **Write**: Same credential types for caching and persistence
- **Service Name**: `claude-code-with-bedrock`
- **Keychain**: Login keychain with `KeychainTrustApplication: true`

**Why Required**: Secure credential storage for OIDC tokens and AWS session credentials when `credential_storage = "keyring"` is configured.

### 2. File System Access - Home Directory
**Triggers**: Potential permission prompts for ~/Library, ~/.aws access

**Write Operations**:
```go
// ~/.aws/credentials (AWS CLI compatibility)
credPath := filepath.Join(home, ".aws", "credentials")

// ~/.claude-code-session/ (session cache)
sessionPath := filepath.Join(home, ".claude-code-session", profile+"-creds.json")

// ~/.claude/ (Claude Desktop integration)  
claudeDir := filepath.Join(home, ".claude")

// ~/.codex/ (Codex tool integration)
codexDir := filepath.Join(home, ".codex")

// Shell profiles for environment variables
zshrc := filepath.Join(home, ".zshrc")
bashrc := filepath.Join(home, ".bashrc")
```

**Read Operations**:
- Configuration files in `~/.claude-code-session/`
- Shell profile files for environment variable updates
- Existing AWS credentials files

**Why Required**: 
- Cache credentials between runs
- Integrate with AWS CLI toolchain
- Configure downstream tools (Claude Desktop, Codex)
- Set environment variables for shell integration

### 3. File System Access - System Directories
**Triggers**: Administrator permission requests

**Write Operations**:
```go
// macOS Application Support (Claude Desktop managed config)
managedConfigPath := filepath.Join(home, "Library", "Application Support", "Claude", "managed_config.json")

// System-wide plugin directory (requires admin rights)
pluginDir := "/Library/Application Support/Claude/org-plugins/nexus-skills"
```

**Why Required**: 
- Configure Claude Desktop MCP servers and managed settings
- Install organization skills system-wide
- Enable enterprise policy management

### 4. Network Access
**Triggers**: "wants to access the network" prompts

**HTTP Clients Used**:
```go
client := &http.Client{Timeout: 5 * time.Second}

// Endpoints contacted:
// - OIDC provider endpoints (Auth0, Okta, Azure, etc.)
// - AWS STS (sts.amazonaws.com)  
// - S3 bucket (claude-code-auth-distribution-916587687563.s3.amazonaws.com)
// - Nexus API (dtxfifv2cj.execute-api.us-east-1.amazonaws.com)
```

**Network Operations**:
- **OIDC Authentication**: Exchange authorization codes for tokens
- **AWS STS**: AssumeRole operations for cross-account access  
- **Configuration Sync**: Download MCP servers, skills, updates from S3
- **Quota API**: Check usage limits via Nexus API
- **Self-Update**: Download newer binary versions

**Why Required**: 
- Core OIDC authentication flows
- AWS credential federation
- Keep configurations and binaries up-to-date
- Enforce organizational policies

### 5. Local Network Server (Callback Server)
**Triggers**: "wants to accept incoming connections" prompts

**Code Location**:
```go
redirectPort := 8400 // Default, configurable
ln, err := portlock.TryAcquire(a.redirectPort)
```

**Operations**:
- Bind to localhost on configurable port (default 8400)
- Receive OAuth callback from browser after authentication
- Single-use server closed immediately after receiving callback

**Why Required**: 
- Complete OAuth 2.0 / OIDC authorization code flow
- Receive tokens from identity provider after user authentication

### 6. Process Execution
**Code Location**:
```go
// Chain AssumeRole operations via AWS CLI
cmd := exec.Command("aws", "sts", "assume-role", ...)

// System preference updates (macOS)
exec.Command("plutil", "-replace", "managedMcpServers", ...)

// Registry updates (Windows)  
exec.Command("reg", "add", regPath, ...)
```

**Why Required**:
- AWS CLI integration for complex credential chaining
- Update system-wide configuration preferences
- Cross-platform configuration management

### 7. System Information Access
**Code Location**:
```go
// User information
usr, _ := user.Current()

// System paths
home, _ := os.UserHomeDir()
execPath, _ := os.Executable()

// Runtime detection
runtime.GOOS, runtime.GOARCH
```

**Why Required**:
- Determine platform-specific paths and configurations
- Self-update mechanism needs executable path
- User-specific configuration and credential storage

## Why Permissions Are Triggered

### macOS Specific Behaviors
1. **Keychain Access**: Any access to macOS keychain requires explicit user consent
2. **Full Disk Access**: Writing to `~/Library/Application Support/` may require FDA permissions
3. **Network Access**: Outbound HTTPS and localhost server binding
4. **Code Signing**: Binary requires proper entitlements for keychain access

### Security Context
- Binary handles sensitive authentication tokens and AWS credentials
- Network communication with identity providers and AWS services
- System-level configuration changes for enterprise management
- Self-update mechanism requires executable replacement

## Recommended Security Configuration

### Current Entitlements (Per User Note)
The current entitlements include development-time permissions:
- `com.apple.security.cs.disable-library-validation`
- `com.apple.security.cs.allow-dyld-environment-variables`

### Production Security Hardening (Future)
1. Remove permissive development entitlements
2. Add `--strict --deep` to codesign verification
3. Pin Team ID in installer verification
4. Implement proper keychain access groups

## Summary

The credential-process binary requires significant system permissions due to its role as a secure credential broker. The permissions enable:

1. **Secure credential storage** (keychain access)
2. **Tool ecosystem integration** (file system access) 
3. **Network authentication** (OIDC flows, AWS API calls)
4. **Enterprise management** (configuration sync, policy enforcement)
5. **User experience** (callback server for browser-based auth)

All permissions serve legitimate security and integration requirements for the AWS credential provider ecosystem.
