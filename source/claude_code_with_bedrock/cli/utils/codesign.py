"""Code signing utilities for macOS binaries."""

import os
import subprocess
import tempfile
from pathlib import Path
from typing import Dict, List, Optional

from rich.console import Console


class CodeSignError(Exception):
    """Raised when code signing fails."""
    pass


class MacOSCodeSigner:
    """Handles macOS code signing and notarization for credential-process binaries."""

    def __init__(self, console: Optional[Console] = None):
        self.console = console or Console()
        self.entitlements_dir = Path(__file__).parents[3] / "codesign"

    def check_signing_requirements(self) -> Dict[str, bool]:
        """Check if code signing requirements are met."""
        requirements = {
            "codesign_available": self._command_exists("codesign"),
            "xcrun_available": self._command_exists("xcrun"),
            "developer_id_available": self._check_developer_id(),
            "keychain_accessible": self._check_keychain_access(),
        }
        return requirements

    def _command_exists(self, command: str) -> bool:
        """Check if a command exists in PATH."""
        try:
            subprocess.run([command, "--version"], capture_output=True, check=True)
            return True
        except (subprocess.CalledProcessError, FileNotFoundError):
            return False

    def _check_developer_id(self) -> bool:
        """Check if Apple Developer ID certificate is available."""
        try:
            result = subprocess.run(
                ["security", "find-identity", "-v", "-p", "codesigning"],
                capture_output=True, text=True, check=True
            )
            # Look for Developer ID Application certificate
            return "Developer ID Application" in result.stdout
        except subprocess.CalledProcessError:
            return False

    def _check_keychain_access(self) -> bool:
        """Check if keychain is accessible for signing."""
        try:
            subprocess.run(
                ["security", "list-keychains"],
                capture_output=True, check=True
            )
            return True
        except subprocess.CalledProcessError:
            return False

    def get_signing_identity(self) -> Optional[str]:
        """Get the first available Apple Developer ID signing identity."""
        try:
            result = subprocess.run(
                ["security", "find-identity", "-v", "-p", "codesigning"],
                capture_output=True, text=True, check=True
            )
            
            # Parse output to find Developer ID Application certificate
            lines = result.stdout.strip().split('\n')
            for line in lines:
                if "Developer ID Application" in line:
                    # Extract the identity (SHA-1 hash or name)
                    parts = line.split('"')
                    if len(parts) >= 2:
                        return parts[1]  # The identity name between quotes
            
            return None
        except subprocess.CalledProcessError:
            return None

    def sign_binary(
        self,
        binary_path: Path,
        binary_type: str = "credential-process",
        identity: Optional[str] = None,
        force: bool = True
    ) -> bool:
        """
        Sign a macOS binary with Apple Developer ID.

        Args:
            binary_path: Path to the binary to sign
            binary_type: Type of binary ('credential-process' or 'otel-helper')
            identity: Signing identity (auto-detected if None)
            force: Force re-signing even if already signed

        Returns:
            True if signing succeeded, False otherwise
        """
        if not binary_path.exists():
            self.console.print(f"[red]Binary not found: {binary_path}[/red]")
            return False

        # Check if running on macOS
        if os.uname().sysname != "Darwin":
            self.console.print("[yellow]Skipping code signing (not on macOS)[/yellow]")
            return True

        # Auto-detect signing identity if not provided
        if identity is None:
            identity = self.get_signing_identity()
            if identity is None:
                self.console.print("[yellow]No Apple Developer ID certificate found, skipping code signing[/yellow]")
                return True

        # Get entitlements file
        entitlements_file = self.entitlements_dir / f"{binary_type}.entitlements"
        if not entitlements_file.exists():
            self.console.print(f"[yellow]Entitlements file not found: {entitlements_file}[/yellow]")
            entitlements_file = None

        try:
            # Build codesign command
            cmd = [
                "codesign",
                "--sign", identity,
                "--timestamp",
                "--options", "runtime",  # Enable hardened runtime
                "--verbose"
            ]

            if force:
                cmd.append("--force")

            if entitlements_file:
                cmd.extend(["--entitlements", str(entitlements_file)])

            cmd.append(str(binary_path))

            self.console.print(f"[cyan]Signing {binary_path.name} with identity: {identity}[/cyan]")
            
            result = subprocess.run(cmd, capture_output=True, text=True)
            
            if result.returncode == 0:
                self.console.print(f"[green]✓ Successfully signed {binary_path.name}[/green]")
                return True
            else:
                self.console.print(f"[red]Failed to sign {binary_path.name}:[/red]")
                self.console.print(f"[red]{result.stderr}[/red]")
                return False

        except Exception as e:
            self.console.print(f"[red]Code signing error: {e}[/red]")
            return False

    def verify_signature(self, binary_path: Path) -> bool:
        """Verify the signature of a binary."""
        if not binary_path.exists():
            return False

        try:
            result = subprocess.run(
                ["codesign", "--verify", "--verbose", str(binary_path)],
                capture_output=True, text=True
            )
            return result.returncode == 0
        except Exception:
            return False

    def notarize_binary(
        self,
        binary_path: Path,
        apple_id: Optional[str] = None,
        app_password: Optional[str] = None,
        team_id: Optional[str] = None
    ) -> bool:
        """
        Notarize a signed binary with Apple.

        Args:
            binary_path: Path to the signed binary
            apple_id: Apple ID for notarization (from environment if None)
            app_password: App-specific password (from keychain if None)
            team_id: Developer team ID (auto-detected if None)

        Returns:
            True if notarization succeeded, False otherwise
        """
        if not binary_path.exists():
            self.console.print(f"[red]Binary not found: {binary_path}[/red]")
            return False

        # Check if running on macOS
        if os.uname().sysname != "Darwin":
            self.console.print("[yellow]Skipping notarization (not on macOS)[/yellow]")
            return True

        # Get credentials from environment or parameters
        apple_id = apple_id or os.environ.get("APPLE_ID")
        app_password = app_password or os.environ.get("APPLE_APP_PASSWORD") or "@keychain:AC_PASSWORD"
        team_id = team_id or os.environ.get("APPLE_TEAM_ID")

        if not apple_id:
            self.console.print("[yellow]APPLE_ID not set, skipping notarization[/yellow]")
            return True

        try:
            # Create a temporary zip file for notarization
            with tempfile.NamedTemporaryFile(suffix=".zip", delete=False) as temp_zip:
                temp_zip_path = Path(temp_zip.name)

            # Create zip archive
            subprocess.run([
                "zip", "-j", str(temp_zip_path), str(binary_path)
            ], check=True, capture_output=True)

            # Submit for notarization
            cmd = [
                "xcrun", "notarytool", "submit", str(temp_zip_path),
                "--apple-id", apple_id,
                "--password", app_password,
                "--wait"
            ]

            if team_id:
                cmd.extend(["--team-id", team_id])

            self.console.print(f"[cyan]Submitting {binary_path.name} for notarization...[/cyan]")
            
            result = subprocess.run(cmd, capture_output=True, text=True)
            
            # Clean up temporary zip
            temp_zip_path.unlink(missing_ok=True)

            if result.returncode == 0:
                self.console.print(f"[green]✓ Successfully notarized {binary_path.name}[/green]")
                
                # Staple the notarization to the binary
                staple_result = subprocess.run([
                    "xcrun", "stapler", "staple", str(binary_path)
                ], capture_output=True, text=True)
                
                if staple_result.returncode == 0:
                    self.console.print(f"[green]✓ Stapled notarization ticket to {binary_path.name}[/green]")
                else:
                    self.console.print(f"[yellow]Warning: Could not staple ticket to {binary_path.name}[/yellow]")
                
                return True
            else:
                self.console.print(f"[red]Failed to notarize {binary_path.name}:[/red]")
                self.console.print(f"[red]{result.stderr}[/red]")
                return False

        except Exception as e:
            self.console.print(f"[red]Notarization error: {e}[/red]")
            return False

    def sign_and_notarize(
        self,
        binary_path: Path,
        binary_type: str = "credential-process",
        identity: Optional[str] = None,
        apple_id: Optional[str] = None,
        app_password: Optional[str] = None,
        team_id: Optional[str] = None
    ) -> bool:
        """
        Complete code signing and notarization workflow.

        Args:
            binary_path: Path to the binary to sign and notarize
            binary_type: Type of binary ('credential-process' or 'otel-helper')
            identity: Signing identity (auto-detected if None)
            apple_id: Apple ID for notarization
            app_password: App-specific password
            team_id: Developer team ID

        Returns:
            True if both signing and notarization succeeded, False otherwise
        """
        # First, sign the binary
        if not self.sign_binary(binary_path, binary_type, identity):
            return False

        # Then notarize it
        return self.notarize_binary(binary_path, apple_id, app_password, team_id)

    def batch_sign_binaries(
        self,
        binaries: List[tuple],  # List of (platform, binary_path) tuples
        identity: Optional[str] = None,
        notarize: bool = True,
        apple_id: Optional[str] = None,
        app_password: Optional[str] = None,
        team_id: Optional[str] = None
    ) -> Dict[str, bool]:
        """
        Sign and optionally notarize multiple binaries.

        Args:
            binaries: List of (platform, binary_path) tuples
            identity: Signing identity
            notarize: Whether to notarize after signing
            apple_id: Apple ID for notarization
            app_password: App-specific password
            team_id: Developer team ID

        Returns:
            Dict mapping binary names to success status
        """
        results = {}
        
        # Filter to only macOS binaries
        macos_binaries = [(plat, path) for plat, path in binaries if plat.startswith("macos")]
        
        if not macos_binaries:
            self.console.print("[dim]No macOS binaries to sign[/dim]")
            return {}

        # Check requirements once
        requirements = self.check_signing_requirements()
        missing_requirements = [req for req, met in requirements.items() if not met]
        
        if missing_requirements:
            self.console.print(f"[yellow]Missing requirements for code signing: {', '.join(missing_requirements)}[/yellow]")
            if not requirements.get("developer_id_available", False):
                self.console.print("[yellow]To enable code signing, install an Apple Developer ID certificate[/yellow]")
            return {}

        for platform, binary_path in macos_binaries:
            binary_name = binary_path.name
            
            # Determine binary type from name
            if "credential-process" in binary_name:
                binary_type = "credential-process"
            elif "otel-helper" in binary_name:
                binary_type = "otel-helper"
            else:
                binary_type = "credential-process"  # Default

            self.console.print(f"\n[bold]Processing {binary_name}[/bold]")
            
            if notarize:
                success = self.sign_and_notarize(
                    binary_path, binary_type, identity, apple_id, app_password, team_id
                )
            else:
                success = self.sign_binary(binary_path, binary_type, identity)
            
            results[binary_name] = success

        return results

    def print_signing_status(self, results: Dict[str, bool]) -> None:
        """Print a summary of signing results."""
        if not results:
            return

        self.console.print("\n[bold]Code Signing Summary:[/bold]")
        
        for binary_name, success in results.items():
            status = "[green]✓[/green]" if success else "[red]✗[/red]"
            self.console.print(f"  {status} {binary_name}")

        successful = sum(1 for success in results.values() if success)
        total = len(results)
        
        if successful == total:
            self.console.print(f"\n[green]All {total} macOS binaries signed successfully[/green]")
        else:
            failed = total - successful
            self.console.print(f"\n[yellow]{successful}/{total} binaries signed successfully, {failed} failed[/yellow]")


def get_codesign_config_from_env() -> Dict[str, Optional[str]]:
    """Get code signing configuration from environment variables."""
    return {
        "identity": os.environ.get("APPLE_SIGNING_IDENTITY"),
        "apple_id": os.environ.get("APPLE_ID"),
        "app_password": os.environ.get("APPLE_APP_PASSWORD"),
        "team_id": os.environ.get("APPLE_TEAM_ID"),
    }


def should_enable_codesigning() -> bool:
    """Determine if code signing should be enabled based on environment."""
    # Enable if running on macOS and either:
    # 1. Developer ID is explicitly configured
    # 2. Running in CI environment with signing credentials
    if os.uname().sysname != "Darwin":
        return False
    
    config = get_codesign_config_from_env()
    
    # If identity is explicitly set, enable signing
    if config["identity"]:
        return True
    
    # If in CI with Apple ID configured, enable signing
    if os.environ.get("CI") and config["apple_id"]:
        return True
    
    # Otherwise, check if Developer ID certificate is available
    signer = MacOSCodeSigner()
    return signer._check_developer_id()
