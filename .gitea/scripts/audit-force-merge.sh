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
#   GITEA_TOKEN, GITEA_HOST, REPO, PR_NUMBER
#   plus one of REQUIRED_CHECKS_JSON (preferred) or REQUIRED_CHECKS (legacy)
#
# REQUIRED_CHECKS_JSON is a JSON object keyed by branch name. Each value
# is an array of status-check context names that branch protection
# requires for that branch. The script looks up the PR's base branch and
# evaluates only the checks declared for that branch.
#
#   {"main": ["CI / all-required (pull_request)", ...],
#    "staging": ["CI / all-required (pull_request)", ...]}
#
# REQUIRED_CHECKS (legacy) is a newline-separated list used when the
# JSON variable is not set. Declared in the workflow YAML rather than
# fetched from /branch_protections (which needs admin scope — 
# has read-only). Trade dynamism for simplicity: when the required-check
# set changes, update both branch protection AND this env. Keeping them
# in sync is less complexity than granting the audit bot admin perms on
# every repo.

set -euo pipefail

: "${GITEA_TOKEN:?required}"
: "${GITEA_HOST:?required}"
: "${REPO:?required}"
: "${PR_NUMBER:?required}"
if [ -z "${REQUIRED_CHECKS_JSON:-}" ] && [ -z "${REQUIRED_CHECKS:-}" ]; then
  echo "::error::Either REQUIRED_CHECKS_JSON or REQUIRED_CHECKS must be set"
  exit 1
fi

OWNER="${REPO%%/*}"
NAME="${REPO##*/}"
API="https://${GITEA_HOST}/api/v1"
AUTH="Authorization: token ${GITEA_TOKEN}"

# 1. Fetch the PR. If not merged, no-op.
# Fail-closed: verify HTTP 200 before parsing. A 401/403/404 means the token
# is invalid or the PR is inaccessible — we must NOT silently treat that as
# "not merged" and skip the audit.
PR_TMP=$(mktemp)
PR_HTTP=$(curl -sS -o "$PR_TMP" -w '%{http_code}' -H "$AUTH" \
  "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}")
PR=$(cat "$PR_TMP")
rm -f "$PR_TMP"
if [ "$PR_HTTP" != "200" ]; then
  echo "::error::GET /pulls/${PR_NUMBER} returned HTTP ${PR_HTTP} — cannot evaluate merge state."
  exit 1
fi
# FAIL-CLOSED: a 200 response with a missing/malformed `merged` field must
# NOT be treated as "not merged" (that would silently skip the audit).
# We verify both presence AND correct type for every field we consume.
PR_SCHEMA_OK=$(echo "$PR" | jq -r '
  (.merged | type == "boolean") and
  (.merge_commit_sha | type == "string") and
  (.merged_by | type == "object") and (.merged_by.login | type == "string") and
  (.base | type == "object") and (.base.ref | type == "string") and
  (.head | type == "object") and (.head.sha | type == "string")
')
if [ "$PR_SCHEMA_OK" != "true" ]; then
  echo "::error::GET /pulls/${PR_NUMBER} returned HTTP 200 but one or more required fields are missing, null, or of wrong type — cannot evaluate force-merge."
  exit 1
fi
MERGED=$(echo "$PR" | jq -r '.merged')
if [ "$MERGED" != "true" ]; then
  echo "::notice::PR #${PR_NUMBER} closed without merge — no audit emission."
  exit 0
fi

MERGE_SHA=$(echo "$PR" | jq -r '.merge_commit_sha')
MERGED_BY=$(echo "$PR" | jq -r '.merged_by.login')
PR_AUTHOR=$(echo "$PR" | jq -r '.user.login // ""')
TITLE=$(echo "$PR" | jq -r '.title // ""')
BASE_BRANCH=$(echo "$PR" | jq -r '.base.ref')
HEAD_SHA=$(echo "$PR" | jq -r '.head.sha')

# 1b. DETECTIVE: reserved-path self-merge (author == merger). The preventive
#     reserved-path-review gate blocks the normal merge button, but a
#     determined admin/force-merge can still bypass it — that is exactly how
#     cp#673 slipped. This backstop emits `incident.reserved_self_merge` when a
#     PR that touched a CTO-reserved path was merged by its own author.
#     Reserved-path set comes from the BASE checkout (.gitea/reserved-paths.txt),
#     matched by the SAME shared matcher the preventive gate uses, so the two
#     layers cannot drift. Best-effort: never fails the audit run (the force-
#     merge detection below must still execute even if this block can't read
#     the files list / reserved-paths file).
RESERVED_PATHS_FILE="${RESERVED_PATHS_FILE:-.gitea/reserved-paths.txt}"
_AUDIT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -n "$PR_AUTHOR" ] && [ "$PR_AUTHOR" = "$MERGED_BY" ] \
   && [ -f "${_AUDIT_DIR}/reserved-path-match.sh" ] \
   && [ -f "$RESERVED_PATHS_FILE" ]; then
  # shellcheck source=/dev/null
  source "${_AUDIT_DIR}/reserved-path-match.sh"
  _RP_FILES=()
  _rp_page=1
  while : ; do
    _rp_tmp=$(mktemp)
    _rp_http=$(curl -sS -o "$_rp_tmp" -w '%{http_code}' -H "$AUTH" \
      "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}/files?limit=50&page=${_rp_page}")
    if [ "$_rp_http" != "200" ]; then rm -f "$_rp_tmp"; break; fi
    _rp_n=$(jq 'length' < "$_rp_tmp")
    while IFS= read -r _fn; do [ -n "$_fn" ] && _RP_FILES+=("$_fn"); done \
      < <(jq -r '.[].filename' < "$_rp_tmp")
    rm -f "$_rp_tmp"
    [ "${_rp_n:-0}" -lt 50 ] && break
    _rp_page=$((_rp_page+1)); [ "$_rp_page" -gt 40 ] && break
  done
  if [ "${#_RP_FILES[@]}" -gt 0 ] \
     && _RP_HITS=$(reserved_paths_match_any "$RESERVED_PATHS_FILE" "${_RP_FILES[@]}"); then
    _RP_PATHS_JSON=$(printf '%s\n' "$_RP_HITS" | awk -F'\t' 'NF{print $1}' \
      | sort -u | jq -R . | jq -s .)
    _RP_NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    jq -nc \
      --arg event_type "incident.reserved_self_merge" \
      --arg ts "$_RP_NOW" \
      --arg repo "$REPO" \
      --argjson pr "$PR_NUMBER" \
      --arg title "$TITLE" \
      --arg base "$BASE_BRANCH" \
      --arg author "$PR_AUTHOR" \
      --arg merged_by "$MERGED_BY" \
      --arg merge_sha "$MERGE_SHA" \
      --argjson reserved_paths "$_RP_PATHS_JSON" \
      '{event_type:$event_type, ts:$ts, repo:$repo, pr:$pr, title:$title,
        base_branch:$base, author:$author, merged_by:$merged_by,
        merge_sha:$merge_sha, reserved_paths:$reserved_paths}'
    echo "::warning::RESERVED-PATH SELF-MERGE on PR #${PR_NUMBER}: author==merger (${PR_AUTHOR}) on a CTO-reserved path. See incident.reserved_self_merge."
  fi
fi

# 2. Required status checks — branch-aware JSON dict takes precedence.
if [ -n "${REQUIRED_CHECKS_JSON:-}" ]; then
  # FAIL-CLOSED: if REQUIRED_CHECKS_JSON is set, the branch entry must exist
  # and be an array. A missing branch or non-array value means the config is
  # malformed or drifted — we must NOT silently treat it as "no checks".
  _RC_JSON_OK=$(echo "$REQUIRED_CHECKS_JSON" | jq -r --arg branch "$BASE_BRANCH" '
    has($branch) and (.[$branch] | type == "array")
  ')
  if [ "$_RC_JSON_OK" != "true" ]; then
    echo "::error::REQUIRED_CHECKS_JSON missing or non-array entry for branch '$BASE_BRANCH' — cannot evaluate required checks."
    exit 1
  fi
  REQUIRED=$(echo "$REQUIRED_CHECKS_JSON" | jq -r --arg branch "$BASE_BRANCH" '.[$branch] | .[]')
else
  REQUIRED="$REQUIRED_CHECKS"
fi
if [ -z "${REQUIRED//[[:space:]]/}" ]; then
  echo "::notice::REQUIRED_CHECKS empty for branch '$BASE_BRANCH' — force-merge not applicable."
  exit 0
fi

# 3. Status-check state at the PR HEAD (where checks ran). The merge
#    commit doesn't get its own checks; we evaluate the PR's last
#    commit, which is what branch protection compared against.
#
# Pagination (status-pagination RCA, #2440-family): the combined
# /commits/{sha}/status endpoint caps its embedded `statuses` array at the
# Gitea default page size (~30). On a high-churn PR an older-but-still-current
# required-context SUCCESS row is pushed PAST that cap, so reading the combined
# view would record that context as `missing` and emit a FALSE-POSITIVE
# force-merge. We instead page through the dedicated /commits/{sha}/statuses
# list to EXHAUSTION (until a short/empty page), accumulating every row.
#
# Fail-closed is preserved end to end: any non-200 page, or a page whose body
# is not a JSON array, aborts with exit 1 (we never treat an unreadable/partial
# page as "no checks"). A genuinely-absent required context appears on NO page,
# so CHECK_STATE has no entry for it → `${...:-missing}` below keeps it
# `missing` → it is still counted as not-green. No fail-open path is added.
PER_PAGE=100
page=1
ALL_STATUSES_TMP=$(mktemp)
printf '[]' > "$ALL_STATUSES_TMP"   # accumulator: a single JSON array of rows
while :; do
  STATUS_TMP=$(mktemp)
  STATUS_HTTP=$(curl -sS -o "$STATUS_TMP" -w '%{http_code}' -H "$AUTH" \
    "${API}/repos/${OWNER}/${NAME}/commits/${HEAD_SHA}/statuses?page=${page}&limit=${PER_PAGE}")
  PAGE_BODY=$(cat "$STATUS_TMP")
  rm -f "$STATUS_TMP"
  if [ "$STATUS_HTTP" != "200" ]; then
    rm -f "$ALL_STATUSES_TMP"
    echo "::error::GET /commits/${HEAD_SHA}/statuses?page=${page} returned HTTP ${STATUS_HTTP} — cannot evaluate required checks."
    exit 1
  fi
  # FAIL-CLOSED: the /statuses endpoint returns a bare JSON array. A non-array
  # body (null/object/string) means the response is malformed — we must NOT
  # treat that as "no checks", which would silently declare all checks green.
  if ! echo "$PAGE_BODY" | jq -e 'type == "array"' >/dev/null 2>&1; then
    rm -f "$ALL_STATUSES_TMP"
    echo "::error::GET /commits/${HEAD_SHA}/statuses?page=${page} returned HTTP 200 but body is not a JSON array — cannot evaluate required checks."
    exit 1
  fi
  PAGE_COUNT=$(echo "$PAGE_BODY" | jq 'length')
  # Append this page's rows to the accumulator (insertion order is preserved
  # but NOT relied upon — the collapse below selects max-by-id per context).
  COMBINED=$(jq -s '.[0] + .[1]' "$ALL_STATUSES_TMP" <(echo "$PAGE_BODY"))
  printf '%s' "$COMBINED" > "$ALL_STATUSES_TMP"
  # Short page (fewer than PER_PAGE rows) ⇒ last page ⇒ stop.
  if [ "$PAGE_COUNT" -lt "$PER_PAGE" ]; then
    break
  fi
  page=$((page + 1))
done
STATUS=$(cat "$ALL_STATUSES_TMP")
rm -f "$ALL_STATUSES_TMP"
declare -A CHECK_STATE
# Gitea's /commits/{sha}/statuses is roughly newest-first but NOT strictly
# monotonic by id (observed first ids 157,155,156,… — local inversions from
# re-runs and page boundaries), so neither first- nor last-occurrence reliably
# yields the current row. Select the MAX-id row per context explicitly
# (order-independent), matching prod-auto-deploy.py's latest_status_for_context.
while IFS=$'\t' read -r ctx state; do
  [ -n "$ctx" ] && CHECK_STATE[$ctx]="$state"
done < <(echo "$STATUS" | jq -r 'group_by(.context) | map(max_by(.id)) | .[] | "\(.context)\t\(.status)"')

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
