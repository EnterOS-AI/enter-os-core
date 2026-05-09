#!/usr/bin/env bash
# sop-tier-check — verify a Gitea PR satisfies the §SOP-6 approval gate.
#
# Reads the PR's tier label, walks approving reviewers, and checks each
# approver's Gitea team membership against the tier's eligible-team set.
# Marks pass only when at least one non-author approver is in an eligible
# team.
#
# Invoked from `.gitea/workflows/sop-tier-check.yml`. The workflow sets
# the env vars below; this script does no IO outside of stdout/stderr +
# the Gitea API.
#
# Required env:
#   GITEA_TOKEN   — bot PAT with read:organization,read:user,
#                   read:issue,read:repository scopes
#   GITEA_HOST    — e.g. git.moleculesai.app
#   REPO          — owner/name (from github.repository)
#   PR_NUMBER     — int (from github.event.pull_request.number)
#   PR_AUTHOR     — login (from github.event.pull_request.user.login)
#
# Optional:
#   SOP_DEBUG=1   — print per-API-call diagnostic lines (HTTP codes,
#                   raw response bodies). Default: off.
#
# Stale-status caveat: Gitea Actions does not always re-fire workflows
# on `labeled` / `pull_request_review:submitted` events. If the
# sop-tier-check status is stale (e.g. red after labels/approvals were
# added), push an empty commit to the PR branch to force a synchronize
# event, OR re-request reviews. Tracked: internal#46.

set -euo pipefail

debug() {
  if [ "${SOP_DEBUG:-}" = "1" ]; then
    echo "  [debug] $*" >&2
  fi
}

# Validate env
: "${GITEA_TOKEN:?GITEA_TOKEN required}"
: "${GITEA_HOST:?GITEA_HOST required}"
: "${REPO:?REPO required (owner/name)}"
: "${PR_NUMBER:?PR_NUMBER required}"
: "${PR_AUTHOR:?PR_AUTHOR required}"

OWNER="${REPO%%/*}"
NAME="${REPO##*/}"
API="https://${GITEA_HOST}/api/v1"
AUTH="Authorization: token ${GITEA_TOKEN}"
echo "::notice::tier-check start: repo=$OWNER/$NAME pr=$PR_NUMBER author=$PR_AUTHOR"

# Sanity: token resolves to a user
WHOAMI=$(curl -sS -H "$AUTH" "${API}/user" | jq -r '.login // ""')
if [ -z "$WHOAMI" ]; then
  echo "::error::GITEA_TOKEN cannot resolve a user via /api/v1/user — check the token scope and that the secret is wired correctly."
  exit 1
fi
echo "::notice::token resolves to user: $WHOAMI"

# 1. Read tier label
LABELS=$(curl -sS -H "$AUTH" "${API}/repos/${OWNER}/${NAME}/issues/${PR_NUMBER}/labels" | jq -r '.[].name')
TIER=""
for L in $LABELS; do
  case "$L" in
    tier:low|tier:medium|tier:high)
      if [ -n "$TIER" ]; then
        echo "::error::Multiple tier labels: $TIER + $L. Apply exactly one."
        exit 1
      fi
      TIER="$L"
    ;;
  esac
done
if [ -z "$TIER" ]; then
  echo "::error::PR has no tier:low|tier:medium|tier:high label. Apply one before merge."
  exit 1
fi
debug "tier=$TIER"

# 2. Tier → eligible teams
case "$TIER" in
  tier:low)    ELIGIBLE="engineers managers ceo" ;;
  tier:medium) ELIGIBLE="managers ceo" ;;
  tier:high)   ELIGIBLE="ceo" ;;
esac
debug "eligible_teams=$ELIGIBLE"

# Resolve team-name → team-id once. /orgs/{org}/teams/{slug}/... endpoints
# don't exist on Gitea 1.22; we have to use /teams/{id}.
ORG_TEAMS_FILE=$(mktemp)
trap 'rm -f "$ORG_TEAMS_FILE"' EXIT
HTTP_CODE=$(curl -sS -o "$ORG_TEAMS_FILE" -w '%{http_code}' -H "$AUTH" \
  "${API}/orgs/${OWNER}/teams")
debug "teams-list HTTP=$HTTP_CODE size=$(wc -c <"$ORG_TEAMS_FILE")"
if [ "${SOP_DEBUG:-}" = "1" ]; then
  echo "  [debug] teams-list body (first 300 chars):" >&2
  head -c 300 "$ORG_TEAMS_FILE" >&2; echo >&2
fi
if [ "$HTTP_CODE" != "200" ]; then
  echo "::error::GET /orgs/${OWNER}/teams returned HTTP $HTTP_CODE — token likely lacks read:org scope. Add a SOP_TIER_CHECK_TOKEN secret with read:organization scope at the org level."
  exit 1
fi
declare -A TEAM_ID
for T in $ELIGIBLE; do
  ID=$(jq -r --arg t "$T" '.[] | select(.name==$t) | .id' <"$ORG_TEAMS_FILE" | head -1)
  if [ -z "$ID" ] || [ "$ID" = "null" ]; then
    VISIBLE=$(jq -r '.[]?.name? // empty' <"$ORG_TEAMS_FILE" 2>/dev/null | tr '\n' ' ')
    echo "::error::Team \"$T\" not found in org $OWNER. Teams visible: $VISIBLE"
    exit 1
  fi
  TEAM_ID[$T]="$ID"
  debug "team-id: $T → $ID"
done

# 3. Read approving reviewers
REVIEWS=$(curl -sS -H "$AUTH" "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}/reviews")
APPROVERS=$(echo "$REVIEWS" | jq -r '[.[] | select(.state=="APPROVED") | .user.login] | unique | .[]')
if [ -z "$APPROVERS" ]; then
  echo "::error::No approving reviews. Tier $TIER requires approval from {$ELIGIBLE} (non-author)."
  exit 1
fi
debug "approvers: $(echo "$APPROVERS" | tr '\n' ' ')"

# 4. For each approver: check non-author + team membership (by id)
OK=""
for U in $APPROVERS; do
  if [ "$U" = "$PR_AUTHOR" ]; then
    debug "skip self-review by $U"
    continue
  fi
  for T in $ELIGIBLE; do
    ID="${TEAM_ID[$T]}"
    CODE=$(curl -sS -o /dev/null -w '%{http_code}' -H "$AUTH" \
      "${API}/teams/${ID}/members/${U}")
    debug "probe: $U in team $T (id=$ID) → HTTP $CODE"
    if [ "$CODE" = "200" ] || [ "$CODE" = "204" ]; then
      echo "::notice::approver $U is in team $T (eligible for $TIER)"
      OK="yes"
      break
    fi
  done
  [ -n "$OK" ] && break
done

if [ -z "$OK" ]; then
  echo "::error::Tier $TIER requires approval from a non-author member of {$ELIGIBLE}. Got approvers: $APPROVERS — none of them satisfied team membership. Set SOP_DEBUG=1 to see per-probe HTTP codes."
  exit 1
fi
echo "::notice::sop-tier-check passed: $TIER, approver in {$ELIGIBLE}"
