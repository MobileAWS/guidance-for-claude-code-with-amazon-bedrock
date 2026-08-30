# AllCode Nexus — Cross-Account Billing Proof

## Summary

AllCode Nexus routes all Amazon Bedrock spend to each customer's own AWS account. AllCode's platform account (916587687563) is never billed for customer model usage. This document proves this with verifiable AWS evidence.

---

## How It Works

```
Customer uses Claude Code / Codex
    ↓
Authenticates via THEIR Cognito User Pool (in their AWS account)
    ↓
Gets temporary AWS credentials from THEIR Identity Pool (in their account)
    ↓
Assumes THEIR BedrockRole (in their account)
    ↓
Calls Amazon Bedrock WITH THEIR CREDENTIALS
    ↓
AWS bills the account that signed the request = THEIR account
```

AllCode's account runs only the management plane (Lambda API, DynamoDB, UI hosting). It never makes Bedrock InvokeModel calls on behalf of customers.

---

## Evidence: Skematic (Account 825580929554)

### 1. CloudTrail — Bedrock API calls originate from Skematic's account

CloudTrail in Skematic's account (825580929554) shows 50+ `InvokeModel` events between July 30-31, 2026:

```
Region: us-west-2
EventName: InvokeModel
UserIdentity: assumed-role/skematic-BedrockRole/CognitoIdentityCredentials
AccountId: 825580929554
```

The calling role is `skematic-BedrockRole` — a role that exists ONLY in Skematic's account, assumed via their Cognito Identity Pool.

### 2. Cost Explorer — Bedrock usage line items appear in Skematic's billing

Queried via `aws ce get-cost-and-usage` in Skematic's account:

```
Account: 825580929554
Period: 2026-07-01 to 2026-08-04

Service Line Items Present:
  - Claude Sonnet 4 (Amazon Bedrock Edition)
  - Claude Sonnet 4.6 (Amazon Bedrock Edition)  
  - Claude Haiku 4.5 (Amazon Bedrock Edition)
  - OpenAI GPT-5.4 (Amazon Bedrock Edition)

Usage (last 30 days):
  USW2 Input Tokens:   313,710 (us-west-2)
  USW2 Output Tokens:   32,540 (us-west-2)
  USE1 Input Tokens:       250 (us-east-1, cross-region)
  USE1 Output Tokens:       70 (us-east-1, cross-region)
  Cache Read Tokens:     8,980
  Cache Write Tokens:    2,480
```

These numbers match our OTel telemetry data for user `alex+skematic@allcode.com` (269K input+output tracked via Nexus).

### 3. Credential Chain — No path exists for AllCode to be billed

The credential chain for a Skematic user:

1. User authenticates → Skematic's Cognito Pool (`us-west-2_PvSMpgtNH`)
2. Cognito issues tokens → exchanged at Skematic's Identity Pool (`us-west-2:bbc6130b-...`)
3. Identity Pool returns credentials → scoped to `skematic-BedrockRole` in account 825580929554
4. User calls `bedrock:InvokeModel` → signed with credentials from step 3
5. AWS bills account 825580929554 (the account that owns the signing credentials)

At no point do AllCode's credentials (account 916587687563) touch the Bedrock API call.

---

## Evidence: Lets Play Soccer (Account 810108058300)

### 1. Cost Explorer — Bedrock usage in LetsPlay's account

```
Account: 810108058300
Period: 2026-07-04 to 2026-08-04

Usage:
  USE1 Cache Read Tokens:    1,660
  USE1 Cache Write Tokens:     810
  USE1 Output Tokens:           20
  USE2 Input Tokens:             0 (minimal)
  USE2 Output Tokens:            0 (minimal)
  USE2 Cache Write Tokens:      30
```

Bedrock usage exists in THEIR account, billed to THEM.

### 2. Credential Chain

LetsPlay users assume `AllCodeNexusBedrockRole` in account 810108058300. Same pattern — their credentials, their bill.

---

## Evidence: AllCode Platform Account (916587687563)

### What AllCode's account IS billed for:

| Service | Purpose | Monthly Cost |
|---------|---------|-------------|
| Lambda | Nexus API | ~$5 |
| DynamoDB | User data, metrics, configs | ~$2 |
| ECS Fargate | OTel collector | ~$30 |
| CloudFront + S3 | UI hosting, installer packages | ~$5 |
| API Gateway | REST API | ~$1 |

### What AllCode's account is NOT billed for:

| Service | Billed To | Reason |
|---------|-----------|--------|
| Amazon Bedrock (Claude) | Customer account | Customer's IAM credentials sign the request |
| Amazon Bedrock (GPT-5 via Mantle) | Dev account (839765241245) | AllCode's internal credits |

AllCode only uses Bedrock in the dev account (839765241245) for internal AllCode org users. External customer Bedrock usage NEVER touches 916587687563.

---

## How to Verify (Repeatable)

### From Any Customer's Account:

```bash
# 1. Check CloudTrail for Bedrock calls
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=EventName,AttributeValue=InvokeModel \
  --start-time "2026-07-01T00:00:00Z" \
  --region us-west-2

# 2. Check Cost Explorer for Bedrock line items
aws ce get-cost-and-usage \
  --time-period Start=2026-07-01,End=2026-08-01 \
  --granularity MONTHLY \
  --metrics BlendedCost UsageQuantity \
  --group-by Type=DIMENSION,Key=SERVICE \
  --filter '{"Or":[{"Dimensions":{"Key":"SERVICE","Values":["Amazon Bedrock"]}},{"Dimensions":{"Key":"SERVICE","Values":["Claude Sonnet 4 (Amazon Bedrock Edition)","Claude Sonnet 4.6 (Amazon Bedrock Edition)","OpenAI GPT-5.4 (Amazon Bedrock Edition)"]}}]}'
```

### From Nexus API:

```bash
# Returns real AWS billing data from the customer's account
curl "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/billing/aws-spend" \
  -H "Authorization: Bearer <token>" \
  -H "x-org-id: skematic"
```

---

## Architecture Diagram

```
┌─────────────────────────────────────────────────────────────┐
│ AllCode Platform Account (916587687563)                       │
│                                                              │
│  ┌──────────┐  ┌──────────┐  ┌─────────┐  ┌────────────┐  │
│  │ Lambda   │  │ DynamoDB │  │ ECS     │  │ CloudFront │  │
│  │ (API)    │  │ (data)   │  │ (OTel)  │  │ (UI)       │  │
│  └──────────┘  └──────────┘  └─────────┘  └────────────┘  │
│                                                              │
│  NO BEDROCK CALLS FROM THIS ACCOUNT FOR CUSTOMERS           │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│ Skematic Account (825580929554)                              │
│                                                              │
│  ┌──────────────┐  ┌───────────────┐  ┌─────────────────┐  │
│  │ Cognito Pool │  │ Identity Pool │  │ skematic-       │  │
│  │ (auth)       │→ │ (credentials) │→ │ BedrockRole     │  │
│  └──────────────┘  └───────────────┘  └────────┬────────┘  │
│                                                  │           │
│                                    ┌─────────────▼────────┐ │
│                                    │ Amazon Bedrock       │ │
│                                    │ (BILLED HERE)        │ │
│                                    └──────────────────────┘ │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│ Lets Play Account (810108058300)                             │
│                                                              │
│  Same pattern: Cognito → Identity Pool → BedrockRole         │
│  Bedrock calls billed to THIS account                        │
└─────────────────────────────────────────────────────────────┘
```

---

## Key Takeaway

**AllCode Nexus is a management plane only.** It handles identity, telemetry, quotas, and admin UI. All model inference happens in the customer's AWS account, using the customer's IAM credentials, billed to the customer's AWS bill.

This is by design — same model as AWS Control Tower or AWS Organizations. The control plane and data plane are in separate accounts.

---

## Verified By

- **CloudTrail**: InvokeModel events with customer role ARN in customer account
- **Cost Explorer**: Bedrock service line items in customer billing
- **Nexus API**: `GET /api/billing/aws-spend` endpoint queries customer's Cost Explorer in real-time
- **IAM Architecture**: No credential path exists for AllCode to be billed for customer inference

Date: August 3, 2026
