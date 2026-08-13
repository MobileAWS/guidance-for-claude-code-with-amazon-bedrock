# AllCode Nexus + Nexus Factory
## Enterprise AI Governance for Amazon Bedrock
### Presentation for AWS Business Clients — Houston, August 2026

---

## Slide 1: The Problem

**AI Sprawl is the New Shadow IT**

- Developers adopting Claude Code, Codex, OpenCode without IT oversight
- No visibility into who's using what, how much they're spending
- No model access controls — anyone can use any model
- No audit trail for compliance
- Billing surprise: one rogue agent session = $500 in tokens

**"Who approved this $47K Bedrock bill?"**

---

## Slide 2: AllCode Nexus — One Platform, Full Control

**The governance platform for AI coding tools on Amazon Bedrock**

Nexus manages Claude Code, Claude Desktop (Cowork), OpenAI Codex, and OpenCode through a single admin portal.

- **SSO Integration** — Okta, Azure AD, IAM Identity Center
- **Per-User Quotas** — Token limits, spend caps, auto-block
- **Model Access Controls** — Choose which models each team can use
- **Real-Time Telemetry** — Per-user, per-model, per-day spend tracking
- **Billing Attribution** — Know exactly who spent what
- **MCP Extensions** — Centrally manage tools (GitHub, Google, Slack)

---

## Slide 3: Governance — What IT Gets

| Capability | What It Does |
|---|---|
| **SSO/RBAC** | Users authenticate with corporate credentials. No API keys to manage. |
| **Quota Policies** | Set monthly/daily token limits per user, team, or org. Block or alert on threshold. |
| **Model Allow-Lists** | Control which Claude/GPT models users can access. Toggle per-user. |
| **Audit Trail** | Every model call tracked: who, when, which model, how many tokens. |
| **Spend Visibility** | Real-time dashboard showing cost by user, team, model. Export to CSV. |
| **Auto-Block** | User hits quota? Automatically blocked. No surprise bills. |

---

## Slide 4: Billing Goes to YOUR Account

**Cross-Account Architecture — You Own Your Spend**

```
Your AWS Account (you control, you pay)
├── Cognito User Pool (your users)
├── Identity Pool (your credentials)  
├── BedrockRole (your IAM)
└── Amazon Bedrock ← BILLED HERE

AllCode Platform Account (management only)
├── Nexus API (Lambda)
├── Dashboard (CloudFront)
└── Telemetry Collector (ECS)
    └── $0 Bedrock charges for your usage
```

- Your developers' Bedrock calls use YOUR credentials
- YOUR AWS bill shows the Bedrock line items
- AllCode never touches your model calls
- Verified via CloudTrail + Cost Explorer in your account

---

## Slide 5: Different Installation Models

| Model | Who It's For | How It Works |
|---|---|---|
| **Self-Service (Setup Page)** | Any enterprise | Customer visits nexus.allcode.com/setup, deploys one CloudFormation stack. Done in 5 minutes. |
| **Managed Deployment** | Large enterprises | AllCode team deploys into customer's AWS account. Full white-glove. |
| **AWS Marketplace** | EDP customers | Subscribe via Marketplace. Counts toward your AWS commit. |
| **Multi-Org (SaaS)** | MSPs / holding companies | Single Nexus instance governs multiple child orgs. Each org has own billing. |

**One installer per org** — covers Claude Code + Codex. Users run one script and they're live.

---

## Slide 6: What Gets Deployed (Customer Account)

**One CloudFormation stack creates everything:**

- ✅ Cognito User Pool + Identity Pool (auth)
- ✅ IAM Roles with least-privilege Bedrock access
- ✅ DynamoDB tables for quota tracking
- ✅ Admin user with email invite
- ✅ Auto-registers with Nexus (custom resource callback)

**What stays in AllCode's account:**
- Nexus API (management plane only)
- Dashboard UI
- Telemetry aggregation

**Customer data never leaves their account.**

---

## Slide 7: The Nexus Factory (Dark Factory)

**Autonomous Software Factory — AI Building AI-Governed Software**

The Nexus Factory is our internal autonomous development system:

- Submit a task → Factory plans, codes, reviews, merges
- Runs on Amazon Bedrock (Claude + GPT-5)
- Iterative: plan → code → inspect → fix → merge
- Produces production-ready PRs with 90-95% quality on first pass

**Why it matters for customers:**
- Custom governance rules? Factory builds it.
- Need a new integration? Factory ships it overnight.
- Feature requests turn around in hours, not sprints.

---

## Slide 8: Tool-Agnostic — Not Locked to Any Vendor

| AI Coding Tool | Status | Model Provider |
|---|---|---|
| Claude Code (Anthropic) | ✅ Fully supported | Claude 4/Sonnet/Haiku via Bedrock |
| Claude Desktop (Cowork) | ✅ Fully supported | Same models, desktop UX |
| OpenAI Codex | ✅ Fully supported | GPT-5.4/5.5/5.6 via Bedrock Mantle |
| OpenCode | ✅ Supported | Any Bedrock model |
| Future tools | 🔄 Plug-in architecture | Any model on Bedrock |

**One governance layer for ALL your AI coding tools.**

Switch models, switch tools — Nexus still governs.

---

## Slide 9: Real Numbers (Live Demo Data)

**Skematic (customer account 825580929554):**
- 2 users onboarded via self-service setup page
- 300K+ tokens tracked with per-user attribution
- Claude Sonnet 4.6 + GPT-5.4 model access
- Bedrock charges confirmed in THEIR Cost Explorer
- $0 charges to AllCode's account

**AllCode (internal, 5 developers):**
- 641K tokens/month across Claude Code + Codex
- Per-user spend visible in dashboard
- Quota enforcement active (225M token monthly cap)

---

## Slide 10: Pricing

| Tier | Users | Price | Includes |
|---|---|---|---|
| **Starter** | Up to 10 | $500/mo | SSO, quotas, dashboard, 1 org |
| **Professional** | Up to 50 | $2,000/mo | + MCP extensions, skills, multi-model |
| **Enterprise** | Unlimited | $5,000/mo | + Factory access, custom integrations, SLA |

**Plus:** Customer pays their own Bedrock usage directly to AWS.

**EDP-eligible** — AllCode Nexus subscription counts toward AWS commit.

---

## Slide 11: 5-Minute Setup (Live)

1. Visit `nexus.allcode.com/setup`
2. Enter org name + admin email + region
3. Click "Launch in AWS Console"
4. CloudFormation deploys everything
5. Admin gets email with credentials
6. Log in → invite team → download installer → coding with governance

**Demo: Let's set up a new org right now.**

---

## Slide 12: Why AllCode

- **AWS Advanced Consulting Partner**
- **Built on open-source AWS guidance** (aws-samples/guidance-for-claude-code-with-amazon-bedrock)
- **Production-proven** — multiple enterprise orgs live today
- **AWS Marketplace listed** — EDP-eligible
- **Transform-ready** — integrates with AWS Transform for modernization governance
- **Partner Revenue Measurement compliant** — AllCode gets attributed for Bedrock consumption

---

## Contact

**AllCode LLC**
andreas@allcode.com
nexus.allcode.com

AWS Partner: Advanced Consulting Partner
AWS Marketplace: AllCode Nexus
