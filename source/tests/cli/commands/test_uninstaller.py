# ABOUTME: Tests that package command generates a valid standalone uninstall.sh

"""Tests for PackageCommand._create_uninstaller."""

import os
import stat
import tempfile
from pathlib import Path

from claude_code_with_bedrock.cli.commands.package import PackageCommand
from claude_code_with_bedrock.config import Profile


def _fake_profile():
    """Minimal profile sufficient for uninstaller generation."""
    return Profile(
        name="ClaudeCode",
        provider_domain="test.okta.com",
        client_id="test-client-id",
        credential_storage="session",
        aws_region="us-east-1",
        identity_pool_name="test-pool",
        allowed_bedrock_regions=["us-east-1"],
        monitoring_enabled=False,
    )


class TestCreateUninstaller:
    """Tests for the generated uninstall.sh script."""

    def test_uninstaller_created_and_executable(self):
        command = PackageCommand()
        profile = _fake_profile()

        with tempfile.TemporaryDirectory() as tmpdir:
            output_dir = Path(tmpdir)
            path = command._create_uninstaller(output_dir, profile)

            assert path == output_dir / "uninstall.sh"
            assert path.exists()

            # Executable bit set (0o755)
            mode = path.stat().st_mode
            assert mode & stat.S_IXUSR
            assert mode & stat.S_IXGRP
            assert mode & stat.S_IXOTH

    def test_uninstaller_content(self):
        command = PackageCommand()
        profile = _fake_profile()

        with tempfile.TemporaryDirectory() as tmpdir:
            output_dir = Path(tmpdir)
            path = command._create_uninstaller(output_dir, profile)
            content = path.read_text(encoding="utf-8")

            # Shebang
            assert content.startswith("#!/bin/bash")

            # Flag parsing for all documented flags
            assert "--yes|-y" in content
            assert "--purge" in content
            assert "--keep-tokens" in content
            assert "--dev" in content
            assert "--prod" in content

            # Known-MCP fallback list is present
            for key in [
                "github",
                "slack",
                "hubspot",
                "activecampaign",
                "zapier",
                "nexus-factory",
                "web-search",
                "partner-central",
                "atlassian",
                "jira",
                "google-drive",
                "google-docs",
                "google-slides",
                "google-workspace",
                "google-docs-&-slides",
            ]:
                assert key in content, f"missing fallback MCP key: {key}"

            # References the managed-MCP state file
            assert ".claude-code-session/nexus-managed-mcps.json" in content

            # Idempotent "nothing to remove" path
            assert "nothing to remove" in content

    def test_uninstaller_written_with_lf_newlines(self):
        command = PackageCommand()
        profile = _fake_profile()

        with tempfile.TemporaryDirectory() as tmpdir:
            output_dir = Path(tmpdir)
            path = command._create_uninstaller(output_dir, profile)
            raw = path.read_bytes()

            # No CRLF line endings (written with newline="\n")
            assert b"\r\n" not in raw
