# ABOUTME: Nexus package-generation Lambda
# ABOUTME: Generates org-specific installer scripts (install.sh / install.bat /
# ABOUTME: ccwb-install.ps1) and writes them to the shared S3 distribution bucket.
# ABOUTME: Supports Codex (OpenAI CLI) Amazon Bedrock integration alongside the
# ABOUTME: standard Claude Code setup when an org has codex_enabled=true.

"""
nexus-package-gen — Async Lambda that produces installer artifacts for a given Nexus org.

Invocation (event payload)
--------------------------
{
    "org_id":           "skematic",          # Required — Nexus org identifier
    "provider_domain":  "skematic.okta.com", # Required — OIDC provider domain
    "client_id":        "0oa1abc2def",       # Required — OIDC client ID
    "aws_region":       "us-east-1",         # Required — org's AWS region
    "identity_pool_id": "us-east-1:abc123",  # Required (cognito) OR federated_role_arn
    "federated_role_arn": null,              # Required (direct STS) OR identity_pool_id
    "federation_type":  "cognito",           # "cognito" | "direct"
    "profile_name":     "ClaudeCode",        # Optional — AWS profile name written to config.json
    "mantle_api_key":   "mk_live_...",       # Optional — key for AWS_BEARER_TOKEN_BEDROCK
    "codex_enabled":    true,                # Optional — generate Codex setup block (default false)
    "codex_org_id":     "skematic",          # Optional — org id used in codex-config.json S3 URL
    "monitoring_enabled": false,             # Optional
    "bedrock_region":   "us-east-2",         # Optional — region written into Claude settings
    "selected_model":   "us.anthropic.claude-sonnet-4-…", # Optional
    "cross_region_profile": "us",            # Optional
    "allowed_bedrock_regions": ["us-east-1"] # Optional
}

The Lambda writes the following objects to S3 under
  s3://claude-code-auth-distribution-916587687563/cowork/{org_id}/
  - install.sh
  - install.bat
  - ccwb-install.ps1
  - config.json         (credential-process configuration)
  - codex-config.json   (Codex / Mantle config; read by install scripts at user-install time)
"""

from __future__ import annotations

import json
import logging
import os
from datetime import datetime, timezone
from typing import Any

import boto3

logger = logging.getLogger(__name__)
logger.setLevel(logging.INFO)

# ---------------------------------------------------------------------------
# AWS clients
# ---------------------------------------------------------------------------
s3 = boto3.client("s3")

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
DISTRIBUTION_BUCKET = os.environ.get(
    "DISTRIBUTION_BUCKET",
    "claude-code-auth-distribution-916587687563",
)
CODEX_S3_BASE = (
    "https://claude-code-auth-distribution-916587687563.s3.amazonaws.com/cowork"
)
DEFAULT_BEDROCK_REGION = "us-east-2"
DEFAULT_PROFILE_NAME = "ClaudeCode"

# ---------------------------------------------------------------------------
# Lambda entry-point
# ---------------------------------------------------------------------------


def lambda_handler(event: dict[str, Any], context: Any) -> dict[str, Any]:
    """Generate installer artifacts and upload them to S3."""

    logger.info("nexus-package-gen invoked: %s", json.dumps(event))

    try:
        # ------------------------------------------------------------------
        # Extract + validate inputs
        # ------------------------------------------------------------------
        org_id = _require(event, "org_id")
        provider_domain = _require(event, "provider_domain")
        client_id = _require(event, "client_id")
        aws_region = _require(event, "aws_region")

        federation_type = event.get("federation_type", "cognito")
        identity_pool_id = event.get("identity_pool_id", "")
        federated_role_arn = event.get("federated_role_arn", "")
        if federation_type == "direct" and not federated_role_arn:
            raise ValueError("federated_role_arn is required when federation_type='direct'")
        if federation_type != "direct" and not identity_pool_id:
            raise ValueError("identity_pool_id is required when federation_type='cognito'")

        profile_name = event.get("profile_name") or DEFAULT_PROFILE_NAME
        mantle_api_key = event.get("mantle_api_key", "")
        codex_enabled: bool = bool(event.get("codex_enabled", False))
        codex_org_id: str = event.get("codex_org_id") or org_id
        monitoring_enabled: bool = bool(event.get("monitoring_enabled", False))
        bedrock_region: str = event.get("bedrock_region") or DEFAULT_BEDROCK_REGION
        selected_model: str = event.get("selected_model", "")
        cross_region_profile: str = event.get("cross_region_profile", "us")
        allowed_bedrock_regions: list[str] = event.get("allowed_bedrock_regions", [aws_region])

        # Derived S3 prefix for this org
        s3_prefix = f"cowork/{org_id}"

        # ------------------------------------------------------------------
        # 1. codex-config.json  (read by install scripts at end-user time)
        # ------------------------------------------------------------------
        codex_config = _build_codex_config(
            codex_enabled=codex_enabled,
            mantle_api_key=mantle_api_key,
            org_id=codex_org_id,
        )
        _put_s3(
            key=f"{s3_prefix}/codex-config.json",
            body=json.dumps(codex_config, indent=2),
            content_type="application/json",
        )
        logger.info("Written codex-config.json for org %s (codex_enabled=%s)", org_id, codex_enabled)

        # ------------------------------------------------------------------
        # 2. config.json  (credential-process configuration for end users)
        # ------------------------------------------------------------------
        credential_config = _build_credential_config(
            profile_name=profile_name,
            provider_domain=provider_domain,
            client_id=client_id,
            aws_region=aws_region,
            federation_type=federation_type,
            identity_pool_id=identity_pool_id,
            federated_role_arn=federated_role_arn,
            selected_model=selected_model,
            cross_region_profile=cross_region_profile,
        )
        _put_s3(
            key=f"{s3_prefix}/config.json",
            body=json.dumps(credential_config, indent=2),
            content_type="application/json",
        )

        # ------------------------------------------------------------------
        # 3. install.sh  (macOS / Linux)
        # ------------------------------------------------------------------
        install_sh = _render_install_sh(
            org_id=org_id,
            provider_domain=provider_domain,
            aws_region=aws_region,
            bedrock_region=bedrock_region,
            profile_name=profile_name,
            codex_enabled=codex_enabled,
            codex_org_id=codex_org_id,
            monitoring_enabled=monitoring_enabled,
        )
        _put_s3(
            key=f"{s3_prefix}/install.sh",
            body=install_sh,
            content_type="text/x-shellscript",
        )

        # ------------------------------------------------------------------
        # 4. install.bat  (Windows CMD)
        # ------------------------------------------------------------------
        install_bat = _render_install_bat(
            org_id=org_id,
            provider_domain=provider_domain,
            aws_region=aws_region,
            profile_name=profile_name,
            codex_enabled=codex_enabled,
            codex_org_id=codex_org_id,
        )
        _put_s3(
            key=f"{s3_prefix}/install.bat",
            body=install_bat,
            content_type="text/plain",
        )

        # ------------------------------------------------------------------
        # 5. ccwb-install.ps1  (Windows PowerShell)
        # ------------------------------------------------------------------
        install_ps1 = _render_install_ps1(
            org_id=org_id,
            provider_domain=provider_domain,
            aws_region=aws_region,
            profile_name=profile_name,
            codex_enabled=codex_enabled,
            codex_org_id=codex_org_id,
        )
        _put_s3(
            key=f"{s3_prefix}/ccwb-install.ps1",
            body=install_ps1,
            content_type="text/plain",
        )

        logger.info("nexus-package-gen completed for org %s", org_id)
        return {
            "statusCode": 200,
            "body": json.dumps(
                {
                    "org_id": org_id,
                    "s3_prefix": s3_prefix,
                    "artifacts": [
                        "codex-config.json",
                        "config.json",
                        "install.sh",
                        "install.bat",
                        "ccwb-install.ps1",
                    ],
                    "codex_enabled": codex_enabled,
                    "generated_at": datetime.now(timezone.utc).isoformat(),
                }
            ),
        }

    except ValueError as exc:
        logger.error("Validation error: %s", exc)
        return {"statusCode": 400, "body": json.dumps({"error": str(exc)})}
    except Exception as exc:  # noqa: BLE001
        logger.exception("Unexpected error in nexus-package-gen")
        return {"statusCode": 500, "body": json.dumps({"error": str(exc)})}


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _require(event: dict, key: str) -> str:
    """Return event[key] or raise ValueError."""
    value = event.get(key)
    if not value:
        raise ValueError(f"Required field '{key}' is missing or empty")
    return value


def _put_s3(key: str, body: str, content_type: str = "text/plain") -> None:
    """Upload a UTF-8 string to the distribution S3 bucket."""
    s3.put_object(
        Bucket=DISTRIBUTION_BUCKET,
        Key=key,
        Body=body.encode("utf-8"),
        ContentType=content_type,
    )


# ---------------------------------------------------------------------------
# Artifact builders
# ---------------------------------------------------------------------------


def _build_codex_config(
    *,
    codex_enabled: bool,
    mantle_api_key: str,
    org_id: str,
) -> dict[str, Any]:
    """Return the object written to codex-config.json.

    This file is fetched by install.sh / install.bat at end-user install time
    to decide whether to write ~/.codex/config.toml and set
    AWS_BEARER_TOKEN_BEDROCK.
    """
    return {
        "org_id": org_id,
        "codex_enabled": codex_enabled,
        # Only include the key when Codex is enabled to avoid leaking it
        # unnecessarily. Install scripts check codex_enabled first.
        **({"mantle_api_key": mantle_api_key} if codex_enabled and mantle_api_key else {}),
        "generated_at": datetime.now(timezone.utc).isoformat(),
    }


def _build_credential_config(
    *,
    profile_name: str,
    provider_domain: str,
    client_id: str,
    aws_region: str,
    federation_type: str,
    identity_pool_id: str,
    federated_role_arn: str,
    selected_model: str,
    cross_region_profile: str,
) -> dict[str, Any]:
    """Return the config.json written to S3 (consumed by credential-process)."""
    profile: dict[str, Any] = {
        "provider_domain": provider_domain,
        "client_id": client_id,
        "aws_region": aws_region,
        "provider_type": _detect_provider_type(provider_domain),
        "credential_storage": "keyring",
        "cross_region_profile": cross_region_profile or "us",
        "sso_enabled": True,
        "federation_type": federation_type,
    }

    if federation_type == "direct":
        profile["federated_role_arn"] = federated_role_arn
        profile["max_session_duration"] = 28800
    else:
        profile["identity_pool_id"] = identity_pool_id

    if selected_model:
        profile["selected_model"] = selected_model

    return {profile_name: profile}


def _detect_provider_type(domain: str) -> str:
    """Infer OIDC provider type from the domain string."""
    from urllib.parse import urlparse

    if not domain:
        return "oidc"

    url = domain if domain.startswith(("http://", "https://")) else f"https://{domain}"
    try:
        host = (urlparse(url).hostname or "").lower()
        if host.endswith((".okta.com", ".oktapreview.com", ".okta-emea.com")):
            return "okta"
        if host.endswith(".auth0.com"):
            return "auth0"
        if host.endswith((".microsoftonline.com", ".windows.net")):
            return "azure"
        if host.endswith(".amazoncognito.com") or (
            host.startswith("cognito-idp.") and ".amazonaws.com" in host
        ):
            return "cognito"
        if host == "accounts.google.com":
            return "google"
    except Exception:  # noqa: BLE001
        pass
    return "auto"


# ---------------------------------------------------------------------------
# Template: install.sh
# ---------------------------------------------------------------------------
_CODEX_SETUP_SH = """\

# ======================================
# Codex Setup (Amazon Bedrock)
# ======================================
CODEX_ENABLED="{codex_enabled_lower}"
CODEX_CONFIG_URL="{codex_config_url}"

if [ "$CODEX_ENABLED" = "true" ] && [ -n "$CODEX_CONFIG_URL" ]; then
    echo
    echo "Setting up Codex (Amazon Bedrock integration)..."

    CODEX_CONFIG_JSON=""
    if command -v curl &> /dev/null; then
        CODEX_CONFIG_JSON=$(curl -sf "$CODEX_CONFIG_URL" 2>/dev/null || echo "")
    elif command -v wget &> /dev/null; then
        CODEX_CONFIG_JSON=$(wget -qO- "$CODEX_CONFIG_URL" 2>/dev/null || echo "")
    fi

    if [ -z "$CODEX_CONFIG_JSON" ]; then
        echo "⚠️  Could not fetch Codex config from S3 — skipping Codex setup."
    else
        CODEX_ORG_ENABLED=$($PYTHON -c "import json,sys; d=json.loads(sys.stdin.read()); print(str(d.get('codex_enabled', False)).lower())" <<< "$CODEX_CONFIG_JSON" 2>/dev/null || echo "false")
        MANTLE_API_KEY=$($PYTHON -c "import json,sys; d=json.loads(sys.stdin.read()); print(d.get('mantle_api_key', ''))" <<< "$CODEX_CONFIG_JSON" 2>/dev/null || echo "")

        if [ "$CODEX_ORG_ENABLED" = "true" ] && [ -n "$MANTLE_API_KEY" ]; then
            mkdir -p ~/.codex
            cat > ~/.codex/config.toml << 'CODEX_TOML_EOF'
model_provider = "amazon-bedrock"
region = "us-east-2"
CODEX_TOML_EOF
            echo "✓ Written ~/.codex/config.toml"

            BEARER_LINE="export AWS_BEARER_TOKEN_BEDROCK=\\"$MANTLE_API_KEY\\""
            SHELL_PROFILES=()
            if [ -f ~/.zshrc ];  then SHELL_PROFILES+=(~/.zshrc);  fi
            if [ -f ~/.bashrc ]; then SHELL_PROFILES+=(~/.bashrc); fi
            if [ ${{#SHELL_PROFILES[@]}} -eq 0 ]; then SHELL_PROFILES+=(~/.zshrc); fi

            for SHELL_PROFILE in "${{SHELL_PROFILES[@]}}"; do
                sed -i.bak '/^export AWS_BEARER_TOKEN_BEDROCK=/d' "$SHELL_PROFILE" 2>/dev/null || true
                rm -f "${{SHELL_PROFILE}}.bak"
                echo "$BEARER_LINE" >> "$SHELL_PROFILE"
                echo "✓ Set AWS_BEARER_TOKEN_BEDROCK in $SHELL_PROFILE"
            done

            echo "✓ Codex configured for Amazon Bedrock"
            echo "  Reload your shell: source ~/.zshrc  (or source ~/.bashrc)"
        else
            echo "ℹ  Codex not enabled for this organisation — skipping."
        fi
    fi
fi
"""


def _render_install_sh(
    *,
    org_id: str,
    provider_domain: str,
    aws_region: str,
    bedrock_region: str,
    profile_name: str,
    codex_enabled: bool,
    codex_org_id: str,
    monitoring_enabled: bool,
) -> str:
    """Render install.sh for macOS / Linux."""

    codex_config_url = f"{CODEX_S3_BASE}/{codex_org_id}/codex-config.json" if codex_org_id else ""
    codex_block = _CODEX_SETUP_SH.format(
        codex_enabled_lower=str(codex_enabled).lower(),
        codex_config_url=codex_config_url,
    )

    timestamp = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC")

    return f"""\
#!/bin/bash
# Claude Code Authentication Installer
# Organisation : {provider_domain}
# Generated    : {timestamp}
# Org ID       : {org_id}
# Codex        : {"enabled" if codex_enabled else "disabled"}

set -e

SCRIPT_DIR="$(cd "$(dirname "${{BASH_SOURCE[0]}}")" && pwd)"
cd "$SCRIPT_DIR"

echo "======================================"
echo "Claude Code Authentication Installer"
echo "======================================"
echo
echo "Organisation: {provider_domain}"
echo

# ---------------------------------------------------------------------------
# Prerequisites
# ---------------------------------------------------------------------------
echo "Checking prerequisites..."

PYTHON=""
if command -v python3 &> /dev/null; then PYTHON="python3"
elif command -v python &> /dev/null; then PYTHON="python"
else
    echo "ERROR: Python is not installed. Install python3 and re-run."
    exit 1
fi
echo "✓ Python found ($($PYTHON --version 2>&1))"

if [ ! -f "config.json" ]; then
    echo "ERROR: config.json not found. Run from inside the extracted package folder."
    exit 1
fi
echo "✓ config.json found"

# ---------------------------------------------------------------------------
# Detect platform
# ---------------------------------------------------------------------------
echo
echo "Detecting platform..."
if [[ "$OSTYPE" == "darwin"* ]]; then
    ARCH=$(uname -m)
    BINARY_SUFFIX="macos-$( [[ "$ARCH" == "arm64" ]] && echo arm64 || echo intel )"
    echo "✓ macOS detected ($ARCH)"
elif [[ "$OSTYPE" == "linux-gnu"* ]]; then
    ARCH=$(uname -m)
    BINARY_SUFFIX="linux-$( [[ "$ARCH" == "aarch64" || "$ARCH" == "arm64" ]] && echo arm64 || echo x64 )"
    echo "✓ Linux detected ($ARCH)"
else
    echo "❌ Unsupported platform: $OSTYPE"
    exit 1
fi

CREDENTIAL_BINARY="credential-process-$BINARY_SUFFIX"
if [ ! -f "$CREDENTIAL_BINARY" ]; then
    echo "❌ Binary not found for your platform: $CREDENTIAL_BINARY"
    exit 1
fi

# ---------------------------------------------------------------------------
# Install credential-process
# ---------------------------------------------------------------------------
echo
echo "Installing authentication tools..."
mkdir -p ~/claude-code-with-bedrock
cp "$CREDENTIAL_BINARY" ~/claude-code-with-bedrock/credential-process
cp config.json ~/claude-code-with-bedrock/
chmod +x ~/claude-code-with-bedrock/credential-process

if [[ "$OSTYPE" == "darwin"* ]]; then
    xattr -d com.apple.quarantine ~/claude-code-with-bedrock/credential-process 2>/dev/null || true
fi
echo "✓ credential-process installed"

OTEL_BINARY="otel-helper-$BINARY_SUFFIX"
if [ -f "$OTEL_BINARY" ]; then
    cp "$OTEL_BINARY" ~/claude-code-with-bedrock/otel-helper
    chmod +x ~/claude-code-with-bedrock/otel-helper
    xattr -d com.apple.quarantine ~/claude-code-with-bedrock/otel-helper 2>/dev/null || true
    echo "✓ otel-helper installed"
fi

# ---------------------------------------------------------------------------
# Claude Code settings
# ---------------------------------------------------------------------------
if [ -d "claude-settings" ] && [ -f "claude-settings/settings.json" ]; then
    echo
    echo "Installing Claude Code settings..."
    mkdir -p ~/.claude
    if [ -f ~/.claude/settings.json ]; then
        cp ~/.claude/settings.json ~/.claude/settings.json.backup-$(date +%Y%m%d-%H%M%S) 2>/dev/null || true
    fi
    sed -e "s|__OTEL_HELPER_PATH__|$HOME/claude-code-with-bedrock/otel-helper|g" \\
        -e "s|__CREDENTIAL_PROCESS_PATH__|$HOME/claude-code-with-bedrock/credential-process|g" \\
        "claude-settings/settings.json" > ~/.claude/settings.json
    echo "✓ Claude Code settings written to ~/.claude/settings.json"
fi

# ---------------------------------------------------------------------------
# AWS profile configuration
# ---------------------------------------------------------------------------
echo
echo "Configuring AWS profiles..."
mkdir -p ~/.aws

PROFILES=$($PYTHON -c "import json; print(' '.join(json.load(open('config.json')).keys()))")
if [ -z "$PROFILES" ]; then
    echo "❌ No profiles found in config.json"
    exit 1
fi

for PROFILE_NAME in $PROFILES; do
    sed -i.bak "/\\[profile $PROFILE_NAME\\]/,/^$/d" ~/.aws/config 2>/dev/null || true
    PROFILE_REGION=$($PYTHON -c "import json; print(json.load(open('config.json')).get('$PROFILE_NAME', {{}}).get('aws_region', '{aws_region}'))")
    cat >> ~/.aws/config << AWSEOF
[profile $PROFILE_NAME]
credential_process = $HOME/claude-code-with-bedrock/credential-process --profile $PROFILE_NAME
region = $PROFILE_REGION
AWSEOF
    echo "  ✓ AWS profile '$PROFILE_NAME' configured (region: $PROFILE_REGION)"
done

# ---------------------------------------------------------------------------
# Claude Code CLI
# ---------------------------------------------------------------------------
echo
if ! command -v claude &> /dev/null; then
    echo "Installing Claude Code CLI..."
    if command -v npm &> /dev/null; then
        if [ -d "$HOME/.nvm" ]; then
            npm install -g @anthropic-ai/claude-code 2>/dev/null && echo "✓ Claude Code CLI installed" || echo "⚠️  Run: npm install -g @anthropic-ai/claude-code"
        else
            sudo npm install -g @anthropic-ai/claude-code 2>/dev/null && echo "✓ Claude Code CLI installed" || echo "⚠️  Run: sudo npm install -g @anthropic-ai/claude-code"
        fi
    else
        echo "⚠️  npm not found. Install Node.js from https://nodejs.org then: npm install -g @anthropic-ai/claude-code"
    fi
else
    echo "✓ Claude Code CLI already installed"
fi
{codex_block}
# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
FIRST_PROFILE=$(echo $PROFILES | awk '{{print $1}}')

echo
echo "======================================"
echo "✓ Installation complete!"
echo "======================================"
echo
echo "Launch Claude Code:"
echo "  export AWS_PROFILE=$FIRST_PROFILE"
echo "  claude"
echo
echo "Note: Authentication will automatically open your browser when needed."
"""


# ---------------------------------------------------------------------------
# Template: install.bat
# ---------------------------------------------------------------------------
_CODEX_SETUP_BAT = """\

REM ======================================
REM Codex Setup (Amazon Bedrock)
REM ======================================
set CODEX_ENABLED={codex_enabled_int}
set CODEX_CONFIG_URL={codex_config_url}

if "%CODEX_ENABLED%"=="1" (
    if not "%CODEX_CONFIG_URL%"=="" (
        echo.
        echo Setting up Codex (Amazon Bedrock integration)...

        for /f "delims=" %%R in ('powershell -NoProfile -Command "try {{ $r = Invoke-RestMethod -Uri '%CODEX_CONFIG_URL%' -UseBasicParsing -ErrorAction Stop; $r | ConvertTo-Json -Compress }} catch {{ '' }}" 2^>nul') do set CODEX_JSON=%%R

        if "%CODEX_JSON%"=="" (
            echo WARNING: Could not fetch Codex config -- skipping Codex setup.
        ) else (
            powershell -NoProfile -Command ^
                "$json = $env:CODEX_JSON | ConvertFrom-Json; " ^
                "if ($json.codex_enabled -and $json.mantle_api_key) {{ " ^
                "  $d = Join-Path $env:USERPROFILE '.codex'; " ^
                "  if (-not (Test-Path $d)) {{ New-Item -ItemType Directory -Path $d | Out-Null }}; " ^
                "  Set-Content (Join-Path $d 'config.toml') \"model_provider = `\"amazon-bedrock`\"`r`nregion = `\"us-east-2`\"\" -Encoding UTF8; " ^
                "  Write-Host '  OK Written %USERPROFILE%\\.codex\\config.toml'; " ^
                "  [Environment]::SetEnvironmentVariable('AWS_BEARER_TOKEN_BEDROCK', $json.mantle_api_key, 'User'); " ^
                "  Write-Host '  OK Set AWS_BEARER_TOKEN_BEDROCK (user env var -- restart terminal to activate)' " ^
                "}} else {{ Write-Host '  INFO Codex not enabled for this org -- skipping.' }}"
        )
    )
)
"""


def _render_install_bat(
    *,
    org_id: str,
    provider_domain: str,
    aws_region: str,
    profile_name: str,
    codex_enabled: bool,
    codex_org_id: str,
) -> str:
    """Render install.bat for Windows CMD."""

    codex_config_url = f"{CODEX_S3_BASE}/{codex_org_id}/codex-config.json" if codex_org_id else ""
    codex_block = _CODEX_SETUP_BAT.format(
        codex_enabled_int="1" if codex_enabled else "0",
        codex_config_url=codex_config_url,
    )
    timestamp = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC")

    return f"""\
@echo off
SETLOCAL ENABLEDELAYEDEXPANSION
cd /d "%~dp0"
REM Claude Code Authentication Installer for Windows
REM Organisation: {provider_domain}
REM Generated   : {timestamp}
REM Org ID      : {org_id}
REM Codex       : {"enabled" if codex_enabled else "disabled"}

echo ======================================
echo Claude Code Authentication Installer
echo ======================================
echo.
echo Organisation: {provider_domain}
echo.

where aws >nul 2>&1
if %errorlevel% neq 0 (
    echo INFO: AWS CLI not found -- not required.
) else (
    echo OK AWS CLI found [optional]
)

if not exist "config.json" (
    echo ERROR: config.json not found. Run from inside the extracted package folder.
    pause & exit /b 1
)

echo.
echo Installing authentication tools...
if not exist "%USERPROFILE%\\claude-code-with-bedrock" mkdir "%USERPROFILE%\\claude-code-with-bedrock"

copy /Y "credential-process-windows.exe" "%USERPROFILE%\\claude-code-with-bedrock\\credential-process.exe" >nul
if %errorlevel% neq 0 (echo ERROR: Failed to copy credential-process-windows.exe & pause & exit /b 1)

if exist "otel-helper-windows.exe" copy /Y "otel-helper-windows.exe" "%USERPROFILE%\\claude-code-with-bedrock\\otel-helper.exe" >nul
copy /Y "config.json" "%USERPROFILE%\\claude-code-with-bedrock\\" >nul

if exist "claude-settings\\settings.json" (
    if not exist "%USERPROFILE%\\.claude" mkdir "%USERPROFILE%\\.claude"
    powershell -NoProfile -Command "$op = $env:USERPROFILE + '\\claude-code-with-bedrock\\otel-helper.exe' -replace '\\\\','/'; $cp = $env:USERPROFILE + '\\claude-code-with-bedrock\\credential-process.exe' -replace '\\\\','/'; (Get-Content 'claude-settings\\settings.json') -replace '__OTEL_HELPER_PATH__',$op -replace '__CREDENTIAL_PROCESS_PATH__',$cp | Set-Content (Join-Path $env:USERPROFILE '.claude\\settings.json')"
    echo OK Claude Code settings written
)

echo.
echo Configuring AWS profiles...
if not exist "%USERPROFILE%\\.aws" mkdir "%USERPROFILE%\\.aws"
powershell -NoProfile -Command "$nl=[char]13+[char]10; $cfg=Get-Content config.json|ConvertFrom-Json; $f=Join-Path $env:USERPROFILE '.aws\\config'; $cp=Join-Path $env:USERPROFILE 'claude-code-with-bedrock\\credential-process.exe'; $x=if(Test-Path $f){{Get-Content $f -Raw}}else{{'}}; foreach($p in $cfg.PSObject.Properties.Name){{ $r=$cfg.$p.aws_region; if(-not $r){{$r='{aws_region}'}}; $x=[regex]::Replace($x,'(?ms)^\\[profile '+[regex]::Escape($p)+'\\].*?(?=^\\[|\\Z)',''); $x=$x.TrimEnd()+$nl+$nl+'[profile '+$p+']'+$nl+'credential_process = '+$cp+' --profile '+$p+$nl+'region = '+$r+$nl; Write-Host('  OK profile '+$p) }}; Set-Content -Path $f -Value $x.TrimStart() -NoNewline -Encoding ASCII"
if %errorlevel% neq 0 (echo ERROR: AWS profile config failed & pause & exit /b 1)

where claude >nul 2>nul
if %errorlevel% neq 0 (
    where npm >nul 2>nul
    if %errorlevel% equ 0 (
        npm install -g @anthropic-ai/claude-code
        echo OK Claude Code CLI installed
    ) else (
        echo WARN npm not found -- install Node.js from https://nodejs.org
    )
) else (
    echo OK Claude Code CLI already installed
)
{codex_block}
echo.
echo ======================================
echo Installation complete!
echo ======================================
echo.
for /f %%p in ('powershell -NoProfile -Command "(Get-Content config.json|ConvertFrom-Json).PSObject.Properties.Name|Select-Object -First 1"') do (
    echo Launch Claude Code:
    echo   set AWS_PROFILE=%%p ^& claude
)
echo.
pause
"""


# ---------------------------------------------------------------------------
# Template: ccwb-install.ps1
# ---------------------------------------------------------------------------

def _render_install_ps1(
    *,
    org_id: str,
    provider_domain: str,
    aws_region: str,
    profile_name: str,
    codex_enabled: bool,
    codex_org_id: str,
) -> str:
    """Render ccwb-install.ps1 — pure PowerShell Windows installer."""

    codex_config_url = f"{CODEX_S3_BASE}/{codex_org_id}/codex-config.json" if codex_org_id else ""
    ps1_codex_enabled = "$true" if codex_enabled else "$false"
    timestamp = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC")

    return f"""\
# ccwb-install.ps1 — Claude Code with Bedrock installer (Windows / PowerShell)
# Organisation : {provider_domain}
# Generated    : {timestamp}
# Org ID       : {org_id}
# Codex        : {"enabled" if codex_enabled else "disabled"}
#
# Usage: .\\ccwb-install.ps1

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $ScriptDir

Write-Host '======================================'
Write-Host 'Claude Code Authentication Installer'
Write-Host '======================================'
Write-Host
Write-Host 'Organisation: {provider_domain}'
Write-Host

# ---------------------------------------------------------------------------
# Prerequisites
# ---------------------------------------------------------------------------
Write-Host 'Checking prerequisites...'
if (-not (Get-Command aws -ErrorAction SilentlyContinue)) {{
    Write-Host '  INFO AWS CLI not found -- not required.'
}} else {{
    Write-Host '  OK   AWS CLI found (optional)'
}}
if (-not (Test-Path 'config.json')) {{
    Write-Error 'config.json not found. Run from inside the extracted package folder.'
    exit 1
}}

# ---------------------------------------------------------------------------
# Install credential-process
# ---------------------------------------------------------------------------
Write-Host
Write-Host 'Installing authentication tools...'
$toolsDir = Join-Path $env:USERPROFILE 'claude-code-with-bedrock'
if (-not (Test-Path $toolsDir)) {{ New-Item -ItemType Directory -Path $toolsDir | Out-Null }}

if (-not (Test-Path 'credential-process-windows.exe')) {{
    Write-Error 'credential-process-windows.exe not found in package folder.'
    exit 1
}}
Copy-Item 'credential-process-windows.exe' (Join-Path $toolsDir 'credential-process.exe') -Force
Write-Host '  OK   credential-process.exe installed'

if (Test-Path 'otel-helper-windows.exe') {{
    Copy-Item 'otel-helper-windows.exe' (Join-Path $toolsDir 'otel-helper.exe') -Force
    Write-Host '  OK   otel-helper.exe installed'
}}
Copy-Item 'config.json' $toolsDir -Force
Write-Host '  OK   config.json copied'

# ---------------------------------------------------------------------------
# Claude Code settings
# ---------------------------------------------------------------------------
if (Test-Path 'claude-settings\\settings.json') {{
    $claudeDir = Join-Path $env:USERPROFILE '.claude'
    if (-not (Test-Path $claudeDir)) {{ New-Item -ItemType Directory -Path $claudeDir | Out-Null }}

    $skipSettings = $false
    if (Test-Path (Join-Path $claudeDir 'settings.json')) {{
        $ans = Read-Host 'Existing Claude Code settings found. Overwrite? (y/N)'
        if ($ans -ne 'y' -and $ans -ne 'Y') {{ $skipSettings = $true }}
    }}
    if (-not $skipSettings) {{
        $otelPath = (Join-Path $toolsDir 'otel-helper.exe')       -replace '\\\\', '/'
        $credPath = (Join-Path $toolsDir 'credential-process.exe') -replace '\\\\', '/'
        (Get-Content 'claude-settings\\settings.json') `
            -replace '__OTEL_HELPER_PATH__',        $otelPath `
            -replace '__CREDENTIAL_PROCESS_PATH__', $credPath `
            | Set-Content (Join-Path $claudeDir 'settings.json')
        Write-Host '  OK   Claude Code settings written'
    }}
}}

# ---------------------------------------------------------------------------
# AWS profile configuration
# ---------------------------------------------------------------------------
Write-Host
Write-Host 'Configuring AWS profiles...'
$awsDir = Join-Path $env:USERPROFILE '.aws'
if (-not (Test-Path $awsDir)) {{ New-Item -ItemType Directory -Path $awsDir | Out-Null }}

$nl          = [char]13 + [char]10
$cfg         = Get-Content 'config.json' | ConvertFrom-Json
$awsConfig   = Join-Path $awsDir 'config'
$credProcess = Join-Path $toolsDir 'credential-process.exe'
$existing    = if (Test-Path $awsConfig) {{ Get-Content $awsConfig -Raw }} else {{ '' }}

foreach ($p in $cfg.PSObject.Properties.Name) {{
    $region   = $cfg.$p.aws_region
    if (-not $region) {{ $region = '{aws_region}' }}
    $pattern  = '(?ms)^\\[profile ' + [regex]::Escape($p) + '\\].*?(?=^\\[|\\Z)'
    $existing = [regex]::Replace($existing, $pattern, '')
    $stanza   = '[profile ' + $p + ']' + $nl + 'credential_process = ' + $credProcess + ' --profile ' + $p + $nl + 'region = ' + $region + $nl
    $existing = $existing.TrimEnd() + $nl + $nl + $stanza
    Write-Host ('  OK   Configured AWS profile: ' + $p)
}}
Set-Content -Path $awsConfig -Value $existing.TrimStart() -NoNewline -Encoding ASCII

# ---------------------------------------------------------------------------
# Claude Code CLI
# ---------------------------------------------------------------------------
Write-Host
if (-not (Get-Command claude -ErrorAction SilentlyContinue)) {{
    Write-Host 'Installing Claude Code CLI...'
    if (Get-Command npm -ErrorAction SilentlyContinue) {{
        npm install -g '@anthropic-ai/claude-code'
        Write-Host '  OK   Claude Code CLI installed'
    }} else {{
        Write-Host '  WARN npm not found. Install Node.js from https://nodejs.org'
        Write-Host '       then run: npm install -g @anthropic-ai/claude-code'
    }}
}} else {{
    Write-Host '  OK   Claude Code CLI already installed'
}}

# ---------------------------------------------------------------------------
# Codex setup (Amazon Bedrock)
# ---------------------------------------------------------------------------
$codexEnabled   = {ps1_codex_enabled}
$codexConfigUrl = '{codex_config_url}'

if ($codexEnabled -and $codexConfigUrl -ne '') {{
    Write-Host
    Write-Host 'Setting up Codex (Amazon Bedrock integration)...'
    try {{
        $codexJson = Invoke-RestMethod -Uri $codexConfigUrl -UseBasicParsing -ErrorAction Stop
    }} catch {{
        Write-Host '  WARN Could not fetch Codex config from S3 -- skipping Codex setup.'
        $codexJson = $null
    }}

    if ($codexJson -and $codexJson.codex_enabled -and $codexJson.mantle_api_key) {{
        # Create ~/.codex and write config.toml
        $codexDir = Join-Path $env:USERPROFILE '.codex'
        if (-not (Test-Path $codexDir)) {{ New-Item -ItemType Directory -Path $codexDir | Out-Null }}

        $toml = "model_provider = `"amazon-bedrock`"`r`nregion = `"us-east-2`""
        Set-Content -Path (Join-Path $codexDir 'config.toml') -Value $toml -Encoding UTF8
        Write-Host ('  OK   Written ' + $codexDir + '\\config.toml')

        # Persist AWS_BEARER_TOKEN_BEDROCK as a user-scoped environment variable
        [Environment]::SetEnvironmentVariable('AWS_BEARER_TOKEN_BEDROCK', $codexJson.mantle_api_key, 'User')
        Write-Host '  OK   Set AWS_BEARER_TOKEN_BEDROCK (user environment variable)'
        Write-Host '  INFO Restart your terminal or re-open PowerShell for the change to take effect.'
    }} elseif ($codexJson) {{
        Write-Host '  INFO Codex not enabled for this organisation -- skipping Codex setup.'
    }}
}}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
Write-Host
Write-Host '======================================'
Write-Host 'Installation complete!'
Write-Host '======================================'
Write-Host
Write-Host 'Available profiles:'
foreach ($p in $cfg.PSObject.Properties.Name) {{ Write-Host ('  - ' + $p) }}
Write-Host
Write-Host 'To use Claude Code:'
Write-Host '  $env:AWS_PROFILE = "<profile-name>"'
Write-Host '  aws sts get-caller-identity'
Write-Host
Write-Host 'Note: Authentication will automatically open your browser when needed.'
"""
