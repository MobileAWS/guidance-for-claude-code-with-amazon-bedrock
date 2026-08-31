# Nexus MCP Authentication Scheme (Unified)

**Status:** Canonical. All MCPs — existing and future — MUST follow this scheme.
**Audience:** Nexus Factory (when adding/modifying MCPs) and human maintainers.

## Why this exists

MCPs were added ad-hoc with five different auth mechanisms. The result: Slack and
GitHub work, but Google (×4) and Atlassian are broken because they relied on OAuth
flows that never persist a per-user token. This document defines ONE mechanism so
every MCP behaves identically: authenticate once, persist across sessions, per-user.

## The single rule

**Every MCP that needs credentials gets them the same way: a per-user token stored
in Nexus (`IntegrationTokens` DynamoDB), fetched by the credential-process at launch,
and injected as an environment variable into the MCP's config.** This is the
"token-injection" pattern already proven by HubSpot, ActiveCampaign, and Nexus Factory.

No MCP may rely on:
- ❌ A file the user must place on disk (e.g. `gcp-oauth.keys.json`)
- ❌ A remote/browser OAuth handshake that stores tokens outside Nexus (e.g. `mcp.atlassian.com/.../authv2`)
- ❌ `mcp-remote` MCPs that auto-launch a browser (already banned)
- ❌ A single shared token hardcoded in the MCP catalog (no per-user attribution)

## Auth classes (pick exactly one per MCP)

Every MCP declares an `auth` class. There are only three:

### 1. `none`
No credentials needed. The MCP runs as-is.
- Examples: Web Search (AgentCore gateway, NONE auth), Partner Central.
- Catalog entry: no `env` secrets, no integration registration.

### 2. `token`
The MCP reads a secret from an environment variable. Nexus owns the token per-user.
- **This is the default and preferred class for anything requiring auth.**
- The secret is stored per-user in `IntegrationTokens` (`pk = <service>#<email>`).
- The credential-process fetches it from `/api/integrations/<service>/token` and injects
  it into the MCP's `env` block in `settings.json` and `.claude.json`.
- Two sub-flavors, both end in the same place (a token in `IntegrationTokens`):
  - `token.apikey` — user pastes an API key / PAT on the `/me` page (e.g. ActiveCampaign, Jira PAT, Google service/OAuth refresh token exchanged server-side).
  - `token.oauth` — user clicks "Connect" on `/me`, Nexus runs a **server-side** PKCE
    OAuth flow, stores the resulting refresh_token, and hands the MCP a fresh access_token
    (e.g. HubSpot). The browser flow happens on nexus.allcode.com, NOT inside the MCP.

### 3. `managed`
A credential Nexus already has from the AWS session (rare). Injected as a header/env by
the credential-process without user interaction (e.g. AgentCore gateways needing a
Bedrock auth header). Only use when the credential is derivable from the user's existing
AWS identity.

> If you are unsure, use `token`. Never invent a fourth mechanism.

## Contract each auth class must satisfy

To add an MCP with `auth: token`, three things must exist and match by `service` name:

1. **Catalog entry** (`NexusMcpCatalog` / `org-<org>-mcps.json`):
   ```json
   "<mcp-key>": {
     "command": "npx",
     "args": ["-y", "<package-that-reads-token-from-env>"],
     "env": { "<ENV_VAR>": "" },        // empty — filled per-user at runtime
     "auth": { "class": "token", "service": "<service>", "envVar": "<ENV_VAR>" }
   }
   ```
   - The chosen npm package MUST accept its credential from `<ENV_VAR>`. If the only
     available package needs a file or its own OAuth, it is NOT eligible — find a
     token-based package or wrap it.

2. **Lambda integration endpoints** (`nexus-ui/api/index.py`):
   - `GET  /api/integrations/<service>/token`   → returns `{"access_token": "..."}` for the caller (refreshing server-side if needed)
   - For `token.oauth`: `GET /api/integrations/<service>/connect` + `/callback` (PKCE)
   - For `token.apikey`: the `/me` page POSTs the key to `/api/integrations/<service>/key`
   - Tokens persist in `IntegrationTokens` keyed `pk=<service>#<email>`.

3. **credential-process registration** (`source/go/.../main.go`, `integrations` slice):
   ```go
   {
     name:     "<service>",
     mcpKey:   "<mcp-key>",
     envVar:   "<ENV_VAR>",
     tokenURL: ".../api/integrations/<service>/token",
     connectURL: "https://nexus.allcode.com/me",
   }
   ```
   The existing `syncIntegrationTokens` loop then handles fetch + inject + "needs auth"
   prompt automatically. No new Go code per MCP — just a new slice entry.

## Migration of the two broken MCPs

| MCP | Was | Becomes |
|-----|-----|---------|
| Atlassian / Jira | `__http__` remote OAuth (authv2) — token never persists | `auth: token.apikey`, package `mcp-atlassian` reading `ATLASSIAN_API_TOKEN` + `ATLASSIAN_URL`; user pastes an Atlassian API token on `/me` |
| Google Drive/Docs/Slides/Workspace | client id/secret in env; package needs `gcp-oauth.keys.json` file | `auth: token.oauth`, server-side Google PKCE flow on `/me`; inject `GOOGLE_OAUTH_ACCESS_TOKEN` (refresh handled by Lambda) into a token-reading Google MCP package |

## Rules for the Factory (enforce on every MCP add/change)

1. Determine the auth class. If credentials are required and it is not derivable from
   the AWS session, it is `token`. Default to `token`.
2. Choose an npm/MCP package that reads its credential from an **environment variable**.
   Reject packages that require an on-disk key file or their own browser OAuth.
3. Add the catalog entry with an `auth` block (`class`, `service`, `envVar`) and an
   empty `env` placeholder.
4. If the service isn't already wired, add the Lambda `/api/integrations/<service>/token`
   endpoint (+ connect/callback for `token.oauth`, or `/key` for `token.apikey`) and the
   `/me` UI affordance.
5. Add the one-line entry to the Go `integrations` slice.
6. Never hardcode a shared secret in the catalog. Never regenerate mobileconfigs on MCP
   change (they are static).
7. Test via the dev-first workflow (`deploy-dev.sh` → verify → `promote-to-prod.sh`).

## Acceptance criteria for "an MCP is correctly integrated"

- Authenticates once (API key paste or single Connect click on `/me`), then works across
  every subsequent Claude Code / Cowork session with no re-auth.
- Per-user: two users have independent tokens; revoking one doesn't affect the other.
- The credential lives only in `IntegrationTokens`, never on disk as a key file, never as
  a shared catalog secret.
- Adding the MCP required only: (a) a catalog entry, (b) a Lambda integration endpoint if
  new, (c) one Go slice entry. No bespoke per-MCP logic.
