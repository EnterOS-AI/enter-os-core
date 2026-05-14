#!/usr/bin/env bash
# Re-run review-check.sh for a slash-command refire and post the protected
# pull_request status context to the PR head SHA.

set -euo pipefail

: "${GITEA_TOKEN:?GITEA_TOKEN required}"
: "${GITEA_HOST:?GITEA_HOST required}"
: "${REPO:?REPO required}"
: "${PR_NUMBER:?PR_NUMBER required}"
: "${TEAM:?TEAM required}"

OWNER="${REPO%%/*}"
NAME="${REPO##*/}"
API="https://${GITEA_HOST}/api/v1"
CONTEXT="${TEAM}-review / approved (pull_request)"
TARGET_URL="https://${GITEA_HOST}/${OWNER}/${NAME}/pulls/${PR_NUMBER}"

authfile=$(mktemp)
prfile=$(mktemp)
postfile=$(mktemp)
# shellcheck disable=SC2329 # invoked by EXIT trap
cleanup() {
  rm -f "$authfile" "$prfile" "$postfile"
}
trap cleanup EXIT

chmod 600 "$authfile"
printf 'header = "Authorization: token %s"\n' "$GITEA_TOKEN" > "$authfile"

code=$(curl -sS -o "$prfile" -w '%{http_code}' -K "$authfile" \
  "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}")
if [ "$code" != "200" ]; then
  echo "::error::GET /pulls/${PR_NUMBER} returned HTTP ${code}"
  head -c 200 "$prfile" >&2 || true
  exit 1
fi

head_sha=$(jq -r '.head.sha // ""' "$prfile")
state=$(jq -r '.state // ""' "$prfile")
if [ -z "$head_sha" ] || [ "$head_sha" = "null" ]; then
  echo "::error::Could not resolve PR head SHA for PR ${PR_NUMBER}"
  exit 1
fi
if [ "$state" != "open" ]; then
  echo "::notice::PR ${PR_NUMBER} is ${state}; ${TEAM}-review refire is a no-op"
  exit 0
fi

set +e
bash .gitea/scripts/review-check.sh
rc=$?
set -e

if [ "$rc" -eq 0 ]; then
  status_state="success"
  description="Refired via /${TEAM}-recheck by ${COMMENT_AUTHOR:-unknown}"
else
  status_state="failure"
  description="Refired via /${TEAM}-recheck; ${TEAM}-review failed"
fi

body=$(jq -nc \
  --arg state "$status_state" \
  --arg context "$CONTEXT" \
  --arg description "$description" \
  --arg target_url "$TARGET_URL" \
  '{state:$state, context:$context, description:$description, target_url:$target_url}')

code=$(curl -sS -o "$postfile" -w '%{http_code}' -X POST \
  -K "$authfile" -H "Content-Type: application/json" \
  -d "$body" \
  "${API}/repos/${OWNER}/${NAME}/statuses/${head_sha}")
if [ "$code" != "200" ] && [ "$code" != "201" ]; then
  echo "::error::POST /statuses/${head_sha} returned HTTP ${code}"
  head -c 200 "$postfile" >&2 || true
  exit 1
fi

echo "::notice::posted ${status_state} for context=\"${CONTEXT}\" on sha=${head_sha}"
exit "$rc"
