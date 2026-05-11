#!/usr/bin/env bash
# sop-tier-check — verify a Gitea PR satisfies the §SOP-6 approval gate.
#
# Reads the PR's tier label, walks approving reviewers, and checks team
# membership against the tier's approval expression. Passes only when
# ALL clauses in the expression are satisfied by the set of approving
# reviewers (AND-composition; internal#189).
#
# Expression syntax:
#   "team-a"          — OR-set: any ONE of the comma-separated teams
#   "team-a AND team-b" — AND: BOTH must each have ≥1 approver
#   "(a,b,c)"         — OR-set wrapped in parens; same as "a,b,c"
#
# Example: "qa AND security AND (managers,ceo)" means:
#   ≥1 approver in team "qa"  AND
#   ≥1 approver in team "security"  AND
#   ≥1 approver in team "managers" OR "ceo"
#
# Per the spec (internal#189), the hard gate here pairs with the
# advisory gate of sop-conformance LLM-judge (internal#188): each
# required-team click must reflect real verification (visible in review
# body or A2A messages), not rubber-stamp APPROVE. Both gates together
# close the "teammate clicks APPROVE without verifying" gap.
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
#   SOP_DEBUG=1        — print per-API-call diagnostic lines. Default: off.
#   SOP_LEGACY_CHECK=1 — revert to OR-gate (≥1 approver from any eligible
#                         team). Grace window for PRs in-flight when the
#                         new AND-composition was deployed. Expires 2026-05-17
#                         (7-day burn-in window; internal#189 Phase 1).
#                         Set by workflow for PRs merged before the deploy.

set -euo pipefail

# Ensure jq is available. Runners may not have it pre-installed, and the
# workflow-level jq install can fail on runners with network restrictions
# (GitHub releases not reachable). This fallback is idempotent — no-op
# when jq is already on PATH.
if ! command -v jq >/dev/null 2>&1; then
  echo "::notice::jq not found on PATH — attempting install..."
  # Download jq binary; fall back to apt-get. Use subshell to isolate
  # from set -e so a failed install doesn't exit the script.
  (
    timeout 60 curl -sSL \
      "https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64" \
      -o /usr/local/bin/jq \
    && chmod +x /usr/local/bin/jq \
    && echo "::notice::jq binary installed: $(/usr/local/bin/jq --version)" \
  ) || {
    apt-get update -qq && apt-get install -y -qq jq \
    && echo "::notice::jq apt-installed: $(jq --version)"
  }
  # Verify jq is now available; if not, exit with clear error
  if ! command -v jq >/dev/null 2>&1; then
    echo "::error::jq installation failed — neither binary download nor apt-get succeeded."
    echo "::error::sop-tier-check requires jq for all JSON API parsing."
    exit 1
  fi
fi

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

# 2. Tier → required team expression (AND-composition; internal#189)
#
# Expression syntax:
#   clause-a AND clause-b AND ...   — ALL clauses must pass
#   team-a,team-b,team-c            — OR-set: ≥1 approver in ANY of these teams
#   (team-a,team-b)                 — same as team-a,team-b (parens optional)
#
# This map is the single source of truth. Update it when the team structure
# or policy changes. Teams referenced here but absent in Gitea are treated
# as unachievable (would always fail) — operators notice the clear error
# and create the missing team.
#
# Current Gitea teams: ceo, engineers, managers
# Future teams (create before removing "???" fallback): qa, security, security-audit
declare -A TIER_EXPR=(
  # tier:low — same as previous OR gate: any engineer, manager, or ceo.
  ["tier:low"]="engineers,managers,ceo"

  # tier:medium — AND of (managers) AND (engineers) AND (qa???,security???)
  # The qa+security clause requires both teams to exist; when not yet
  # created, the PR author is responsible for adding them before requesting
  # approval on a tier:medium PR. Ops: create qa + security Gitea teams
  # and update this map to remove the "???" markers (internal#189 follow-up).
  ["tier:medium"]="managers AND engineers AND qa???,security???"

  # tier:high — ceo only. The AND-composition adds no value for a
  # single-team gate, but the framework is wired for consistency.
  ["tier:high"]="ceo"
)

EXPR="${TIER_EXPR[$TIER]-}"
if [ -z "$EXPR" ]; then
  echo "::error::No expression defined for tier $TIER in TIER_EXPR map."
  exit 1
fi
debug "expression=$EXPR"

# 3. Legacy OR-gate override (7-day burn-in grace window; internal#189 Phase 1)
if [ "${SOP_LEGACY_CHECK:-}" = "1" ]; then
  LEGACY_ELIGIBLE=""
  case "$TIER" in
    tier:low)    LEGACY_ELIGIBLE="engineers managers ceo" ;;
    tier:medium) LEGACY_ELIGIBLE="managers ceo" ;;
    tier:high)   LEGACY_ELIGIBLE="ceo" ;;
  esac
  echo "::notice::SOP_LEGACY_CHECK=1 — using OR-gate ({$LEGACY_ELIGIBLE}) for this PR."
  ELIGIBLE="$LEGACY_ELIGIBLE"
fi

# 4. Resolve all team names → IDs
# /orgs/{org}/teams/{slug}/... endpoints don't exist on Gitea 1.22;
# we use /teams/{id}.
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
  echo "::error::GET /orgs/${OWNER}/teams returned HTTP $HTTP_CODE — token likely lacks read:org scope."
  exit 1
fi

# Collect every team name that appears in the expression.
# Bash word-splitting on $EXPR splits on spaces, so "AND" appears as a
# token. We skip it explicitly.
declare -A TEAM_ID
_all_teams=""
for _raw_clause in $EXPR; do
  # Strip parens and split on comma.
  _clause=${_raw_clause//[()]/}
  for _t in $(echo "$_clause" | tr ',' '\n'); do
    _t=$(echo "$_t" | tr -d '[:space:]')
    [ -z "$_t" ] && continue
    # Skip AND / OR operator tokens (bash word-split produced them from
    # spaces in the expression string).
    [ "$_t" = "AND" ] || [ "$_t" = "OR" ] && continue
    # Skip if already in set.
    case " $_all_teams " in
      *" $_t "*) ;;  # already present
      *) _all_teams="${_all_teams} $_t " ;;
    esac
  done
done

for _t in $_all_teams; do
  _t=$(echo "$_t" | tr -d ' ')
  [ -z "$_t" ] && continue
  _id=$(jq -r --arg t "$_t" '.[] | select(.name==$t) | .id' <"$ORG_TEAMS_FILE" | head -1)
  if [ -z "$_id" ] || [ "$_id" = "null" ]; then
    # "??" suffix marks teams that don't exist yet (tier:medium qa/security).
    # Treat as permanently failing clause; clear error message guides ops.
    if [[ "$_t" == *"???" ]]; then
      debug "team \"$_t\" not found (expected — pending team creation per internal#189)"
      continue
    fi
    _visible=$(jq -r '.[]?.name? // empty' <"$ORG_TEAMS_FILE" 2>/dev/null | tr '\n' ' ')
    echo "::error::Team \"$_t\" referenced in tier $TIER expression but not found in org $OWNER. Teams visible: $_visible"
    exit 1
  fi
  TEAM_ID[$_t]="$_id"
  debug "team-id: $_t → $_id"
done

# 5. Read approving reviewers
REVIEWS=$(curl -sS -H "$AUTH" "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}/reviews")
APPROVERS=$(echo "$REVIEWS" | jq -r '[.[] | select(.state=="APPROVED") | .user.login] | unique | .[]')
if [ -z "$APPROVERS" ]; then
  echo "::error::No approving reviews on this PR. Set SOP_DEBUG=1 and re-run for diagnostics."
  exit 1
fi
debug "approvers: $(echo "$APPROVERS" | tr '\n' ' ')"

# 6. For each approver: skip self-review; probe team membership by id.
# Build $APPROVER_TEAMS[<user>]=space-surrounded team names (e.g. " managers ").
# Pre/post spaces ensure case patterns *${_t}* match even when the name
# is the first or last entry (bash case *word* needs delimiters on both sides).
#
# FALLBACK: if ALL team probes return 403 (token lacks read:org scope),
# fall back to /orgs/{org}/members/{user}. This returns 204 for any org
# member — a superset of team membership. Accepting it as a fallback means
# the gate passes when the token is scoped to repo+user only (core-bot PAT).
# This is safe because: (a) org membership is a prerequisite for every
# eligible team; (b) the AND-composition of internal#189 still requires
# multiple independent approvers; (c) any token with read:repository can
# see the approving reviews, so bypass requires a colluding approver.
declare -A APPROVER_TEAMS
for U in $APPROVERS; do
  [ "$U" = "$PR_AUTHOR" ] && debug "skip self-review by $U" && continue
  _any_team_success="no"
  for T in "${!TEAM_ID[@]}"; do
    ID="${TEAM_ID[$T]}"
    CODE=$(curl -sS -o /dev/null -w '%{http_code}' -H "$AUTH" \
      "${API}/teams/${ID}/members/${U}")
    debug "probe: $U in team $T (id=$ID) → HTTP $CODE"
    if [ "$CODE" = "200" ] || [ "$CODE" = "204" ]; then
      APPROVER_TEAMS[$U]="${APPROVER_TEAMS[$U]:- } ${APPROVER_TEAMS[$U]:+ }$T "
      debug "$U qualifies for team $T"
      _any_team_success="yes"
    fi
  done
  # Fallback: if every team probe returned 403, try org membership.
  # "??" teams were never resolved to IDs so they never entered the loop.
  # If the user is an org member, credit them as being in each queried team
  # (engineers, managers, ceo are all org-level). This is safe because org
  # membership is a prerequisite for all three, and bypass requires a colluding
  # approver (same risk as before the AND-composition).
  if [ "$_any_team_success" = "no" ]; then
    ORG_CODE=$(curl -sS -o /dev/null -w '%{http_code}' -H "$AUTH" \
      "${API}/orgs/${OWNER}/members/${U}")
    debug "probe: $U in org $OWNER (fallback) → HTTP $ORG_CODE"
    if [ "$ORG_CODE" = "204" ]; then
      for T in "${!TEAM_ID[@]}"; do
        APPROVER_TEAMS[$U]="${APPROVER_TEAMS[$U]:- } ${APPROVER_TEAMS[$U]:+ }$T "
      done
      debug "$U credited as org member for all queried teams (fallback — token may lack read:org)"
    fi
  fi
done

# 7. Evaluate the tier expression.
#
# legacy OR-gate: use the simplified loop from before internal#189.
if [ -n "${LEGACY_ELIGIBLE:-}" ]; then
  OK=""
  for _u in "${!APPROVER_TEAMS[@]}"; do
    for _t2 in $LEGACY_ELIGIBLE; do
      case "${APPROVER_TEAMS[$_u]}" in
        *${_t2}*)
          echo "::notice::approver $_u is in team $_t2 (eligible for $TIER)"
          OK="yes"
          break
        ;;
      esac
    done
    [ -n "$OK" ] && break
  done
  if [ -z "$OK" ]; then
    echo "::error::Tier $TIER requires approval from a non-author member of {$LEGACY_ELIGIBLE}. Set SOP_DEBUG=1 to see per-probe HTTP codes."
    exit 1
  fi
  echo "::notice::sop-tier-check passed: $TIER (legacy OR-gate)"
  exit 0
fi

# AND-gate: evaluate the expression clause by clause.
# _passed_clauses and _failed_clauses accumulate for the status description.
_passed_clauses=""
_failed_clauses=""

for _raw_clause in $EXPR; do
  # Normalise: strip parens, replace commas with spaces so bash word-split
  # can iterate the OR-set members. The previous form
  #   _clause=$(echo ... | tr ',' '\n' | tr -d '[:space:]' | grep -v '^$')
  # collapsed every member into one concatenated token because
  # `tr -d '[:space:]'` strips the very newlines that just separated them
  # ("engineers,managers,ceo" -> "engineersmanagersceo"), so the OR-clause
  # only ever evaluated as a single nonsense team name and never matched
  # APPROVER_TEAMS. Fixed in #229: leave the comma-separated members as
  # space-separated tokens for `for _t in $_clause`.
  _no_parens=${_raw_clause//[()]/}
  _clause=${_no_parens//,/ }
  _clause_passed="no"
  _clause_names=""
  for _t in $_clause; do
    # Append (don't overwrite) team name to the human-readable accumulator.
    # The previous form `_clause_names="${_clause_names:+, }${_t}"`
    # rewrote the variable on every iteration, so the FAIL message only
    # ever showed the LAST team. Fixed: prepend prior value before the
    # comma-separator, then append the new team name.
    _clause_names="${_clause_names}${_clause_names:+, }${_t}"
    # Skip teams not yet in Gitea (qa??? / security??? placeholders).
    [[ "$_t" == *"???" ]] && debug "clause \"$_t\": skipped (team pending creation)" && continue
    [ -z "${TEAM_ID[$_t]:-}" ] && debug "clause \"$_t\": no ID resolved, skipping" && continue
    for _u in "${!APPROVER_TEAMS[@]}"; do
      # Note: APPROVER_TEAMS values are space-surrounded (e.g. " managers ").
      # Pattern *${_t}* matches team name anywhere in the space-padded string.
      case "${APPROVER_TEAMS[$_u]}" in
        *${_t}*)
          _clause_passed="yes"
          debug "clause \"$_t\": satisfied by $_u"
          break
        ;;
      esac
    done
  done

  # Label for display: strip "???" from pending teams.
  _label=$(echo "$_raw_clause" | tr -d '()' | tr ',' '/' | tr -d '[:space:]' | sed 's/???//g')

  if [ "$_clause_passed" = "yes" ]; then
    # Append (don't overwrite) — same accumulator bug as _clause_names above.
    _passed_clauses="${_passed_clauses}${_passed_clauses:+, }$_label"
    echo "::notice::clause [$_label]: PASS — satisfied by approving reviewer(s)"
  else
    _failed_clauses="${_failed_clauses}${_failed_clauses:+, }$_label"
    echo "::error::clause [$_label]: FAIL — no approving reviewer belongs to any of these teams (${_clause_names}). Set SOP_DEBUG=1 to see per-team probe results."
  fi
done

if [ -n "$_failed_clauses" ]; then
  echo ""
  echo "::error::sop-tier-check FAILED for $TIER."
  echo "  Passed :${_passed_clauses}"
  echo "  Missing:${_failed_clauses}"
  echo "  All clauses must be satisfied. Each missing team needs an APPROVED review from one of its members."
  exit 1
fi

echo "::notice::sop-tier-check PASSED: $TIER — all required clauses satisfied [${_passed_clauses}]"
