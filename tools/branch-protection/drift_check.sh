#!/usr/bin/env bash
# tools/branch-protection/drift_check.sh — compare the live branch
# protection on staging + main against what apply.sh would set. Used
# by branch-protection-drift.yml (cron) to catch out-of-band UI edits.
#
# Exit codes:
#   0 — live state matches the script
#   1 — drift detected (output shows the diff)
#   2 — gh API call failed

set -euo pipefail

REPO="Molecule-AI/molecule-core"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXIT_CODE=0

check_branch() {
  local branch="$1"
  local want
  want=$(bash "$SCRIPT_DIR/apply.sh" --dry-run --branch "$branch" 2>&1 |
    sed -n '/^{$/,/^}$/p' |
    jq -S '.required_status_checks.checks | map(.context) | sort')
  local have
  if ! have=$(gh api "repos/$REPO/branches/$branch/protection/required_status_checks" 2>/dev/null |
    jq -S '.checks | map(.context) | sort'); then
    echo "drift_check: FAIL to fetch $branch protection (gh API error)"
    return 2
  fi
  if [[ "$want" != "$have" ]]; then
    echo "=== DRIFT on $branch ==="
    echo "want:"; echo "$want"
    echo "have:"; echo "$have"
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
