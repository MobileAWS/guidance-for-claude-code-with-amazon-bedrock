# Nexus In-Account Self-Update — Phase 2 Spec

> Builds on Phase 1 (`NEXUS_INACCOUNT_PHASE1_SPEC.md`). Goal: the in-account Nexus
> stack keeps itself current WITHOUT AllCode having any access to the customer account.

## The core risk (read this first)

A self-update mechanism means: **one bad update can break every customer's Nexus
simultaneously, and we cannot log into their accounts to fix it.** This is the single
highest-stakes system in the product. Every design decision below exists to contain that
blast radius. Safety > speed, always.

## What "the stack" updates

The in-account Nexus has three updatable layers:
1. **Lambda code** (api, device-auth, package-gen, post-confirm) — versioned S3 artifacts.
2. **UI** (the React SPA in the customer's S3 bucket).
3. **Infrastructure** (the CloudFormation template itself — new resources, IAM, etc.).

Layers 1 & 2 are LOW risk (swap code/files). Layer 3 is HIGH risk (CFN changes can fail
mid-update and wedge a stack). Phase 2 handles 1 & 2 first; template updates (3) are
manual/customer-approved for now.

## Design — pull-based, customer-account-driven

AllCode NEVER pushes into customer accounts. Instead, a small **updater Lambda inside the
customer's stack** (EventBridge, e.g. daily) PULLS from AllCode's public, signed artifact
channel and applies low-risk updates locally. This keeps the "no AllCode access" guarantee.

```
AllCode publishes ->  s3://<allcode-public-artifacts>/nexus-inaccount/channel/<CHANNEL>.json
                      (version + per-artifact S3 keys + sha256, all signed)
                                    |  (customer's updater Lambda pulls, daily)
Customer account:     Updater Lambda -> verify sha256 -> update its own Lambda code + UI
```

## Safety mechanisms (all mandatory)

### S1. Staged rollout channels
AllCode publishes to CHANNELS, not one global version:
- `canary`  — AllCode's own test account only
- `early`   — opt-in early adopters (a few friendly customers)
- `stable`  — everyone else (the default)
Each customer stack is pinned to a channel (parameter, default `stable`). A new release goes
canary -> early -> stable over days. A bad release is caught in canary/early, never reaching
the `stable` fleet.

### S2. Signed + hash-verified artifacts
Every artifact has a sha256 in the channel manifest. The updater verifies the hash before
applying (same pattern the credential-process binary already uses). Reject on mismatch.
(Future: sign the manifest itself with KMS/asymmetric key; verify signature in-updater.)

### S3. Version gating + no-downgrade
Updater only applies versions STRICTLY NEWER than current. Never downgrades. Records the
applied version locally (DynamoDB) so it's idempotent and auditable.

### S4. Health check + auto-rollback (Lambda layer)
After updating a Lambda's code, the updater invokes a health endpoint. If it fails, it
rolls back to the previous version (Lambda keeps prior versions; publish an alias, flip it,
verify, keep old version for rollback). No healthy check = no promote.

### S5. Customer control
- `UpdateChannel` parameter (canary/early/stable).
- `AutoUpdateEnabled` parameter (default true; enterprises can set false and update manually).
- All updates logged to a local `NexusUpdateLog` table (audit: what changed, when, from/to).

### S6. Kill switch
AllCode can set a channel manifest flag `paused: true` to halt all pulls fleet-wide (e.g. if
a bad release slips through). Customers' updaters see `paused` and skip. This is the ONLY
fleet-wide lever AllCode has — and it can only STOP updates, never force them.

## Explicitly deferred (NOT in Phase 2)

- **Automatic CloudFormation template self-update** (infra layer 3) — too risky to auto-apply
  a CFN change that could wedge the stack with no access to recover. Template updates are
  customer-initiated (they re-run the updated template) until we have a proven safe pattern.
- Asymmetric manifest signing (start with sha256; add KMS signing before GA).

## Build sequence

1. **P2.1** — Channel manifest format + an AllCode `publish` script (writes canary/early/stable
   manifests with versions + sha256 to the public artifact bucket).
2. **P2.2** — Updater Lambda (in the customer stack) + EventBridge schedule + NexusUpdateLog
   table + UpdateChannel/AutoUpdateEnabled params. Pulls manifest, verifies, updates Lambda
   code + UI. Version-gated, idempotent.
3. **P2.3** — Health check + rollback (S4) via Lambda versions/aliases.
4. **P2.4** — Kill switch (S6) + staged-rollout publish workflow (canary->early->stable).
5. **P2.5** — Validate in dev: publish a v2 to canary, confirm a canary-pinned stack updates
   and a stable-pinned stack does NOT; simulate a bad update and confirm rollback + kill switch.

## Risks / human-owned

- The publish process (deciding what goes canary->stable) stays human-controlled.
- CFN template changes stay manual until proven safe.
- Every release smoke-tested in canary (AllCode account) before `early`.

---
*Phase 2 spec. Pull-based, staged, hash-verified, health-checked, with a kill switch. AllCode
never gains account access. Build dev-first; validate the staged/rollback/kill-switch paths
before any real customer is on auto-update.*
