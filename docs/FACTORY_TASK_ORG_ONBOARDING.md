# Factory Task: Automated Org Onboarding for AllCode Nexus

## Objective

Build an automated org onboarding system that provisions a new enterprise org from scratch — given only an org slug and customer AWS account ID. The system should create all required infrastructure (Cognito, IAM roles, policies) in the customer's account and register the org in the Nexus platform, then produce a working installer download.

Currently this is a 12-step manual process (see `docs/ORG_ONBOARDING_GUIDE.md`). Automate it.

---

## Task 1: Fix _get_org_config() (Quick Fix)

**File**: `nexus-ui/api/index.py`  
**Function**: `_get_org_config()` (line ~2472)

The function generates the org-specific `config.json` that goes into installer packages. It's missing two critical fields that orgs need for cross-account Bedrock access:

### Current (broken for cross-account orgs):
```python
return {
    profile_name: {
        "provider_domain": provider_domain,
        "client_id": client_id,
        "aws_region": region,
        "provider_type": "cognito",
        "credential_storage": "keyring",
        "cross_region_profile": "us",
        "identity_pool_id": identity_pool_id,
        "federation_type": "cognito",
        "cognito_user_pool_id": user_pool_id,
        "selected_model": "us.anthropic.claude-sonnet-4-20250514-v1:0",
        "quota_api_endpoint": "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/quotas",
        "quota_fail_mode": "open",
        "quota_check_interval": 30,
    }
}
```

### Required (add bedrock_role_arn):
```python
config = {
    profile_name: {
        "provider_domain": provider_domain,
        "client_id": client_id,
        "aws_region": region,
        "provider_type": "cognito",
        "credential_storage": "keyring",
        "cross_region_profile": "us",
        "identity_pool_id": identity_pool_id,
        "federation_type": "cognito",
        "cognito_user_pool_id": user_pool_id,
        "selected_model": "us.anthropic.claude-sonnet-4-20250514-v1:0",
        "quota_api_endpoint": "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/quotas",
        "quota_fail_mode": "open",
        "quota_check_interval": 30,
    }
}

# Add bedrock_role_arn if org uses cross-account billing
bedrock_role_arn = org.get("bedrock_role_arn", "")
if bedrock_role_arn:
    config[profile_name]["bedrock_role_arn"] = bedrock_role_arn

return config
```

**Why**: Without `bedrock_role_arn` in the config, the credential-process binary on user machines can't chain-assume to the Bedrock billing role. Users get "Access Denied" on model calls.

---

## Task 2: Build Org Onboarding API Endpoint

**File**: `nexus-ui/api/index.py`  
**New endpoint**: `POST /api/admin/onboard-org`

Build a Lambda endpoint that automates org provisioning. The admin provides:
- `org_slug` (string, lowercase, no spaces)
- `org_display_name` (string)
- `customer_account_id` (string, 12-digit AWS account ID)
- `customer_region` (string, e.g., "us-west-2")
- `admin_email` (string, first admin user to create)

The endpoint should:

### Step A: Create Cognito resources (in customer account)
Use STS to assume `AllCodeNexusConnector` role in the customer's account, then:

1. Create User Pool (`{org_slug}-claude-code`)
2. Create domain (`{org_slug}-nexus`)
3. Create app client (`{org_slug}-cli`) with PKCE flow, callback `http://localhost:8400/callback`
4. Create Identity Pool (`{org_slug}-nexus-pool`) linked to the user pool
5. Create admin user with provided email

### Step B: Create IAM resources (in customer account)
Still using the assumed role:

1. Create `AllCodeNexusBedrockRole` with:
   - Trust policy: Cognito identity pool (authenticated users)
   - Inline policy `BedrockAccess`: bedrock:InvokeModel, bedrock:InvokeModelWithResponseStream, bedrock:GetFoundationModel, bedrock:ListFoundationModels on *
2. Add `BedrockAccess` inline policy to existing `AllCodeNexusConnector` role
3. Set identity pool roles (authenticated → AllCodeNexusBedrockRole)

### Step C: Register in Nexus (local account)
1. Put item in DynamoDB NexusOrganizations table with all fields
2. Return success with the generated config summary

### Prerequisites for this to work:
- The customer must FIRST create `AllCodeNexusConnector` role in their account with trust to `916587687563:root`
- That's the only manual step — everything else is automated
- Alternative: provide a CloudFormation template the customer deploys that creates the connector role

### API Response:
```json
{
  "status": "success",
  "org_slug": "skematic",
  "user_pool_id": "us-west-2_XXXXXXXX",
  "client_id": "xxxxxxxxxxxxxxxxxx",
  "identity_pool_id": "us-west-2:xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx",
  "provider_domain": "skematic-nexus.auth.us-west-2.amazoncognito.com",
  "bedrock_role_arn": "arn:aws:iam::825580929554:role/AllCodeNexusBedrockRole",
  "admin_user": "admin@skematic.com",
  "download_url": "https://nexus.allcode.com/download (org will auto-detect)"
}
```

### Error handling:
- If resources already exist (pool, role, etc), skip creation and use existing
- If AssumeRole fails, return clear error: "AllCodeNexusConnector role not found or trust not configured in account {id}"
- Validate inputs (account ID format, valid region, slug format)

---

## Task 3: Admin UI for Org Onboarding (Optional, stretch)

**File**: `src/pages/Settings.tsx` (nexus-ui repo) or new `src/pages/AdminOrgs.tsx`

Add a simple form in the Nexus admin portal:
- Input fields: Org slug, Display name, Account ID, Region, Admin email
- Submit button calls `POST /api/admin/onboard-org`
- Shows progress/results
- Only visible to AllCode admin users

---

## Constraints

- ADDITIVE ONLY: Don't rewrite existing functions. Add new code alongside.
- Backend code goes in the guidance repo at `nexus-ui/api/index.py`
- Frontend code goes in the nexus-ui repo at `src/`
- Don't commit build artifacts (.pyc, __pycache__, node_modules)
- The Lambda runs Python 3.12 with boto3 available
- All AWS calls from the Lambda use `boto3` — no CLI
- The `AllCodeNexusConnector` role assumption is already proven to work (916→customer account)
- Import any new boto3 services at function level, not module level (Lambda cold start optimization)
- Follow existing patterns in the Lambda file (use `response()` helper, extract params from event, etc.)

## Testing

- Task 1: Deploy Lambda, hit `/api/download?platform=mac` as a non-allcode org user, verify config.json includes `bedrock_role_arn`
- Task 2: Call `POST /api/admin/onboard-org` with test data, verify resources created
- Task 3: Load Settings/Admin page, fill form, submit, verify end-to-end
