# Credential Helper Session-Name Contract (Go)

The credential-process is now a **single Go implementation**. The legacy
Python variant (`source/credential_provider/`) was removed in the Go-only
migration (Phase 2b). There is no longer a second variant to keep in parity
with — but the session-name **output contract** below is an external contract
that must stay stable.

## Critical contract

`buildSessionName()` (Go, `source/go/internal/federation/sts.go`) must keep:

- Claim priority: `email` → `sub` → `"claude-code"`
- Sanitization regex: `[^\w+=,.@-]` → `"-"`
- Length limits: email = 64 chars, sub = 32 chars
- Fallback format: `"claude-code-{sub_sanitized}"`

## Why

The STS RoleSessionName appears in CUR 2.0 `line_item_iam_principal`. The
value is a **stable external contract**: if a change alters how a given user's
JWT maps to a session name, that user's historical cost attribution splits
across two identities (e.g. `alice@acme.com` vs `claude-code-alice`). Treat
`buildSessionName` output as append-only/stable, not free to refactor.

## Testing

Any change to `buildSessionName` (or the claims it reads) requires a Go
regression test:
- Feed representative JWT claims → assert the exact session name
- Edge cases: no email, no sub, pipe-delimited sub (`auth0|12345`), >64 char email
- Verify sanitization output is unchanged for the documented inputs

> Note: `config.json` is still written by the Python `ccwb` CLI and read by the
> Go binary, so the **config** struct parity rule ([[config-sync]]) still
> applies — that is a different contract from this one.

*Issues: #204 (session name truncation), #58 (recursion)*
