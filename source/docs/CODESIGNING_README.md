# macOS Code Signing Implementation

This document describes the macOS code signing implementation added to the credential-process binary packaging system.

## What Was Added

### 1. Entitlements Files

- `source/codesign/credential-process.entitlements` - Entitlements for the main credential process binary
- `source/codesign/otel-helper.entitlements` - Entitlements for the OTEL helper binary

These files specify the required macOS permissions:
- Keychain access for secure credential storage
- Network access for OAuth and AWS API calls  
- Hardened runtime compatibility for Go binaries
- App sandbox permissions for security

### 2. Code Signing Utility

- `source/claude_code_with_bedrock/cli/utils/codesign.py` - Complete code signing implementation

Features:
- Automatic Apple Developer ID detection
- Code signing with proper entitlements
- Apple notarization workflow
- Batch processing for multiple binaries
- Comprehensive error handling and status reporting
- CI/CD friendly configuration via environment variables

### 3. Package Command Integration

Updated `source/claude_code_with_bedrock/cli/commands/package.py`:
- Automatic code signing detection and execution
- Integration with existing Go cross-compilation workflow
- Smart installer script generation that handles both signed and unsigned binaries
- Improved macOS Gatekeeper handling

### 4. Installer Improvements

Enhanced installer scripts now:
- Detect and handle signed binaries properly
- Remove quarantine flags only when necessary
- Display signature verification status
- Provide clear guidance for Gatekeeper interactions

### 5. CI/CD Workflow

Added `.github/workflows/sign-binaries.yml`:
- Automated code signing in GitHub Actions
- Certificate management for CI environments
- Separate workflows for signed/unsigned builds
- Artifact uploading and verification

### 6. Documentation

- `source/docs/CODE_SIGNING.md` - Complete setup and usage guide
- Environment variable configuration
- Troubleshooting section
- Security best practices

## How It Works

### Automatic Detection

The system automatically enables code signing when:

1. Running on macOS (`uname -s` returns "Darwin")
2. Apple Developer ID certificate is available in Keychain
3. `codesign` tool is accessible

### Environment Configuration

Code signing behavior is controlled by environment variables:

```bash
# Required for notarization
export APPLE_ID="your-apple-id@company.com"
export APPLE_APP_PASSWORD="xxxx-xxxx-xxxx-xxxx" # App-specific password
export APPLE_TEAM_ID="XXXXXXXXXX" # 10-character Team ID

# Optional
export APPLE_SIGNING_IDENTITY="Developer ID Application: Your Company"
export ENABLE_NOTARIZATION="true"
```

### Build Integration

During the packaging process:

1. **Build Phase**: Go binaries are compiled as usual
2. **Signing Phase**: macOS binaries are automatically signed if configured
3. **Verification Phase**: Signatures are verified and reported
4. **Installer Generation**: Smart installer handles signed/unsigned scenarios

### Graceful Degradation

The implementation is designed to fail gracefully:
- If signing credentials aren't available, builds continue with unsigned binaries
- Users get clear messaging about signature status
- Unsigned binaries still work (with manual Gatekeeper approval)

## Benefits

### For End Users

1. **No Gatekeeper Warnings**: Signed binaries are immediately trusted by macOS
2. **No Permission Re-prompts**: Consistent binary identity prevents repeated keychain access prompts
3. **Professional Experience**: Organization appears as verified publisher
4. **Enhanced Security**: Cryptographic verification of binary integrity

### For Administrators

1. **Enterprise Ready**: Meets corporate security requirements
2. **Automated Deployment**: No manual intervention needed for binary trust
3. **Audit Trail**: Clear signature and notarization records
4. **Scalable**: Works across development, staging, and production environments

### For Developers

1. **Zero Configuration**: Works automatically when credentials are available
2. **CI/CD Integration**: Seamless GitHub Actions workflow
3. **Debug Support**: Comprehensive logging and verification tools
4. **Cross-Platform**: Doesn't affect Linux/Windows builds

## Security Implementation

### Entitlements Design

The entitlements are minimal and specific:
- Only request necessary permissions
- Use app sandbox for containment
- Enable hardened runtime for security
- Allow Go runtime requirements

### Certificate Management

- Supports both individual and organization Developer IDs
- Automatic identity detection from Keychain
- Secure credential handling in CI environments
- No hardcoded certificates or keys

### Notarization Integration  

- Optional Apple notarization for additional trust
- Automated submission and stapling
- Proper error handling and retry logic
- Status verification and reporting

## Usage Examples

### Basic Usage

```bash
# Automatic signing (if configured)
poetry run ccwb package --target-platform macos-arm64

# Explicit control
poetry run ccwb package --sign-binaries --notarize
```

### CI Configuration

```yaml
# GitHub Actions
env:
  APPLE_ID: ${{ secrets.APPLE_ID }}
  APPLE_APP_PASSWORD: ${{ secrets.APPLE_APP_PASSWORD }}
  APPLE_TEAM_ID: ${{ secrets.APPLE_TEAM_ID }}
  ENABLE_NOTARIZATION: "true"
```

### Verification

```bash
# Check signature
codesign --verify --verbose dist/profile/timestamp/credential-process-macos-arm64

# View details  
codesign -dv dist/profile/timestamp/credential-process-macos-arm64

# Verify notarization
xcrun stapler validate dist/profile/timestamp/credential-process-macos-arm64
```

## Compatibility

- **macOS Versions**: 10.15+ (Catalina and later)
- **Xcode**: Command Line Tools sufficient
- **Go Versions**: All supported versions (signing is post-compilation)
- **Architecture**: Both Intel (x64) and Apple Silicon (ARM64)

## Future Enhancements

Potential improvements for future versions:

1. **Keychain Integration**: Direct certificate provisioning
2. **Multiple Identities**: Support for different signing contexts
3. **Timestamp Verification**: Enhanced notarization tracking
4. **Custom Entitlements**: Per-organization entitlement templates
5. **Signing Metrics**: Analytics on signing success rates

## Implementation Notes

### Design Decisions

1. **Optional by Default**: Signing doesn't break existing workflows
2. **Environment Driven**: Configuration via env vars for CI/CD compatibility
3. **Batch Processing**: Efficient signing of multiple binaries
4. **Rich Reporting**: Detailed status and error information
5. **Security First**: Minimal permissions and secure credential handling

### Error Handling

- Graceful fallback to unsigned builds
- Clear error messages with remediation steps
- Detailed logging for troubleshooting
- CI-friendly exit codes and output

### Testing Strategy

- Automated syntax validation
- CI workflow testing
- Manual verification procedures
- Cross-platform compatibility checks

This implementation provides enterprise-grade code signing capabilities while maintaining the simplicity and reliability of the existing packaging system.
