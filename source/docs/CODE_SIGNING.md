# macOS Code Signing Setup

This guide explains how to configure macOS code signing and notarization for the credential-process binary to eliminate Gatekeeper warnings and permission re-prompts.

## Overview

macOS code signing provides several benefits:

1. **Eliminates Gatekeeper warnings**: Signed binaries are trusted by macOS
2. **Prevents permission re-prompts**: Consistent binary identity across updates  
3. **Professional deployment**: Shows your organization as the verified publisher
4. **Security**: Ensures binary integrity and authenticity

## Prerequisites

### 1. Apple Developer Account

You need an active Apple Developer Program membership ($99/year):
- Individual Account: For single developers
- Organization Account: For companies (recommended for enterprise deployment)

Sign up at: https://developer.apple.com/programs/

### 2. Developer ID Certificate

After enrolling, create a Developer ID Application certificate:

1. Visit [Apple Developer Certificates](https://developer.apple.com/account/resources/certificates/list)
2. Click the "+" button to create a new certificate
3. Select "Developer ID Application" 
4. Follow the instructions to generate and download the certificate
5. Install the certificate in your macOS Keychain

### 3. App-Specific Password (for notarization)

Create an app-specific password for notarization:

1. Sign in to [appleid.apple.com](https://appleid.apple.com)
2. Go to "Sign-In and Security" → "App-Specific Passwords"
3. Click "Generate Password" and name it "Claude Code Notarization"
4. Save the generated password securely

## Configuration

### Environment Variables

Set the following environment variables for automated signing:

```bash
# Apple Developer credentials
export APPLE_ID="your-apple-id@company.com"
export APPLE_APP_PASSWORD="xxxx-xxxx-xxxx-xxxx"  # App-specific password
export APPLE_TEAM_ID="XXXXXXXXXX"  # Your 10-character Team ID

# Optional: Specific signing identity (auto-detected if not set)
export APPLE_SIGNING_IDENTITY="Developer ID Application: Your Company (XXXXXXXXXX)"

# Enable notarization (optional, requires Apple ID credentials)
export ENABLE_NOTARIZATION="true"
```

## Usage

### Automatic Signing

Code signing is automatically enabled when:

1. Running on macOS
2. A Developer ID certificate is installed
3. Environment variables are configured

The `ccwb package` command will automatically sign macOS binaries:

```bash
# Package with automatic code signing
poetry run ccwb package --target-platform macos-arm64

# Package all platforms (macOS binaries will be signed)  
poetry run ccwb package --target-platform all
```

## Security Best Practices

### Certificate Management

1. **Store certificates securely**: Use CI secret management
2. **Rotate regularly**: Update certificates before expiration  
3. **Limit access**: Restrict who can access signing credentials
4. **Monitor usage**: Track when and where certificates are used

For complete setup instructions and troubleshooting, see the full documentation.
