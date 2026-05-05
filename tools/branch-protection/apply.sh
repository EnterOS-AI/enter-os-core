#!/usr/bin/env bash
# tools/branch-protection/apply.sh — idempotently apply branch
# protection to molecule-core's `staging` and `main` branches.
#
# Why a script: GitHub's branch protection lives in repo settings, so
# changes are usually clicked through the UI and lost between admins.
# This script makes the config reproducible — diff it against the live
# state, change the file, run it, done. Single source of truth that
# shows up in code review.
#
# Usage:
#   tools/branch-protection/apply.sh                # apply both branches
#   tools/branch-protection/apply.sh --dry-run      # show payload only
#   tools/branch-protection/apply.sh --branch staging
#
# Requires: gh CLI authenticated as a repo admin. The script uses gh's
# token (no separate PAT needed).

set -euo pipefail

REPO="Molecule-AI/molecule-core"
DRY_RUN=0
ONLY_BRANCH=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --branch)  ONLY_BRANCH="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: $0 [--dry-run] [--branch <name>]"
      exit 0
      ;;
    *) echo "Unknown arg: $1" >&2; exit 1 ;;
  esac
done

# Required-check matrices. Each branch's set is the canonical list of
# check NAMES (from each workflow's job-name). Adding/removing a check
# here is the place to do it. Match docs/e2e-coverage.md.
#
# Why staging gets E2E API + Canvas E2E (this PR's addition): both
# already use the always-emit pattern (path-filter no-ops emit SUCCESS),
# so making them required can't deadlock a PR that doesn't touch their
# paths. The other E2Es (SaaS, External) need a refactor to that
# pattern before they can be required — tracked as follow-up.

read -r -d '' STAGING_CHECKS <<'EOF' || true
Analyze (go)
Analyze (javascript-typescript)
Analyze (python)
Canvas (Next.js)
Canvas tabs E2E
Detect changes
E2E API Smoke Test
Platform (Go)
Python Lint & Test
Scan diff for credential-shaped strings
Shellcheck (E2E scripts)
EOF

read -r -d '' MAIN_CHECKS <<'EOF' || true
Analyze (go)
Analyze (javascript-typescript)
Analyze (python)
Canvas (Next.js)
Canvas tabs E2E
Detect changes
E2E API Smoke Test
PR-built wheel + import smoke
Platform (Go)
Python Lint & Test
Scan diff for credential-shaped strings
Shellcheck (E2E scripts)
EOF

build_payload() {
  local checks="$1"
  local require_reviews="$2"  # true / false
  local checks_json
  checks_json=$(printf '%s\n' "$checks" | jq -Rs '
    split("\n")
    | map(select(length > 0))
    | map({context: ., app_id: -1})
  ')
  jq -n \
    --argjson checks "$checks_json" \
    --argjson reviews "$require_reviews" \
    '{
      required_status_checks: {
        strict: false,
        checks: $checks
      },
      enforce_admins: false,
      required_pull_request_reviews: (
        if $reviews then
          { required_approving_review_count: 1, dismiss_stale_reviews: true }
        else null end
      ),
      restrictions: null,
      allow_deletions: false,
      allow_force_pushes: false,
      block_creations: false,
      required_conversation_resolution: true,
      required_linear_history: false,
      lock_branch: false,
      allow_fork_syncing: true
    }'
}

apply_branch() {
  local branch="$1"
  local checks="$2"
  local require_reviews="$3"
  local payload
  payload=$(build_payload "$checks" "$require_reviews")
  if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "=== branch: $branch ==="
    echo "$payload" | jq .
    return
  fi
  echo "Applying branch protection on $branch..."
  printf '%s' "$payload" | gh api -X PUT \
    "repos/$REPO/branches/$branch/protection" \
    --input -
  echo "Applied: $branch"
}

if [[ -z "$ONLY_BRANCH" || "$ONLY_BRANCH" == "staging" ]]; then
  apply_branch staging "$STAGING_CHECKS" true
fi
if [[ -z "$ONLY_BRANCH" || "$ONLY_BRANCH" == "main" ]]; then
  apply_branch main "$MAIN_CHECKS" true
fi
