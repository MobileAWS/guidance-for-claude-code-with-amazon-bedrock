#!/usr/bin/env bash
#
# publish-nexus-release.sh — AllCode-side release publisher for the in-account Nexus
# self-update system (Phase 2).
#
# Publishes Lambda + UI artifacts and writes a CHANNEL manifest (canary/early/stable) to the
# public artifact bucket. Customer updater Lambdas PULL this manifest, verify sha256, and
# apply low-risk updates (Lambda code + UI) locally. AllCode never touches customer accounts.
#
# SAFETY: a release should go canary -> early -> stable over days. Never publish straight to
# stable. Smoke-test in the canary (AllCode) account first.
#
# Usage:
#   ./publish-nexus-release.sh <channel> <version> [--pause] [--unpause]
#     channel : canary | early | stable
#     version : e.g. 2026.09.01-1  (must be strictly-newer than what's live on the channel)
#     --pause : write paused:true to the channel (kill switch — halts all pulls on that channel)
#
# Env / config:
#   ARTIFACT_BUCKET   (default: nexus-public-artifacts-916587687563)
#   AWS_PROFILE       (default: allcode-admin)
#   AWS_REGION        (default: us-east-1)
#
set -euo pipefail

CHANNEL="${1:-}"
VERSION="${2:-}"
FLAG="${3:-}"

ARTIFACT_BUCKET="${ARTIFACT_BUCKET:-nexus-public-artifacts-916587687563}"
AWS_PROFILE="${AWS_PROFILE:-allcode-admin}"
AWS_REGION="${AWS_REGION:-us-east-1}"
PREFIX="nexus-inaccount"

die() { echo "ERROR: $*" >&2; exit 1; }

case "$CHANNEL" in
  canary|early|stable) ;;
  *) die "channel must be canary|early|stable (got '$CHANNEL')" ;;
esac

# Kill switch: pause/unpause a channel without a new release.
if [[ "$FLAG" == "--pause" || "$FLAG" == "--unpause" ]]; then
  PAUSED="true"; [[ "$FLAG" == "--unpause" ]] && PAUSED="false"
  MANIFEST_KEY="${PREFIX}/channel/${CHANNEL}.json"
  cur=$(aws s3 cp "s3://${ARTIFACT_BUCKET}/${MANIFEST_KEY}" - --profile "$AWS_PROFILE" --region "$AWS_REGION" 2>/dev/null || echo '{}')
  echo "$cur" | python3 -c "
import json,sys
m=json.load(sys.stdin) if sys.stdin.read else {}
" 2>/dev/null || cur='{}'
  echo "$cur" | python3 -c "
import json,sys
m=json.load(sys.stdin)
m['paused']=${PAUSED^}
print(json.dumps(m,indent=2))
" > /tmp/nexus-manifest.json
  aws s3 cp /tmp/nexus-manifest.json "s3://${ARTIFACT_BUCKET}/${MANIFEST_KEY}" \
    --profile "$AWS_PROFILE" --region "$AWS_REGION" --content-type application/json --cache-control "no-cache, no-store"
  echo "channel '${CHANNEL}' paused=${PAUSED}"
  exit 0
fi

[[ -n "$VERSION" ]] || die "version required (e.g. 2026.09.01-1)"

# Repo root (this script lives in scripts/ under the guidance repo, nexus-ui alongside).
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
UI_DIR="${ROOT}/nexus-ui"
FN_DIR="${UI_DIR}/functions"
API_FILE="${UI_DIR}/api/index.py"

sha256() { shasum -a 256 "$1" | awk '{print $1}'; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "==> Packaging + uploading artifacts for ${CHANNEL} ${VERSION}"

# --- Lambda artifacts (api + 3 supporting) ---
declare -A LAMBDAS=(
  [api]="${API_FILE}"
  [device-auth]="${FN_DIR}/device-auth/index.py"
  [package-gen]="${FN_DIR}/package-gen/index.py"
  [post-confirm]="${FN_DIR}/post-confirm/index.py"
)

declare -A KEYS
declare -A HASHES
for name in "${!LAMBDAS[@]}"; do
  src="${LAMBDAS[$name]}"
  [[ -f "$src" ]] || die "missing source: $src"
  z="${WORK}/${name}.zip"
  ( cd "$(dirname "$src")" && zip -q "$z" "$(basename "$src")" )
  key="${PREFIX}/${name}/${VERSION}.zip"
  aws s3 cp "$z" "s3://${ARTIFACT_BUCKET}/${key}" --profile "$AWS_PROFILE" --region "$AWS_REGION" --quiet
  # Manifest stores the key RELATIVE to the artifact base (which already ends in ${PREFIX}),
  # so the updater fetches ${ARTIFACT_BASE}/${relkey} without doubling the prefix.
  KEYS[$name]="${name}/${VERSION}.zip"
  HASHES[$name]="$(sha256 "$z")"
  echo "    ${name}: ${key}"
done

# --- UI artifact (built SPA tarball) ---
echo "==> Building UI"
( cd "$UI_DIR" && VITE_API_URL="" npx vite build >/dev/null 2>&1 )
UI_TAR="${WORK}/ui-${VERSION}.tar.gz"
( cd "${UI_DIR}/dist" && tar -czf "$UI_TAR" . )
UI_KEY="${PREFIX}/ui/${VERSION}.tar.gz"
aws s3 cp "$UI_TAR" "s3://${ARTIFACT_BUCKET}/${UI_KEY}" --profile "$AWS_PROFILE" --region "$AWS_REGION" --quiet
UI_HASH="$(sha256 "$UI_TAR")"
UI_RELKEY="ui/${VERSION}.tar.gz"
echo "    ui: ${UI_KEY}"

# --- Write the channel manifest ---
cat > "${WORK}/manifest.json" <<JSON
{
  "channel": "${CHANNEL}",
  "version": "${VERSION}",
  "paused": false,
  "published_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "artifacts": {
    "api":          { "key": "${KEYS[api]}",          "sha256": "${HASHES[api]}" },
    "device-auth":  { "key": "${KEYS[device-auth]}",  "sha256": "${HASHES[device-auth]}" },
    "package-gen":  { "key": "${KEYS[package-gen]}",  "sha256": "${HASHES[package-gen]}" },
    "post-confirm": { "key": "${KEYS[post-confirm]}", "sha256": "${HASHES[post-confirm]}" },
    "ui":           { "key": "${UI_RELKEY}",         "sha256": "${UI_HASH}" }
  }
}
JSON

aws s3 cp "${WORK}/manifest.json" "s3://${ARTIFACT_BUCKET}/${PREFIX}/channel/${CHANNEL}.json" \
  --profile "$AWS_PROFILE" --region "$AWS_REGION" --content-type application/json --cache-control "no-cache, no-store"

# --- Sign the manifest with KMS (asymmetric) so customers can verify authenticity ---
# The updater embeds the PUBLIC key and rejects a manifest whose signature doesn't verify —
# so a compromised artifact bucket cannot serve a malicious manifest.
SIGNING_KEY="${SIGNING_KEY:-alias/nexus-manifest-signing}"
MSG_B64=$(base64 < "${WORK}/manifest.json" | tr -d '\n')
SIG=$(aws kms sign --key-id "$SIGNING_KEY" \
  --message "fileb://${WORK}/manifest.json" \
  --message-type RAW --signing-algorithm ECDSA_SHA_256 \
  --profile "$AWS_PROFILE" --region "$AWS_REGION" \
  --query "Signature" --output text 2>/dev/null)
if [ -n "$SIG" ]; then
  echo "$SIG" > "${WORK}/manifest.sig"
  aws s3 cp "${WORK}/manifest.sig" "s3://${ARTIFACT_BUCKET}/${PREFIX}/channel/${CHANNEL}.json.sig" \
    --profile "$AWS_PROFILE" --region "$AWS_REGION" --content-type text/plain --cache-control "no-cache, no-store"
  echo "    signed manifest -> ${CHANNEL}.json.sig"
else
  echo "    WARNING: manifest signing failed (SIGNING_KEY=$SIGNING_KEY) — published unsigned"
fi

echo "==> Published ${VERSION} to '${CHANNEL}'"
echo "    manifest: s3://${ARTIFACT_BUCKET}/${PREFIX}/channel/${CHANNEL}.json"
echo ""
echo "Promotion path: canary -> early -> stable. Smoke-test canary before promoting."
