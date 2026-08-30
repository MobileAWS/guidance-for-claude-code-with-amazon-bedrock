# AllCode Nexus — New Organization Onboarding Guide

This guide describes the complete end-to-end process for onboarding a new enterprise organization to AllCode Nexus. After completing these steps, the org's developers can download one installer, authenticate with their corporate credentials, and use Claude Code + Codex on Amazon Bedrock with full governance.

## Overview

Onboarding a new org requires work in TWO AWS accounts:

1. **Customer's AWS account** — Cognito User Pool (auth), IAM roles (Bedrock access)
2. **Nexus production account (916587687563)** — DynamoDB record, installer package generation

## Prerequisites

- Admin access to the customer's AWS account
- Admin access to the Nexus production account (916587687563)
- Customer's AWS account ID
- Desired org slug (lowercase, no spaces, e.g., "skematic", "lets-play")
- Customer's preferred AWS region

---

## Step 1: Create Cognito User Pool (Customer Account)

Create a Cognito User Pool in the customer's AWS account for developer authentication.

**Region**: Customer's preferred region (e.g., us-west-2, us-east-1)

```bash
# Create the user pool
aws cognito-idp create-user-pool \
  --pool-name "{org-slug}-claude-code" \
  --auto-verified-attributes email \
  --username-attributes email \
  --policies '{"PasswordPolicy":{"MinimumLength":8,"RequireUppercase":true,"RequireLowercase":true,"RequireNumbers":true,"RequireSymbols":false}}' \
  --schema '[{"Name":"email","Required":true,"Mutable":true}]' \
  --region {customer-region}
```

Save the `UserPool.Id` from the output (e.g., `us-west-2_YAaS9DXaP`).

## Step 2: Create Cognito Domain (Customer Account)

```bash
aws cognito-idp create-user-pool-domain \
  --domain "{org-slug}-nexus" \
  --user-pool-id {user-pool-id} \
  --region {customer-region}
```

This creates the hosted UI at: `https://{org-slug}-nexus.auth.{region}.amazoncognito.com`

## Step 3: Create App Client (Customer Account)

Create an app client for the CLI credential-process to use (PKCE flow, no client secret).

```bash
aws cognito-idp create-user-pool-client \
  --user-pool-id {user-pool-id} \
  --client-name "{org-slug}-cli" \
  --explicit-auth-flows ALLOW_USER_SRP_AUTH ALLOW_REFRESH_TOKEN_AUTH ALLOW_USER_PASSWORD_AUTH \
  --supported-identity-providers COGNITO \
  --callback-urls '["http://localhost:8400/callback"]' \
  --logout-urls '["http://localhost:8400/logout"]' \
  --allowed-o-auth-flows code \
  --allowed-o-auth-scopes openid email profile \
  --allowed-o-auth-flows-user-pool-client \
  --generate-secret false \
  --region {customer-region}
```

Save the `UserPoolClient.ClientId` from the output (e.g., `576j85ee7nf5q7gfd0946ngnrd`).

## Step 4: Create Cognito Identity Pool (Customer Account)

The identity pool federates Cognito tokens into temporary AWS credentials.

```bash
aws cognito-identity create-identity-pool \
  --identity-pool-name "{org-slug}-nexus-pool" \
  --allow-unauthenticated-identities false \
  --cognito-identity-providers '[{"ClientId":"{client-id}","ProviderName":"cognito-idp.{customer-region}.amazonaws.com/{user-pool-id}","ServerSideTokenCheck":true}]' \
  --region {customer-region}
```

Save the `IdentityPoolId` from the output (e.g., `us-west-2:xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`).

## Step 5: Create IAM Role — AllCodeNexusConnector (Customer Account)

This role is assumed by the Nexus platform (from account 916587687563) to validate users and read metrics.

```bash
aws iam create-role \
  --role-name AllCodeNexusConnector \
  --assume-role-policy-document '{
    "Version": "2012-10-17",
    "Statement": [
      {
        "Effect": "Allow",
        "Principal": {
          "AWS": "arn:aws:iam::916587687563:root"
        },
        "Action": "sts:AssumeRole",
        "Condition": {}
      }
    ]
  }'
```

## Step 6: Create IAM Role — AllCodeNexusBedrockRole (Customer Account)

This role provides Bedrock access. The credential-process on user machines assumes this role (chained from Cognito identity pool credentials).

```bash
aws iam create-role \
  --role-name AllCodeNexusBedrockRole \
  --assume-role-policy-document '{
    "Version": "2012-10-17",
    "Statement": [
      {
        "Effect": "Allow",
        "Principal": {
          "Federated": "cognito-identity.amazonaws.com"
        },
        "Action": "sts:AssumeRoleWithWebIdentity",
        "Condition": {
          "StringEquals": {
            "cognito-identity.amazonaws.com:aud": "{identity-pool-id}"
          },
          "ForAnyValue:StringLike": {
            "cognito-identity.amazonaws.com:amr": "authenticated"
          }
        }
      }
    ]
  }'
```

## Step 7: Attach Bedrock Policies (Customer Account)

Add Bedrock invoke permissions to BOTH roles:

```bash
# BedrockAccess policy on AllCodeNexusConnector
aws iam put-role-policy \
  --role-name AllCodeNexusConnector \
  --policy-name BedrockAccess \
  --policy-document '{
    "Version": "2012-10-17",
    "Statement": [
      {
        "Effect": "Allow",
        "Action": [
          "bedrock:InvokeModel",
          "bedrock:InvokeModelWithResponseStream",
          "bedrock:GetFoundationModel",
          "bedrock:ListFoundationModels",
          "bedrock:GetUseCaseForModelAccess"
        ],
        "Resource": "*"
      }
    ]
  }'

# Same policy on AllCodeNexusBedrockRole
aws iam put-role-policy \
  --role-name AllCodeNexusBedrockRole \
  --policy-name BedrockAccess \
  --policy-document '{
    "Version": "2012-10-17",
    "Statement": [
      {
        "Effect": "Allow",
        "Action": [
          "bedrock:InvokeModel",
          "bedrock:InvokeModelWithResponseStream",
          "bedrock:GetFoundationModel",
          "bedrock:ListFoundationModels",
          "bedrock:GetUseCaseForModelAccess"
        ],
        "Resource": "*"
      }
    ]
  }'

# ReadMetrics policy on AllCodeNexusConnector (for dashboard)
aws iam put-role-policy \
  --role-name AllCodeNexusConnector \
  --policy-name ReadMetrics \
  --policy-document '{
    "Version": "2012-10-17",
    "Statement": [
      {
        "Effect": "Allow",
        "Action": [
          "cloudwatch:GetMetricData",
          "cloudwatch:ListMetrics"
        ],
        "Resource": "*"
      }
    ]
  }'
```

## Step 8: Set Identity Pool Roles (Customer Account)

Link the Bedrock role to the identity pool:

```bash
aws cognito-identity set-identity-pool-roles \
  --identity-pool-id "{identity-pool-id}" \
  --roles '{"authenticated":"arn:aws:iam::{customer-account-id}:role/AllCodeNexusBedrockRole"}' \
  --region {customer-region}
```

## Step 9: Create Admin User in Cognito (Customer Account)

```bash
# Create admin user
aws cognito-idp admin-create-user \
  --user-pool-id {user-pool-id} \
  --username "admin@{org-domain}.com" \
  --user-attributes Name=email,Value="admin@{org-domain}.com" Name=email_verified,Value=true \
  --temporary-password "TempPass123!" \
  --region {customer-region}

# Set permanent password (optional — user can change on first login)
aws cognito-idp admin-set-user-password \
  --user-pool-id {user-pool-id} \
  --username "admin@{org-domain}.com" \
  --password "SecurePassword123!" \
  --permanent \
  --region {customer-region}
```

---

## Step 10: Register Org in Nexus DynamoDB (Nexus Account — 916587687563)

Insert the org record into the NexusOrganizations table:

```bash
aws dynamodb put-item \
  --table-name NexusOrganizations \
  --item '{
    "pk": {"S": "ORG#{org-slug}"},
    "sk": {"S": "DETAILS"},
    "name": {"S": "{Org Display Name}"},
    "status": {"S": "active"},
    "account_id": {"S": "{customer-account-id}"},
    "role_arn": {"S": "arn:aws:iam::{customer-account-id}:role/AllCodeNexusConnector"},
    "bedrock_role_arn": {"S": "arn:aws:iam::{customer-account-id}:role/AllCodeNexusBedrockRole"},
    "user_pool_id": {"S": "{user-pool-id}"},
    "client_id": {"S": "{client-id}"},
    "provider_domain": {"S": "{org-slug}-nexus.auth.{customer-region}.amazoncognito.com"},
    "region": {"S": "{customer-region}"},
    "created_at": {"S": "{iso-timestamp}"}
  }' \
  --region us-east-1
```

## Step 11: Verify Download Endpoint

The Nexus Download page uses the `_get_org_config()` function which reads from DynamoDB and generates an org-specific `config.json`. It then takes the base installer package from S3, replaces `config.json` with the org-specific version, and serves it as a presigned download.

The generated `config.json` for the org looks like:

```json
{
  "{org-slug}-{region}": {
    "provider_domain": "{org-slug}-nexus.auth.{region}.amazoncognito.com",
    "client_id": "{client-id}",
    "aws_region": "{region}",
    "provider_type": "cognito",
    "credential_storage": "keyring",
    "cross_region_profile": "us",
    "identity_pool_id": "{identity-pool-id}",
    "federation_type": "cognito",
    "cognito_user_pool_id": "{user-pool-id}",
    "selected_model": "us.anthropic.claude-sonnet-4-20250514-v1:0",
    "quota_api_endpoint": "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/quotas",
    "quota_fail_mode": "open",
    "quota_check_interval": 30
  }
}
```

**NOTE**: The `_get_org_config()` function in `nexus-ui/api/index.py` currently does NOT include `identity_pool_id` or `bedrock_role_arn` in the generated config. These fields should be added to the function if the org uses cross-account Bedrock billing (chain-assume pattern).

## Step 12: Test the Flow

1. **Portal login**: Go to `https://nexus.allcode.com`, enter org slug at login
2. **Download**: Click Download → installer zip should be org-specific
3. **Install**: Run `install.sh` — should configure AWS profile with org-specific config
4. **Auth**: Run `claude-code` — browser opens to `{org-slug}-nexus.auth.{region}.amazoncognito.com`
5. **Model access**: After auth, Claude Code should invoke models on Bedrock

---

## Reference: Existing Orgs

| Org | Account | Region | Pool | Status |
|-----|---------|--------|------|--------|
| allcode | 916587687563 | us-east-1 | us-east-1_3mbtSSlmt | ✅ Active (internal, uses bedrock_role_arn chain to 839765241245) |
| lets-play | 810108058300 | us-east-1 | us-east-2_oVIbFbxum | ✅ Active (external customer) |
| skematic | 825580929554 | us-east-1 | us-west-2_YAaS9DXaP | ⚠️ Incomplete (missing BedrockAccess policy) |
| jamaica-collections | 111315604986 | us-east-1 | — | ❌ No Cognito pool |
| account-connections | 492528177532 | us-east-1 | — | ❌ No Cognito pool |

## Reference: Architecture Flow

```
User runs install.sh
  → Copies credential-process binary to ~/claude-code-with-bedrock/
  → Writes config.json with org-specific Cognito + region details
  → Configures AWS CLI profile with credential_process pointing to binary
  → Installs Claude Code CLI + Codex CLI

User runs claude-code
  → AWS CLI calls credential-process
  → credential-process reads config.json
  → Opens browser to {org}-nexus.auth.{region}.amazoncognito.com
  → User authenticates with email/password
  → Gets Cognito tokens
  → Exchanges tokens for AWS credentials via Identity Pool
  → (Optional) Chain-assumes to a separate Bedrock billing role
  → Returns temporary STS credentials to Claude Code
  → Claude Code calls Bedrock with those credentials
```

## Reference: Key Files

- `nexus-ui/api/index.py` → `_get_org_config()` function (line ~2472) generates org config from DynamoDB
- `nexus-ui/api/index.py` → `handle_download()` function (line ~2295) serves org-specific installer
- `source/go/cmd/credential-process/main.go` → Go binary that handles auth + credential exchange
- Base installer package: `s3://claude-code-auth-distribution-916587687563/allcode-nexus/latest.zip`

## Troubleshooting

- **"Access Denied" on Bedrock calls**: BedrockAccess policy missing from the role, or identity pool not linked to the correct role
- **"User pool does not exist"**: Pool was created in a different region than configured
- **Browser shows error on auth**: App client callback URL must be `http://localhost:8400/callback`
- **credential-process returns empty**: Config.json missing `provider_domain` or `client_id`
- **Download returns default package**: Org not found in DynamoDB NexusOrganizations table
