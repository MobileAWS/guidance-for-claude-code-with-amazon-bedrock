# ABOUTME: Tests for shared CLI utility helpers
# ABOUTME: Verifies clear_cached_credentials

"""Tests for cli/utils/helpers.py."""

import configparser

from unittest.mock import patch

from claude_code_with_bedrock.cli.utils.helpers import (
    clear_cached_credentials,
)


class TestClearCachedCredentials:
    """Tests for clear_cached_credentials utility."""

    def test_clears_ccwb_credential_section(self, tmp_path):
        """Removes credential section created by ccwb."""
        cred_file = tmp_path / ".aws" / "credentials"
        cred_file.parent.mkdir(parents=True)
        cred_file.write_text(
            "[my-profile]\n"
            "credential_process = /usr/local/bin/claude-code-with-bedrock credential-process --profile my-profile\n"
        )
        with patch("claude_code_with_bedrock.cli.utils.helpers.Path.home", return_value=tmp_path):
            result = clear_cached_credentials("my-profile")
        assert result is True
        # Verify section is gone
        config = configparser.ConfigParser()
        config.read(cred_file)
        assert "my-profile" not in config

    def test_does_not_clear_unrelated_credentials(self, tmp_path):
        """Does not touch credential sections not created by ccwb."""
        cred_file = tmp_path / ".aws" / "credentials"
        cred_file.parent.mkdir(parents=True)
        cred_file.write_text(
            "[my-profile]\n"
            "aws_access_key_id = AKIAIOSFODNN7EXAMPLE\n"  # pragma: allowlist secret
            "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\n"  # pragma: allowlist secret
        )
        with patch("claude_code_with_bedrock.cli.utils.helpers.Path.home", return_value=tmp_path):
            result = clear_cached_credentials("my-profile")
        assert result is False

    def test_returns_false_when_no_credentials_file(self, tmp_path):
        """Returns False when ~/.aws/credentials doesn't exist."""
        with patch("claude_code_with_bedrock.cli.utils.helpers.Path.home", return_value=tmp_path):
            result = clear_cached_credentials("any-profile")
        assert result is False

    def test_returns_false_when_profile_not_in_credentials(self, tmp_path):
        """Returns False when profile section doesn't exist."""
        cred_file = tmp_path / ".aws" / "credentials"
        cred_file.parent.mkdir(parents=True)
        cred_file.write_text("[other-profile]\naws_access_key_id = test\n")
        with patch("claude_code_with_bedrock.cli.utils.helpers.Path.home", return_value=tmp_path):
            result = clear_cached_credentials("my-profile")
        assert result is False
