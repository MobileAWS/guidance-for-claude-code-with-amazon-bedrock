#!/usr/bin/env bash
#
# setup-nexus-artifact-buckets.sh — Create + configure AllCode's per-region PUBLIC artifact
# buckets for the in-account Nexus bootstrap. Each supported AWS region needs its OWN bucket
# (nexus-public-artifacts-<region>) because a Lambda's code bucket must be same-region as the
# Lambda. Run once per region you want to support (idempotent).
#
# Usage:
#   ./setup-nexus-artifact-buckets.sh                 # sets up the default region list
#   ./setup-nexus-artifact-buckets.sh us-east-1 eu-west-1   # specific regions
#
# Env:  AWS_PROFILE (default: allcode-admin)
#
set -euo pipefail

AWS_PROFILE="${AWS_PROFILE:-allcode-admin}"
# Default supported regions (extend as GTM requires). us-east-1 first (also hosts CloudFront/ACM).
DEFAULT_REGIONS="us-east-1 us-west-2 eu-west-1 eu-central-1 ap-southeast-1 ap-southeast-2"
REGIONS="${*:-$DEFAULT_REGIONS}"

for region in $REGIONS; do
  bucket="nexus-public-artifacts-${region}"
  echo "==> ${bucket} (${region})"

  # Create (us-east-1 has no LocationConstraint; others require it)
  if [ "$region" = "us-east-1" ]; then
    aws s3api create-bucket --bucket "$bucket" --profile "$AWS_PROFILE" --region "$region" 2>/dev/null || true
  else
    aws s3api create-bucket --bucket "$bucket" --profile "$AWS_PROFILE" --region "$region" \
      --create-bucket-configuration "LocationConstraint=${region}" 2>/dev/null || true
  fi

  # Allow the public-read policy on the artifact prefix
  aws s3api put-public-access-block --bucket "$bucket" --profile "$AWS_PROFILE" --region "$region" \
    --public-access-block-configuration "BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=false,RestrictPublicBuckets=false" >/dev/null

  # Public-read scoped ONLY to the artifact prefix (product artifacts; no secrets)
  aws s3api put-bucket-policy --bucket "$bucket" --profile "$AWS_PROFILE" --region "$region" --policy "{
    \"Version\":\"2012-10-17\",
    \"Statement\":[{
      \"Sid\":\"PublicReadProductArtifacts\",
      \"Effect\":\"Allow\",
      \"Principal\":\"*\",
      \"Action\":\"s3:GetObject\",
      \"Resource\":\"arn:aws:s3:::${bucket}/nexus-inaccount/*\"
    }]
  }" >/dev/null
  echo "    ready (public-read on nexus-inaccount/*)"
done

echo ""
echo "Done. Publish artifacts to all regions with:  ./scripts/publish-nexus-release.sh stable <version> --all-regions"
