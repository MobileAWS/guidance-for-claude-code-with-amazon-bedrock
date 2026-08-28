# Dev → Prod Deployment Workflow

**Rule: all changes go to DEV first, get verified, then get promoted to PROD.**
No more pushing straight to production.

## Architecture

Both environments share the **same data** (DynamoDB, Cognito) and the **same Lambda
function code base**, but are isolated by Lambda **aliases** + separate API Gateways:

| | Frontend | API Gateway | Lambda alias | Version |
|---|---|---|---|---|
| **DEV** | dev-nexus.allcode.com | `5ws93rfch3` | `dev` | `$LATEST` (newest code) |
| **PROD** | nexus.allcode.com | `dtxfifv2cj` | `prod` | frozen (e.g. v5) |

- Deploying code updates `$LATEST` → **dev updates instantly, prod is untouched**.
- Promotion publishes a new immutable version and repoints the `prod` alias at it.
- Frontend isolation: `.env.development` points the dev UI at the dev API; `.env.production`
  points the prod UI at the prod API. Both builds are verified to contain only their own API URL.

## Everyday workflow

```bash
# 1. Make your code changes (Lambda api/index.py and/or frontend src/)

# 2. Deploy to DEV
./scripts/deploy-dev.sh              # backend + frontend
./scripts/deploy-dev.sh backend      # backend only
./scripts/deploy-dev.sh frontend     # frontend only

# 3. Verify at https://dev-nexus.allcode.com
#    - click through the actual feature you changed
#    - the deploy script also runs a smoke test automatically

# 4. Promote to PROD (runs the 28-test org suite against dev first; aborts if any fail)
./scripts/promote-to-prod.sh         # backend + frontend
./scripts/promote-to-prod.sh backend # backend only
./scripts/promote-to-prod.sh frontend# frontend only
```

## Rollback (if prod breaks after a promotion)

`promote-to-prod.sh` prints the previous version and the exact rollback command. To roll back manually:

```bash
# List versions to find the last-good one
aws lambda list-versions-by-function --function-name allcode-nexus-ui-api \
  --profile allcode-admin --region us-east-1 --query "Versions[].Version" --output text

# Point prod alias back at the known-good version (e.g. 5)
aws lambda update-alias --function-name allcode-nexus-ui-api --name prod \
  --function-version 5 --profile allcode-admin --region us-east-1
```

Rollback is instant — no rebuild needed, because every promoted version is kept immutably.

## AWS accounts / profiles

- **Lambda + prod S3 UI + both API Gateways**: prod account `916587687563` → profile `allcode-admin`
- **Dev S3 UI bucket** (`allcode-nexus-ui-839765241245`): dev account `839765241245` → profile `nexus-dev`

## Notes

- Data is shared. A bad **write** in dev code can still affect prod data. Read-path changes are
  fully safe to test in dev. For risky write-path changes, test against a throwaway test user.
- `git` still matters — the factory pulls from git and reverts uncommitted changes. Commit after
  promoting.
