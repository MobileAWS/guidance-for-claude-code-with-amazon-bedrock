#!/bin/bash
# deploy-dev.sh — Deploy code to the DEV environment ONLY.
#
# Backend: updates Lambda $LATEST (the 'dev' alias points here).
#          Prod alias stays frozen, so prod is NOT affected.
# Frontend: builds with .env.development and syncs to the dev S3 bucket + CloudFront.
#
# Usage:
#   ./scripts/deploy-dev.sh            # deploy both backend + frontend to dev
#   ./scripts/deploy-dev.sh backend    # backend only
#   ./scripts/deploy-dev.sh frontend   # frontend only
set -euo pipefail

REPO="/Users/andreas/Documents/SRC/guidance-for-claude-code-with-amazon-bedrock"
UI="$REPO/nexus-ui"
PROFILE="allcode-admin"          # prod account (916587687563) — Lambda lives here
DEV_UI_PROFILE="nexus-dev"       # dev account (839765241245) — dev UI S3 bucket lives here
REGION="us-east-1"
FUNCTION="allcode-nexus-ui-api"
DEV_API="https://5ws93rfch3.execute-api.us-east-1.amazonaws.com"
DEV_UI_BUCKET="allcode-nexus-ui-839765241245"
DEV_CF_DIST="E3DGBF0DUD9B57"

WHAT="${1:-all}"

deploy_backend() {
  echo "==> [DEV] Validating Lambda syntax..."
  python3.12 -c "compile(open('$UI/api/index.py').read(), 'index.py', 'exec')"

  echo "==> [DEV] Deploying code to \$LATEST (dev alias)..."
  ( cd "$UI" && zip -j /tmp/lambda-dev.zip api/index.py >/dev/null )
  aws lambda update-function-code --function-name "$FUNCTION" \
    --zip-file fileb:///tmp/lambda-dev.zip --profile "$PROFILE" --region "$REGION" \
    --query "Version" --output text >/dev/null
  aws lambda wait function-updated --function-name "$FUNCTION" --profile "$PROFILE" --region "$REGION"
  echo "    Dev backend now running latest code. Prod alias unchanged."
}

deploy_frontend() {
  echo "==> [DEV] Building frontend (mode=development)..."
  ( cd "$UI" && npm run build -- --mode development 2>&1 | grep -iE "error|built in" | head -5 )

  echo "==> [DEV] Syncing to dev S3 bucket..."
  aws s3 sync "$UI/dist/" "s3://$DEV_UI_BUCKET/" --delete --profile "$DEV_UI_PROFILE" --region "$REGION" >/dev/null
  aws cloudfront create-invalidation --distribution-id "$DEV_CF_DIST" --paths "/*" \
    --profile "$DEV_UI_PROFILE" --region "$REGION" --query "Invalidation.Id" --output text
}

case "$WHAT" in
  backend)  deploy_backend ;;
  frontend) deploy_frontend ;;
  all)      deploy_backend; deploy_frontend ;;
  *) echo "Usage: $0 [all|backend|frontend]"; exit 1 ;;
esac

echo ""
echo "==> [DEV] Running smoke test against dev API..."
USERS=$(curl -s "$DEV_API/api/users" -H "x-org-id: allcode" | python3.12 -c "import json,sys; print(len(json.load(sys.stdin).get('users',[])))" 2>/dev/null || echo "0")
if [ "$USERS" -gt 0 ]; then
  echo "    ✅ Dev API healthy ($USERS users)"
else
  echo "    ⚠️  Dev API returned no users — check for errors before promoting!"
fi

echo ""
echo "Deployed to DEV. Verify at https://dev-nexus.allcode.com"
echo "When confident, promote to prod:  ./scripts/promote-to-prod.sh"
