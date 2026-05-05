#!/usr/bin/env bash
# tools/branch-protection/drift_check.sh — compare the live branch
# protection on staging + main against what apply.sh would set. Used
# by branch-protection-drift.yml (cron) to catch out-of-band UI edits.
#
# Pre-2026-05-05 version diffed only required_status_checks.checks —
# would have missed a UI click that flipped enforce_admins or
# dismiss_stale_reviews. Now compares the full normalized payload so
# any silent rewrite of admin/review/lock/deletion settings trips the
# drift gate.
#
# Exit codes:
#   0 — live state matches the script
#   1 — drift detected (output shows the diff)
#   2 — gh API call failed

set -euo pipefail

REPO="Molecule-AI/molecule-core"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXIT_CODE=0

# Normalise the GET /branches/:b/protection response so we can compare
# against apply.sh's payload. The GET response inflates booleans into
# {url, enabled} sub-objects and bypass list users/apps into full
# user/app objects with avatar_url etc — strip those down to match
# the input shape.
NORMALISE_LIVE='{
  required_status_checks: (
    .required_status_checks
    | { strict: .strict,
        checks: (.checks | map({context}) | sort_by(.context)) }
  ),
  enforce_admins: (
    if (.enforce_admins | type) == "object"
    then .enforce_admins.enabled
    else .enforce_admins end
  ),
  required_pull_request_reviews: (
    .required_pull_request_reviews
    | if . == null then null else
        { required_approving_review_count,
          dismiss_stale_reviews,
          require_code_owner_reviews,
          require_last_push_approval,
          bypass_pull_request_allowances: (
            if .bypass_pull_request_allowances == null then null
            else {
              users: (.bypass_pull_request_allowances.users // [] | map(.login) | sort),
              teams: (.bypass_pull_request_allowances.teams // [] | map(.slug) | sort),
              apps:  (.bypass_pull_request_allowances.apps  // [] | map(.slug) | sort)
            } end
          )
        }
      end
  ),
  restrictions: (
    if .restrictions == null then null
    else { users: (.restrictions.users | map(.login) | sort),
           teams: (.restrictions.teams | map(.slug) | sort),
           apps:  (.restrictions.apps  | map(.slug) | sort) }
    end
  ),
  allow_deletions: (
    if (.allow_deletions | type) == "object" then .allow_deletions.enabled
    else (.allow_deletions // false) end
  ),
  allow_force_pushes: (
    if (.allow_force_pushes | type) == "object" then .allow_force_pushes.enabled
    else (.allow_force_pushes // false) end
  ),
  block_creations: (
    if (.block_creations | type) == "object" then .block_creations.enabled
    else (.block_creations // false) end
  ),
  required_conversation_resolution: (
    if (.required_conversation_resolution | type) == "object"
    then .required_conversation_resolution.enabled
    else (.required_conversation_resolution // false) end
  ),
  required_linear_history: (
    if (.required_linear_history | type) == "object" then .required_linear_history.enabled
    else (.required_linear_history // false) end
  ),
  lock_branch: (
    if (.lock_branch | type) == "object" then .lock_branch.enabled
    else (.lock_branch // false) end
  ),
  allow_fork_syncing: (
    if (.allow_fork_syncing | type) == "object" then .allow_fork_syncing.enabled
    else (.allow_fork_syncing // false) end
  )
}'

# Apply.sh's payload is already in the input shape; we just need to
# canonicalise the checks order and fill in optional fields with their
# defaults so the comparison aligns.
NORMALISE_SCRIPT='{
  required_status_checks: {
    strict: .required_status_checks.strict,
    checks: (.required_status_checks.checks | map({context}) | sort_by(.context))
  },
  enforce_admins: .enforce_admins,
  required_pull_request_reviews: (
    if .required_pull_request_reviews == null then null else
      { required_approving_review_count: .required_pull_request_reviews.required_approving_review_count,
        dismiss_stale_reviews: .required_pull_request_reviews.dismiss_stale_reviews,
        require_code_owner_reviews: (.required_pull_request_reviews.require_code_owner_reviews // false),
        require_last_push_approval: (.required_pull_request_reviews.require_last_push_approval // false),
        bypass_pull_request_allowances: (
          if .required_pull_request_reviews.bypass_pull_request_allowances == null then null
          else {
            users: (.required_pull_request_reviews.bypass_pull_request_allowances.users // [] | sort),
            teams: (.required_pull_request_reviews.bypass_pull_request_allowances.teams // [] | sort),
            apps:  (.required_pull_request_reviews.bypass_pull_request_allowances.apps  // [] | sort)
          } end
        )
      }
    end
  ),
  restrictions: .restrictions,
  allow_deletions: (.allow_deletions // false),
  allow_force_pushes: (.allow_force_pushes // false),
  block_creations: (.block_creations // false),
  required_conversation_resolution: (.required_conversation_resolution // false),
  required_linear_history: (.required_linear_history // false),
  lock_branch: (.lock_branch // false),
  allow_fork_syncing: (.allow_fork_syncing // false)
}'

check_branch() {
  local branch="$1"
  local want
  want=$(bash "$SCRIPT_DIR/apply.sh" --dry-run --branch "$branch" 2>&1 |
    sed -n '/^{$/,/^}$/p' |
    jq -S "$NORMALISE_SCRIPT")
  local have_raw
  if ! have_raw=$(gh api "repos/$REPO/branches/$branch/protection" 2>/dev/null); then
    echo "drift_check: FAIL to fetch $branch protection (gh API error)"
    return 2
  fi
  local have
  have=$(echo "$have_raw" | jq -S "$NORMALISE_LIVE")
  if [[ "$want" != "$have" ]]; then
    echo "=== DRIFT on $branch ==="
    diff <(echo "$want") <(echo "$have") || true
    return 1
  fi
  echo "OK: $branch matches desired state"
}

for b in staging main; do
  if ! check_branch "$b"; then
    EXIT_CODE=1
  fi
done
exit "$EXIT_CODE"
