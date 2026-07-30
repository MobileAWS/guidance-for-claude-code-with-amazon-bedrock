# Factory Task: Self-Service Org Setup Page

## Objective

Build a self-service setup page at `nexus.allcode.com/setup` where new customers can onboard themselves. They click a link, deploy a CloudFormation template in their AWS account, and they're done. No manual steps, no back-and-forth with AllCode admins.

## User Flow

1. Customer visits `https://nexus.allcode.com/setup` (no login required)
2. Page explains what AllCode Nexus does, shows a form: **Org Name** and **AWS Region** (dropdown: us-east-1, us-east-2, us-west-2, eu-west-1)
3. Customer fills in the form and clicks **"Launch in AWS"**
4. Button opens a CloudFormation quick-create URL in their AWS Console with the org name and region pre-filled
5. Customer clicks "Create Stack" in their AWS Console (only thing they do)
6. CloudFormation stack deploys everything (Cognito, IAM, Identity Pool, admin user)
7. A custom resource Lambda in the stack calls `POST https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/setup/register` with the stack outputs
8. Nexus registers the org in DynamoDB
9. Customer receives an email (from Cognito) with their temporary password
10. Customer logs into `nexus.allcode.com` with their org name and credentials

## What to Build

### 1. Frontend: Setup Page (`src/pages/Setup.tsx`)

A new page at `/setup` route. **No authentication required** — this is a public landing page.

Content:
- Headline: "Set Up Your Organization"
- Subtext: "Deploy AllCode Nexus in your AWS account in under 5 minutes. One CloudFormation stack handles everything."
- Form fields:
  - **Organization Name** (text, lowercase, no spaces — validated with pattern `[a-z0-9-]+`)
  - **Admin Email** (email input)
  - **AWS Region** (dropdown: us-east-1, us-east-2, us-west-2, eu-west-1, eu-central-1)
- Button: **"Launch in AWS Console"**
- On click: opens a new tab with the CloudFormation quick-create URL:
  ```
  https://{region}.console.aws.amazon.com/cloudformation/home?region={region}#/stacks/quickcreate?templateURL=https://nexus-templates.s3.us-east-1.amazonaws.com/nexus-customer-onboarding.yaml&stackName=AllCodeNexus&param_OrganizationName={orgName}&param_AdminEmail={email}
  ```
- Below the button: "What this creates" expandable section listing what gets deployed (Cognito pool, IAM roles, Bedrock access)
- Below that: "Already deployed? Check your email for login credentials."

Style: Use the same MUI theme as the rest of the app. Clean, simple, professional.

### 2. Frontend: Add route (`src/App.tsx`)

Add `/setup` route that renders `Setup.tsx`. This route must NOT require authentication (no `AuthGuard` wrapper).

### 3. Backend: Registration Endpoint (`api/index.py`)

New handler: `handle_setup_register(event)`  
New route: `POST /api/setup/register`  
**No authentication required** (this is called by the CloudFormation custom resource Lambda in the customer's account).

The endpoint receives:
```json
{
  "org_slug": "skematic",
  "account_id": "825580929554",
  "region": "us-west-2",
  "user_pool_id": "us-west-2_XXXXXXXX",
  "client_id": "xxxxxxxxxxxxxxxxxx",
  "identity_pool_id": "us-west-2:xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx",
  "bedrock_role_arn": "arn:aws:iam::825580929554:role/skematic-BedrockRole",
  "connector_role_arn": "arn:aws:iam::825580929554:role/AllCodeNexusConnector",
  "admin_email": "admin@skematic.com"
}
```

It should:
1. Validate all required fields
2. Check org doesn't already exist in DynamoDB (return 409 if it does)
3. Insert into NexusOrganizations table:
   ```python
   {
       "pk": f"ORG#{org_slug}",
       "sk": "DETAILS",
       "name": org_slug,
       "status": "active",
       "account_id": account_id,
       "region": region,
       "user_pool_id": user_pool_id,
       "client_id": client_id,
       "identity_pool_id": identity_pool_id,
       "provider_domain": f"{org_slug}-nexus.auth.{region}.amazoncognito.com",
       "role_arn": connector_role_arn,
       "bedrock_role_arn": bedrock_role_arn,
       "created_at": datetime.now(timezone.utc).isoformat(),
   }
   ```
4. Return 201 with `{"status": "registered", "org_slug": org_slug}`

Security: Add a shared secret header check (`x-nexus-setup-token`) that the CloudFormation custom resource includes. The token value is: `nexus-setup-2026-allcode`. Reject requests without this header.

### 4. CloudFormation Template Update (`infra/nexus-customer-onboarding.yaml`)

Add a custom resource at the end of the template that calls our registration endpoint after all resources are created.

Add these resources to the existing template:

```yaml
  # Lambda that calls Nexus API to register this org
  RegistrationFunction:
    Type: AWS::Lambda::Function
    Properties:
      Runtime: python3.12
      Handler: index.handler
      Timeout: 30
      Role: !GetAtt RegistrationFunctionRole.Arn
      Code:
        ZipFile: |
          import json
          import urllib.request
          import cfnresponse
          
          def handler(event, context):
              if event['RequestType'] == 'Delete':
                  cfnresponse.send(event, context, cfnresponse.SUCCESS, {})
                  return
              
              props = event['ResourceProperties']
              data = {
                  "org_slug": props["OrgSlug"],
                  "account_id": props["AccountId"],
                  "region": props["Region"],
                  "user_pool_id": props["UserPoolId"],
                  "client_id": props["ClientId"],
                  "identity_pool_id": props["IdentityPoolId"],
                  "bedrock_role_arn": props["BedrockRoleArn"],
                  "connector_role_arn": props["ConnectorRoleArn"],
                  "admin_email": props["AdminEmail"],
              }
              
              req = urllib.request.Request(
                  "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/setup/register",
                  data=json.dumps(data).encode(),
                  headers={
                      "Content-Type": "application/json",
                      "x-nexus-setup-token": "nexus-setup-2026-allcode"
                  },
                  method="POST"
              )
              
              try:
                  resp = urllib.request.urlopen(req)
                  cfnresponse.send(event, context, cfnresponse.SUCCESS, {"Status": "Registered"})
              except Exception as e:
                  cfnresponse.send(event, context, cfnresponse.FAILED, {"Error": str(e)})
  
  RegistrationFunctionRole:
    Type: AWS::IAM::Role
    Properties:
      AssumeRolePolicyDocument:
        Version: '2012-10-17'
        Statement:
          - Effect: Allow
            Principal:
              Service: lambda.amazonaws.com
            Action: sts:AssumeRole
      ManagedPolicyArns:
        - arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole

  RegisterWithNexus:
    Type: Custom::NexusRegistration
    DependsOn:
      - IdentityPoolRoleAttachment
      - AdminUser
      - NexusConnectorRole
    Properties:
      ServiceToken: !GetAtt RegistrationFunction.Arn
      OrgSlug: !Ref OrganizationName
      AccountId: !Ref AWS::AccountId
      Region: !Ref AWS::Region
      UserPoolId: !Ref UserPool
      ClientId: !Ref UserPoolClient
      IdentityPoolId: !Ref IdentityPool
      BedrockRoleArn: !GetAtt BedrockRole.Arn
      ConnectorRoleArn: !GetAtt NexusConnectorRole.Arn
      AdminEmail: !Ref AdminEmail
```

### 5. Host the Template Publicly

Upload `infra/nexus-customer-onboarding.yaml` to a PUBLIC S3 bucket so CloudFormation quick-create links work. Use bucket: `nexus-templates` (create if needed) with public read access, or use an existing public bucket.

The template URL used in the setup page must be publicly accessible (CloudFormation requires this for quick-create links).

---

## Constraints

- ALL code is in ONE repo: `https://github.com/MobileAWS/allcode-nexus-ui`
  - Backend Lambda: `api/index.py`
  - Frontend React: `src/`
  - CloudFormation: `infra/`
- ADDITIVE ONLY: Don't rewrite existing pages or functions
- The Setup page must NOT require authentication (it's for new customers who don't have an account yet)
- Follow existing patterns in `api/index.py` (use `response()` helper, add route to ROUTES dict)
- Follow existing MUI/React patterns in `src/pages/`
- The CloudFormation template already exists at `infra/nexus-customer-onboarding.yaml` — ADD the custom resource, don't rewrite the template
- Don't commit build artifacts

## API Gateway Note

After deploying the Lambda, the route `POST /api/setup/register` must be added to API Gateway (api-id: `dtxfifv2cj`, integration: `msmcmp1`). This is a manual step done by the deployer.

## Testing

1. Visit `/setup` page without being logged in — should render
2. Fill in form, click "Launch in AWS Console" — should open correct CloudFormation URL with params
3. Deploy the stack in a test account — custom resource should call the registration endpoint
4. Check DynamoDB — org should be registered
5. Log in to Nexus with the new org credentials
