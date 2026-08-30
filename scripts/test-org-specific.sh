#!/bin/bash
# AllCode Nexus - Pre-deployment Validation Tests
# Run this BEFORE promoting dev → prod
# Usage: ./scripts/test-org-specific.sh [dev|prod]
#
# Tests that org-specific configs are correct and don't contaminate each other.

set -e

ENV="${1:-dev}"
if [ "$ENV" = "prod" ]; then
    API_URL="https://dtxfifv2cj.execute-api.us-east-1.amazonaws.com"
    S3_BUCKET="claude-code-auth-distribution-916587687563"
    AWS_PROFILE="allcode-admin"
else
    # Dev API Gateway -> Lambda 'dev' alias ($LATEST). Shares prod data.
    API_URL="https://5ws93rfch3.execute-api.us-east-1.amazonaws.com"
    S3_BUCKET="claude-code-auth-distribution-916587687563"
    AWS_PROFILE="allcode-admin"
fi

PASS=0
FAIL=0
ERRORS=""

pass() { PASS=$((PASS+1)); echo "  ✅ $1"; }
fail() { FAIL=$((FAIL+1)); ERRORS="$ERRORS\n  ❌ $1"; echo "  ❌ $1"; }

echo "======================================"
echo "Nexus Org-Specific Validation Tests"
echo "Environment: $ENV"
echo "API: $API_URL"
echo "======================================"
echo ""

# ─── 1. Auth Config Tests ─────────────────────────────────────────────────────
echo "▶ Auth Config (GET /api/orgs/auth-config)"

# AllCode
RESULT=$(curl -s "$API_URL/api/orgs/auth-config?org=allcode")
if echo "$RESULT" | grep -q "us-east-1_3mbtSSlmt" 2>/dev/null; then
    pass "AllCode auth config → us-east-1_3mbtSSlmt"
else
    fail "AllCode auth config wrong: $RESULT"
fi

# LetsPlay
RESULT=$(curl -s "$API_URL/api/orgs/auth-config?org=lets-play")
if echo "$RESULT" | grep -q "us-east-2_oVIbFbxum" 2>/dev/null; then
    pass "LetsPlay auth config → us-east-2_oVIbFbxum"
else
    fail "LetsPlay auth config wrong: $RESULT"
fi

# Skematic
RESULT=$(curl -s "$API_URL/api/orgs/auth-config?org=skematic")
if echo "$RESULT" | grep -q "skematic" 2>/dev/null && ! echo "$RESULT" | grep -q "not found\|not fully" 2>/dev/null; then
    pass "Skematic auth config returns valid data"
else
    fail "Skematic auth config wrong: $RESULT"
fi

echo ""

# ─── 2. MCP Servers Org Isolation ─────────────────────────────────────────────
echo "▶ MCP Servers Org Isolation (GET /api/mcp-servers)"

# AllCode MCPs
RESULT=$(curl -s "$API_URL/api/mcp-servers" -H "x-org-id: allcode")
if echo "$RESULT" | grep -q "HubSpot" 2>/dev/null; then
    pass "AllCode has HubSpot"
else
    fail "AllCode missing HubSpot"
fi
if echo "$RESULT" | grep -q "ActiveCampaign" 2>/dev/null; then
    fail "AllCode has ActiveCampaign (should be LetsPlay only)"
else
    pass "AllCode does NOT have ActiveCampaign"
fi

# LetsPlay MCPs
RESULT=$(curl -s "$API_URL/api/mcp-servers" -H "x-org-id: lets-play")
if echo "$RESULT" | grep -q "ActiveCampaign" 2>/dev/null; then
    pass "LetsPlay has ActiveCampaign"
else
    fail "LetsPlay missing ActiveCampaign"
fi
if echo "$RESULT" | grep -q "HubSpot" 2>/dev/null; then
    fail "LetsPlay has HubSpot (should be AllCode only)"
else
    pass "LetsPlay does NOT have HubSpot"
fi
if echo "$RESULT" | grep -q "Zapier" 2>/dev/null; then
    fail "LetsPlay has Zapier (should be removed)"
else
    pass "LetsPlay does NOT have Zapier"
fi

echo ""

# ─── 3. S3 MCP Config Files ──────────────────────────────────────────────────
echo "▶ S3 MCP Config Files"

# AllCode MCP config
RESULT=$(aws s3 cp s3://$S3_BUCKET/cowork/org-allcode-mcps.json - --profile $AWS_PROFILE --region us-east-1 2>/dev/null)
if echo "$RESULT" | grep -q "hubspot" 2>/dev/null; then
    pass "org-allcode-mcps.json has hubspot"
else
    fail "org-allcode-mcps.json missing hubspot"
fi
if echo "$RESULT" | grep -q "zapier" 2>/dev/null; then
    fail "org-allcode-mcps.json has zapier (should be removed)"
else
    pass "org-allcode-mcps.json does NOT have zapier"
fi

# LetsPlay MCP config
RESULT=$(aws s3 cp s3://$S3_BUCKET/cowork/org-lets-play-mcps.json - --profile $AWS_PROFILE --region us-east-1 2>/dev/null)
if echo "$RESULT" | grep -q "activecampaign" 2>/dev/null; then
    pass "org-lets-play-mcps.json has activecampaign"
else
    fail "org-lets-play-mcps.json missing activecampaign"
fi
if echo "$RESULT" | grep -q "zapier" 2>/dev/null; then
    fail "org-lets-play-mcps.json has zapier (should be removed)"
else
    pass "org-lets-play-mcps.json does NOT have zapier"
fi

echo ""

# ─── 4. Mobileconfig Org Isolation ───────────────────────────────────────────
echo "▶ Mobileconfig Org Isolation"

# AllCode mobileconfig
RESULT=$(aws s3 cp s3://$S3_BUCKET/cowork/cowork-3p.mobileconfig - --profile $AWS_PROFILE --region us-east-1 2>/dev/null)
if echo "$RESULT" | grep -q "allcode-dev-us-east-1" 2>/dev/null; then
    pass "cowork-3p.mobileconfig → allcode-dev-us-east-1"
else
    fail "cowork-3p.mobileconfig has WRONG profile: $(echo "$RESULT" | grep -o 'inferenceBedrockProfile.*' | head -1)"
fi
if echo "$RESULT" | grep -q "lets-play" 2>/dev/null; then
    fail "cowork-3p.mobileconfig CONTAMINATED with lets-play!"
else
    pass "cowork-3p.mobileconfig NOT contaminated with lets-play"
fi
if echo "$RESULT" | grep -q "(AllCode)" 2>/dev/null; then
    pass "cowork-3p.mobileconfig has org name '(AllCode)'"
else
    fail "cowork-3p.mobileconfig missing org name in display"
fi

# LetsPlay mobileconfig
RESULT=$(aws s3 cp s3://$S3_BUCKET/cowork/org-lets-play-cowork-3p.mobileconfig - --profile $AWS_PROFILE --region us-east-1 2>/dev/null)
if echo "$RESULT" | grep -q "lets-play-us-east-2" 2>/dev/null; then
    pass "org-lets-play-cowork-3p.mobileconfig → lets-play-us-east-2"
else
    fail "LetsPlay mobileconfig has WRONG profile"
fi
if echo "$RESULT" | grep -q "allcode-dev-us-east-1" 2>/dev/null; then
    fail "LetsPlay mobileconfig CONTAMINATED with allcode!"
else
    pass "LetsPlay mobileconfig NOT contaminated with allcode"
fi
if echo "$RESULT" | grep -q "(Lets Play)" 2>/dev/null; then
    pass "LetsPlay mobileconfig has org name '(Lets Play)'"
else
    fail "LetsPlay mobileconfig missing org name in display"
fi

# Skematic mobileconfig
RESULT=$(aws s3 cp s3://$S3_BUCKET/cowork/org-skematic-cowork-3p.mobileconfig - --profile $AWS_PROFILE --region us-east-1 2>/dev/null)
if echo "$RESULT" | grep -q "skematic-us-west-2" 2>/dev/null; then
    pass "org-skematic-cowork-3p.mobileconfig → skematic-us-west-2"
else
    fail "Skematic mobileconfig has WRONG profile"
fi

echo ""

# ─── 5. Cowork Config Org Isolation ──────────────────────────────────────────
echo "▶ Cowork Config Org Isolation"

# AllCode cowork config
RESULT=$(aws s3 cp s3://$S3_BUCKET/cowork/cowork-3p-config.json - --profile $AWS_PROFILE --region us-east-1 2>/dev/null)
if echo "$RESULT" | grep -q "allcode-dev-us-east-1" 2>/dev/null; then
    pass "cowork-3p-config.json → allcode-dev-us-east-1"
else
    fail "cowork-3p-config.json has wrong profile"
fi

# LetsPlay cowork config  
RESULT=$(aws s3 cp s3://$S3_BUCKET/cowork/org-lets-play-cowork-3p-config.json - --profile $AWS_PROFILE --region us-east-1 2>/dev/null)
if echo "$RESULT" | grep -q "lets-play-us-east-2" 2>/dev/null; then
    pass "org-lets-play-cowork-3p-config.json → lets-play-us-east-2"
else
    fail "LetsPlay cowork config has wrong profile"
fi

echo ""

# ─── 6. Installer Package Validation ─────────────────────────────────────────
echo "▶ Installer Package Validation"

TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

# AllCode installer
aws s3 cp s3://$S3_BUCKET/packages/mac/latest.zip $TMPDIR/allcode.zip --profile $AWS_PROFILE --region us-east-1 --quiet 2>/dev/null
if [ -f $TMPDIR/allcode.zip ]; then
    unzip -qo $TMPDIR/allcode.zip -d $TMPDIR/allcode
    PROFILE=$(python3 -c "import json; print(list(json.load(open('$TMPDIR/allcode/allcode-nexus/config.json')).keys())[0])" 2>/dev/null)
    if [ "$PROFILE" = "allcode-dev-us-east-1" ]; then
        pass "AllCode installer config.json → allcode-dev-us-east-1"
    else
        fail "AllCode installer config.json → $PROFILE (wrong!)"
    fi
    if grep -q 'date +%s' $TMPDIR/allcode/allcode-nexus/install.sh 2>/dev/null; then
        pass "AllCode installer has cache-busting"
    else
        fail "AllCode installer missing cache-busting"
    fi
    if grep -q 'TMPDIR.*id -u' $TMPDIR/allcode/allcode-nexus/install.sh 2>/dev/null; then
        pass "AllCode installer has user-specific temp path"
    else
        fail "AllCode installer missing user-specific temp path"
    fi
    if grep -q 'npm config set prefix' $TMPDIR/allcode/allcode-nexus/install.sh 2>/dev/null; then
        pass "AllCode installer has npm user-local prefix"
    else
        fail "AllCode installer missing npm user-local prefix"
    fi
else
    fail "Could not download AllCode installer"
fi

# LetsPlay installer
aws s3 cp s3://$S3_BUCKET/packages/mac/org-lets-play-latest.zip $TMPDIR/lps.zip --profile $AWS_PROFILE --region us-east-1 --quiet 2>/dev/null
if [ -f $TMPDIR/lps.zip ]; then
    unzip -qo $TMPDIR/lps.zip -d $TMPDIR/lps
    PROFILE=$(python3 -c "import json; print(list(json.load(open('$TMPDIR/lps/allcode-nexus/config.json')).keys())[0])" 2>/dev/null)
    if [ "$PROFILE" = "lets-play-us-east-2" ]; then
        pass "LetsPlay installer config.json → lets-play-us-east-2"
    else
        fail "LetsPlay installer config.json → $PROFILE (wrong!)"
    fi
else
    fail "Could not download LetsPlay installer"
fi

echo ""

# ─── 7. Cross-contamination Check ────────────────────────────────────────────
echo "▶ Cross-contamination Check"

# Make sure no AllCode files mention lets-play
ALLCODE_CONFIGS=$(aws s3 cp s3://$S3_BUCKET/cowork/cowork-3p.mobileconfig - --profile $AWS_PROFILE --region us-east-1 2>/dev/null)
ALLCODE_COWORK=$(aws s3 cp s3://$S3_BUCKET/cowork/cowork-3p-config.json - --profile $AWS_PROFILE --region us-east-1 2>/dev/null)
if echo "$ALLCODE_CONFIGS$ALLCODE_COWORK" | grep -q "lets-play" 2>/dev/null; then
    fail "CROSS-CONTAMINATION: AllCode configs contain 'lets-play'"
else
    pass "No cross-contamination: AllCode configs clean"
fi

# Make sure no LetsPlay files mention allcode-dev
LPS_CONFIGS=$(aws s3 cp s3://$S3_BUCKET/cowork/org-lets-play-cowork-3p.mobileconfig - --profile $AWS_PROFILE --region us-east-1 2>/dev/null)
LPS_COWORK=$(aws s3 cp s3://$S3_BUCKET/cowork/org-lets-play-cowork-3p-config.json - --profile $AWS_PROFILE --region us-east-1 2>/dev/null)
if echo "$LPS_CONFIGS$LPS_COWORK" | grep -q "allcode-dev" 2>/dev/null; then
    fail "CROSS-CONTAMINATION: LetsPlay configs contain 'allcode-dev'"
else
    pass "No cross-contamination: LetsPlay configs clean"
fi

echo ""

# ─── Summary ─────────────────────────────────────────────────────────────────
echo "======================================"
echo "Results: $PASS passed, $FAIL failed"
echo "======================================"

if [ $FAIL -gt 0 ]; then
    echo ""
    echo "FAILURES:"
    echo -e "$ERRORS"
    echo ""
    echo "⛔ DO NOT DEPLOY TO PROD"
    exit 1
else
    echo ""
    echo "✅ ALL TESTS PASSED — safe to deploy to prod"
    exit 0
fi
