# Google Drive via Org Service Account — Design

> Goal (from the call + your direction): admin configures Google ONCE; every employee is
> auto-connected to Google Drive with ZERO auth and ZERO /me visit. Employees do nothing.

## How it works (domain-wide delegation)

1. **Admin, once:** in Google Workspace Admin, creates a **service account** with
   **domain-wide delegation** (DWD) for the needed scopes (Drive, Docs, etc.), then pastes the
   service-account JSON key into Nexus (admin Integrations page).
2. **Nexus stores it** org-level: `IntegrationTokens` pk=ORG#<org>, sk=`google-sa`
   (value = the SA JSON). One org credential, like the HubSpot/Jira org keys.
3. **credential-process (per employee):** injects into the google-drive MCP env:
   - `GOOGLE_SERVICE_ACCOUNT_KEY_JSON` = the org SA JSON (fetched from Nexus)
   - `USER_GOOGLE_EMAIL` = the employee's OWN email (from their authenticated identity)
   - `DWD_ALLOWED_DOMAINS` = the org's domain (safety allowlist)
   Drops `--single-user` token reliance — the SA impersonates the user.
4. **Result:** workspace-mcp uses the SA to impersonate `USER_GOOGLE_EMAIL` → the employee
   accesses THEIR OWN Drive. No OAuth, no consent, no /me. Auto-connected.

## Why this is the right model
- **Employees do nothing** — matches "easy for employees."
- **Per-user data** — impersonation means each employee sees their own Drive (not a shared one).
- **Admin-controlled** — one SA, scoped by the admin, revocable centrally.
- **No local HTTP server / no per-user OAuth** — simpler + more robust than on-demand auth.

## What to build
1. **Admin UI** (Integrations page "Team Connections"): "Google Workspace" card — paste SA JSON
   + org domain → saves org-level.
2. **Backend**: extend `handle_org_integration` to accept `integration=google-sa`
   (store SA JSON + domain). Add a token endpoint that serves the SA to employees.
3. **Go credential-process**: for the google integration, if an org SA exists, inject
   `GOOGLE_SERVICE_ACCOUNT_KEY_JSON` + `USER_GOOGLE_EMAIL` (caller's email) + `DWD_ALLOWED_DOMAINS`
   into the google-drive MCP env, instead of the per-user OAuth token.

## Admin one-time setup (documented for the admin)
- Google Cloud: create service account + JSON key; enable Drive/Docs APIs.
- Google Workspace Admin → Security → API Controls → Domain-wide Delegation: authorize the SA's
  client ID for the scopes (e.g. https://www.googleapis.com/auth/drive).
- Paste the SA JSON + your domain into Nexus. Done — all employees connected.

## Env vars (confirmed from workspace-mcp v1.25.2 docs)
- GOOGLE_SERVICE_ACCOUNT_KEY_JSON (inline SA key) or GOOGLE_SERVICE_ACCOUNT_KEY_FILE
- DWD_ALLOWED_DOMAINS (domain allowlist)
- USER_GOOGLE_EMAIL (impersonation target = the employee)

## Note
Gmail stays separate/optional (personal inbox via per-user OAuth if ever needed) — the org SA
is for Drive/Docs/Sheets/Slides (org Workspace resources).
