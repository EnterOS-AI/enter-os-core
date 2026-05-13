#!/usr/bin/env bash
# review-check — evaluate whether a PR satisfies a single team-review gate.
#
# RFC#324 Step 1 of 5 — qa-review + security-review check workflows.
#
# This is the shared evaluator invoked by:
#   .gitea/workflows/qa-review.yml      (TEAM=qa,      TEAM_ID=20)
#   .gitea/workflows/security-review.yml (TEAM=security, TEAM_ID=21)
#
# Pass condition (per RFC#324 v1.1 addendum):
#   ≥ 1 review on the PR where:
#     • state == APPROVED
#     • review.dismissed == false
#     • review.user.login != PR.user.login (non-author)
#     • review.user.login ∈ team-members
#
# Strict mode (default OFF for v1; see RFC trade-off note):
#   If REVIEW_CHECK_STRICT=1, additionally require review.commit_id == PR.head.sha.
#   With dismiss_stale_reviews: true at the protection layer, stale reviews
#   are already dismissed, so the additional commit_id check is belt-and-
#   suspenders. Keeping it off in v1 simplifies semantics; flip in a follow-up
#   PR if reviewer telemetry shows residual stale-APPROVE merges.
#
# Privilege gate (RFC#324 v1.3 §A1.1 — INFORMATIONAL ONLY):
#   The /qa-recheck and /security-recheck slash-commands can be triggered
#   by anyone who can comment on the PR. The workflow's privilege step
#   logs collaborator-status but does NOT gate execution of this script.
#   Why this is safe: this evaluator is read-only and idempotent —
#   reading `pulls/{N}/reviews` and `teams/{id}/members/{u}` can't be
#   influenced by who triggered the run. If a real team-member APPROVE
#   exists the gate flips green; otherwise it stays red. A
#   non-collaborator commenting /qa-recheck cannot manufacture a green
#   gate. Original (v1.2) design with `if:`-gating of this step was
#   fail-open (skipped-via-`if:` job still publishes the status as
#   `success`) — corrected in v1.3 per hongming-pc review 1421.
#
# Trust boundary (RFC A4):
#   This script is loaded from the BASE branch (sourced via .gitea/scripts/
#   on the workflow's checkout-of-base). It does NOT execute any PR-HEAD
#   code. It only reads PR review state via the Gitea API.
#
# Token scope (RFC A1-α):
#   The job's own conclusion (exit 0 / exit 1) is what publishes the
#   `qa-review / approved` / `security-review / approved` status context.
#   NO `POST /statuses` call here → NO `write:repository` scope on the
#   token. `read:organization` (for team-membership probe) and
#   `read:repository` (for PR + reviews) are enough.
#
# Required env:
#   GITEA_TOKEN — least-priv read:repository + read:organization. See note
#                 below about the team-membership API requiring the token
#                 owner to be in the queried team (Gitea 1.22.6 quirk).
#   GITEA_HOST  — e.g. git.moleculesai.app
#   REPO        — owner/name (from github.repository)
#   PR_NUMBER   — int (from github.event.pull_request.number or
#                 github.event.issue.number for issue_comment events)
#   TEAM        — short team name (qa | security) for log lines
#   TEAM_ID     — Gitea team id (20=qa, 21=security at time of writing)
#
# Optional:
#   REVIEW_CHECK_DEBUG=1 — per-API-call diagnostic lines
#   REVIEW_CHECK_STRICT=1 — also require review.commit_id == pr.head.sha

set -euo pipefail

# jq is required for JSON parsing. It is pre-baked into the runner-base
# image (per RFC#268 workflow-smoke), so the only reason we'd not find it
# is a broken runner. The previous fallback dance (apt-get + curl to
# /usr/local/bin/jq) cannot succeed on a uid-1001 rootless runner
# (#391/#402 + feedback_ci_runner_install_needs_writable_path), so it's
# dropped. Fail loud with a clear diagnostic rather than attempt an
# install that physically cannot work.
if ! command -v jq >/dev/null 2>&1; then
  echo "::error::jq missing from runner-base image — bake it into the runner image (see RFC#268 workflow-smoke / feedback_ci_runner_install_needs_writable_path). This evaluator cannot run without jq."
  exit 1
fi

: "${GITEA_TOKEN:?GITEA_TOKEN required}"
: "${GITEA_HOST:?GITEA_HOST required}"
: "${REPO:?REPO required (owner/name)}"
: "${PR_NUMBER:?PR_NUMBER required}"
: "${TEAM:?TEAM required (qa|security)}"
: "${TEAM_ID:?TEAM_ID required (integer)}"

OWNER="${REPO%%/*}"
NAME="${REPO##*/}"
API="https://${GITEA_HOST}/api/v1"

# Token-in-argv fix (#541): write the Authorization header to a mode-600
# temp file instead of passing it via curl -H "$AUTH" (which puts the
# secret token value in the process table for any process to read via
# /proc/<pid>/cmdline or ps -ef). The curl config file is read by curl
# itself and never appears in the argv of the curl subprocess.
CURL_AUTH_FILE=$(mktemp -p /tmp curl-auth.XXXXXX)
chmod 600 "$CURL_AUTH_FILE"
printf 'header = "Authorization: token %s"\n' "$GITEA_TOKEN" > "$CURL_AUTH_FILE"

# Pre-create temp files so cleanup trap can reference them by name
# (bash trap 'function' EXIT expands variables at trap-fire time, not def time).
PR_JSON=$(mktemp)
REVIEWS_JSON=$(mktemp)
TEAM_PROBE_TMP=$(mktemp)

cleanup() {
  rm -f "$CURL_AUTH_FILE" "$PR_JSON" "$REVIEWS_JSON" "$TEAM_PROBE_TMP"
}
trap cleanup EXIT

debug() {
  if [ "${REVIEW_CHECK_DEBUG:-}" = "1" ]; then
    echo "  [debug] $*" >&2
  fi
}

echo "::notice::${TEAM}-review evaluating repo=${OWNER}/${NAME} pr=${PR_NUMBER} team_id=${TEAM_ID}"

# --- Fetch the PR (for author + head.sha) ---
HTTP_CODE=$(curl -sS -o "$PR_JSON" -w '%{http_code}' \
  -K "$CURL_AUTH_FILE" "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}")
if [ "$HTTP_CODE" != "200" ]; then
  echo "::error::GET /pulls/${PR_NUMBER} returned HTTP ${HTTP_CODE} (token scope?)"
  cat "$PR_JSON" >&2
  exit 1
fi
PR_AUTHOR=$(jq -r '.user.login // ""' "$PR_JSON")
PR_HEAD_SHA=$(jq -r '.head.sha // ""' "$PR_JSON")
PR_STATE=$(jq -r '.state // ""' "$PR_JSON")
debug "pr_author=${PR_AUTHOR} pr_head=${PR_HEAD_SHA:0:7} pr_state=${PR_STATE}"

if [ "$PR_STATE" != "open" ]; then
  echo "::notice::PR ${PR_NUMBER} is ${PR_STATE} — exiting 0 (closed PRs do not gate)"
  exit 0
fi
if [ -z "$PR_AUTHOR" ] || [ -z "$PR_HEAD_SHA" ]; then
  echo "::error::PR ${PR_NUMBER} missing user.login or head.sha — webhook payload malformed"
  exit 1
fi

# --- Fetch all reviews on the PR ---
HTTP_CODE=$(curl -sS -o "$REVIEWS_JSON" -w '%{http_code}' \
  -K "$CURL_AUTH_FILE" "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}/reviews")
if [ "$HTTP_CODE" != "200" ]; then
  echo "::error::GET /pulls/${PR_NUMBER}/reviews returned HTTP ${HTTP_CODE}"
  cat "$REVIEWS_JSON" >&2
  exit 1
fi

# Filter: state=APPROVED, not-dismissed, non-author. Optionally strict-mode
# adds commit_id==head.sha (off by default; see header).
JQ_FILTER='.[]
  | select(.state == "APPROVED")
  | select(.dismissed != true)
  | select(.user.login != $author)'
if [ "${REVIEW_CHECK_STRICT:-}" = "1" ]; then
  JQ_FILTER="${JQ_FILTER}
  | select(.commit_id == \$head)"
fi
JQ_FILTER="${JQ_FILTER}
  | .user.login"

CANDIDATES=$(jq -r --arg author "$PR_AUTHOR" --arg head "$PR_HEAD_SHA" "$JQ_FILTER" "$REVIEWS_JSON" | sort -u)
debug "candidate non-author approvers: $(echo "$CANDIDATES" | tr '\n' ' ')"

if [ -z "$CANDIDATES" ]; then
  echo "::error::${TEAM}-review awaiting non-author APPROVE from ${TEAM} team (no candidates yet)"
  exit 1
fi

# --- Probe team membership per candidate ---
# Endpoint: GET /api/v1/teams/{id}/members/{username}
#   200/204 → is member
#   403     → token owner is not in this team (Gitea 1.22.6 'Must be a team
#             member' constraint — see follow-up issue for token-provisioning)
#   404     → not a member
for U in $CANDIDATES; do
  CODE=$(curl -sS -o "$TEAM_PROBE_TMP" -w '%{http_code}' \
    -K "$CURL_AUTH_FILE" "${API}/teams/${TEAM_ID}/members/${U}")
  debug "probe ${U} in team ${TEAM} (id=${TEAM_ID}) → HTTP ${CODE}"
  case "$CODE" in
    200|204)
      echo "::notice::${TEAM}-review APPROVED by ${U} (team=${TEAM})"
      exit 0
      ;;
    403)
      # Token owner is not in the team being probed; the API refuses to
      # confirm membership. This is the RFC#324 follow-up token-scope gap.
      # Fail closed — never grant approval on a 403; surface clearly.
      echo "::error::team-probe for ${U} in ${TEAM} returned 403 (token owner not in ${TEAM} team — RFC#324 token-scope follow-up). Cannot confirm membership; failing closed."
      cat "$TEAM_PROBE_TMP" >&2
      exit 1
      ;;
    404)
      debug "${U} not a member of ${TEAM}"
      ;;
    *)
      echo "::warning::team-probe for ${U} in ${TEAM} returned unexpected HTTP ${CODE}"
      cat "$TEAM_PROBE_TMP" >&2
      ;;
  esac
done

echo "::error::${TEAM}-review awaiting non-author APPROVE from ${TEAM} team (candidates: $(echo "$CANDIDATES" | tr '\n' ',' | sed 's/,$//') — none are in team)"
exit 1
