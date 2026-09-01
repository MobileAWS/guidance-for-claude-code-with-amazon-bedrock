# Nexus GTM: Bootstrap-Into-Customer-Account Architecture

> Status: **Strategic direction** (from GTM call, Aug 2026). Not yet implemented.
> This document captures the architectural pivot and how existing Nexus building
> blocks map to it. It is a planning artifact, not a spec.

## The pivot in one sentence

Stop running a **multi-tenant control plane** in AllCode's account. Instead, let each
customer **bootstrap the Factory into their own AWS account**, where it self-updates and
phones home lightweight usage — so we never host their data or carry their blast radius.

## Why (enterprise GTM rationale)

Enterprises strongly prefer software that runs **in their own AWS account**:

- **Data residency / security review** — "your governance data never leaves your account"
  clears enterprise security review dramatically faster than "trust our multi-tenant SaaS."
- **No shared blast radius** — a bug or breach in one tenant cannot touch another; there is
  no shared production plane to compromise.
- **Scales on the customer's infra**, not ours — our cost/scaling burden drops sharply.
- **AWS Marketplace-native** — the customer-deployed CloudFormation model is the blessed
  ISV distribution path and enables Marketplace metering/billing.

Trade-off we accept: onboarding requires the customer to run a stack in their AWS account
(vs. "just log into our SaaS"), and support/debugging shifts to phone-home telemetry rather
than direct access.

## Old model vs. new model

| | Multi-tenant control plane (old) | Bootstrap-into-customer-account (new) |
|---|---|---|
| Who hosts | AllCode hosts every tenant | Customer hosts their own Factory |
| Data location | AllCode account | Customer account (never leaves) |
| Isolation | Logical, per-tenant, our responsibility | Physical — separate accounts |
| Updates | We push to shared plane | Each Factory **self-updates** |
| Our ops burden | High (maintain control plane) | Low (ship artifacts + a self-updater) |
| Blast radius | Shared across tenants | Contained per customer |
| Enterprise trust | "our data in your system?" | "runs in OUR account" ✅ |
| Marketplace | Custom SaaS metering | Native CFN + Marketplace metering |

## The AWS pattern (this is a blessed, documented path)

This is the **single-tenant / customer-deployed ISV model**. Key references:

- **Building a cross-account CI/CD pipeline for single-tenant SaaS** —
  https://aws.amazon.com/blogs/devops/cross-account-ci-cd-pipeline-single-tenant-saas/
  ("single-tenant installations consist of dedicated production environments for each
  customer, without any shared resources across tenants")
- **CloudFormation Templates 101 for Sellers in AWS Marketplace** —
  https://aws.amazon.com/blogs/awsmarketplace/cloudformation-templates-101-for-sellers-in-aws-marketplace/
  (how the customer deploys our stack into their own account)
- **AWS Release Management System (RMS)** —
  https://aws.amazon.com/marketplace/pp/prodview-e4757fk3vn4uo
  ("install, update, and manage your software across hundreds of customer AWS accounts
  without manual effort" — reference model for the self-update system)
- **CloudFormation StackSets** (self-managed permissions) — for fleet-wide updates if we
  ever coordinate across a customer's own multi-account org.

## How Nexus's existing pieces map to this (we already built the primitives)

The pivot is an **inversion of the hosting model**, not a rewrite. We already have:

| GTM requirement | Existing Nexus building block |
|---|---|
| "Bootstrap the Factory into a customer account" | `deployment/infrastructure/` CloudFormation + `ccwb deploy` — already deploys the stack. Package it as a one-click customer-run template. |
| "Dumber self-update system" | The credential-process **already self-updates** (`checkForUpdate()` pulls a signed binary + `version.json` from S3). Extend this pattern from the binary to the whole Factory stack. |
| "Factory reports tenancy occupation" | We already phone-home usage (OTEL → CloudWatch, `reportPlatform`, quota metrics). Point this at a lightweight metering endpoint for billing, NOT a control plane. |
| "Don't do the multi-tenant control plane" | Retire the shared per-tenant Lambda/DynamoDB. Each customer gets their own isolated stack. |
| "Modernization GPS — explore everything in your AWS tenant" | New: a **read-only** discovery agent that scans the customer's AWS account to guide modernization (see below). |
| Config-driven per environment | `config.json` + dev/prod separation already prove per-deployment configuration works. |

## Components of the new model

1. **Bootstrap template** — a customer-deployable CloudFormation stack (direct or via AWS
   Marketplace) that stands up the Factory in the customer's account. Parameterized by their
   IdP, region, model choice (we already collect these in `ccwb init`).

2. **Self-update system** ("dumber" than a control plane):
   - Each Factory checks a signed artifact channel (S3 + `version.json`, as the binary does today).
   - **Staged rollout discipline** is mandatory — a bad self-update hits all customers at once.
     Reuse the dev/prod separation we built: canary → staged → GA.
   - Optionally adopt AWS RMS for fleet update orchestration.

3. **Phone-home metering** (not a control plane):
   - The Factory reports usage/occupancy (seats, tokens, active users) to a lightweight
     metering endpoint for billing + Marketplace metering.
   - Minimal, privacy-respecting — no customer data, just counters.

4. **Modernization GPS** (AWS environment discovery):
   - A **read-only, least-privilege** agent that inventories the customer's AWS account
     (workloads, languages, dependencies, modernization candidates).
   - IAM boundary must be read-only + explicit consent, or it becomes a security-review blocker.
   - Output feeds modernization recommendations / Factory task generation.

## Risks & discipline required

- **Update safety**: a bad self-update affects every customer. Non-negotiable: canary +
  staged rollout, signed artifacts, easy rollback. (Our dev/prod split is the seed of this.)
- **Support without access**: we can't log into the customer's account. Phone-home telemetry
  and good diagnostics ("Factory reports occupation") are how we support remotely.
- **Onboarding friction**: customer must run a stack. Mitigate with a genuinely one-click
  CFN/Marketplace launch and strong defaults.
- **Modernization GPS IAM**: read-only, least-privilege, consented — or it fails security review.

## Suggested next steps (not yet started)

1. Prototype the **bootstrap CloudFormation template** a customer runs in a fresh account.
2. Design the **self-update channel** for the full stack (extend the binary's model), with
   staged-rollout safety built in.
3. Scope **Modernization GPS**: what it scans, its IAM boundary, and its output format.
4. Evaluate **AWS Marketplace** listing (CFN-based) for distribution + metering.

---
*Captured from GTM call, Aug 2026. Owner: AllCode. This supersedes the multi-tenant control
plane direction for enterprise go-to-market.*
