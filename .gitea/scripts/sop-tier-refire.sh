#!/usr/bin/env bash
# sop-tier-refire — re-evaluate sop-tier-check and POST status to PR head SHA.
#
# Invoked from `.gitea/workflows/sop-tier-refire.yml` when a repo
# MEMBER/OWNER/COLLABORATOR comments `/refire-tier-check` on a PR.
#
# Behavior:
#
# 1. Resolve PR head SHA + author from PR_NUMBER.
# 2. Rate-limit: if the sop-tier-check context has been POSTed in the
#    last 30 seconds, skip (prevents comment-spam status thrash).
# 3. Invoke `.gitea/scripts/sop-tier-check.sh` with the same env the
#    canonical workflow provides. This is DRY: we re-use the exact AND-
#    composition gate logic, not a watered-down approving-count check.
# 4. POST the resulting status (success on exit 0, failure on non-zero)
#    to `/repos/.../statuses/{HEAD_SHA}` with context
#    "sop-tier-check / tier-check (pull_request)" — the same context name
#    branch protection requires.
#
# Required env (set by sop-tier-refire.yml):
#   GITEA_TOKEN    — org-level SOP_TIER_CHECK_TOKEN (read:org/user/issue/repo)
#   GITEA_HOST     — e.g. git.moleculesai.app
#   REPO           — owner/name
#   PR_NUMBER      — PR number from issue_comment payload
#   COMMENT_AUTHOR — login of the commenter (logged for audit)
#
# Optional:
#   SOP_DEBUG=1                — verbose per-API-call diagnostics
#   SOP_REFIRE_RATE_LIMIT_SEC  — override the 30s rate-limit (default 30)
#   SOP_REFIRE_DISABLE_RATE_LIMIT=1 — for tests; skips the rate-limit check

set -euo pipefail

debug() {
  if [ "${SOP_DEBUG:-}" = "1" ]; then
    echo "  [debug] $*" >&2
  fi
}

: "${GITEA_TOKEN:?GITEA_TOKEN required}"
: "${GITEA_HOST:?GITEA_HOST required}"
: "${REPO:?REPO required (owner/name)}"
: "${PR_NUMBER:?PR_NUMBER required}"
: "${COMMENT_AUTHOR:=unknown}"

OWNER="${REPO%%/*}"
NAME="${REPO##*/}"
API="https://${GITEA_HOST}/api/v1"
AUTH="Authorization: token ${GITEA_TOKEN}"
CONTEXT="sop-tier-check / tier-check (pull_request)"
RATE_LIMIT_SEC="${SOP_REFIRE_RATE_LIMIT_SEC:-30}"

echo "::notice::sop-tier-refire start: repo=$OWNER/$NAME pr=$PR_NUMBER commenter=$COMMENT_AUTHOR"

# 1. Fetch PR details — need head.sha and user.login.
PR_FILE=$(mktemp)
trap 'rm -f "$PR_FILE"' EXIT
PR_HTTP=$(curl -sS -o "$PR_FILE" -w '%{http_code}' -H "$AUTH" \
  "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}")
if [ "$PR_HTTP" != "200" ]; then
  echo "::error::GET /pulls/$PR_NUMBER returned HTTP $PR_HTTP (body $(head -c 200 "$PR_FILE"))"
  exit 1
fi
HEAD_SHA=$(jq -r '.head.sha' <"$PR_FILE")
PR_AUTHOR=$(jq -r '.user.login' <"$PR_FILE")
PR_STATE=$(jq -r '.state' <"$PR_FILE")
if [ -z "$HEAD_SHA" ] || [ "$HEAD_SHA" = "null" ]; then
  echo "::error::Could not resolve head.sha from PR #$PR_NUMBER response"
  exit 1
fi
debug "head_sha=$HEAD_SHA pr_author=$PR_AUTHOR state=$PR_STATE"

if [ "$PR_STATE" != "open" ]; then
  echo "::notice::PR #$PR_NUMBER state is $PR_STATE; refire is a no-op on closed PRs."
  exit 0
fi

# 2. Rate-limit: skip if our context was updated in the last $RATE_LIMIT_SEC.
# Gitea statuses endpoint returns latest first; we check the most recent
# entry for our context name.
if [ "${SOP_REFIRE_DISABLE_RATE_LIMIT:-}" != "1" ]; then
  STATUSES_FILE=$(mktemp)
  trap 'rm -f "$PR_FILE" "$STATUSES_FILE"' EXIT
  ST_HTTP=$(curl -sS -o "$STATUSES_FILE" -w '%{http_code}' -H "$AUTH" \
    "${API}/repos/${OWNER}/${NAME}/statuses/${HEAD_SHA}?limit=50&sort=newest")
  debug "statuses-list HTTP=$ST_HTTP"
  if [ "$ST_HTTP" = "200" ]; then
    LAST_UPDATED=$(jq -r --arg c "$CONTEXT" \
      '[.[] | select(.context == $c)] | first | .updated_at // ""' \
      <"$STATUSES_FILE")
    if [ -n "$LAST_UPDATED" ] && [ "$LAST_UPDATED" != "null" ]; then
      # Parse RFC3339 → epoch. Use python -c for portability (date(1) -d
      # differs between BSD/GNU; the Gitea runner is Ubuntu so GNU date
      # works, but we keep python for future container variance).
      LAST_EPOCH=$(python3 -c "import sys,datetime;print(int(datetime.datetime.fromisoformat(sys.argv[1].replace('Z','+00:00')).timestamp()))" "$LAST_UPDATED" 2>/dev/null || echo "0")
      NOW_EPOCH=$(date -u +%s)
      AGE=$((NOW_EPOCH - LAST_EPOCH))
      debug "last status update: $LAST_UPDATED ($AGE seconds ago)"
      if [ "$AGE" -lt "$RATE_LIMIT_SEC" ] && [ "$AGE" -ge 0 ]; then
        echo "::notice::sop-tier-refire rate-limited — last status update was ${AGE}s ago (<${RATE_LIMIT_SEC}s window). Try again shortly."
        exit 0
      fi
    fi
  fi
fi

# 3. Invoke sop-tier-check.sh with the env it expects. Capture exit code.
# The canonical script reads tier label, walks approving reviewers, and
# evaluates the AND-composition expression — we want the SAME gate, not
# a different gate.
#
# SOP_REFIRE_TIER_CHECK_SCRIPT env var lets tests substitute a mock —
# sop-tier-check.sh uses bash 4+ associative arrays which trigger a known
# bash 3.2 parser bug (`tier: unbound variable` from declare -A with
# `set -u`). Linux Gitea runners ship bash 4/5 so production is fine;
# the override exists so the bash 3.2 dev box can still exercise the
# refire glue logic end-to-end.
SCRIPT="${SOP_REFIRE_TIER_CHECK_SCRIPT:-$(dirname "$0")/sop-tier-check.sh}"
if [ ! -f "$SCRIPT" ]; then
  echo "::error::sop-tier-check.sh not found at $SCRIPT — refire requires the canonical script"
  exit 1
fi

# Re-invoke. Pipe stdout/stderr through so the runner log shows the
# tier-check decision inline.
set +e
GITEA_TOKEN="$GITEA_TOKEN" \
  GITEA_HOST="$GITEA_HOST" \
  REPO="$REPO" \
  PR_NUMBER="$PR_NUMBER" \
  PR_AUTHOR="$PR_AUTHOR" \
  SOP_DEBUG="${SOP_DEBUG:-0}" \
  SOP_LEGACY_CHECK="${SOP_LEGACY_CHECK:-0}" \
  bash "$SCRIPT"
TIER_EXIT=$?
set -e
debug "sop-tier-check.sh exit=$TIER_EXIT"

# 4. POST the resulting status.
if [ "$TIER_EXIT" -eq 0 ]; then
  STATE="success"
  DESCRIPTION="Refired via /refire-tier-check by $COMMENT_AUTHOR"
else
  STATE="failure"
  DESCRIPTION="Refired via /refire-tier-check; tier-check failed (see workflow log)"
fi

# Status target_url points at the runner log so a curious reviewer can
# follow it back. SERVER_URL + RUN_ID + JOB_ID isn't trivially constructible
# from the bash env on Gitea 1.22.6, so we point at the PR itself.
TARGET_URL="https://${GITEA_HOST}/${OWNER}/${NAME}/pulls/${PR_NUMBER}"

POST_BODY=$(jq -nc \
  --arg state "$STATE" \
  --arg context "$CONTEXT" \
  --arg description "$DESCRIPTION" \
  --arg target_url "$TARGET_URL" \
  '{state:$state, context:$context, description:$description, target_url:$target_url}')

POST_FILE=$(mktemp)
trap 'rm -f "$PR_FILE" "${STATUSES_FILE:-}" "$POST_FILE"' EXIT
POST_HTTP=$(curl -sS -o "$POST_FILE" -w '%{http_code}' \
  -X POST -H "$AUTH" -H "Content-Type: application/json" \
  -d "$POST_BODY" \
  "${API}/repos/${OWNER}/${NAME}/statuses/${HEAD_SHA}")
if [ "$POST_HTTP" != "200" ] && [ "$POST_HTTP" != "201" ]; then
  echo "::error::POST /statuses/$HEAD_SHA returned HTTP $POST_HTTP (body $(head -c 200 "$POST_FILE"))"
  exit 1
fi

echo "::notice::sop-tier-refire posted state=$STATE for context=\"$CONTEXT\" on sha=$HEAD_SHA"
exit "$TIER_EXIT"
