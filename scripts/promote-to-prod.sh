#!/bin/bash
# promote-to-prod.sh — Promote the CURRENTLY-DEPLOYED dev code to production.
#
# This publishes the current Lambda $LATEST as a new version and repoints the
# 'prod' alias at it. It also builds+deploys the frontend to the prod bucket.
#
# SAFETY: Runs the org-specific test suite against DEV first. If tests fail,
#         promotion is aborted and prod stays on the current stable version.
#
# Usage:
#   ./scripts/promote-to-prod.sh            # promote backend + frontend
#   ./scripts/promote-to-prod.sh backend    # backend only
#   ./scripts/promote-to-prod.sh frontend   # frontend only
set -euo pipefail

REPO="/Users/andreas/Documents/SRC/guidance-for-claude-code-with-amazon-bedrock"
UI="$REPO/nexus-ui"
PROFILE="allcode-admin"
REGION="us-east-1"
FUNCTION="allcode-nexus-ui-api"
PROD_UI_BUCKET="allcode-nexus-ui-nexusuibucket-gwioxou1paii"
PROD_CF_DIST="E2PX4JCVG447YO"

WHAT="${1:-all}"

echo "==> [PROMOTE] Running org-specific tests against DEV first..."
if ! "$REPO/scripts/test-org-specific.sh" dev 2>&1 | tail -3; then
  echo "❌ Tests FAILED against dev. Aborting promotion. Prod is unchanged."
  exit 1
fi

promote_backend() {
  # Capture the version prod is on now, so we can roll back if needed.
  local prev_version
  prev_version=$(aws lambda get-alias --function-name "$FUNCTION" --name prod \
    --profile "$PROFILE" --region "$REGION" --query "FunctionVersion" --output text)
  echo "==> [PROMOTE] Current prod version: $prev_version (rollback target)"

  echo "==> [PROMOTE] Publishing \$LATEST as a new immutable version..."
  local new_version
  new_version=$(aws lambda publish-version --function-name "$FUNCTION" \
    --description "Promoted to prod $(date +%Y%m%d-%H%M)" \
    --profile "$PROFILE" --region "$REGION" --query "Version" --output text)
  echo "    Published version $new_version"

  echo "==> [PROMOTE] Pointing prod alias -> version $new_version..."
  aws lambda update-alias --function-name "$FUNCTION" --name prod \
    --function-version "$new_version" --profile "$PROFILE" --region "$REGION" \
    --query "FunctionVersion" --output text >/dev/null
  echo "    Prod now on version $new_version."
  echo "    Rollback if needed: aws lambda update-alias --function-name $FUNCTION --name prod --function-version $prev_version --profile $PROFILE --region $REGION"
}

promote_frontend() {
  echo "==> [PROMOTE] Building frontend (mode=production)..."
  ( cd "$UI" && npm run build -- --mode production 2>&1 | grep -iE "error|built in" | head -5 )
  echo "==> [PROMOTE] Syncing to prod S3 bucket..."
  aws s3 sync "$UI/dist/" "s3://$PROD_UI_BUCKET/" --delete --profile "$PROFILE" --region "$REGION" >/dev/null
  aws cloudfront create-invalidation --distribution-id "$PROD_CF_DIST" --paths "/*" \
    --profile "$PROFILE" --region "$REGION" --query "Invalidation.Id" --output text
}

case "$WHAT" in
  backend)  promote_backend ;;
  frontend) promote_frontend ;;
  all)      promote_backend; promote_frontend ;;
  *) echo "Usage: $0 [all|backend|frontend]"; exit 1 ;;
esac

echo ""
echo "==> [PROMOTE] Smoke test against PROD..."
USERS=$(curl -s "https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com/api/users" -H "x-org-id: allcode" | python3.12 -c "import json,sys; print(len(json.load(sys.stdin).get('users',[])))" 2>/dev/null || echo "0")
if [ "$USERS" -gt 0 ]; then
  echo "    ✅ Prod healthy ($USERS users)"
else
  echo "    ⚠️  Prod smoke test returned no users — investigate immediately!"
fi
echo ""
echo "Promoted to PROD. Verify at https://nexus.allcode.com"
