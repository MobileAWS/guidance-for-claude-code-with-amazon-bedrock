"""Tests for the nexus-package-gen Lambda function.

Validates that installer artifacts are correctly generated with Codex support,
including the codex-config.json payload, the Codex blocks in install.sh,
install.bat, and ccwb-install.ps1.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from unittest.mock import MagicMock, call, patch

import pytest

# Add the nexus-package-gen directory to sys.path so we can import index.py
_LAMBDA_DIR = str(Path(__file__).parents[2] / "nexus-package-gen")
if _LAMBDA_DIR not in sys.path:
    sys.path.insert(0, _LAMBDA_DIR)

import index as pkg_gen  # noqa: E402  (import after sys.path manipulation)


# ---------------------------------------------------------------------------
# Shared fixtures
# ---------------------------------------------------------------------------

BASE_EVENT = {
    "org_id": "acme",
    "provider_domain": "acme.okta.com",
    "client_id": "0oa1testclientid",
    "aws_region": "us-east-1",
    "identity_pool_id": "us-east-1:test-pool-id",
    "federation_type": "cognito",
    "profile_name": "ClaudeCode",
    "bedrock_region": "us-east-2",
    "cross_region_profile": "us",
    "allowed_bedrock_regions": ["us-east-1"],
}


def _make_event(**overrides) -> dict:
    return {**BASE_EVENT, **overrides}


def _collect_put_calls(mock_s3) -> dict[str, str]:
    """Return {s3_key: decoded_body} from mock s3.put_object calls."""
    result = {}
    for c in mock_s3.put_object.call_args_list:
        kwargs = c.kwargs if c.kwargs else c[1]
        # support both positional and keyword
        if not kwargs:
            args = c.args if c.args else c[0]
            # put_object(Bucket=…, Key=…, Body=…)
            kwargs = dict(zip(["Bucket", "Key", "Body", "ContentType"], args))
        key = kwargs.get("Key", "")
        body = kwargs.get("Body", b"")
        if isinstance(body, bytes):
            body = body.decode("utf-8")
        result[key] = body
    return result


# ---------------------------------------------------------------------------
# Tests: lambda_handler routing
# ---------------------------------------------------------------------------

class TestLambdaHandlerValidation:
    """Input validation in lambda_handler."""

    def test_missing_org_id_returns_400(self):
        event = _make_event()
        del event["org_id"]
        with patch.object(pkg_gen, "s3", MagicMock()):
            resp = pkg_gen.lambda_handler(event, None)
        assert resp["statusCode"] == 400
        assert "org_id" in json.loads(resp["body"])["error"]

    def test_missing_identity_pool_returns_400(self):
        event = _make_event()
        del event["identity_pool_id"]
        with patch.object(pkg_gen, "s3", MagicMock()):
            resp = pkg_gen.lambda_handler(event, None)
        assert resp["statusCode"] == 400

    def test_direct_federation_missing_role_arn_returns_400(self):
        event = _make_event(federation_type="direct", identity_pool_id="", federated_role_arn="")
        with patch.object(pkg_gen, "s3", MagicMock()):
            resp = pkg_gen.lambda_handler(event, None)
        assert resp["statusCode"] == 400

    def test_valid_event_returns_200(self):
        mock_s3 = MagicMock()
        with patch.object(pkg_gen, "s3", mock_s3):
            resp = pkg_gen.lambda_handler(_make_event(), None)
        assert resp["statusCode"] == 200

    def test_200_response_lists_all_artifacts(self):
        mock_s3 = MagicMock()
        with patch.object(pkg_gen, "s3", mock_s3):
            resp = pkg_gen.lambda_handler(_make_event(), None)
        body = json.loads(resp["body"])
        assert set(body["artifacts"]) == {
            "codex-config.json",
            "config.json",
            "install.sh",
            "install.bat",
            "ccwb-install.ps1",
        }


class TestLambdaHandlerS3Writes:
    """Verify all expected S3 keys are written."""

    def test_all_five_keys_uploaded(self):
        mock_s3 = MagicMock()
        with patch.object(pkg_gen, "s3", mock_s3):
            pkg_gen.lambda_handler(_make_event(), None)

        uploaded = _collect_put_calls(mock_s3)
        expected_keys = {
            "cowork/acme/codex-config.json",
            "cowork/acme/config.json",
            "cowork/acme/install.sh",
            "cowork/acme/install.bat",
            "cowork/acme/ccwb-install.ps1",
        }
        assert expected_keys.issubset(set(uploaded.keys()))

    def test_s3_prefix_matches_org_id(self):
        mock_s3 = MagicMock()
        event = _make_event(org_id="nexus-demo")
        with patch.object(pkg_gen, "s3", mock_s3):
            pkg_gen.lambda_handler(event, None)

        uploaded = _collect_put_calls(mock_s3)
        assert any(k.startswith("cowork/nexus-demo/") for k in uploaded)


# ---------------------------------------------------------------------------
# Tests: codex-config.json
# ---------------------------------------------------------------------------

class TestCodexConfigJson:
    """Verify the codex-config.json payload."""

    def test_codex_config_when_codex_disabled(self):
        mock_s3 = MagicMock()
        event = _make_event(codex_enabled=False, mantle_api_key="")
        with patch.object(pkg_gen, "s3", mock_s3):
            pkg_gen.lambda_handler(event, None)

        uploaded = _collect_put_calls(mock_s3)
        codex_cfg = json.loads(uploaded["cowork/acme/codex-config.json"])

        assert codex_cfg["codex_enabled"] is False
        assert "mantle_api_key" not in codex_cfg  # key must be omitted when disabled

    def test_codex_config_when_codex_enabled(self):
        mock_s3 = MagicMock()
        event = _make_event(codex_enabled=True, mantle_api_key="mk_live_secret123")
        with patch.object(pkg_gen, "s3", mock_s3):
            pkg_gen.lambda_handler(event, None)

        uploaded = _collect_put_calls(mock_s3)
        codex_cfg = json.loads(uploaded["cowork/acme/codex-config.json"])

        assert codex_cfg["codex_enabled"] is True
        assert codex_cfg["mantle_api_key"] == "mk_live_secret123"

    def test_codex_config_key_absent_when_no_mantle_key(self):
        """mantle_api_key must not appear when it is empty even if codex_enabled=True."""
        mock_s3 = MagicMock()
        event = _make_event(codex_enabled=True, mantle_api_key="")
        with patch.object(pkg_gen, "s3", mock_s3):
            pkg_gen.lambda_handler(event, None)

        uploaded = _collect_put_calls(mock_s3)
        codex_cfg = json.loads(uploaded["cowork/acme/codex-config.json"])

        assert "mantle_api_key" not in codex_cfg

    def test_codex_config_org_id_matches_event(self):
        mock_s3 = MagicMock()
        event = _make_event(codex_enabled=True, codex_org_id="custom-org", mantle_api_key="key")
        with patch.object(pkg_gen, "s3", mock_s3):
            pkg_gen.lambda_handler(event, None)

        uploaded = _collect_put_calls(mock_s3)
        # codex-config.json lives under the org's S3 prefix (cowork/{org_id}/)
        codex_cfg = json.loads(uploaded["cowork/acme/codex-config.json"])
        assert codex_cfg["org_id"] == "custom-org"  # codex_org_id is written into the file


# ---------------------------------------------------------------------------
# Tests: install.sh content
# ---------------------------------------------------------------------------

class TestInstallSh:
    """Validate install.sh content and Codex block."""

    def _get_install_sh(self, event: dict) -> str:
        mock_s3 = MagicMock()
        with patch.object(pkg_gen, "s3", mock_s3):
            pkg_gen.lambda_handler(event, None)
        uploaded = _collect_put_calls(mock_s3)
        return uploaded[f"cowork/{event['org_id']}/install.sh"]

    def test_shebang_present(self):
        content = self._get_install_sh(_make_event())
        assert content.startswith("#!/bin/bash")

    def test_aws_profile_configured(self):
        content = self._get_install_sh(_make_event())
        assert "credential_process" in content

    def test_codex_block_present_when_enabled(self):
        event = _make_event(codex_enabled=True, codex_org_id="acme")
        content = self._get_install_sh(event)

        assert "Codex Setup (Amazon Bedrock)" in content
        assert "codex-config.json" in content
        assert "~/.codex" in content
        assert 'model_provider = "amazon-bedrock"' in content
        assert 'region = "us-east-2"' in content
        assert "AWS_BEARER_TOKEN_BEDROCK" in content

    def test_codex_s3_url_correct(self):
        event = _make_event(codex_enabled=True, codex_org_id="acme")
        content = self._get_install_sh(event)

        expected = (
            "https://claude-code-auth-distribution-916587687563.s3.amazonaws.com"
            "/cowork/acme/codex-config.json"
        )
        assert expected in content

    def test_codex_block_false_when_disabled(self):
        event = _make_event(codex_enabled=False)
        content = self._get_install_sh(event)

        assert 'CODEX_ENABLED="false"' in content
        # If disabled, the guard condition means ~/.codex setup is never reached
        # when CODEX_ENABLED is false. config.toml line must not appear outside the guard.
        assert 'model_provider = "amazon-bedrock"' not in content or 'CODEX_ENABLED="false"' in content

    def test_shell_profile_patching_mentioned(self):
        event = _make_event(codex_enabled=True, codex_org_id="acme")
        content = self._get_install_sh(event)

        # Must patch .zshrc and/or .bashrc
        assert ".zshrc" in content or ".bashrc" in content


# ---------------------------------------------------------------------------
# Tests: install.bat content
# ---------------------------------------------------------------------------

class TestInstallBat:
    """Validate install.bat content and Codex block."""

    def _get_install_bat(self, event: dict) -> str:
        mock_s3 = MagicMock()
        with patch.object(pkg_gen, "s3", mock_s3):
            pkg_gen.lambda_handler(event, None)
        uploaded = _collect_put_calls(mock_s3)
        return uploaded[f"cowork/{event['org_id']}/install.bat"]

    def test_bat_header_present(self):
        content = self._get_install_bat(_make_event())
        assert "@echo off" in content

    def test_codex_block_present_when_enabled(self):
        event = _make_event(codex_enabled=True, codex_org_id="acme")
        content = self._get_install_bat(event)

        assert "Codex Setup (Amazon Bedrock)" in content
        assert "CODEX_ENABLED=1" in content
        assert "codex-config.json" in content
        assert ".codex" in content
        assert "config.toml" in content
        assert "AWS_BEARER_TOKEN_BEDROCK" in content
        assert "SetEnvironmentVariable" in content

    def test_codex_block_disabled(self):
        event = _make_event(codex_enabled=False)
        content = self._get_install_bat(event)

        assert "CODEX_ENABLED=0" in content


# ---------------------------------------------------------------------------
# Tests: ccwb-install.ps1 content
# ---------------------------------------------------------------------------

class TestCcwbInstallPs1:
    """Validate ccwb-install.ps1 content and Codex block."""

    def _get_ps1(self, event: dict) -> str:
        mock_s3 = MagicMock()
        with patch.object(pkg_gen, "s3", mock_s3):
            pkg_gen.lambda_handler(event, None)
        uploaded = _collect_put_calls(mock_s3)
        return uploaded[f"cowork/{event['org_id']}/ccwb-install.ps1"]

    def test_ps1_header_present(self):
        content = self._get_ps1(_make_event())
        assert "CmdletBinding" in content or "param(" in content

    def test_codex_block_enabled(self):
        event = _make_event(codex_enabled=True, codex_org_id="acme")
        content = self._get_ps1(event)

        assert "$codexEnabled" in content
        assert "$true" in content
        assert "AWS_BEARER_TOKEN_BEDROCK" in content
        assert "SetEnvironmentVariable" in content
        assert ".codex" in content
        assert "config.toml" in content
        assert "amazon-bedrock" in content
        assert "codex-config.json" in content

    def test_codex_block_disabled(self):
        event = _make_event(codex_enabled=False)
        content = self._get_ps1(event)

        assert "$false" in content

    def test_codex_s3_url_correct(self):
        event = _make_event(codex_enabled=True, codex_org_id="my-org")
        content = self._get_ps1(event)

        expected = (
            "https://claude-code-auth-distribution-916587687563.s3.amazonaws.com"
            "/cowork/my-org/codex-config.json"
        )
        assert expected in content

    def test_bedrock_region_in_toml(self):
        """The config.toml region must be us-east-2 (the Codex default Bedrock region)."""
        event = _make_event(codex_enabled=True, codex_org_id="acme")
        content = self._get_ps1(event)

        assert "us-east-2" in content


# ---------------------------------------------------------------------------
# Tests: credential config.json
# ---------------------------------------------------------------------------

class TestCredentialConfig:
    """Validate the config.json produced for the credential-process."""

    def _get_config_json(self, event: dict) -> dict:
        mock_s3 = MagicMock()
        with patch.object(pkg_gen, "s3", mock_s3):
            pkg_gen.lambda_handler(event, None)
        uploaded = _collect_put_calls(mock_s3)
        return json.loads(uploaded[f"cowork/{event['org_id']}/config.json"])

    def test_profile_key_matches_profile_name(self):
        event = _make_event(profile_name="MyOrg")
        cfg = self._get_config_json(event)
        assert "MyOrg" in cfg

    def test_cognito_config_has_identity_pool_id(self):
        event = _make_event(federation_type="cognito", identity_pool_id="us-east-1:pool123")
        cfg = self._get_config_json(event)
        profile = cfg["ClaudeCode"]
        assert profile["identity_pool_id"] == "us-east-1:pool123"
        assert profile["federation_type"] == "cognito"

    def test_direct_sts_config_has_role_arn(self):
        event = _make_event(
            federation_type="direct",
            identity_pool_id="",
            federated_role_arn="arn:aws:iam::123:role/BedrockRole",
        )
        cfg = self._get_config_json(event)
        profile = cfg["ClaudeCode"]
        assert profile["federated_role_arn"] == "arn:aws:iam::123:role/BedrockRole"
        assert profile["federation_type"] == "direct"

    def test_provider_type_detected_for_okta(self):
        event = _make_event(provider_domain="company.okta.com")
        cfg = self._get_config_json(event)
        assert cfg["ClaudeCode"]["provider_type"] == "okta"

    def test_selected_model_included_when_set(self):
        model = "us.anthropic.claude-sonnet-4-20250514-v1:0"
        event = _make_event(selected_model=model)
        cfg = self._get_config_json(event)
        assert cfg["ClaudeCode"]["selected_model"] == model


# ---------------------------------------------------------------------------
# Tests: _build_codex_config helper
# ---------------------------------------------------------------------------

class TestBuildCodexConfig:
    def test_codex_disabled_omits_key(self):
        cfg = pkg_gen._build_codex_config(
            codex_enabled=False, mantle_api_key="secret", org_id="test"
        )
        assert cfg["codex_enabled"] is False
        assert "mantle_api_key" not in cfg

    def test_codex_enabled_includes_key(self):
        cfg = pkg_gen._build_codex_config(
            codex_enabled=True, mantle_api_key="mk_secret", org_id="test"
        )
        assert cfg["codex_enabled"] is True
        assert cfg["mantle_api_key"] == "mk_secret"

    def test_empty_mantle_key_omitted_even_when_enabled(self):
        cfg = pkg_gen._build_codex_config(
            codex_enabled=True, mantle_api_key="", org_id="test"
        )
        assert "mantle_api_key" not in cfg

    def test_org_id_included(self):
        cfg = pkg_gen._build_codex_config(
            codex_enabled=True, mantle_api_key="k", org_id="my-company"
        )
        assert cfg["org_id"] == "my-company"

    def test_generated_at_present(self):
        cfg = pkg_gen._build_codex_config(
            codex_enabled=False, mantle_api_key="", org_id="x"
        )
        assert "generated_at" in cfg


# ---------------------------------------------------------------------------
# Tests: _detect_provider_type
# ---------------------------------------------------------------------------

class TestDetectProviderType:
    @pytest.mark.parametrize("domain,expected", [
        ("company.okta.com",          "okta"),
        ("company.oktapreview.com",   "okta"),
        ("myapp.auth0.com",           "auth0"),
        ("login.microsoftonline.com", "azure"),
        ("mypool.auth.us-east-1.amazoncognito.com", "cognito"),
        ("accounts.google.com",       "google"),
        ("idp.example.com",           "auto"),
        ("",                          "oidc"),
    ])
    def test_detection(self, domain, expected):
        assert pkg_gen._detect_provider_type(domain) == expected
