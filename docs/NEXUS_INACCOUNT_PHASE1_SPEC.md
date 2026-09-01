# Nexus In-Account Deployment — Phase 1 Engineering Spec

> Turns the GTM pivot (`NEXUS_GTM_BOOTSTRAP_ARCHITECTURE.md`) into a concrete build plan.
> Goal of Phase 1: **move the Nexus control plane INTO the customer's AWS account** so a
> customer's deployment is fully self-contained — no cross-account read role, no central
> web app reaching into their data.

## Where we are today (verified from existing code)

`nexus-ui/infra/nexus-customer-onboarding.yaml` ALREADY deploys, into the customer account:
- Cognito User Pool + Identity Pool + Bedrock IAM role
- `ClaudeCodeMetrics` + `QuotaPolicies` DynamoDB tables (customer data stays local ✅)
- `NexusConnectorRole` — cross-account role letting AllCode READ their metrics
- A registration custom resource that phones home to `/api/setup/register`

What is still CENTRAL (in AllCode's account) and must move:
1. The **Nexus web UI + management Lambda** (`nexus.allcode.com`, `api/index.py`) — reaches
   into customer accounts via the connector role.
2. **Metering** (`functions/metering/index.py`) — hourly, assumes the cross-account role and
   calls Marketplace `BatchMeterUsage`.

So Phase 1 is NOT a rewrite — it is relocating two things into the onboarding stack and
removing the cross-account dependency.

## Target end-state (Phase 1)

A customer runs ONE CloudFormation stack (from Marketplace or direct) and gets, entirely in
their own account:
- Auth (Cognito) + Bedrock access  [already there]
- Data (metrics, quotas)           [already there]
- **The Nexus management UI + API** [NEW — moves in]
- **Local metering** to Marketplace [NEW — moves in]
- No `NexusConnectorRole`; AllCode no longer reads their account. [REMOVED]

AllCode's only inbound signal = lightweight phone-home usage counters (for support/visibility),
NOT data access.

## Design decisions (need confirmation before build)

### D1. How does the in-account UI run? — RECOMMEND: Lambda + API Gateway + CloudFront/S3
- `api/index.py` is already a single-file Lambda → deploy it as a Lambda in the customer stack.
- Static React UI → S3 + CloudFront (or API Gateway static). Lowest cost, no servers.
- Alternative (ECS/Fargate) = overkill for this workload. **Recommend Lambda+S3+CF.**

### D2. UI reads data locally (remove connector role)
- In-account, the management Lambda reads the LOCAL `ClaudeCodeMetrics`/`QuotaPolicies`
  directly (same account) — simpler and MORE secure than cross-account assume.
- Delete `NexusConnectorRole` from the onboarding stack. This is the core enterprise-trust win.

### D3. Metering runs in the customer account
- Relocate `functions/metering/index.py` into the customer stack (EventBridge hourly).
- It reads LOCAL metrics (no assume-role) and calls Marketplace `BatchMeterUsage` directly.
- Requires `aws-marketplace:BatchMeterUsage` on the in-account metering role + the
  Marketplace product code as a stack parameter.
- NOTE: `BatchMeterUsage` from the buyer account requires the product to be configured for it;
  confirm the Marketplace product's metering model supports seller-authorized buyer metering,
  OR keep metering seller-side but fed by phone-home counters (fallback — see D4).

### D4. Phone-home (replaces cross-account read)
- The in-account Factory POSTs usage counters (tokens, seats, active users) to an AllCode
  metering endpoint on a schedule. Minimal, no customer data.
- This is the fallback for metering if D3's buyer-side BatchMeterUsage isn't permitted.

### D5. Config — no hardcoded AllCode account
- Today the onboarding template hardcodes `AllCodeAccountId` + the registration URL. In the
  in-account model, the UI/API run locally so most of that goes away. Keep only the phone-home
  endpoint as a parameter (with a sane default).

## Migration story (existing tenants)

- AllCode / LetsPlay / Skematic stay on the CURRENT central model during transition.
- NEW Marketplace customers get the in-account stack.
- Run BOTH models in parallel; migrate existing tenants later (they redeploy the new stack in
  their account, data re-homes locally). No forced cutover.

## Build sequence (Phase 1)

1. **P1.1** — Add the management Lambda (`api/index.py`) as a resource in
   `nexus-customer-onboarding.yaml`, wired to LOCAL DynamoDB (no connector role). Acceptance:
   stack deploys in a fresh account; the API responds; reads local metrics.
2. **P1.2** — Add the static UI (S3 + CloudFront) to the stack, pointing at the in-account API.
   Acceptance: the Nexus UI loads in-account and shows the org's own data.
3. **P1.3** — Remove `NexusConnectorRole` + cross-account assume paths. Acceptance: no
   cross-account role exists; AllCode cannot read the account; UI still works.
4. **P1.4** — Relocate metering into the stack (D3) or wire phone-home counters (D4).
   Acceptance: usage is metered to Marketplace from the customer account (or counters arrive
   at AllCode); billing continues correctly.
5. **P1.5** — End-to-end: deploy the whole stack into a THROWAWAY fresh AWS account, confirm a
   self-contained Nexus with zero AllCode data access. This validates the GTM promise.

## Explicitly deferred (later phases)

- Self-update of the in-account stack (Phase 2) — needs staged-rollout safety design.
- Modernization GPS read-only discovery agent (Phase 4) — separate product.

## Risks / human-owned

- **Marketplace metering path** — money-critical; validate D3 vs D4 carefully before cutover.
- **Self-update blast radius** — deferred to Phase 2 with canary→staged→GA discipline.
- **IAM least-privilege** — the in-account roles must be minimal; security-review gate.

---
*Phase 1 spec. Build directly (not via factory). Dev-first: land on dev, test in a throwaway
account, promote only on approval. Does not touch the live central control plane until P1.5
validates the in-account model.*

## APPENDIX — Full Nexus resource inventory (from code audit)

The Marketplace subscriber gets the COMPLETE Nexus product self-contained in their account.
Verified from `nexus-ui/api/index.py` + deployed Lambdas:

**DynamoDB tables (7):**
- `ClaudeCodeMetrics`, `QuotaPolicies`  [already in onboarding stack]
- `UserQuotaMetrics`, `NexusOrganizations`, `NexusMcpCatalog`, `NexusSkillsCatalog`,
  `IntegrationTokens`  [ADD]

**Lambdas (in-account):**
- `allcode-nexus-ui-api` (main API, ~5,355 lines — S3-referenced package, not inline)  [ADD]
- `nexus-device-auth`, `nexus-package-gen`, `nexus-post-confirmation`  [ADD]

**Stay central in AllCode's account (NOT in customer stack):**
- `nexus-metering`, `nexus-registration` — Marketplace billing/registration run on AllCode's
  seller account. Customer Nexus phones home usage counters to feed metering (D4).

**Frontend + distribution (in-account):**
- React UI -> private S3 + CloudFront (+ ACM cert, optional WAF)
- Installer distribution bucket (serves credential-process binaries to the org's developers)

**API env vars (all derivable from the stack's own resources):**
MCP_TABLE, METRICS_TABLE, ORGS_TABLE, POLICIES_TABLE, QUOTA_TABLE, SKILLS_TABLE,
COGNITO_USER_POOL_ID, DISTRIBUTION_BUCKET, CORS_ORIGIN, SELECTED_MODEL, CROSS_REGION_PROFILE,
BEDROCK_ACCESS_POLICY_ARN, ALERT_TOPIC_ARN, AWS_REGION

## Professional / enterprise requirements (build in from the start)

1. Runs entirely in the customer account — no AllCode data access (remove NexusConnectorRole).
2. HTTPS via CloudFront + ACM; clean domain.
3. Optional AWS WAF on CloudFront (toggle).
4. Least-privilege IAM per Lambda (security-review gate).
5. Lambda code as VERSIONED S3 artifacts (enables clean self-update, Phase 2).
6. Clean stack-level uninstall (delete stack = full teardown).

## PHASE 1 STATUS: COMPLETE & VALIDATED (2026-09-01)

Built `nexus-ui/infra/nexus-inaccount-bootstrap.yaml` — a single CloudFormation
template that deploys a complete standalone Nexus into a customer account.

Validated in the Nexus-dev account (prefix-namespaced, zero-cost):
- One-shot `create-stack` → full Nexus stands up (8 DynamoDB tables + 4 Lambdas +
  Cognito auth + Bedrock role + S3/CloudFront UI) in a single operation.
- Dashboard loads via CloudFront (HTTP 200); /api/* routes through the SAME
  CloudFront domain to the in-account API (HTTP 200, returns real Cognito user).
- Single prebuilt UI works via relative /api paths — no per-account rebuild.
- Namespacing (ResourcePrefix) confirmed collision-free: coexisted with the
  account's existing unprefixed Nexus tables.
- Clean teardown: delete-stack removed 100% of resources (enterprise uninstall).
- NO cross-account role — nothing reaches back to AllCode (the trust win, inherent).

Remaining before Marketplace GA (later phases):
- Phase 2: self-update of the in-account stack (staged rollout safety).
- Phone-home usage counters -> AllCode metering (D4).
- BYO-IdP federation (Okta/Azure) on top of the default Cognito pool.
- Wire the Marketplace Fulfillment/launch flow to this template.
