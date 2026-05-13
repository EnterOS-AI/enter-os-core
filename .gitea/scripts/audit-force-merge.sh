#!/usr/bin/env bash
# audit-force-merge — detect a §SOP-6 force-merge after PR close, emit
# `incident.force_merge` to stdout as structured JSON.
#
# Vector's docker_logs source picks up runner stdout; the JSON gets
# shipped to Loki on molecule-canonical-obs, indexable by event_type.
# Query example:
#
#   {host="operator"} |= "event_type" |= "incident.force_merge" | json
#
# A force-merge is detected when a PR closed-with-merged=true had at
# least one of the repo's required-status-check contexts in a state
# other than "success" at the merge commit's SHA. That's exactly what
# the Gitea force_merge:true API call lets through, so it's a faithful
# detector of the override path.
#
# Triggers on `pull_request_target: closed` (loaded from base branch
# per §SOP-6 security model). No-op when merged=false.
#
# Required env (set by the workflow):
#   GITEA_TOKEN, GITEA_HOST, REPO, PR_NUMBER, REQUIRED_CHECKS
#
# REQUIRED_CHECKS is a newline-separated list of status-check context
# names that branch protection requires. Declared in the workflow YAML
# rather than fetched from /branch_protections (which needs admin
# scope — sop-tier-bot has read-only). Trade dynamism for simplicity:
# when the required-check set changes, update both branch protection
# AND this env. Keeping them in sync is less complexity than granting
# the audit bot admin perms on every repo.

set -euo pipefail

: "${GITEA_TOKEN:?required}"
: "${GITEA_HOST:?required}"
: "${REPO:?required}"
: "${PR_NUMBER:?required}"
: "${REQUIRED_CHECKS:?required (newline-separated context names)}"

OWNER="${REPO%%/*}"
NAME="${REPO##*/}"
API="https://${GITEA_HOST}/api/v1"
AUTH="Authorization: token ${GITEA_TOKEN}"

# 1. Fetch the PR. If not merged, no-op.
PR=$(curl -sS -H "$AUTH" "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}")
MERGED=$(echo "$PR" | jq -r '.merged // false')
if [ "$MERGED" != "true" ]; then
  echo "::notice::PR #${PR_NUMBER} closed without merge — no audit emission."
  exit 0
fi

# NOTE: no || true — with set -euo pipefail, jq parse failures (e.g. field
# missing from API response) propagate as hard errors. Use jq's // operator
# for graceful defaults instead of bash || true guards. This was re-added by
# 8c343e3a ("fix(gitea): add || true guards to jq pipelines") — reverted
# here because the guards mask silent failures that hide malformed API responses.
MERGE_SHA=$(echo "$PR" | jq -r '.merge_commit_sha // empty')
MERGED_BY=$(echo "$PR" | jq -r '.merged_by.login // "unknown"')
TITLE=$(echo "$PR" | jq -r '.title // ""')
BASE_BRANCH=$(echo "$PR" | jq -r '.base.ref // "main"')
HEAD_SHA=$(echo "$PR" | jq -r '.head.sha // empty')

if [ -z "$MERGE_SHA" ]; then
  echo "::warning::PR #${PR_NUMBER} merged=true but no merge_commit_sha — cannot evaluate force-merge."
  exit 0
fi

# 2. Required status checks declared in the workflow env.
REQUIRED="$REQUIRED_CHECKS"
if [ -z "${REQUIRED//[[:space:]]/}" ]; then
  echo "::notice::REQUIRED_CHECKS empty — force-merge not applicable."
  exit 0
fi

# 3. Status-check state at the PR HEAD (where checks ran). The merge
#    commit doesn't get its own checks; we evaluate the PR's last
#    commit, which is what branch protection compared against.
STATUS=$(curl -sS -H "$AUTH" \
  "${API}/repos/${OWNER}/${NAME}/commits/${HEAD_SHA}/status")
declare -A CHECK_STATE
while IFS=$'\t' read -r ctx state; do
  [ -n "$ctx" ] && CHECK_STATE[$ctx]="$state"
done < <(echo "$STATUS" | jq -r '.statuses // [] | .[] | "\(.context)\t\(.status)"')

# 4. For each required check, was it green at merge? YAML block scalars
#    (`|`) leave a trailing newline; skip blank/whitespace-only lines.
FAILED_CHECKS=()
while IFS= read -r req; do
  trimmed="${req#"${req%%[![:space:]]*}"}"   # ltrim
  trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"  # rtrim
  [ -z "$trimmed" ] && continue
  state="${CHECK_STATE[$trimmed]:-missing}"
  if [ "$state" != "success" ]; then
    FAILED_CHECKS+=("${trimmed}=${state}")
  fi
done <<< "$REQUIRED"

if [ "${#FAILED_CHECKS[@]}" -eq 0 ]; then
  echo "::notice::PR #${PR_NUMBER} merged with all required checks green — not a force-merge."
  exit 0
fi

# 5. Emit structured audit event.
NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# jq -R (raw input) converts each line to a JSON string; jq -s wraps into array.
# If FAILED_CHECKS is unexpectedly empty (shouldn't happen — we exit above),
# this produces []. No || true needed.
FAILED_JSON=$(printf '%s\n' "${FAILED_CHECKS[@]}" | jq -R . | jq -s .)

# Print as a single-line JSON so Vector's parse_json transform can pick
# it up cleanly from docker_logs.
jq -nc \
  --arg event_type "incident.force_merge" \
  --arg ts "$NOW" \
  --arg repo "$REPO" \
  --argjson pr "$PR_NUMBER" \
  --arg title "$TITLE" \
  --arg base "$BASE_BRANCH" \
  --arg merged_by "$MERGED_BY" \
  --arg merge_sha "$MERGE_SHA" \
  --argjson failed_checks "$FAILED_JSON" \
  '{event_type: $event_type, ts: $ts, repo: $repo, pr: $pr, title: $title,
    base_branch: $base, merged_by: $merged_by, merge_sha: $merge_sha,
    failed_checks: $failed_checks}'

echo "::warning::FORCE-MERGE detected on PR #${PR_NUMBER} by ${MERGED_BY}: ${#FAILED_CHECKS[@]} required check(s) not green at merge time."
