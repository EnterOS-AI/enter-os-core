#!/usr/bin/env bash
# reserved-path-review — PREVENTIVE layer of the self-merge guard.
#
# Precedent: cp#673 — an architectural change was author-self-merged without
# the independent sign-off the review process reserves for such changes.
#
# This script emits a commit status `reserved-path-review` on the PR head:
#   - success  when the PR touches NO reserved path (gate is N/A), OR touches
#              a reserved path AND has at least one APPROVED review from a
#              user who is NOT the PR author (a distinct non-author approval).
#   - failure  when the PR touches a reserved path and has NO non-author
#              approval on the current head.
#
# Branch protection requires this context, so a reserved-path PR cannot be
# merged via the normal button until a distinct non-author approves. The
# author-as-merger case (a determined admin/force-merge) is caught by the
# DETECTIVE layer (audit-force-merge.sh emits incident.reserved_self_merge).
#
# Security model mirrors audit-force-merge.yml: runs from pull_request_target
# with the base-branch checkout (base.sha), so a PR author cannot rewrite this
# gate on their own PR. Reads reserved-paths.txt from the BASE checkout.
#
# Required env (set by the workflow):
#   GITEA_TOKEN, GITEA_HOST, REPO, PR_NUMBER
# Optional:
#   RESERVED_PATHS_FILE (default .gitea/reserved-paths.txt)

set -euo pipefail

: "${GITEA_TOKEN:?required}"
: "${GITEA_HOST:?required}"
: "${REPO:?required}"
: "${PR_NUMBER:?required}"
RESERVED_PATHS_FILE="${RESERVED_PATHS_FILE:-.gitea/reserved-paths.txt}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
source "${SCRIPT_DIR}/reserved-path-match.sh"

OWNER="${REPO%%/*}"
NAME="${REPO##*/}"
API="https://${GITEA_HOST}/api/v1"
AUTH="Authorization: token ${GITEA_TOKEN}"
CONTEXT="reserved-path-review"

_get() { # _get <url> -> body on stdout, fail-closed on non-200
  local url="$1" tmp http
  tmp=$(mktemp)
  http=$(curl -sS -o "$tmp" -w '%{http_code}' -H "$AUTH" "$url")
  cat "$tmp"; rm -f "$tmp"
  [ "$http" = "200" ] || { echo "::error::GET $url -> HTTP $http" >&2; return 1; }
}

# 1. PR meta: author + head sha (fail-closed on schema).
PR=$(_get "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}")
PR_OK=$(echo "$PR" | jq -r '
  (.user|type=="object") and (.user.login|type=="string") and
  (.head|type=="object") and (.head.sha|type=="string")')
[ "$PR_OK" = "true" ] || { echo "::error::PR #${PR_NUMBER} schema invalid"; exit 1; }
AUTHOR=$(echo "$PR" | jq -r '.user.login')
HEAD_SHA=$(echo "$PR" | jq -r '.head.sha')

post_status() { # post_status <state> <description>
  local state="$1" desc="$2"
  curl -sS -o /dev/null -w '%{http_code}\n' -X POST \
    -H "$AUTH" -H "Content-Type: application/json" \
    "${API}/repos/${OWNER}/${NAME}/statuses/${HEAD_SHA}" \
    -d "$(jq -nc --arg s "$state" --arg c "$CONTEXT" --arg d "$desc" \
          '{state:$s, context:$c, description:$d}')"
}

# 2. Changed files for the PR (paginate; fail-closed on non-200).
CHANGED=()
page=1
while : ; do
  resp=$(_get "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}/files?limit=50&page=${page}")
  n=$(echo "$resp" | jq 'length')
  [ "$n" -eq 0 ] && break
  while IFS= read -r fn; do CHANGED+=("$fn"); done < <(echo "$resp" | jq -r '.[].filename')
  [ "$n" -lt 50 ] && break
  page=$((page+1))
  [ "$page" -gt 40 ] && { echo "::error::pagination runaway"; exit 1; }
done

# 3. Reserved-path match.
#    Matcher contract (reserved-path-match.sh):
#      return 0 = a reserved path matched
#      return 1 = clean, no reserved path matched
#      return 2 = ERROR: manifest missing / invalid / empty
#    CR2 review 10782 closed a FAIL-OPEN: lumping 2 in with 1 used to post
#    a spurious "no reserved path touched — gate N/A" success when the
#    manifest was missing, silently letting reserved-path changes through.
#    We now branch explicitly and fail-CLOSED on any non-0/1 (incl. 2).
#
#    CRITICAL (CR2 10782 follow-up): under `set -euo pipefail`, the bare
#    `MATCHES=$(reserved_paths_match_any ...)` ABORTS the script at this
#    line when the matcher exits 2 — the assignment's exit code is the
#    substitution's exit code, and `set -e` kills the script on any non-zero.
#    The fail-CLOSED `*)` arm below would NEVER run, defeating the fix.
#    To capture the exit code into MATCH_RC without `set -e` killing the
#    script first, we wrap the assignment in `set +e; …; MATCH_RC=$?; set -e`.
#    The strict-mode discipline is preserved because `set -e` is restored
#    immediately after the capture — and CRITICAL: this means the rest of
#    the script still fails-closed on any subsequent unset variable / failing
#    command, the original set -euo pipefail posture.
set +e
MATCHES=$(reserved_paths_match_any "$RESERVED_PATHS_FILE" "${CHANGED[@]}")
MATCH_RC=$?
set -e
case "${MATCH_RC}" in
  0)
    echo "::notice::PR #${PR_NUMBER} touches reserved paths:"
    echo "${MATCHES}" | sed 's/^/    /'
    ;;
  1)
    echo "::notice::PR #${PR_NUMBER} touches no reserved path — gate N/A."
    post_status "success" "No CTO-reserved path touched."
    exit 0
    ;;
  *)
    # 2 (or any other non-0/1) is an ERROR. Fail-CLOSED: do NOT pass.
    echo "::error::reserved-paths.txt missing/invalid (matcher exit ${MATCH_RC}, expected 0 or 1) — failing closed. Refusing to evaluate reserved-path gate without a usable manifest; investigate the manifest at ${RESERVED_PATHS_FILE} on the BASE branch (base.sha) and re-run."
    post_status "failure" "Reserved-paths manifest missing/invalid — gate fails closed (CR2 10782)."
    exit 1
    ;;
esac

# 4. Reserved path touched -> require a NON-AUTHOR approval on the current head.
#    Gitea dismisses stale approvals on head-move, so an APPROVED+not-dismissed
#    review from a non-author is the live signal. (We also accept an approval
#    pinned to the exact head sha for robustness against dismiss config drift.)
REVIEWS=$(_get "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}/reviews")
NONAUTHOR_APPROVALS=$(echo "$REVIEWS" | jq -r --arg author "$AUTHOR" --arg head "$HEAD_SHA" '
  [ .[]
    | select(.state=="APPROVED")
    | select((.user.login // "") != $author)
    | select((.dismissed // false) == false)
    | select(((.commit_id // "") == $head) or (.commit_id == null) or (.commit_id == ""))
  ] | length')

if [ "${NONAUTHOR_APPROVALS:-0}" -ge 1 ]; then
  echo "::notice::reserved-path PR has ${NONAUTHOR_APPROVALS} non-author approval(s) on head ${HEAD_SHA:0:10} — gate satisfied."
  post_status "success" "Reserved path: non-author approval present."
  exit 0
fi

echo "::error::PR #${PR_NUMBER} touches a CTO-reserved path but has NO non-author approval on the current head. A distinct non-author (author != approver) must approve before merge."
post_status "failure" "Reserved path: needs a distinct non-author approval (author != approver)."
exit 1
