# ABOUTME: Tests for utility helper wiring into CLI commands
# ABOUTME: Verifies clear_cached_credentials is properly used

"""Tests verifying utility helpers are wired into commands."""

from pathlib import Path


class TestClearCachedCredentialsWiring:
    """Verify clear_cached_credentials is used in destroy command."""

    def test_destroy_command_clears_credentials(self):
        """destroy.py calls clear_cached_credentials after successful stack destruction."""
        source = (Path(__file__).resolve().parents[3] / "claude_code_with_bedrock" / "cli" / "commands" / "destroy.py").read_text(encoding="utf-8")
        assert "from claude_code_with_bedrock.cli.utils.helpers import clear_cached_credentials" in source
        assert "clear_cached_credentials(profile_name)" in source

    def test_credential_cleanup_after_stack_destroy(self):
        """Credential cleanup happens after stacks are destroyed, not before."""
        source = (Path(__file__).resolve().parents[3] / "claude_code_with_bedrock" / "cli" / "commands" / "destroy.py").read_text(encoding="utf-8")
        idx_destroy = source.find("stack destroyed")
        idx_clear = source.find("clear_cached_credentials(profile_name)")
        assert idx_destroy < idx_clear, "Credentials should be cleared after stack destruction"
