# ABOUTME: Package command for building distribution packages
# ABOUTME: Creates ready-to-distribute packages with embedded configuration

"""Package command - Build distribution packages."""

import json
import os
import subprocess
from datetime import datetime
from pathlib import Path

import questionary
from cleo.commands.command import Command
from cleo.helpers import option
from rich.console import Console
from rich.panel import Panel

from claude_code_with_bedrock.cli.utils.aws import get_stack_outputs
from claude_code_with_bedrock.cli.utils.display import display_configuration_info
from claude_code_with_bedrock.config import Config
from claude_code_with_bedrock.models import (
    get_source_region_for_profile,
)


class PackageCommand(Command):
    """
    Build distribution packages for your organization

    package
        {--target-platform=macos : Target platform (macos, linux, all)}
    """

    name = "package"
    description = "Build distribution packages with embedded configuration"

    options = [
        option(
            "target-platform", description="Target platform for binary (macos, linux, all)", flag=False, default="all"
        ),
        option(
            "profile", description="Configuration profile to use (defaults to active profile)", flag=False, default=None
        ),
        option(
            "regenerate-installers",
            description="Regenerate installer scripts using existing binaries from latest dist",
            flag=True,
        ),
        option(
            "go",
            description="[DEFAULT] Build binaries using Go cross-compilation (native binaries, no AV false positives)",
            flag=True,
        ),
    ]

    def handle(self) -> int:
        """Execute the package command."""
        console = Console()

        # Load configuration first
        config = Config.load()
        # Use specified profile or default to active profile, or fall back to "ClaudeCode"
        profile_name = self.option("profile") or config.active_profile or "ClaudeCode"
        profile = config.get_profile(profile_name)

        if not profile:
            console.print("[red]No deployment found. Run 'poetry run ccwb init' first.[/red]")
            return 1

        # Regenerate installers from existing binaries (no rebuild needed)
        if self.option("regenerate-installers"):
            return self._regenerate_installers(profile, profile_name, console)

        # The explicit --go flag is still accepted (a no-op now that Go is the
        # only build path). Go cross-compiles all platforms, including Windows,
        # from a single machine.

        # Interactive prompts if not provided via CLI
        target_platform = self.option("target-platform")
        if target_platform == "all":  # Default value, prompt user
            # Build list of available platform choices.
            # Note: the bare "macos"/"linux" aliases (still accepted via --target-platform) are
            # omitted here because they resolve to a fixed default arch, not the host's. The prompt
            # offers the explicit arch variants so the user picks deliberately.
            # With Go cross-compilation, every platform (including Windows) is always available.
            platform_choices = [
                "macos-arm64",
                "macos-intel",
                "linux-x64",
                "linux-arm64",
                "windows",
            ]

            # Use checkbox for multiple selection (require at least one)
            selected_platforms = questionary.checkbox(
                "Which platform(s) do you want to build for? (Use space to select, enter to confirm)",
                choices=platform_choices,
                validate=lambda x: len(x) > 0 or "You must select at least one platform",
            ).ask()

            # Use the selected platforms (guaranteed to have at least one due to validation)
            target_platform = selected_platforms if len(selected_platforms) > 1 else selected_platforms[0]

        # Prompt for co-authorship preference (default to No - opt-in approach)
        include_coauthored_by = questionary.confirm(
            "Include 'Co-Authored-By: Claude' in git commits?",
            default=False,
        ).ask()

        # Prompt for custom OTel resource attributes (only when monitoring is enabled)
        otel_resource_attributes = None
        if profile.monitoring_enabled:
            customize_otel = questionary.confirm(
                "Customize telemetry resource attributes? (department, team, cost center)",
                default=False,
            ).ask()

            if customize_otel:
                console.print(
                    "[dim]Example: department=platform, team.id=infra-core, "
                    "cost_center=CC-4521, organization=acme-corp[/dim]"
                )
                department = questionary.text("Department:", default="engineering").ask()
                team_id = questionary.text("Team ID:", default="default").ask()
                cost_center = questionary.text("Cost center:", default="default").ask()
                organization = questionary.text("Organization:", default="default").ask()
                otel_resource_attributes = (
                    f"department={department},team.id={team_id},cost_center={cost_center},organization={organization}"
                )

        # Validate platform
        valid_platforms = ["macos", "macos-arm64", "macos-intel", "linux", "linux-x64", "linux-arm64", "windows", "all"]
        if isinstance(target_platform, list):
            for platform_name in target_platform:
                if platform_name not in valid_platforms:
                    console.print(
                        f"[red]Invalid platform: {platform_name}. Valid options: {', '.join(valid_platforms)}[/red]"
                    )
                    return 1
        elif target_platform not in valid_platforms:
            console.print(
                f"[red]Invalid platform: {target_platform}. Valid options: {', '.join(valid_platforms)}[/red]"
            )
            return 1

        # Get federation identifier — try profile first, fall back to CloudFormation
        identity_pool_id = None
        federated_role_arn = None
        federation_type = profile.federation_type

        if not getattr(profile, "sso_enabled", True):
            # SSO disabled — no auth stack to query, no federation needed
            console.print("[dim]SSO disabled — skipping auth stack lookup[/dim]")
        elif federation_type == "direct" and getattr(profile, "federated_role_arn", None):
            federated_role_arn = profile.federated_role_arn
            console.print(f"[dim]Using role ARN from profile: {federated_role_arn}[/dim]")
        elif federation_type != "direct" and getattr(profile, "identity_pool_name", None):
            identity_pool_id = profile.identity_pool_name
            console.print(f"[dim]Using identity pool from profile: {identity_pool_id}[/dim]")
        else:
            # Fall back to CloudFormation stack outputs
            console.print("[yellow]Fetching deployment information from CloudFormation...[/yellow]")
            stack_outputs = get_stack_outputs(
                profile.stack_names.get("auth", f"{profile.identity_pool_name}-stack"), profile.aws_region
            )

            if not stack_outputs:
                console.print("[red]Could not fetch stack outputs. Is the stack deployed?[/red]")
                return 1

            federation_type = stack_outputs.get("FederationType", profile.federation_type)

            if federation_type == "direct":
                federated_role_arn = stack_outputs.get("DirectSTSRoleArn")
                if not federated_role_arn or federated_role_arn == "N/A":
                    federated_role_arn = stack_outputs.get("FederatedRoleArn")
                if not federated_role_arn or federated_role_arn == "N/A":
                    console.print("[red]Direct STS Role ARN not found in stack outputs.[/red]")
                    return 1
            else:
                identity_pool_id = stack_outputs.get("IdentityPoolId")
                if not identity_pool_id:
                    console.print("[red]Identity Pool ID not found in stack outputs.[/red]")
                    return 1

        # Welcome
        console.print(
            Panel.fit(
                "[bold cyan]Package Builder[/bold cyan]\n\n"
                f"Creating distribution package for {profile.provider_domain}",
                border_style="cyan",
                padding=(1, 2),
            )
        )

        # Create timestamped output directory under profile name.
        # Resolve to absolute immediately: the Go build runs subprocesses with
        # cwd=<go source dir>, and a relative path here would be interpreted
        # relative to THAT cwd (binaries would land in source/go/dist/... while
        # the config files stay in source/dist/...). Absolute path keeps binaries
        # and config co-located.
        timestamp = datetime.now().strftime("%Y-%m-%d-%H%M%S")
        output_dir = (Path.cwd() / "dist" / profile_name / timestamp).resolve()

        # Create output directory
        output_dir.mkdir(parents=True, exist_ok=True)

        # Create embedded configuration based on federation type
        embedded_config = {
            "provider_domain": profile.provider_domain,
            "client_id": profile.client_id,
            "region": profile.aws_region,
            "allowed_bedrock_regions": profile.allowed_bedrock_regions,
            "package_timestamp": timestamp,
            "package_version": "1.0.0",
            "federation_type": federation_type,
        }

        # Add federation-specific configuration
        if federation_type == "direct":
            embedded_config["federated_role_arn"] = federated_role_arn
            embedded_config["max_session_duration"] = profile.max_session_duration
        else:
            embedded_config["identity_pool_id"] = identity_pool_id

        # Show what will be packaged using shared display utility
        display_configuration_info(profile, identity_pool_id or federated_role_arn, format_type="simple")

        # Build package
        console.print("\n[bold]Building package...[/bold]")

        # Build executable(s) via Go cross-compilation. Every platform builds from
        # any machine, so "all" always expands to the full target matrix.
        all_platforms = ["macos-arm64", "macos-intel", "linux-x64", "linux-arm64", "windows"]
        if target_platform == "all" or (isinstance(target_platform, list) and "all" in target_platform):
            # Go cross-compiles every platform, so "all" always expands to the full matrix.
            platforms_to_build = all_platforms.copy()
        elif isinstance(target_platform, list):
            # User selected multiple platforms via checkbox
            platforms_to_build = []
            for platform_choice in target_platform:
                if platform_choice not in platforms_to_build:
                    platforms_to_build.append(platform_choice)
        else:
            # Single platform specified
            platforms_to_build = [target_platform]

        built_executables = []
        built_otel_helpers = []

        console.print()

        # Go cross-compilation: build all selected platforms at once
        console.print("[cyan]Building Go binaries (cross-compilation)...[/cyan]")
        try:
            go_results = self._build_go_binaries(output_dir, platforms_to_build, profile.monitoring_enabled)
            built_executables = go_results["executables"]
            built_otel_helpers = go_results["otel_helpers"]
        except Exception as e:
            console.print(f"[red]Go build failed: {e}[/red]")
            return 1

        # Check if any binaries were built
        if not built_executables:
            console.print("\n[red]Error: No binaries were successfully built.[/red]")
            console.print("Please check the error messages above.")
            return 1

        # Create configuration
        console.print("\n[cyan]Creating configuration...[/cyan]")
        # Pass the appropriate identifier based on federation type
        federation_identifier = federated_role_arn if federation_type == "direct" else identity_pool_id
        self._create_config(output_dir, profile, federation_identifier, federation_type, profile_name, console)

        # Create installer
        console.print("[cyan]Creating installer script...[/cyan]")
        self._create_installer(output_dir, profile, built_executables, built_otel_helpers)

        # Copy shell wrapper for OTEL helper (Layer 2 caching - the shell wrapper
        # warms the per-profile cache before the Go otel-helper binary runs)
        if built_otel_helpers:
            import shutil as _shutil

            shell_wrapper_src = Path(__file__).parent.parent.parent.parent / "otel_helper" / "otel-helper.sh"
            if shell_wrapper_src.exists():
                shell_wrapper_dst = output_dir / "otel-helper.sh"
                _shutil.copy2(shell_wrapper_src, shell_wrapper_dst)
                shell_wrapper_dst.chmod(0o755)
                console.print("[green]✓ OTEL helper shell wrapper included[/green]")

        # Create documentation
        console.print("[cyan]Creating documentation...[/cyan]")
        self._create_documentation(output_dir, profile, timestamp)

        # Always create Claude Code settings (required for Bedrock configuration)
        console.print("[cyan]Creating Claude Code settings...[/cyan]")
        self._create_claude_settings(output_dir, profile, include_coauthored_by, profile_name, otel_resource_attributes)

        # Generate CoWork 3P MDM configuration if enabled
        if profile.cowork_3p_enabled:
            console.print("\n[cyan]Generating CoWork 3P MDM configuration...[/cyan]")
            self._generate_cowork_3p_mdm_config(output_dir, profile, profile_name)

        # Summary
        console.print("\n[green]✓ Package created successfully![/green]")
        console.print(f"\nOutput directory: [cyan]{output_dir}[/cyan]")
        console.print("\nPackage contents:")

        # Show which binaries were built
        for platform_name, executable_path in built_executables:
            binary_name = executable_path.name
            console.print(f"  • {binary_name} - Authentication executable for {platform_name}")

        console.print("  • config.json - Configuration")
        console.print("  • install.sh - Installation script for macOS/Linux")
        # Check if Windows installer exists (created when Windows binaries are present)
        if (output_dir / "install.bat").exists():
            console.print("  • install.bat - Installation script for Windows")
            console.print("  • ccwb-install.ps1 - PowerShell installer (called by install.bat)")
        console.print("  • README.md - Installation instructions")
        if profile.monitoring_enabled and (output_dir / "claude-settings" / "settings.json").exists():
            console.print("  • claude-settings/settings.json - Claude Code telemetry settings")
            for platform_name, otel_helper_path in built_otel_helpers:
                console.print(f"  • {otel_helper_path.name} - OTEL helper executable for {platform_name}")
        if profile.cowork_3p_enabled:
            if (output_dir / "cowork-3p-config.json").exists():
                console.print("  • cowork-3p-config.json - CoWork 3P MDM configuration (JSON)")
            if (output_dir / "cowork-3p.mobileconfig").exists():
                console.print("  • cowork-3p.mobileconfig - CoWork 3P MDM profile (macOS)")
            if (output_dir / "cowork-3p.reg").exists():
                console.print("  • cowork-3p.reg - CoWork 3P registry file (Windows)")

        # Next steps
        console.print("\n[bold]Distribution steps:[/bold]")
        console.print("1. Send users the entire dist folder")
        console.print("2. Users run: chmod +x install.sh && ./install.sh")
        console.print("3. Authentication is configured automatically")

        console.print("\n[bold]To test locally:[/bold]")
        console.print(f"cd {output_dir}")
        console.print("chmod +x install.sh && ./install.sh")

        # Show next steps
        console.print("\n[bold]Next steps:[/bold]")

        # Only show distribute command if distribution is enabled
        if profile.enable_distribution:
            console.print("To create a distribution package: [cyan]poetry run ccwb distribute[/cyan]")
        else:
            console.print("Share the dist folder with your users for installation")

        return 0

    def _build_go_binaries(self, output_dir: Path, platforms: list, monitoring_enabled: bool) -> dict:
        """Build binaries using Go cross-compilation.

        Produces native statically-linked binaries for all platforms from a single machine.
        No per-platform toolchains needed — Go cross-compiles every target.

        Returns dict with 'executables' and 'otel_helpers' lists of (platform, Path) tuples.
        """
        go_src = Path(__file__).parents[3] / "go"
        if not go_src.exists():
            raise FileNotFoundError(f"Go source directory not found at {go_src}")

        # Verify Go is installed
        try:
            result = subprocess.run(["go", "version"], capture_output=True, text=True, check=True)
            self.line(f"  <info>{result.stdout.strip()}</info>")
        except (FileNotFoundError, subprocess.CalledProcessError):
            raise RuntimeError(
                "Go is not installed or not in PATH. Install from https://go.dev/dl/ or run: brew install go"
            )

        platform_map = {
            "macos-arm64": ("darwin", "arm64"),
            "macos-intel": ("darwin", "amd64"),
            "macos": ("darwin", "arm64"),  # Default to arm64 for generic macos
            "linux-x64": ("linux", "amd64"),
            "linux-arm64": ("linux", "arm64"),
            "linux": ("linux", "amd64"),  # Default to amd64 for generic linux
            "windows": ("windows", "amd64"),
        }

        executables = []
        otel_helpers = []

        binaries_to_build = ["credential-process"]
        if monitoring_enabled:
            binaries_to_build.append("otel-helper")

        for plat in platforms:
            if plat not in platform_map:
                raise ValueError(f"Unsupported platform for Go build: {plat}")

            goos, goarch = platform_map[plat]

            for binary in binaries_to_build:
                if plat == "windows":
                    suffix = "-windows.exe"
                else:
                    suffix = f"-{plat}"

                output_name = f"{binary}{suffix}"
                output_path = output_dir / output_name

                self.line(f"  Building <comment>{output_name}</comment>...")

                env = {**os.environ, "GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": "0"}
                # Windows: do NOT strip (-s -w). Defender cloud ML (Wacatac.B!ml)
                # flags stripped Go binaries. The .syso PE version-info files in
                # cmd/*/ are auto-linked by the Go compiler to help further.
                ldflags = "" if plat == "windows" else "-s -w"
                cmd = [
                    "go",
                    "build",
                    "-trimpath",
                    "-ldflags",
                    ldflags,
                    "-o",
                    str(output_path),
                    f"./cmd/{binary}/",
                ]
                result = subprocess.run(cmd, cwd=str(go_src), env=env, capture_output=True, text=True)
                if result.returncode != 0:
                    raise RuntimeError(f"Go build failed for {output_name}:\n{result.stderr}")

                if binary == "credential-process":
                    executables.append((plat, output_path))
                else:
                    otel_helpers.append((plat, output_path))

        self.line(f"  <info>Built {len(executables) + len(otel_helpers)} binaries</info>")
        return {"executables": executables, "otel_helpers": otel_helpers}

    def _regenerate_installers(self, profile, profile_name: str, console: Console) -> int:
        """Regenerate installer scripts using existing binaries from the latest dist folder."""
        import shutil

        # Find latest dist folder for this profile
        dist_base = Path("./dist") / profile_name
        if not dist_base.exists():
            console.print(f"[red]No dist folder found for profile '{profile_name}'.[/red]")
            console.print("Run 'ccwb package' first to build binaries.")
            return 1

        # Find the latest timestamped directory
        timestamp_dirs = sorted(
            [d for d in dist_base.iterdir() if d.is_dir()],
            key=lambda d: d.name,
            reverse=True,
        )
        if not timestamp_dirs:
            console.print(f"[red]No builds found in {dist_base}.[/red]")
            return 1

        source_dir = timestamp_dirs[0]
        console.print(f"[cyan]Using existing binaries from: {source_dir}[/cyan]")

        # Detect existing binaries and otel helpers
        binary_patterns = {
            "macos-arm64": "credential-process-macos-arm64",
            "macos-intel": "credential-process-macos-intel",
            "linux-x64": "credential-process-linux-x64",
            "linux-arm64": "credential-process-linux-arm64",
            "windows": "credential-process-windows.exe",
        }
        otel_patterns = {
            "macos-arm64": "otel-helper-macos-arm64",
            "macos-intel": "otel-helper-macos-intel",
            "linux-x64": "otel-helper-linux-x64",
            "linux-arm64": "otel-helper-linux-arm64",
            "windows": "otel-helper-windows.exe",
        }

        built_executables = []
        built_otel_helpers = []
        for plat, binary_name in binary_patterns.items():
            binary_path = source_dir / binary_name
            if binary_path.exists():
                built_executables.append((plat, binary_path))
        for plat, helper_name in otel_patterns.items():
            helper_path = source_dir / helper_name
            if helper_path.exists():
                built_otel_helpers.append((plat, helper_path))

        if not built_executables:
            console.print("[red]No binaries found in the dist folder.[/red]")
            return 1

        console.print(f"[green]Found {len(built_executables)} binaries, {len(built_otel_helpers)} OTEL helpers[/green]")
        for plat, path in built_executables:
            console.print(f"  • {path.name}")

        # Create new timestamped output directory
        timestamp = datetime.now().strftime("%Y-%m-%d-%H%M%S")
        output_dir = Path("./dist") / profile_name / timestamp
        output_dir.mkdir(parents=True, exist_ok=True)

        # Copy existing binaries to new output dir
        console.print("\n[cyan]Copying binaries...[/cyan]")
        for plat, binary_path in built_executables:
            shutil.copy2(binary_path, output_dir / binary_path.name)
        for plat, helper_path in built_otel_helpers:
            shutil.copy2(helper_path, output_dir / helper_path.name)

        # Get federation info — try profile first, fall back to CloudFormation
        federation_type = profile.federation_type
        federation_identifier = None

        if federation_type == "direct" and getattr(profile, "federated_role_arn", None):
            federation_identifier = profile.federated_role_arn
            console.print(f"[dim]Using role ARN from profile: {federation_identifier}[/dim]")
        elif federation_type != "direct" and getattr(profile, "identity_pool_name", None):
            federation_identifier = profile.identity_pool_name
            console.print(f"[dim]Using identity pool from profile: {federation_identifier}[/dim]")
        else:
            console.print("[cyan]Fetching deployment information from CloudFormation...[/cyan]")
            stack_outputs = get_stack_outputs(
                profile.stack_names.get("auth", f"{profile.identity_pool_name}-stack"), profile.aws_region
            )
            if not stack_outputs:
                console.print("[red]Could not fetch stack outputs. Is the stack deployed?[/red]")
                return 1

            federation_type = stack_outputs.get("FederationType", profile.federation_type)
            if federation_type == "direct":
                federation_identifier = stack_outputs.get("DirectSTSRoleArn") or stack_outputs.get("FederatedRoleArn")
            else:
                federation_identifier = stack_outputs.get("IdentityPoolId")

        if not federation_identifier or federation_identifier == "N/A":
            console.print("[red]Federation identifier not found in profile or stack outputs.[/red]")
            return 1

        # Prompt for co-authorship and OTEL attributes
        include_coauthored_by = questionary.confirm(
            "Include 'Co-Authored-By: Claude' in git commits?", default=False
        ).ask()

        otel_resource_attributes = None
        if profile.monitoring_enabled:
            customize_otel = questionary.confirm("Customize telemetry resource attributes?", default=False).ask()
            if customize_otel:
                department = questionary.text("Department:", default="engineering").ask()
                team_id = questionary.text("Team ID:", default="default").ask()
                cost_center = questionary.text("Cost center:", default="default").ask()
                organization = questionary.text("Organization:", default="default").ask()
                otel_resource_attributes = (
                    f"department={department},team.id={team_id},cost_center={cost_center},organization={organization}"
                )

        # Regenerate config.json
        console.print("[cyan]Generating configuration...[/cyan]")
        self._create_config(output_dir, profile, federation_identifier, federation_type, profile_name)

        # Regenerate installer scripts
        console.print("[cyan]Generating installer scripts...[/cyan]")
        self._create_installer(output_dir, profile, built_executables, built_otel_helpers)

        # Regenerate documentation
        console.print("[cyan]Generating documentation...[/cyan]")
        self._create_documentation(output_dir, profile, timestamp)

        # Regenerate Claude Code settings
        console.print("[cyan]Generating Claude Code settings...[/cyan]")
        self._create_claude_settings(output_dir, profile, include_coauthored_by, profile_name, otel_resource_attributes)

        # Summary
        console.print("\n[green]✓ Installers regenerated successfully![/green]")
        console.print(f"\nOutput directory: [cyan]{output_dir}[/cyan]")
        console.print("\nRegenerated files:")
        console.print("  • config.json")
        console.print("  • install.sh")
        if (output_dir / "install.bat").exists():
            console.print("  • install.bat")
            console.print("  • ccwb-install.ps1")
        console.print("  • README.md")
        if (output_dir / "claude-settings" / "settings.json").exists():
            console.print("  • claude-settings/settings.json")
        console.print(f"\nBinaries copied from: [dim]{source_dir}[/dim]")
        console.print(
            "\n[bold]Next: Run '[cyan]poetry run ccwb distribute --per-os[/cyan]' to create distribution packages.[/bold]"
        )
        return 0

    def _create_config(
        self,
        output_dir: Path,
        profile,
        federation_identifier: str,
        federation_type: str = "cognito",
        profile_name: str = "ClaudeCode",
        console=None,
    ) -> Path:
        """Create the configuration file.

        Args:
            output_dir: Directory to write config.json to
            profile: Profile object with configuration
            federation_identifier: Identity pool ID or role ARN
            federation_type: "cognito" or "direct"
            profile_name: Name to use as key in config.json (defaults to "ClaudeCode" for backward compatibility)
        """
        sso_enabled = getattr(profile, "sso_enabled", True)
        config = {
            profile_name: {
                "provider_domain": profile.provider_domain,
                "client_id": profile.client_id,
                "aws_region": profile.aws_region,
                "provider_type": profile.provider_type or self._detect_provider_type(profile.provider_domain),
                "credential_storage": profile.credential_storage,
                "cross_region_profile": profile.cross_region_profile or "us",
                "sso_enabled": sso_enabled,
            }
        }

        # Add the appropriate federation field based on type
        if not sso_enabled:
            pass  # No OIDC/Cognito fields needed — credential-process uses ambient chain
        elif federation_type == "direct":
            config[profile_name]["federated_role_arn"] = federation_identifier
            config[profile_name]["federation_type"] = "direct"
            config[profile_name]["max_session_duration"] = profile.max_session_duration
        else:
            config[profile_name]["identity_pool_id"] = federation_identifier
            config[profile_name]["federation_type"] = "cognito"

        # Add cognito_user_pool_id if it's a Cognito provider
        if profile.provider_type == "cognito" and profile.cognito_user_pool_id:
            config[profile_name]["cognito_user_pool_id"] = profile.cognito_user_pool_id

        # Add selected_model if available
        if hasattr(profile, "selected_model") and profile.selected_model:
            config[profile_name]["selected_model"] = profile.selected_model

        # Google OAuth requires client_secret for server-side token exchange (PKCE alone
        # is insufficient for the installed-app flow used by the credential-process binary).
        if getattr(profile, "provider_type", None) == "google" and getattr(profile, "client_secret", None):
            config[profile_name]["client_secret"] = profile.client_secret

        # Add confidential client fields for Azure AD if present.
        # client_secret is never written to config.json — it lives in the OS keyring.
        # End users set it with: credential-process --set-client-secret --profile <profile>
        if getattr(profile, "azure_auth_mode", None):
            config[profile_name]["azure_auth_mode"] = profile.azure_auth_mode
        if getattr(profile, "client_certificate_path", None):
            config[profile_name]["client_certificate_path"] = profile.client_certificate_path
            config[profile_name]["client_certificate_key_path"] = profile.client_certificate_key_path
            # Warn if the paths are absolute — they are machine-specific and will not
            # resolve on end-user machines with different install layouts.
            cert_is_absolute = Path(profile.client_certificate_path).is_absolute()
            key_is_absolute = Path(profile.client_certificate_key_path).is_absolute()
            if (cert_is_absolute or key_is_absolute) and console:
                console.print(
                    "\n[yellow]Warning: certificate paths in config.json are absolute and will not "
                    "resolve on machines where the files are stored elsewhere.[/yellow]"
                )
                console.print("[yellow]Instruct end users to set the following environment variables:[/yellow]")
                console.print("[dim]  AZURE_CLIENT_CERTIFICATE_PATH=<path/to/cert.pem>[/dim]")
                console.print("[dim]  AZURE_CLIENT_CERTIFICATE_KEY_PATH=<path/to/key.pem>[/dim]\n")

        # Add Generic OIDC endpoint fields (CyberArk, PingFederate, Keycloak, ForgeRock, etc.)
        if profile.provider_type == "generic":
            for field in (
                "oidc_issuer_url",
                "oidc_authorization_endpoint",
                "oidc_token_endpoint",
                "oidc_jwks_uri",
                "oidc_thumbprint",
            ):
                value = getattr(profile, field, None)
                if value:
                    config[profile_name][field] = value

        # Add custom redirect port if configured
        if getattr(profile, "redirect_port", None):
            config[profile_name]["redirect_port"] = profile.redirect_port

        # Add quota enforcement settings if configured
        if getattr(profile, "quota_api_endpoint", None):
            config[profile_name]["quota_api_endpoint"] = profile.quota_api_endpoint
            config[profile_name]["quota_fail_mode"] = getattr(profile, "quota_fail_mode", "open")
            config[profile_name]["quota_check_interval"] = getattr(profile, "quota_check_interval", 30)

        config_path = output_dir / "config.json"
        with open(config_path, "w", encoding="utf-8") as f:
            json.dump(config, f, indent=2)
        return config_path

    def _get_bedrock_region_for_profile(self, profile) -> str:
        """Get the correct AWS region for Bedrock API calls based on user-selected source region."""
        return get_source_region_for_profile(profile)

    def _detect_provider_type(self, domain: str) -> str:
        """Auto-detect provider type from domain."""
        from urllib.parse import urlparse

        if not domain:
            return "oidc"

        # Handle both full URLs and domain-only inputs
        url_to_parse = domain if domain.startswith(("http://", "https://")) else f"https://{domain}"

        try:
            parsed = urlparse(url_to_parse)
            hostname = parsed.hostname

            if not hostname:
                return "oidc"

            hostname_lower = hostname.lower()

            # Check for exact domain match or subdomain match
            # Using endswith with leading dot prevents bypass attacks
            okta_domains = (".okta.com", ".oktapreview.com", ".okta-emea.com")
            if hostname_lower.endswith(okta_domains) or hostname_lower in (
                "okta.com",
                "oktapreview.com",
                "okta-emea.com",
            ):
                return "okta"
            elif hostname_lower.endswith(".auth0.com") or hostname_lower == "auth0.com":
                return "auth0"
            elif hostname_lower.endswith(".microsoftonline.com") or hostname_lower == "microsoftonline.com":
                return "azure"
            elif hostname_lower.endswith(".windows.net") or hostname_lower == "windows.net":
                return "azure"
            elif hostname_lower.endswith(".amazoncognito.com") or hostname_lower == "amazoncognito.com":
                return "cognito"
            elif hostname_lower.startswith("cognito-idp.") and ".amazonaws.com" in hostname_lower:
                return "cognito"
            else:
                return "auto"  # Let credential-process auto-detect from domain at runtime
        except Exception:
            return "auto"  # Let credential_provider auto-detect from domain at runtime

    def _create_installer(self, output_dir: Path, profile, built_executables, built_otel_helpers=None) -> Path:
        """Create simple installer script.

        The bundle may already ship external install.sh and ccwb-install.ps1
        scripts (copied alongside the Go-built binaries). The inline templates
        below are the standard installer generation path; we skip them if
        external scripts are already in place.
        """
        installer_path = output_dir / "install.sh"
        external_ps1 = output_dir / "ccwb-install.ps1"
        if installer_path.exists() and external_ps1.exists():
            self.line("  <info>Using existing installer scripts (install.sh, ccwb-install.ps1)</info>")
            return installer_path

        # Determine which binaries were built
        platforms_built = [platform for platform, _ in built_executables]

        installer_content = f"""#!/bin/bash
# Claude Code Authentication Installer
# Organization: {profile.provider_domain}
# Generated: {datetime.now().strftime("%Y-%m-%d %H:%M:%S")}

set -e

SCRIPT_DIR="$(cd "$(dirname "${{BASH_SOURCE[0]}}")" && pwd)"
cd "$SCRIPT_DIR"

echo "======================================"
echo "Claude Code Authentication Installer"
echo "======================================"
echo
echo "Organization: {profile.provider_domain}"
echo


# Check prerequisites
echo "Checking prerequisites..."

# Auto-install Python 3 if missing
if ! command -v python3 &> /dev/null; then
    echo "⚠️  Python 3 not found. Installing..."
    if [[ "$OSTYPE" == "darwin"* ]]; then
        if command -v brew &> /dev/null; then
            brew install python@3.12
        else
            echo "Installing Homebrew first..."
            /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
            eval "$(/opt/homebrew/bin/brew shellenv 2>/dev/null || /usr/local/bin/brew shellenv 2>/dev/null)"
            brew install python@3.12
        fi
    elif [[ "$OSTYPE" == "linux-gnu"* ]]; then
        if command -v apt-get &> /dev/null; then
            sudo apt-get update && sudo apt-get install -y python3
        elif command -v yum &> /dev/null; then
            sudo yum install -y python3
        elif command -v dnf &> /dev/null; then
            sudo dnf install -y python3
        fi
    fi
    if command -v python3 &> /dev/null; then
        echo "✓ Python 3 installed"
    else
        echo "❌ Failed to install Python 3. Please install manually."
        exit 1
    fi
else
    echo "✓ Python 3 found"
fi

# Auto-install AWS CLI if missing
if ! command -v aws &> /dev/null; then
    echo "⚠️  AWS CLI not found. Installing..."
    if [[ "$OSTYPE" == "darwin"* ]]; then
        if command -v brew &> /dev/null; then
            brew install awscli
        else
            curl "https://awscli.amazonaws.com/AWSCLIV2.pkg" -o "/tmp/AWSCLIV2.pkg"
            sudo installer -pkg /tmp/AWSCLIV2.pkg -target /
            rm -f /tmp/AWSCLIV2.pkg
        fi
    elif [[ "$OSTYPE" == "linux-gnu"* ]]; then
        ARCH=$(uname -m)
        if [[ "$ARCH" == "aarch64" ]] || [[ "$ARCH" == "arm64" ]]; then
            curl "https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip" -o "/tmp/awscliv2.zip"
        else
            curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "/tmp/awscliv2.zip"
        fi
        unzip -q /tmp/awscliv2.zip -d /tmp/aws-install
        if command -v sudo &> /dev/null; then
            sudo /tmp/aws-install/aws/install
        else
            /tmp/aws-install/aws/install
        fi
        rm -rf /tmp/awscliv2.zip /tmp/aws-install
    fi
    if command -v aws &> /dev/null; then
        echo "✓ AWS CLI installed"
    else
        echo "❌ Failed to install AWS CLI. Please install from https://aws.amazon.com/cli/"
        exit 1
    fi
else
    echo "✓ AWS CLI found"
fi

# Validate prerequisites and resolve a Python interpreter for config parsing
HAS_ERRORS=false

if command -v aws &> /dev/null; then
    echo "✓ AWS CLI found (optional)"
else
    echo "ℹ  AWS CLI not found — not required. The credential process binary handles authentication directly."
fi

if [ ! -f "config.json" ]; then
    echo "ERROR: config.json not found in current directory"
    echo "       Make sure you are running this from the extracted package folder"
    HAS_ERRORS=true
fi

# Find a Python interpreter (needed for config parsing)
PYTHON=""
if command -v python3 &> /dev/null; then
    PYTHON="python3"
elif command -v python &> /dev/null; then
    PYTHON="python"
else
    echo "ERROR: Python is not installed (python3 or python)"
    echo "       Python is needed to parse configuration files"
    HAS_ERRORS=true
fi

if [ "$HAS_ERRORS" = "true" ]; then
    exit 1
fi

if [ ! -f "claude-settings/settings.json" ]; then
    echo "WARNING: claude-settings/settings.json not found"
    echo "         Claude Code IDE settings will not be configured automatically"
    echo ""
fi

echo "OK Prerequisites validated"

# Detect platform and architecture
echo
echo "Detecting platform and architecture..."
if [[ "$OSTYPE" == "darwin"* ]]; then
    PLATFORM="macos"
    ARCH=$(uname -m)
    if [[ "$ARCH" == "arm64" ]]; then
        echo "✓ Detected macOS ARM64 (Apple Silicon)"
        BINARY_SUFFIX="macos-arm64"
    else
        echo "✓ Detected macOS Intel"
        BINARY_SUFFIX="macos-intel"
    fi
elif [[ "$OSTYPE" == "linux-gnu"* ]]; then
    PLATFORM="linux"
    ARCH=$(uname -m)
    if [[ "$ARCH" == "aarch64" ]] || [[ "$ARCH" == "arm64" ]]; then
        echo "✓ Detected Linux ARM64"
        BINARY_SUFFIX="linux-arm64"
    else
        echo "✓ Detected Linux x64"
        BINARY_SUFFIX="linux-x64"
    fi
else
    echo "❌ Unsupported platform: $OSTYPE"
    echo "   This installer supports macOS and Linux only."
    exit 1
fi

# Check if binary for platform exists
CREDENTIAL_BINARY="credential-process-$BINARY_SUFFIX"
OTEL_BINARY="otel-helper-$BINARY_SUFFIX"

if [ ! -f "$CREDENTIAL_BINARY" ]; then
    echo "❌ Binary not found for your platform: $CREDENTIAL_BINARY"
    echo "   Please ensure you have the correct package for your architecture."
    exit 1
fi
"""

        installer_content += f"""
# ---------------------------------------------------------------------------
# Determine install scope (dev vs prod) and target install directory.
# Prod installs use ~/claude-code-with-bedrock (no NEXUS_ENV in credential_process).
# Dev installs use ~/claude-code-with-bedrock-dev and set NEXUS_ENV=dev so the two
# can coexist without clobbering each other. Scope is taken from NEXUS_ENV in the
# environment (default: prod).
# ---------------------------------------------------------------------------
if [ "${{NEXUS_ENV:-}}" = "dev" ]; then
    NEXUS_SCOPE="dev"
    INSTALL_DIR="$HOME/claude-code-with-bedrock-dev"
    UNINSTALL_SCOPE_FLAG="--dev"
else
    NEXUS_SCOPE="prod"
    INSTALL_DIR="$HOME/claude-code-with-bedrock"
    UNINSTALL_SCOPE_FLAG="--prod"
fi

# ---------------------------------------------------------------------------
# Idempotent pre-install cleanup.
# If a previous Nexus install is detected (either scoped install dir exists, OR a
# matching [profile] block already lives in ~/.aws/config), supersede it cleanly
# by running the bundled uninstall.sh for the SAME scope being installed. We pass
# --keep-tokens so server-side tokens are preserved. This removes stale dirs, the
# old aws profile block and stale MCP/session state BEFORE writing new state.
# Guarded so it is silent and non-fatal when uninstall.sh is not bundled.
# ---------------------------------------------------------------------------
PRIOR_INSTALL_DETECTED=false
if [ -d "$HOME/claude-code-with-bedrock" ] || [ -d "$HOME/claude-code-with-bedrock-dev" ]; then
    PRIOR_INSTALL_DETECTED=true
fi
if [ "$PRIOR_INSTALL_DETECTED" != "true" ] && [ -f ~/.aws/config ]; then
    # Any profile from config.json already present?
    for _PN in $($PYTHON -c "import json; print(' '.join(json.load(open('config.json')).keys()))" 2>/dev/null); do
        if grep -q "^\\[profile $_PN\\]" ~/.aws/config 2>/dev/null; then
            PRIOR_INSTALL_DETECTED=true
            break
        fi
    done
fi

if [ "$PRIOR_INSTALL_DETECTED" = "true" ]; then
    echo
    echo "Detected a previous installation — cleaning up before reinstalling..."
    if [ -x "$SCRIPT_DIR/uninstall.sh" ]; then
        "$SCRIPT_DIR/uninstall.sh" --yes --keep-tokens "$UNINSTALL_SCOPE_FLAG" || true
    elif [ -f "$SCRIPT_DIR/uninstall.sh" ]; then
        bash "$SCRIPT_DIR/uninstall.sh" --yes --keep-tokens "$UNINSTALL_SCOPE_FLAG" || true
    else
        echo "  (bundled uninstall.sh not found — skipping cleanup, continuing)"
    fi
fi

# Create directory
echo
echo "Installing authentication tools..."
mkdir -p "$INSTALL_DIR"
mkdir -p ~/claude-code-with-bedrock

# Copy appropriate binary
cp "$CREDENTIAL_BINARY" ~/claude-code-with-bedrock/credential-process

# Copy config
cp config.json ~/claude-code-with-bedrock/
chmod +x ~/claude-code-with-bedrock/credential-process

# macOS Gatekeeper + Keychain notices
if [[ "$OSTYPE" == "darwin"* ]]; then
    # Remove quarantine flag added by macOS when downloading unsigned binaries.
    # Without this, Gatekeeper blocks execution with "Apple could not verify..." dialog.
    xattr -d com.apple.quarantine ~/claude-code-with-bedrock/credential-process 2>/dev/null || true
    echo
    echo "⚠️  macOS Keychain Access:"
    echo "   On first use, macOS will ask for permission to access the keychain."
    echo "   This is normal and required for secure credential storage."
    echo "   Click 'Always Allow' when prompted."
fi

# Copy Claude Code settings if present
if [ -d "claude-settings" ]; then
    echo
    echo "Installing Claude Code settings..."
    mkdir -p ~/.claude

    # Copy settings and replace placeholders
    if [ -f "claude-settings/settings.json" ]; then
        # Always apply telemetry settings (merge with existing)
        if [ -f ~/.claude/settings.json ]; then
            echo "Existing Claude Code settings found"
            # Backup existing settings
            BACKUP_NAME="settings.json.backup-$(date +%Y%m%d-%H%M%S)"
            cp ~/.claude/settings.json ~/.claude/$BACKUP_NAME
            echo "  Backed up to: ~/.claude/$BACKUP_NAME"
            read -p "Overwrite with new settings? (Y/n): " -n 1 -r
            echo
            # Default to Yes if user just presses enter (empty REPLY)
            if [[ -z "$REPLY" ]]; then
                REPLY="y"
            fi
            if [[ ! $REPLY =~ ^[Yy]$ ]]; then
                echo "Skipping Claude Code settings..."
                SKIP_SETTINGS=true
            fi
        fi

        if [ "$SKIP_SETTINGS" != "true" ]; then
            # Replace placeholders and write settings
            sed -e "s|__OTEL_HELPER_PATH__|$HOME/claude-code-with-bedrock/otel-helper|g" \
                -e "s|__CREDENTIAL_PROCESS_PATH__|$HOME/claude-code-with-bedrock/credential-process|g" \
                "claude-settings/settings.json" > ~/.claude/settings.json

            # Verify placeholders were replaced
            if grep -q '__CREDENTIAL_PROCESS_PATH__\\|__OTEL_HELPER_PATH__' ~/.claude/settings.json 2>/dev/null; then
                echo "WARNING: Some path placeholders were not replaced in settings.json"
                echo "         You may need to edit the file manually: ~/.claude/settings.json"
            else
                echo "OK Claude Code settings configured: ~/.claude/settings.json"
            fi
        fi
    fi
fi

# Copy OTEL helper executable if present
if [ -f "$OTEL_BINARY" ]; then
    echo
    echo "Installing OTEL helper..."
    cp "$OTEL_BINARY" ~/claude-code-with-bedrock/otel-helper
    chmod +x ~/claude-code-with-bedrock/otel-helper
    xattr -d com.apple.quarantine ~/claude-code-with-bedrock/otel-helper 2>/dev/null || true
    echo "✓ OTEL helper installed"
fi

# Add debug info if OTEL helper was installed
if [ -f ~/claude-code-with-bedrock/otel-helper ]; then
    echo "The OTEL helper will extract user attributes from authentication tokens"
    echo "and include them in metrics. To test the helper, run:"
    echo "  ~/claude-code-with-bedrock/otel-helper --test"
fi

# Update AWS config
echo
echo "Configuring AWS profiles..."
mkdir -p ~/.aws

# Read all profiles from config.json
PROFILES=$($PYTHON -c "import json; profiles = list(json.load(open('config.json')).keys()); print(' '.join(profiles))")

if [ -z "$PROFILES" ]; then
    echo "❌ No profiles found in config.json"
    exit 1
fi

echo "Found profiles: $PROFILES"
echo

# Get region from package settings (for Bedrock calls, not infrastructure)
if [ -f "claude-settings/settings.json" ]; then
    DEFAULT_REGION=$($PYTHON -c "
import json
print(json.load(open('claude-settings/settings.json'))['env']['AWS_REGION'])
" 2>/dev/null || echo "{profile.aws_region}")
else
    DEFAULT_REGION="{profile.aws_region}"
fi

# Configure each profile
for PROFILE_NAME in $PROFILES; do
    echo "Configuring AWS profile: $PROFILE_NAME"

    # Robustly remove ONLY the exact [profile <name>] INI block (if present) so we
    # never end up with two blocks sharing the same profile name. This replaces the
    # old naive `sed` range-delete which could clobber adjacent blocks or leave dupes.
    # It parses ~/.aws/config as INI sections and drops just the matching section,
    # regardless of whether the previous block was a dev or prod variant.
    if [ -f ~/.aws/config ]; then
        NEXUS_PROFILE_NAME="$PROFILE_NAME" $PYTHON - "$HOME/.aws/config" << 'PYEOF' || true
import os, sys, re

path = sys.argv[1]
target = os.environ.get("NEXUS_PROFILE_NAME", "")
header = "[profile %s]" % target

with open(path, "r", encoding="utf-8") as fh:
    lines = fh.readlines()

out = []
skip = False
section_re = re.compile(r"^\\s*\\[")
for line in lines:
    stripped = line.strip()
    if section_re.match(line):
        # Starting a new section: skip only if it's the exact matching profile block.
        skip = (stripped == header)
        if skip:
            continue
    if skip:
        continue
    out.append(line)

# Trim trailing blank lines so appended block stays tidy.
while out and out[-1].strip() == "":
    out.pop()

with open(path, "w", encoding="utf-8") as fh:
    fh.write("".join(out))
    if out:
        fh.write("\\n")
PYEOF
    fi

    # Get profile-specific region from config.json
    PROFILE_REGION=$($PYTHON -c "
import json
print(json.load(open('config.json')).get('$PROFILE_NAME', {{}}).get('aws_region', '$DEFAULT_REGION'))
")

    # Build the credential_process line. Dev installs prefix `env NEXUS_ENV=dev` and
    # use the -dev install dir so dev and prod never clobber each other's profile.
    if [ "$NEXUS_SCOPE" = "dev" ]; then
        CRED_PROCESS="env NEXUS_ENV=dev $INSTALL_DIR/credential-process --profile $PROFILE_NAME"
    else
        CRED_PROCESS="$INSTALL_DIR/credential-process --profile $PROFILE_NAME"
    fi

    # Add fresh profile block (guaranteed single block per name after dedupe above).
    cat >> ~/.aws/config << EOF
[profile $PROFILE_NAME]
credential_process = $CRED_PROCESS
region = $PROFILE_REGION
EOF
    echo "  ✓ Created AWS profile '$PROFILE_NAME'"
done

# Post-install validation
echo
echo "Validating installation..."
if [ -f ~/claude-code-with-bedrock/credential-process ]; then
    echo "  OK credential-process: ~/claude-code-with-bedrock/credential-process"
else
    echo "  FAIL credential-process not found at: ~/claude-code-with-bedrock/credential-process"
fi
if [ -f ~/.claude/settings.json ]; then
    echo "  OK settings.json: ~/.claude/settings.json"
else
    echo "  WARN settings.json not found at: ~/.claude/settings.json"
fi

# Install Node.js if missing (required for Claude Code CLI)
if ! command -v node &> /dev/null; then
    echo "⚠️  Node.js not found. Installing..."
    if [[ "$OSTYPE" == "darwin"* ]]; then
        if command -v brew &> /dev/null; then
            brew install node
        else
            curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.3/install.sh | bash
            export NVM_DIR="$HOME/.nvm" && [ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"
            nvm install --lts
        fi
    elif [[ "$OSTYPE" == "linux-gnu"* ]]; then
        curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash - 2>/dev/null
        if command -v apt-get &> /dev/null; then
            sudo apt-get install -y nodejs
        elif command -v yum &> /dev/null; then
            sudo yum install -y nodejs
        fi
    fi
    if command -v node &> /dev/null; then
        echo "✓ Node.js installed ($(node --version))"
    else
        echo "❌ Could not install Node.js. Install from https://nodejs.org"
    fi
else
    echo "✓ Node.js found ($(node --version))"
fi

# Install Claude Code CLI if not already installed
if ! command -v claude &> /dev/null; then
    echo "Installing Claude Code CLI..."
    if command -v npm &> /dev/null; then
        # Don't use sudo with nvm (user-space node manager)
        if [ -d "$HOME/.nvm" ]; then
            npm install -g @anthropic-ai/claude-code 2>/dev/null && echo "✓ Claude Code CLI installed" || echo "⚠️  Could not install Claude Code CLI. Run: npm install -g @anthropic-ai/claude-code"
        else
            sudo npm install -g @anthropic-ai/claude-code 2>/dev/null && echo "✓ Claude Code CLI installed" || echo "⚠️  Could not install Claude Code CLI. Run: sudo npm install -g @anthropic-ai/claude-code"
        fi
    else
        echo "⚠️  npm not found. Install Node.js from https://nodejs.org then run:"
        echo "    npm install -g @anthropic-ai/claude-code"
    fi
else
    echo "✓ Claude Code CLI already installed"
fi

# ---------------------------------------------------------------------------
# Ownership safety net.
# Some of the steps above (AWS CLI pkg install, npm -g install, etc.) may run under
# sudo and can leave root-owned files inside $HOME. Reclaim ownership for the current
# user so subsequent (non-sudo) runs and normal use are not blocked. Each chown is
# guarded so it only runs when the path exists, and is non-fatal.
_NEXUS_OWNER="$(id -un)"
_NEXUS_GROUP="$(id -gn)"
for _p in \
    "$HOME/.npm" \
    "$HOME/claude-code-with-bedrock" \
    "$HOME/claude-code-with-bedrock-dev" \
    "$HOME/.claude" \
    "$HOME/.claude.json" \
    "$HOME/.claude-code-session"; do
    if [ -e "$_p" ]; then
        chown -R "$_NEXUS_OWNER":"$_NEXUS_GROUP" "$_p" 2>/dev/null || true
    fi
done

echo
echo "======================================"
echo "Installation complete!"
echo "======================================"
echo
echo "Available profiles:"
for PROFILE_NAME in $PROFILES; do
    echo "  - $PROFILE_NAME"
done
echo
echo "To use Claude Code authentication:"
echo "  export AWS_PROFILE=<profile-name>"
echo "  aws sts get-caller-identity"
echo
echo "Example:"
FIRST_PROFILE=$(echo $PROFILES | awk '{{print $1}}')
echo "  export AWS_PROFILE=$FIRST_PROFILE"
echo "  aws sts get-caller-identity"
echo
echo "Note: Authentication will automatically open your browser when needed."
echo

# Create a launcher script on PATH
echo "Creating Claude launcher..."
LAUNCHER="/usr/local/bin/claude-code"
cat > /tmp/claude-code-launcher << 'LAUNCHER_EOF'
#!/bin/bash
# Claude Code Launcher - AllCode Nexus
export AWS_PROFILE="PROFILE_PLACEHOLDER"
export CLAUDE_CODE_USE_BEDROCK=1
unset ANTHROPIC_API_KEY
exec claude "$@"
LAUNCHER_EOF

# Replace placeholder with actual profile
sed -i.bak "s/PROFILE_PLACEHOLDER/$FIRST_PROFILE/" /tmp/claude-code-launcher
rm -f /tmp/claude-code-launcher.bak

# Install to PATH (try /usr/local/bin, fall back to ~/bin)
if [ -w /usr/local/bin ]; then
    cp /tmp/claude-code-launcher /usr/local/bin/claude-code
    chmod +x /usr/local/bin/claude-code
    echo "✓ Created /usr/local/bin/claude-code"
else
    sudo cp /tmp/claude-code-launcher /usr/local/bin/claude-code 2>/dev/null && sudo chmod +x /usr/local/bin/claude-code 2>/dev/null
    if [ $? -eq 0 ]; then
        echo "✓ Created /usr/local/bin/claude-code"
    else
        mkdir -p ~/bin
        cp /tmp/claude-code-launcher ~/bin/claude-code
        chmod +x ~/bin/claude-code
        echo "✓ Created ~/bin/claude-code"
        echo "  Add ~/bin to your PATH if not already: export PATH=\\"\\$HOME/bin:\\$PATH\\""
    fi
fi
rm -f /tmp/claude-code-launcher

echo
echo "======================================"
echo "✓ Installation complete!"
echo "======================================"
echo
echo "To launch Claude Code, just run:"
echo "  claude-code"
echo
echo "Or use the standard command with the profile:"
echo "  export AWS_PROFILE=$FIRST_PROFILE"
echo "  claude"
echo

# Ask if they want to launch now
read -p "Launch Claude Code now? (Y/n): " -n 1 -r
echo
if [[ -z "$REPLY" ]] || [[ $REPLY =~ ^[Yy]$ ]]; then
    export AWS_PROFILE="$FIRST_PROFILE"
    export CLAUDE_CODE_USE_BEDROCK=1
    unset ANTHROPIC_API_KEY
    echo "Starting Claude Code..."
    exec claude
fi

# -----------------------------------------------------------------------
# Optional: Codex configuration
# If the installer bundle contains a codex-config.json file (written by the
# package builder when the organisation has Codex enabled), configure
# ~/.codex/config.toml and update shell RC files with the Bedrock bearer
# token.  When the file is absent the block is silently skipped.
# -----------------------------------------------------------------------
if [ -f "codex-config.json" ]; then
    CODEX_API_KEY=$($PYTHON -c "
import json
print(json.load(open('codex-config.json')).get('codex_api_key', ''))
" 2>/dev/null || echo "")
    CODEX_MODEL_PROVIDER=$($PYTHON -c "
import json
print(json.load(open('codex-config.json')).get('model_provider', 'amazon-bedrock'))
" 2>/dev/null || echo "amazon-bedrock")

    if [ -n "$CODEX_API_KEY" ]; then
        mkdir -p ~/.codex
        cat > ~/.codex/config.toml << CODEX_TOML
model_provider = "amazon-bedrock"
bedrock_api_key = "$CODEX_API_KEY"
CODEX_TOML

        # Append the bearer-token export to both common RC files so it is
        # available regardless of which shell the user runs.
        for RC_FILE in ~/.zshrc ~/.bashrc; do
            if [ -f "$RC_FILE" ] || [[ "$RC_FILE" == ~/.zshrc && "$OSTYPE" == "darwin"* ]]; then
                # Remove any pre-existing line to avoid duplicates on re-runs.
                grep -v 'AWS_BEARER_TOKEN_BEDROCK' "$RC_FILE" > /tmp/_rc_tmp 2>/dev/null && mv /tmp/_rc_tmp "$RC_FILE" || true
                echo "export AWS_BEARER_TOKEN_BEDROCK=$CODEX_API_KEY" >> "$RC_FILE"
            fi
        done

        echo "✓ Codex configuration installed"
    fi
fi
"""

        installer_path = output_dir / "install.sh"
        with open(installer_path, "w", encoding="utf-8", newline="\n") as f:
            f.write(installer_content)
        installer_path.chmod(0o755)

        # Create .command wrapper for macOS double-click install
        command_path = output_dir / "Install Claude Code.command"
        with open(command_path, "w", encoding="utf-8") as f:
            f.write('#!/bin/bash\ncd "$(dirname "$0")"\n./install.sh\necho\necho "Launching Claude Code..."\nclaude\n')
        command_path.chmod(0o755)

        # Create run.sh - quick launcher that sets profile and verifies credentials
        run_script = output_dir / "run.sh"
        run_content = f"""#!/bin/bash
# Claude Code Quick Launcher
# Sets the AWS profile and verifies credentials before launching Claude Code

# Use first available profile from config.json
PROFILE=$(python3 -c "import json; print(list(json.load(open('$HOME/claude-code-with-bedrock/config.json')).keys())[0])" 2>/dev/null || echo "{profile.name}")

export AWS_PROFILE="$PROFILE"
echo "Using AWS profile: $PROFILE"
echo

# Verify credentials
echo "Verifying credentials..."
if aws sts get-caller-identity > /dev/null 2>&1; then
    echo "✓ Authenticated"
    echo
    # Launch Claude Code
    exec claude "$@"
else
    echo "❌ Authentication failed. Opening browser for SSO login..."
    ~/claude-code-with-bedrock/credential-process --profile "$PROFILE" > /dev/null 2>&1
    if aws sts get-caller-identity > /dev/null 2>&1; then
        echo "✓ Authenticated"
        echo
        exec claude "$@"
    else
        echo "❌ Could not authenticate. Please check your configuration."
        exit 1
    fi
fi
"""
        with open(run_script, "w", encoding="utf-8") as f:
            f.write(run_content)
        run_script.chmod(0o755)

        # Create Windows installer when Windows binaries are present
        if "windows" in platforms_built:
            self._create_windows_installer(output_dir, profile)

        return installer_path

    def _create_windows_installer(self, output_dir: Path, profile) -> Path:
        """Create Windows batch installer script."""

        installer_content = f"""@echo off
SETLOCAL ENABLEDELAYEDEXPANSION
cd /d "%~dp0"
REM Claude Code Authentication Installer for Windows
REM Organization: {profile.provider_domain}
REM Generated: {datetime.now().strftime("%Y-%m-%d %H:%M:%S")}

echo ======================================
echo Claude Code Authentication Installer
echo ======================================
echo.
echo Organization: {profile.provider_domain}
echo.

REM Check prerequisites
echo Checking prerequisites...

where aws >nul 2>&1
if %errorlevel% neq 0 (
    echo INFO: AWS CLI not found -- not required. The credential process binary handles authentication directly.
) else (
    echo OK AWS CLI found [optional]
)

echo OK Prerequisites found
echo.

REM Create directory
echo Installing authentication tools...
if not exist "%USERPROFILE%\\claude-code-with-bedrock" mkdir "%USERPROFILE%\\claude-code-with-bedrock"

REM Copy credential process executable with renamed target
echo Copying credential process...
copy /Y "credential-process-windows.exe" "%USERPROFILE%\\claude-code-with-bedrock\\credential-process.exe" >nul
if %errorlevel% neq 0 (
    echo ERROR: Failed to copy credential-process-windows.exe
    pause
    exit /b 1
)

REM Copy OTEL helper if it exists with renamed target
if exist "otel-helper-windows.exe" (
    echo Copying OTEL helper...
    copy /Y "otel-helper-windows.exe" "%USERPROFILE%\\claude-code-with-bedrock\\otel-helper.exe" >nul
)

REM Copy configuration
echo Copying configuration...
copy /Y "config.json" "%USERPROFILE%\\claude-code-with-bedrock\\" >nul

REM Copy Claude Code settings if they exist
if exist "claude-settings" (
    echo Copying Claude Code telemetry settings...
    if not exist "%USERPROFILE%\\.claude" mkdir "%USERPROFILE%\\.claude"

    REM Copy settings and replace placeholders
    if exist "claude-settings\\settings.json" (
        set SKIP_SETTINGS=false
        if exist "%USERPROFILE%\\.claude\\settings.json" (
            echo Existing Claude Code settings found
            set /p OVERWRITE="Overwrite with new settings? (y/n): "
            if /i not "%OVERWRITE%"=="y" (
                echo Skipping Claude Code settings...
                set SKIP_SETTINGS=true
            )
        )

        if not "%SKIP_SETTINGS%"=="true" (
            REM Use PowerShell to replace placeholders
            powershell -Command "$otelPath = $env:USERPROFILE + '\\claude-code-with-bedrock\\otel-helper.exe' -replace '\\\\', '/'; $credPath = $env:USERPROFILE + '\\claude-code-with-bedrock\\credential-process.exe' -replace '\\\\', '/'; (Get-Content 'claude-settings\\settings.json') -replace '__OTEL_HELPER_PATH__', $otelPath -replace '__CREDENTIAL_PROCESS_PATH__', $credPath | Set-Content (Join-Path $env:USERPROFILE '.claude\\settings.json')"
            echo OK Claude Code settings configured
        )
    )
)

REM Configure AWS profiles
echo.
echo Configuring AWS profiles...

REM Configure AWS profiles by writing %USERPROFILE%\\.aws\\config directly (no AWS CLI dependency)
if not exist "%USERPROFILE%\\.aws" mkdir "%USERPROFILE%\\.aws"

REM Purge any stale stanza from %USERPROFILE%\\.aws\\credentials. The credential
REM chain resolves that file before credential_process in %USERPROFILE%\\.aws\\config,
REM so a leftover [profile-name] block (e.g. EXPIRED placeholder written by an
REM older ccwb auth logout) would shadow credential_process and break Cowork
REM Desktop with a 403 InvalidClientTokenId.
powershell -NoProfile -Command "$ErrorActionPreference = 'Stop'; $awsCreds = Join-Path $env:USERPROFILE '.aws\\credentials'; if (Test-Path $awsCreds) {{ $cfg = Get-Content config.json | ConvertFrom-Json; $existing = Get-Content $awsCreds -Raw; foreach ($p in $cfg.PSObject.Properties.Name) {{ $pattern = '(?ms)^\\[' + [regex]::Escape($p) + '\\].*?(?=^\\[|\\Z)'; $existing = [regex]::Replace($existing, $pattern, '') }}; Set-Content -Path $awsCreds -Value $existing.TrimStart() -NoNewline -Encoding ASCII }}"

powershell -NoProfile -Command "$ErrorActionPreference = 'Stop'; $nl = [char]13 + [char]10; $cfg = Get-Content config.json | ConvertFrom-Json; $awsConfig = Join-Path $env:USERPROFILE '.aws\\config'; $credProcess = Join-Path $env:USERPROFILE 'claude-code-with-bedrock\\credential-process.exe'; $existing = if (Test-Path $awsConfig) {{ Get-Content $awsConfig -Raw }} else {{ '' }}; foreach ($p in $cfg.PSObject.Properties.Name) {{ $region = $cfg.$p.aws_region; if (-not $region) {{ $region = '{profile.aws_region}' }}; $pattern = '(?ms)^\\[profile ' + [regex]::Escape($p) + '\\].*?(?=^\\[|\\Z)'; $existing = [regex]::Replace($existing, $pattern, ''); $stanza = '[profile ' + $p + ']' + $nl + 'credential_process = ' + $credProcess + ' --profile ' + $p + $nl + 'region = ' + $region + $nl; $existing = $existing.TrimEnd() + $nl + $nl + $stanza; Write-Host ('  OK Configured AWS profile ' + $p) }}; Set-Content -Path $awsConfig -Value $existing.TrimStart() -NoNewline -Encoding ASCII"
if %errorlevel% neq 0 (
    echo ERROR: Failed to configure AWS profiles
    pause
    exit /b 1
)

echo.
echo ======================================

REM Install Claude Code CLI if not already installed
where claude >nul 2>nul
if %errorlevel% neq 0 (
    echo Installing Claude Code CLI...
    where npm >nul 2>nul
    if %errorlevel% equ 0 (
        npm install -g @anthropic-ai/claude-code
        if %errorlevel% equ 0 (
            echo OK Claude Code CLI installed
        ) else (
            echo WARNING: Could not install Claude Code CLI. Run: npm install -g @anthropic-ai/claude-code
        )
    ) else (
        echo WARNING: npm not found. Install Node.js from https://nodejs.org then run:
        echo     npm install -g @anthropic-ai/claude-code
    )
) else (
    echo OK Claude Code CLI already installed
)

echo.
echo ======================================
echo Installation complete!
echo ======================================
echo.
echo Available profiles:
for /f %%p in ('powershell -NoProfile -Command "(Get-Content config.json | ConvertFrom-Json).PSObject.Properties.Name"') do (
    echo   - %%p
)
echo.
echo To use Claude Code authentication:
echo   set AWS_PROFILE=^<profile-name^>
echo   aws sts get-caller-identity
echo.
echo Example:
for /f %%p in ('powershell -NoProfile -Command "(Get-Content config.json | ConvertFrom-Json).PSObject.Properties.Name | Select-Object -First 1"') do (
    echo   set AWS_PROFILE=%%p
    echo   aws sts get-caller-identity
)
echo.
echo Note: Authentication will automatically open your browser when needed.
echo.

REM -----------------------------------------------------------------------
REM Optional: Codex configuration
REM If the installer bundle contains a codex-config.json file (written by
REM the package builder when the organisation has Codex enabled), configure
REM %%USERPROFILE%%\.codex\config.toml and persist the Bedrock bearer token
REM as a permanent user environment variable.  When the file is absent the
REM block is silently skipped.
REM -----------------------------------------------------------------------
if exist "codex-config.json" (
    powershell -NoProfile -Command "$ErrorActionPreference = 'Stop'; $cfg = Get-Content 'codex-config.json' | ConvertFrom-Json; $apiKey = $cfg.codex_api_key; $provider = if ($cfg.model_provider) {{ $cfg.model_provider }} else {{ 'amazon-bedrock' }}; if ($apiKey) {{ $codexDir = Join-Path $env:USERPROFILE '.codex'; if (-not (Test-Path $codexDir)) {{ New-Item -ItemType Directory -Path $codexDir | Out-Null }}; $toml = \"model_provider = `\"amazon-bedrock`\"`r`nbedrock_api_key = `\"$apiKey`\"\"; Set-Content -Path (Join-Path $codexDir 'config.toml') -Value $toml -Encoding UTF8; [System.Environment]::SetEnvironmentVariable('AWS_BEARER_TOKEN_BEDROCK', $apiKey, 'User'); Write-Host '✓ Codex configuration installed' }}"
)

pause
"""

        installer_path = output_dir / "install.bat"
        with open(installer_path, "w", encoding="utf-8") as f:
            f.write(installer_content)

        # Note: chmod not needed on Windows batch files
        return installer_path

    def _create_documentation(self, output_dir: Path, profile, timestamp: str):
        """Create user documentation."""
        readme_content = f"""# Claude Code Authentication Setup

## Quick Start

### macOS/Linux

1. Extract the package:
   ```bash
   unzip claude-code-package-*.zip
   cd claude-code-package
   ```

2. Run the installer:
   ```bash
   chmod +x install.sh && ./install.sh
   ```

3. Use the AWS profile:
   ```bash
   export AWS_PROFILE=ClaudeCode
   aws sts get-caller-identity
   ```

### Windows

#### Step 1: Download the Package
```powershell
# Use the Invoke-WebRequest command provided by your IT administrator
Invoke-WebRequest -Uri "URL_PROVIDED" -OutFile "claude-code-package.zip"
```

#### Step 2: Extract the Package

**Option A: Using Windows Explorer**
1. Right-click on `claude-code-package.zip`
2. Select "Extract All..."
3. Choose a destination folder
4. Click "Extract"

**Option B: Using PowerShell**
```powershell
# Extract to current directory
Expand-Archive -Path "claude-code-package.zip" -DestinationPath "claude-code-package"

# Navigate to the extracted folder
cd claude-code-package
```

**Option C: Using Command Prompt**
```cmd
# If you have tar available (Windows 10 1803+)
tar -xf claude-code-package.zip

# Or use PowerShell from Command Prompt
powershell -command "Expand-Archive -Path 'claude-code-package.zip' -DestinationPath 'claude-code-package'"

cd claude-code-package
```

#### Step 3: Run the Installer
```cmd
install.bat
```

The installer will:
- Check for AWS CLI installation
- Copy authentication tools to `%USERPROFILE%\\claude-code-with-bedrock`
- Configure the AWS profile "ClaudeCode"
- Test the authentication

#### Step 4: Use Claude Code
```cmd
# Set the AWS profile
set AWS_PROFILE=ClaudeCode

# Verify authentication works
aws sts get-caller-identity

# Your browser will open automatically for authentication if needed
```

For PowerShell users:
```powershell
$env:AWS_PROFILE = "ClaudeCode"
aws sts get-caller-identity
```

## What This Does

- Installs the Claude Code authentication tools
- Configures your AWS CLI to use {profile.provider_domain} for authentication
- Sets up automatic credential refresh via your browser

## Requirements

- Python 3.8 or later
- AWS CLI v2
- pip3

## Troubleshooting

### macOS Keychain Access Popup
On first use, macOS will ask for permission to access the keychain. This is normal and required for \
secure credential storage. Click "Always Allow" to avoid repeated prompts.

### Authentication Issues
If you encounter issues with authentication:
- Ensure you're assigned to the Claude Code application in your identity provider
- Check that port 8400 is available for the callback
- Contact your IT administrator for help

### Authentication Behavior

The system handles authentication automatically:
- Your browser will open when authentication is needed
- Credentials are cached securely to avoid repeated logins
- Bad credentials are automatically cleared and re-authenticated

To manually clear cached credentials (if needed):
```bash
~/claude-code-with-bedrock/credential-process --clear-cache
```

This will force re-authentication on your next AWS command.

### Browser doesn't open
Check that you're not in an SSH session. The browser needs to open on your local machine.

## Support

Contact your IT administrator for help.

Configuration Details:
- Organization: {profile.provider_domain}
- Region: {profile.aws_region}
- Package Version: {timestamp}"""

        # Add analytics information if enabled
        if profile.monitoring_enabled and getattr(profile, "analytics_enabled", True):
            analytics_section = f"""

## Analytics Dashboard

Your organization has enabled advanced analytics for Claude Code usage. You can access detailed metrics \
and reports through AWS Athena.

To view analytics:
1. Open the AWS Console in region {profile.aws_region}
2. Navigate to Athena
3. Select the analytics workgroup and database
4. Run pre-built queries or create custom reports

Available metrics include:
- Token usage by user
- Cost allocation
- Model usage patterns
- Activity trends
"""
            readme_content += analytics_section

        readme_content += "\n"

        with open(output_dir / "README.md", "w", encoding="utf-8") as f:
            f.write(readme_content)

    def _create_claude_settings(
        self,
        output_dir: Path,
        profile: object,
        include_coauthored_by: bool = True,
        profile_name: str = "ClaudeCode",
        otel_resource_attributes: str | None = None,
    ) -> None:
        """Create Claude Code settings.json with Bedrock and optional monitoring configuration."""
        console = Console()

        try:
            # Create claude-settings directory (visible, not hidden)
            claude_dir = output_dir / "claude-settings"
            claude_dir.mkdir(exist_ok=True)

            # Start with basic settings required for Bedrock
            settings = {
                "env": {
                    "CLAUDE_CODE_USE_BEDROCK": "1",
                    # AWS_REGION determines which regional Bedrock endpoint the SDK uses.
                    "AWS_REGION": self._get_bedrock_region_for_profile(profile),
                    # AWS_PROFILE is used by both AWS SDK and otel-helper
                    "AWS_PROFILE": profile_name,
                    # AWS_CREDENTIAL_PROCESS allows the AWS SDK to obtain credentials
                    # directly without requiring the AWS CLI or ~/.aws/config.
                    # The __CREDENTIAL_PROCESS_PATH__ placeholder is replaced by
                    # install.sh/install.bat with the actual binary path at install time.
                    "AWS_CREDENTIAL_PROCESS": f"__CREDENTIAL_PROCESS_PATH__ --profile {profile_name}",
                }
            }

            # Add includeCoAuthoredBy setting if user wants to disable it (Claude Code defaults to true)
            # Only add the field if the user wants it disabled
            if not include_coauthored_by:
                settings["includeCoAuthoredBy"] = False

            # Add awsAuthRefresh for session-based credential storage
            if profile.credential_storage == "session":
                settings["awsAuthRefresh"] = f"__CREDENTIAL_PROCESS_PATH__ --profile {profile_name}"

            # Add ANTHROPIC_MODEL if user selected a model during init
            if hasattr(profile, "selected_model") and profile.selected_model:
                from claude_code_with_bedrock.models import get_claude_code_alias, resolve_model_for_tier

                # Use a Claude Code alias (sonnet/opus/opusplan/haiku) so ANTHROPIC_MODEL
                # feeds through the DEFAULT_*_MODEL resolution chain for CRIS-aware routing.
                # model_alias is set during ccwb init (e.g. opus vs opusplan for Opus models).
                alias = getattr(profile, "model_alias", None) or get_claude_code_alias(profile.selected_model)
                settings["env"]["ANTHROPIC_MODEL"] = alias or profile.selected_model

                # Set all model tier env vars using the CRIS prefix from init.
                # Claude Code uses these to resolve the correct CRIS-prefixed
                # models for each tier (small/fast, default sonnet/opus/haiku).
                # This ensures all tiers respect the admin's routing geography
                # choice and works correctly with model aliases like 'opus', 'sonnet', 'haiku', 'opusplan'.
                cris_prefix = getattr(profile, "cross_region_profile", None) or "us"

                haiku_model = resolve_model_for_tier("haiku", cris_prefix)
                sonnet_model = resolve_model_for_tier("sonnet", cris_prefix)
                opus_model = resolve_model_for_tier("opus", cris_prefix)

                if haiku_model:
                    settings["env"]["ANTHROPIC_SMALL_FAST_MODEL"] = haiku_model
                    settings["env"]["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = haiku_model
                if sonnet_model:
                    settings["env"]["ANTHROPIC_DEFAULT_SONNET_MODEL"] = sonnet_model
                if opus_model:
                    settings["env"]["ANTHROPIC_DEFAULT_OPUS_MODEL"] = opus_model

                # Override with Application Inference Profile ARNs when configured
                opus_arn = getattr(profile, "inference_profile_opus_arn", None)
                sonnet_arn = getattr(profile, "inference_profile_sonnet_arn", None)
                haiku_arn = getattr(profile, "inference_profile_haiku_arn", None)

                if opus_arn:
                    settings["env"]["ANTHROPIC_DEFAULT_OPUS_MODEL"] = opus_arn
                if sonnet_arn:
                    settings["env"]["ANTHROPIC_DEFAULT_SONNET_MODEL"] = sonnet_arn
                if haiku_arn:
                    settings["env"]["ANTHROPIC_SMALL_FAST_MODEL"] = haiku_arn
                    settings["env"]["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = haiku_arn

                # Override ANTHROPIC_MODEL with the primary inference profile ARN
                # so Claude Code uses the inference profile for all code paths.
                # Only override if the matching tier has an ARN configured —
                # otherwise ANTHROPIC_MODEL stays on the CRIS model ID.
                model_id = profile.selected_model
                if "opus" in model_id and opus_arn:
                    settings["env"]["ANTHROPIC_MODEL"] = opus_arn
                elif "sonnet" in model_id and sonnet_arn:
                    settings["env"]["ANTHROPIC_MODEL"] = sonnet_arn
                elif "haiku" in model_id and haiku_arn:
                    settings["env"]["ANTHROPIC_MODEL"] = haiku_arn

            # Configure telemetry only when monitoring is enabled
            if profile.monitoring_enabled:
                # Default to the local sidecar collector for reliable token counting
                otel_endpoint = "http://localhost:4318"
                settings["env"].update(
                    {
                        "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
                        "OTEL_METRICS_EXPORTER": "otlp",
                        "OTEL_LOGS_EXPORTER": "otlp",
                        "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
                        "OTEL_EXPORTER_OTLP_ENDPOINT": otel_endpoint,
                        "OTEL_RESOURCE_ATTRIBUTES": "department=engineering,team.id=default,cost_center=default,organization=default",
                    }
                )
                settings["otelHeadersHelper"] = "__OTEL_HELPER_PATH__"

                # Try to get a custom endpoint from the monitoring stack (overrides the sidecar default)
                # Get monitoring stack outputs
                monitoring_stack = profile.stack_names.get("monitoring", f"{profile.identity_pool_name}-otel-collector")
                cmd = [
                    "aws",
                    "cloudformation",
                    "describe-stacks",
                    "--stack-name",
                    monitoring_stack,
                    "--region",
                    profile.aws_region,
                    "--query",
                    "Stacks[0].Outputs",
                    "--output",
                    "json",
                ]

                result = subprocess.run(cmd, capture_output=True, text=True)
                if result.returncode == 0:
                    outputs = json.loads(result.stdout)
                    endpoint = None

                    for output in outputs:
                        if output["OutputKey"] == "CollectorEndpoint":
                            endpoint = output["OutputValue"]
                            break

                    if endpoint:
                        # Add monitoring configuration
                        settings["env"].update(
                            {
                                "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
                                "OTEL_METRICS_EXPORTER": "otlp",
                                "OTEL_LOGS_EXPORTER": "otlp",
                                "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
                                "OTEL_EXPORTER_OTLP_ENDPOINT": endpoint,
                                # Add basic OTEL resource attributes for multi-team support
                                "OTEL_RESOURCE_ATTRIBUTES": "department=engineering,team.id=default, \
                                cost_center=default,organization=default",
                            }
                        )

                        # Add the helper executable for generating OTEL headers with user attributes
                        # Use a placeholder that will be replaced by the installer script based on platform
                        settings["otelHeadersHelper"] = "__OTEL_HELPER_PATH__"

                        is_https = endpoint.startswith("https://")
                        console.print(f"[dim]Added monitoring with {'HTTPS' if is_https else 'HTTP'} endpoint[/dim]")
                        if not is_https:
                            console.print(
                                "[dim]WARNING: Using HTTP endpoint - consider enabling HTTPS for production[/dim]"
                            )
                    else:
                        console.print("[yellow]Warning: No monitoring endpoint found in stack outputs[/yellow]")
                else:
                    console.print("[yellow]Warning: Could not fetch monitoring stack outputs[/yellow]")

            # Save settings.json
            settings_path = claude_dir / "settings.json"
            with open(settings_path, "w", encoding="utf-8") as f:
                json.dump(settings, f, indent=2)

            console.print("[dim]Created Claude Code settings for Bedrock configuration[/dim]")

        except Exception as e:
            console.print(f"[yellow]Warning: Could not create Claude Code settings: {e}[/yellow]")

    def _generate_cowork_3p_mdm_config(
        self,
        output_dir: Path,
        profile,
        profile_name: str = "ClaudeCode",
    ) -> None:
        """Generate Claude Cowork 3P MDM configuration files.

        Delegates to shared utilities in cli/utils/cowork_3p.py to ensure
        consistency with the standalone 'ccwb cowork generate' command.
        """
        from claude_code_with_bedrock.cli.utils.cowork_3p import (
            add_monitoring_config,
            build_mdm_config,
            derive_model_aliases,
            generate_all,
        )

        console = Console()

        try:
            bedrock_region = self._get_bedrock_region_for_profile(profile)
            model_aliases = derive_model_aliases()

            mdm_config = build_mdm_config(
                bedrock_region=bedrock_region,
                model_aliases=model_aliases,
                profile_name=profile_name,
            )

            add_monitoring_config(mdm_config, profile, console)
            generate_all(output_dir, mdm_config, console)

        except Exception as e:
            console.print(f"[yellow]Warning: Could not generate CoWork 3P config: {e}[/yellow]")
