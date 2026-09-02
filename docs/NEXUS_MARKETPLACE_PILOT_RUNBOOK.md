# Nexus In-Account Marketplace — Pilot Runbook

> Goal: prove the FULL customer flow (subscribe → deploy → use → meter) through the existing
> SaaS listing before flipping the live fulfillment URL. Run this with ONE friendly/internal
> customer (or a second AWS account you control) — NOT a random Marketplace buyer.

## Listing facts (confirmed)
- Product: **AllCode Nexus: Enterprise Controls for Claude Code with Bedrock**
- Entity ID: `prod-ha5jbnrl55zm4` (SaaS, "Deployed on AWS: Yes")
- Pricing: single usage dimension **`bedrock_usage_cents`**, $0.01/unit → Quantity = cost_cents
- Fulfillment: post-subscribe → the registration Lambda's Launch Stack page (in-account deploy)

## Pre-pilot checklist (AllCode side)
- [ ] Get the real **SaaS ProductCode** from Marketplace Management Portal (NOT the prod- entity id).
- [ ] Deploy the **metering-receiver** Lambda in AllCode's account with:
      `MARKETPLACE_PRODUCT_CODE=<real product code>`, `MARKETPLACE_DIMENSION=bedrock_usage_cents`,
      behind an API Gateway at a stable URL (e.g. `api.nexus.allcode.com/metering/report`).
- [ ] Confirm per-region artifact buckets exist for the pilot region
      (`setup-nexus-artifact-buckets.sh <region>` + publish `--all-regions`).
- [ ] Do NOT change the live listing's Fulfillment URL yet (pilot uses the Launch URL directly).

## Pilot steps (customer side)
1. **Subscribe** on the Marketplace listing (or use a private offer to the pilot account).
   - Confirms the SaaS entitlement + gives the `CustomerIdentifier`.
2. **Launch the stack** — hand them the Launch URL directly (bypassing the fulfillment redirect
   for the pilot):
   `https://<region>.console.aws.amazon.com/cloudformation/home?region=<region>#/stacks/quickcreate?templateURL=https://nexus-public-artifacts-<region>.s3.amazonaws.com/nexus-inaccount/templates/nexus-inaccount-bootstrap.yaml&stackName=allcode-nexus&param_OrganizationName=<org>&param_AdminEmail=<email>&param_MarketplaceCustomerId=<customer-id>`
   - They fill OrgName + AdminEmail, click Create. ~5 min → working Nexus.
3. **Log in** to their Nexus dashboard (CloudFront URL from stack Output `NexusUrl`), as admin.
4. **Download the org installer** from their Nexus, install credential-process, sign in.
5. **Make a real Claude call** on Bedrock through the installed tooling (proves the full runtime).
6. **Wait for phone-home** (hourly) OR invoke the phone-home Lambda manually — confirm usage
   counters POST to the AllCode metering-receiver.
7. **Confirm metering** — receiver records the report + calls BatchMeterUsage against the real
   ProductCode. Check the Marketplace metering shows the usage for that CustomerIdentifier.

## Success criteria
- [ ] Stack deploys clean in the pilot region (no cross-account errors — the whole point)
- [ ] Dashboard loads, admin login works, org installer works
- [ ] A real Bedrock inference succeeds end-to-end
- [ ] Phone-home usage lands at the receiver; BatchMeterUsage succeeds (no double-billing)
- [ ] Self-update: publish a canary bump, confirm the pilot's updater applies it (signature-verified)

## After a clean pilot
- [ ] Set the live listing **Fulfillment URL** → the registration Launch page (flips the real flow)
- [ ] Deploy the registration Lambda's Launch page to prod (currently held back)
- [ ] Announce / migrate existing multi-tenant customers to the in-account model over time

## Rollback / safety
- The pilot does NOT touch the live fulfillment URL, so real buyers are unaffected until step
  "After a clean pilot".
- If metering mis-bills in the pilot, the receiver is idempotent (per org+period) and the
  ProductCode can be corrected before any real customer is on it.
