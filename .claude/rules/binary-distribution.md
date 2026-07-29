# Binary Distribution

## Rule
- The end-user binaries (credential-process, otel-helper) are native **Go** binaries, cross-compiled for all 5 platforms via `ccwb package`. Go is the implementation — not one option among several.
- macOS: Gatekeeper quarantines unsigned binaries
- Windows: Defender/SmartScreen blocks unsigned
- Cold start target: <100ms (the Go binary comfortably meets this)
- Keep the Go binary lean — don't add heavy dependencies that regress startup

## Why
Unsigned binaries trigger security warnings. The Go binary delivers fast startup and clean AV scans; users expect fast startup times for credential processes.

## Platform-Specific Issues
- **macOS:** Unsigned binaries get quarantined by Gatekeeper. Document `xattr -cr` workaround.
- **Windows:** Unsigned executables can trigger Defender/SmartScreen. Document exclusion path. Note: the Go Windows binary must stay unstripped (no `-s -w`) and carry PE version info to avoid Defender ML false positives.

## Historical context (why Go was adopted)
The end-user binaries were previously built with **PyInstaller** (macOS/Linux) and **Nuitka via AWS CodeBuild** (Windows). That pipeline had two recurring problems that motivated the move to Go:
- **Cold start regression:** heavy Python imports regressed cold start from <100ms to ~10s under PyInstaller.
- **AV false positives:** Nuitka-compiled Windows binaries triggered Defender (Error 225).

The Go binary is now the sole runtime — fast cold start, clean AV scans, and cross-compilation for all platforms from a single machine. PyInstaller/Nuitka/CodeBuild are no longer part of the build.

## Solutions
- Use the Go binary for the credential process (fast startup, clean AV)
- Document security exclusion procedures
- Keep the Go binary's dependency surface small to protect startup time
- Target <100ms cold start time

## Related Issues
#27, #145, #223, #237, #395