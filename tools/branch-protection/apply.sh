#!/usr/bin/env bash
# tools/branch-protection/apply.sh — idempotently apply branch
# protection to molecule-core's `staging` and `main` branches.
#
# Single source of truth for the protection settings. Diff this file
# against the live state (drift_check.sh handles that nightly + on
# every PR that touches this directory).
#
# Why each branch has its OWN payload section instead of a shared
# template: pre-2026-05-05 the script generated both branches from a
# shared template that hard-coded enforce_admins=false,
# dismiss_stale_reviews=true, strict=false, allow_fork_syncing=true,
# and dropped bypass_pull_request_allowances. Live staging had
# enforce_admins=true, dismiss_stale_reviews=false, strict=true,
# allow_fork_syncing=false, and a bypass list. Running the script
# would have silently weakened protection on every dimension at once.
# Per-branch payloads codify the deliberate per-branch policy that
# already lives on the repo, with the script's net contribution
# being ONLY the explicit additions to required_status_checks.
#
# Per memory feedback_dismiss_stale_reviews_blocks_promote.md,
# dismiss_stale_reviews=true silently re-blocks every auto-promote PR
# (cost the user 2.5h once already on staging — confirming we keep
# this OFF on staging is load-bearing for the auto-promote chain).
#
# Usage:
#   tools/branch-protection/apply.sh                # apply both branches
#   tools/branch-protection/apply.sh --dry-run      # show payload only
#   tools/branch-protection/apply.sh --branch staging
#   tools/branch-protection/apply.sh --skip-preflight  # skip check-name validation
#
# Requires: gh CLI authenticated as a repo admin. The script uses gh's
# token (no separate PAT needed).

set -euo pipefail

REPO="Molecule-AI/molecule-core"
DRY_RUN=0
ONLY_BRANCH=""
SKIP_PREFLIGHT=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --branch)  ONLY_BRANCH="$2"; shift 2 ;;
    --skip-preflight) SKIP_PREFLIGHT=1; shift ;;
    -h|--help)
      echo "Usage: $0 [--dry-run] [--branch <name>] [--skip-preflight]"
      exit 0
      ;;
    *) echo "Unknown arg: $1" >&2; exit 1 ;;
  esac
done

# ─── Required-check matrices ──────────────────────────────────────
# Each branch's set is the canonical list of check NAMES (from each
# workflow's job-name). Adding/removing a check here is the place to
# do it. Match docs/e2e-coverage.md.

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

checks_to_json() {
  printf '%s\n' "$1" | jq -Rs '
    split("\n")
    | map(select(length > 0))
    | map({context: ., app_id: -1})
  '
}

# ─── Per-branch payloads (each preserves live-state policy) ───────
# Staging payload — preserves the live values that pre-2026-05-05's
# apply.sh would have silently rewritten:
#   enforce_admins=true, dismiss_stale_reviews=false, strict=true,
#   allow_fork_syncing=false, bypass list = HongmingWang-Rabbit + molecule-ai app.
build_staging_payload() {
  local checks_json
  checks_json=$(checks_to_json "$STAGING_CHECKS")
  jq -n \
    --argjson checks "$checks_json" \
    '{
      required_status_checks: {
        strict: true,
        checks: $checks
      },
      enforce_admins: true,
      required_pull_request_reviews: {
        required_approving_review_count: 1,
        dismiss_stale_reviews: false,
        require_code_owner_reviews: false,
        require_last_push_approval: false,
        bypass_pull_request_allowances: {
          users: ["HongmingWang-Rabbit"],
          teams: [],
          apps: ["molecule-ai"]
        }
      },
      restrictions: null,
      allow_deletions: false,
      allow_force_pushes: false,
      block_creations: false,
      required_conversation_resolution: true,
      required_linear_history: false,
      lock_branch: false,
      allow_fork_syncing: false
    }'
}

# Main payload — preserves the live values:
#   enforce_admins=false, dismiss_stale_reviews=true, strict=true,
#   allow_fork_syncing=false, NO bypass list.
# main intentionally has different settings than staging because main
# is the deploy target — the auto-promote app pushes to main without
# the friction of an admin-bypass list, and stale-review dismissal
# is acceptable here because every change has already cleared
# staging review.
build_main_payload() {
  local checks_json
  checks_json=$(checks_to_json "$MAIN_CHECKS")
  jq -n \
    --argjson checks "$checks_json" \
    '{
      required_status_checks: {
        strict: true,
        checks: $checks
      },
      enforce_admins: false,
      required_pull_request_reviews: {
        required_approving_review_count: 1,
        dismiss_stale_reviews: true,
        require_code_owner_reviews: false,
        require_last_push_approval: false
      },
      restrictions: null,
      allow_deletions: false,
      allow_force_pushes: false,
      block_creations: false,
      required_conversation_resolution: true,
      required_linear_history: false,
      lock_branch: false,
      allow_fork_syncing: false
    }'
}

# ─── R3 preflight: validate every desired check name has at least
# one historical run ──────────────────────────────────────────────
# Pre-fix the script accepted arbitrary strings into
# required_status_checks.checks. A typo like "Canvas Tabs E2E" vs
# "Canvas tabs E2E" → GH accepts → every PR is blocked forever
# waiting for a context that never emits. The preflight hits the
# /commits/{sha}/check-runs endpoint and asserts each desired name
# has at least one matching run. Skippable via --skip-preflight for
# the case where you're adding a brand-new workflow whose first run
# hasn't fired yet.
preflight_check_names() {
  local branch="$1"
  local checks="$2"
  local sha
  sha=$(gh api "repos/$REPO/commits/$branch" --jq '.sha' 2>/dev/null || echo "")
  if [[ -z "$sha" ]]; then
    echo "preflight: WARN cannot resolve $branch tip SHA, skipping check-name validation" >&2
    return 0
  fi
  local known_names
  known_names=$(gh api "repos/$REPO/commits/$sha/check-runs?per_page=100" \
    --jq '.check_runs | map(.name)' 2>/dev/null || echo "[]")
  local missing=()
  while IFS= read -r name; do
    [[ -z "$name" ]] && continue
    if ! echo "$known_names" | jq -e --arg n "$name" 'index($n) != null' >/dev/null; then
      missing+=("$name")
    fi
  done <<< "$checks"
  if [[ ${#missing[@]} -gt 0 ]]; then
    echo "preflight: $branch — these check names are NOT in the historical check-runs for the tip SHA:" >&2
    printf '  - %s\n' "${missing[@]}" >&2
    echo "If they're truly new (workflow added but never run), re-run with --skip-preflight." >&2
    echo "Otherwise typos here will permanently block every PR — fix the names." >&2
    return 1
  fi
}

apply_branch() {
  local branch="$1"
  local checks="$2"
  local payload_fn="$3"
  local payload
  payload=$($payload_fn)
  if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "=== branch: $branch ==="
    echo "$payload" | jq .
    return
  fi
  if [[ "$SKIP_PREFLIGHT" -eq 0 ]]; then
    if ! preflight_check_names "$branch" "$checks"; then
      echo "FAIL: preflight on $branch caught typos or missing workflows. Aborting." >&2
      return 1
    fi
  fi
  echo "Applying branch protection on $branch..."
  printf '%s' "$payload" | gh api -X PUT \
    "repos/$REPO/branches/$branch/protection" \
    --input -
  echo "Applied: $branch"
}

if [[ -z "$ONLY_BRANCH" || "$ONLY_BRANCH" == "staging" ]]; then
  apply_branch staging "$STAGING_CHECKS" build_staging_payload
fi
if [[ -z "$ONLY_BRANCH" || "$ONLY_BRANCH" == "main" ]]; then
  apply_branch main "$MAIN_CHECKS" build_main_payload
fi
