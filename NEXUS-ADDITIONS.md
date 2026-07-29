# AllCode Nexus — What We Added On Top of the Upstream Repo

## Overview

The upstream repo (`aws-solutions-library-samples/guidance-for-claude-code-with-amazon-bedrock`) provides:
- `ccwb` CLI tool for IT admins to deploy/configure
- CloudFormation templates for IAM, Cognito, monitoring
- Python credential-process + otel-helper binaries
- Install scripts for end users

**Nexus adds a full management portal + multi-tenant SaaS platform on top.**

---

## What Nexus Adds (Not in Upstream)

### 1. Management Portal (nexus-ui repo)
A React/MUI web application at `nexus.allcode.com` with:

| Page | Purpose |
|------|---------|
| **Dashboard** | Org-wide token usage, cost (from AWS Cost Explorer), daily chart, top users |
| **Users** | User management, invite, block/unblock, platform detection, per-user tokens |
| **User Detail** | Individual user's 30-day usage chart |
| **Quotas** | Per-user/per-team quota policies with enforcement |
| **Billing** | Monthly billing with per-user cost breakdown, month selector |
| **Models** | Enable/disable models with instant IAM enforcement |
| **Extensions** | MCP server catalog — admin adds MCPs, auto-pushes to Claude Desktop & Code |
| **Skills** | Reusable prompt/skill catalog per org |
| **Performance** | Per-user sessions, active time, tokens/session, lines of code |
| **Settings** | Platform status |
| **Setup Guide** | Self-service org onboarding (deploy CF stack, connect) |
| **Download** | Platform-specific installers (macOS/Linux/Windows), Cowork configs, profile switching |
| **Resources** | Documentation, whitepapers, case studies (10 HTML docs) |
| **Device Verify** | Device auth flow approval page |
| **Audit Log** | Admin action history |

### 2. Backend Lambda (`nexus-api/index.py`)
A single Lambda (~2000 lines) that handles ALL API endpoints:

- `/api/metrics/summary` — dashboard data (reads from UserQuotaMetrics + Cost Explorer)
- `/api/users` — CRUD, org-filtered, with platform/last-active
- `/api/users/{id}` — detail with 30-day CloudWatch chart
- `/api/users/platform` — credential-process reports OS
- `/api/quotas` — CRUD for quota policies
- `/api/billing/report` — CSV export
- `/api/config/models` — model enable/disable + IAM policy update
- `/api/mcp-servers` — MCP catalog CRUD + auto-regenerate Cowork configs
- `/api/skills` — skills catalog CRUD
- `/api/performance` — per-user CloudWatch metrics
- `/api/metrics/activity` — recent activity feed
- `/api/orgs` — list organizations
- `/api/orgs/provision` — self-service org creation + package generation
- `/api/download` — presigned URL generation for platform packages
- `/api/device/code`, `/api/device/token`, `/api/device/verify` — device auth flow

### 3. Device Auth Flow (`nexus-device-auth/index.py`)
Eliminates port/redirect issues. User gets a code, approves in browser, CLI polls for completion.

### 4. Customer Onboarding Template (`nexus-customer-onboarding.yaml`)
CloudFormation template customers deploy in their account:
- Cognito User Pool + Identity Pool
- IAM Bedrock role (with trust for our Identity Pool)
- Connector role (for cross-account metrics reading)
- Optional CloudTrail for Bedrock
- Cost Explorer permissions

### 5. Multi-Tenant Org System
- Cognito groups (`org-skematic`, `org-lets-play`, etc.)
- All API endpoints filter by org
- Dashboard/Users/Billing scoped to selected org
- Org admin vs user roles (`org-{id}-admins` group)
- Super admin sees all orgs

### 6. Model Enforcement via IAM
When admin disables a model in the portal:
- Lambda updates the IAM policy on the Bedrock role
- Removes that model's ARN from allowed resources
- Takes effect immediately for all users (no reinstall)

### 7. MCP Auto-Sync (Go credential-process modification)
- Admin adds MCP in Extensions page → S3 configs regenerate
- Go credential-process fetches `claude-code-mcps.json` from S3 every 5 min
- Merges into user's `~/.claude/settings.json`
- Claude Code picks up new MCPs without reinstall

### 8. Org Switching (`--org` flag)
- `claude-code --org skematic` — tags telemetry to Skematic
- Credential-process saves active org to session file
- OTel helper reads it and includes in `organization.id` attribute
- Multi-org users prompted on first auth

### 9. Cowork Config Auto-Generation
When admin changes Extensions (MCPs):
- `.mobileconfig` regenerated on S3 (with `managedMcpServers`)
- `cowork-3p-config.json` regenerated
- `claude-code-mcps.json` regenerated
- All publicly accessible for download

### 10. Marketplace / Metering (`nexus-metering/index.py`)
AWS Marketplace integration for billing customers.

### 11. Package Generation (`nexus-package-gen/index.py`)
Async Lambda that builds org-specific installer packages.

### 12. Post-Confirm Hook (`nexus-post-confirm/index.py`)
Cognito trigger after user confirms — sets up initial state.

### 13. Registration (`nexus-registration/index.py`)
Self-registration flow.

### 14. SES Email Integration
Invite emails from `nexus@allcode.com` with DKIM (not Cognito default).

### 15. Go Binary Modifications
On top of upstream Go source:
- `--org` flag for org switching
- `syncMcpServers()` — fetches MCPs from S3 and merges into settings.json
- `readActiveOrg()` / `saveActiveOrg()` — session file for org persistence
- OTel helper reads active-org for telemetry tagging

---

## Modified Upstream Files (Key Changes)

| File | What we changed |
|------|----------------|
| `credential_provider/__main__.py` | Device auth flow, session storage, keyring fallback, 30s timeout, OIDC direct auth, model enforcement, platform reporting |
| `otel_helper/__main__.py` | Session file first (no keyring on Linux), latency fix from beta |
| `config.py` | Added cowork_credential_mode, cowork_3p_extra_keys, helper TTL |
| `cowork_3p.py` | Pulled from beta — inferenceCredentialHelper support |
| `package.py` | Skip stack query when identity_pool_id known, sudo fallback in install.sh |
| CloudFormation templates | Various fixes for our deployment |

---

## Infrastructure (AWS Account 916587687563)

| Resource | Purpose |
|----------|---------|
| Lambda: `allcode-nexus-ui-api` | Main API |
| Lambda: `nexus-device-auth` | Device auth |
| Lambda: `nexus-package-gen` | Package builder |
| Lambda: `claude-code-quota-check` | Quota enforcement |
| Lambda: `claude-code-quota-monitor` | Usage aggregation |
| API Gateway: `dtxfifv2cj` | HTTP API |
| CloudFront: `E2PX4JCVG447YO` | UI distribution |
| S3: `allcode-nexus-ui-nexusuibucket-*` | Frontend hosting |
| S3: `claude-code-auth-distribution-*` | Packages, configs |
| DynamoDB: `ClaudeCodeMetrics` | Telemetry metrics |
| DynamoDB: `UserQuotaMetrics` | Per-user token counts |
| DynamoDB: `QuotaPolicies` | Quota rules |
| DynamoDB: `NexusOrganizations` | Org configs |
| DynamoDB: `NexusMcpCatalog` | MCP servers + skills |
| Cognito: `us-east-1_3mbtSSlmt` | User auth |
| Cognito Identity Pool | AWS credential federation |

---

## What's NOT in Upstream (Pure Nexus)
- The entire `nexus-ui` repo
- All `nexus-*` Lambda functions
- `nexus-customer-onboarding.yaml`
- `nexus-transform.yaml`, `nexus-ui.yaml`, `nexus-marketplace.yaml`
- Device auth flow
- Multi-tenant org system
- Model enforcement via IAM
- MCP catalog + auto-sync
- Skills catalog
- Performance monitoring page
- Cost Explorer billing
- SES email integration
- All the DynamoDB tables above
