# Google Workspace — Admin Setup Runbook (Slack-style, zero employee effort)

This runbook makes **Google Workspace work exactly like Slack in Nexus**: the admin does a
one-time setup, and then **every employee is auto-connected to their own Google Drive, Docs,
Sheets, Slides, and Calendar** with **zero clicks** — no browser, no `/me` page, nothing.

## Why one-time admin setup is required (and unavoidable)

Slack, HubSpot, and Jira let a single shared token act for the whole org, so the admin pastes
one token and everyone is connected. Google is different: it will only let **one credential
act as all your users** through a mechanism called **domain-wide delegation (DWD)**, and DWD
**must be explicitly turned on by a Google Workspace Super Admin**. That toggle is the price of
zero-click Google. Every product that offers zero-click Google (including Slack's own Google
features) requires this same step. There is no way around it — but it takes ~15 minutes, once.

Once done, employees never authenticate to Google at all. The Nexus credential helper uses the
service account to impersonate each employee and connect them to *their own* Google data.

---

## What you need

- **Google Workspace Super Admin** access (admin.google.com)
- Access to **Google Cloud Console** (console.cloud.google.com) — any project works
- ~15 minutes

---

## Part A — Create the service account + JSON key (Google Cloud Console)

1. Go to **console.cloud.google.com** → select (or create) a project.
2. **Enable the APIs** the integration uses. Go to **APIs & Services → Library** and enable:
   - Google Drive API
   - Google Docs API
   - Google Sheets API
   - Google Slides API
   - Google Calendar API
   (Search each by name → **Enable**.)
3. Go to **APIs & Services → Credentials → Create credentials → Service account**.
   - Name it e.g. `nexus-workspace` → **Create and continue** → skip optional roles → **Done**.
4. Open the new service account → **Keys** tab → **Add key → Create new key → JSON** → **Create**.
   - A `.json` file downloads. **This is the file you paste into Nexus. Keep it secret.**
5. On the service account's **Details** tab, copy its **Unique ID (Client ID)** — a long number.
   You need this in Part B. (It's also inside the JSON as `client_id`.)

---

## Part B — Enable domain-wide delegation + authorize scopes (Google Admin console)

1. Go to **admin.google.com** (must be a Super Admin).
2. Navigate to **Security → Access and data control → API controls → Domain-wide delegation**
   → **Manage Domain-Wide Delegation** → **Add new**.
3. In **Client ID**, paste the service account's **Unique ID / Client ID** from Part A step 5.
4. In **OAuth scopes** (comma-separated), paste **exactly** these:

   ```
   https://www.googleapis.com/auth/drive,
   https://www.googleapis.com/auth/documents,
   https://www.googleapis.com/auth/spreadsheets,
   https://www.googleapis.com/auth/presentations,
   https://www.googleapis.com/auth/calendar
   ```

   > For **read-only** access instead, use the `.readonly` variants (e.g.
   > `https://www.googleapis.com/auth/drive.readonly`). Gmail is handled separately and is
   > always read-only in Nexus.

5. Click **Authorize**.

That's it on the Google side. Domain-wide delegation is now enabled for that service account
across your whole workspace domain.

---

## Part C — Paste the service account into Nexus (the "set once" step)

1. Sign in to Nexus as an **admin** → go to the **MCP servers** page (Extensions).
2. Find **Google Workspace** → click the **⚙️ gear** (Configure).
3. In the **"Connect Google Workspace for the whole org"** panel:
   - **Service account JSON key**: paste the entire contents of the `.json` file from Part A.
   - **Workspace domain**: your domain, e.g. `allcode.com`.
4. Click **Connect for whole org**.
   - Nexus validates the JSON, confirms it's a service account, and stores it org-shared.
   - You'll see: *"Org service account connected — every employee auto-connects to their own
     Google Drive."*

---

## What happens for employees (nothing)

- No browser prompt, no "Connect Google", no `/me` visit.
- The next time their Nexus credential helper runs, it fetches the org service account,
  impersonates the employee (via `USER_GOOGLE_EMAIL` = their own address, using domain-wide
  delegation), and connects them to **their own** Drive/Docs/Sheets/Slides/Calendar.
- In Claude, they can immediately ask things like *"search my Google Drive"* and it just works.

---

## Verification (admin)

After Part C, confirm the status endpoint reports it configured:

```bash
curl -s "$NEXUS_API/api/admin/org-integration/status" \
  -H "Authorization: Bearer $ID_TOKEN" | jq '.integrations["google-sa"]'
# expect: { "configured": true, "sa_client_email": "nexus-workspace@...", "domain": "allcode.com", ... }
```

Then have one employee restart Claude and try a Google Drive command. It should work with no
authentication prompt.

---

## Security notes

- The service account JSON is a **secret credential**. Store it only in Nexus (and your secure
  vault). Do not commit it to source control or paste it in chat/tickets.
- Domain-wide delegation grants the service account broad access to impersonate users for the
  authorized scopes. Grant only the scopes you need. Use `.readonly` scopes if you want to
  restrict Nexus to read-only Google access.
- To revoke: remove the client ID from Google Admin → Domain-wide delegation, and delete the
  key from the service account in Cloud Console. In Nexus, re-open the Google Workspace gear
  and clear/replace the service account.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Employee sees `client_secret.json not found` / OAuth window | Service account not configured (or employee on old binary) | Complete Part C; ensure employee's Nexus binary is current |
| `unauthorized_client` from Google | Scopes not authorized, or wrong Client ID in DWD | Re-check Part B step 3–4; the Client ID must be the SA's **Unique ID** |
| `access_denied` for a specific user | That user isn't in the delegated domain, or API not enabled | Confirm the user's email domain matches, and Part A step 2 APIs are enabled |
| Works for admin but not others | Impersonation email wrong | Nexus sets `USER_GOOGLE_EMAIL` to each caller automatically; ensure the user's Nexus login email matches their Google email |
