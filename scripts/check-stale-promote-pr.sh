#!/usr/bin/env bash
# scripts/check-stale-promote-pr.sh
#
# Scan open auto-promote PRs (base=main head=staging) for the
# silent-block failure mode that motivated issue #2975:
#   - PR sat for hours with mergeStateStatus=BLOCKED
#   - reviewDecision=REVIEW_REQUIRED (auto-merge armed but waiting
#     on a human approval that never comes)
#
# When found, emit:
#   - GitHub Actions notice/warning lines (workflow summary surface)
#   - Optionally post a comment on the PR (--comment)
#
# Exit code is the count of stale PRs found, capped at 125 so callers
# can detect "alarm fired" via `if ! check-stale-promote-pr.sh; then …`.
# Exit 0 = clean, exit ≥1 = at least N stale PRs need attention.
#
# Used by .github/workflows/auto-promote-stale-alarm.yml. Logic lives
# here (not inline in the workflow YAML) so we can:
#   - Unit-test it with a fixture (see test-check-stale-promote-pr.sh)
#   - Run it ad-hoc by an operator: `scripts/check-stale-promote-pr.sh`
#   - Reuse the same surface in any sibling workflow that needs the same
#     check (SSOT — one detector, many callers).
#
# Requires: `curl`, `jq`. `GITEA_TOKEN` (or `GITHUB_TOKEN` / `GH_TOKEN`
# for back-compat) in the workflow context. Reads `GITHUB_SERVER_URL`
# / `GITEA_API_URL` for the Gitea base, defaulting to
# https://git.moleculesai.app/api/v1.
#
# Post-2026-05-06 (Gitea migration, issue #75): the previous version
# called `gh pr list/view/comment`, all of which hit GitHub.com's
# GraphQL or /api/v3 REST shapes. Gitea exposes /api/v1/ only (no
# GraphQL → 405, no /api/v3 → 404). So this script now talks to the
# Gitea v1 API directly via curl. The fixture-driven unit tests are
# unchanged — they bypass the live fetch via PR_FIXTURE and still pass
# the historical (GitHub-shape) JSON which `detect_stale` consumes.

set -euo pipefail

# -----------------------------------------------------------------------------
# Inputs
# -----------------------------------------------------------------------------

# Threshold beyond which a BLOCKED+REVIEW_REQUIRED promote PR is "stale"
# enough to alarm. 4 hours is the floor: most legitimate gates clear
# inside an hour, so 4× headroom is plenty for slow CI without false-
# alarming. Override via env for tests + edge ops.
STALE_HOURS="${STALE_HOURS:-4}"

# Repo defaults to GITHUB_REPOSITORY (act_runner sets this in workflow
# context). Tests pass --repo explicitly.
REPO="${GITHUB_REPOSITORY:-}"

# Whether to post a comment to the PR. Off by default to avoid noise on
# manual ad-hoc runs; the cron workflow turns it on.
POST_COMMENT="${POST_COMMENT:-false}"

# Where to read the open-PR JSON from. Empty = call Gitea live. Tests
# point this at a fixture file.
PR_FIXTURE="${PR_FIXTURE:-}"

# Where to read "now" from. Empty = real clock. Tests freeze time so
# the staleness math is deterministic.
NOW_OVERRIDE="${NOW_OVERRIDE:-}"

# Gitea API base. act_runner forwards github.server_url as
# GITHUB_SERVER_URL; for the molecule-ai fleet that's
# https://git.moleculesai.app. Append /api/v1 to get the REST root.
# Override directly via GITEA_API_URL for tests / non-default hosts.
GITEA_API_URL="${GITEA_API_URL:-${GITHUB_SERVER_URL:-https://git.moleculesai.app}/api/v1}"

# Token. Workflow context sets GITHUB_TOKEN; we accept GITEA_TOKEN as
# the explicit name and GH_TOKEN for back-compat with operator habits
# from the GitHub era. First non-empty wins.
GITEA_TOKEN="${GITEA_TOKEN:-${GITHUB_TOKEN:-${GH_TOKEN:-}}}"

while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="$2"; shift 2 ;;
    --comment) POST_COMMENT="true"; shift ;;
    --no-comment) POST_COMMENT="false"; shift ;;
    --fixture) PR_FIXTURE="$2"; shift 2 ;;
    --stale-hours) STALE_HOURS="$2"; shift 2 ;;
    -h|--help)
      sed -n '1,/^set /p' "$0" | grep '^# ' | sed 's/^# //'
      exit 0
      ;;
    *) echo "unknown arg: $1" >&2; exit 64 ;;
  esac
done

if [ -z "$REPO" ] && [ -z "$PR_FIXTURE" ]; then
  echo "::error::REPO env (or GITHUB_REPOSITORY) required when no fixture given" >&2
  exit 2
fi

# -----------------------------------------------------------------------------
# Clock helpers — split out so tests can freeze time
# -----------------------------------------------------------------------------

now_epoch() {
  if [ -n "$NOW_OVERRIDE" ]; then
    printf '%s\n' "$NOW_OVERRIDE"
  else
    date -u +%s
  fi
}

# Parse RFC3339 timestamps the way Gitea / GitHub emit them (e.g.
# "2026-05-05T23:15:00Z"). gnu-date uses -d, bsd-date uses -j -f. Cover
# both because the workflow runs on ubuntu-latest (gnu) but operators
# may run this script on macOS (bsd).
to_epoch() {
  local ts="$1"
  # gnu-date path first.
  if date -u -d "$ts" +%s 2>/dev/null; then
    return 0
  fi
  # bsd-date fallback — strip optional fractional seconds before %S.
  local ts_clean="${ts%%.*}"
  ts_clean="${ts_clean%Z}Z"
  date -u -j -f "%Y-%m-%dT%H:%M:%SZ" "$ts_clean" +%s 2>/dev/null || {
    echo "::error::cannot parse timestamp: $ts" >&2
    return 1
  }
}

# -----------------------------------------------------------------------------
# Fetch open auto-promote PRs
# -----------------------------------------------------------------------------

# Gitea v1 returns PRs with the canonical Gitea shape (number, title,
# created_at, html_url, mergeable, state). The previous GitHub-CLI
# version returned a derived `mergeStateStatus` / `reviewDecision`
# pair which only GitHub computes — Gitea doesn't expose them
# natively. Rebuild equivalents:
#
#   mergeStateStatus = BLOCKED  ↔ Gitea: state==open AND mergeable==true
#                                  AND no APPROVED review yet
#                                  (i.e. branch protection is gating
#                                  the auto-merge pending an approval)
#   reviewDecision   = REVIEW_REQUIRED  ↔ Gitea: 0 APPROVED reviews
#
# This mirrors the SAME silent-block failure mode the GitHub version
# detected: auto-merge armed, branch protection requires 1 review,
# nobody's approved yet.
#
# Implementation: pull the open PR list base=main, then for each PR
# pull /pulls/{n}/reviews and synthesize the GitHub-shape JSON the
# rest of the script + the test fixtures consume.
fetch_prs() {
  if [ -n "$PR_FIXTURE" ]; then
    cat "$PR_FIXTURE"
    return 0
  fi
  if [ -z "$GITEA_TOKEN" ]; then
    echo "::error::GITEA_TOKEN / GITHUB_TOKEN unset — cannot fetch PRs from $GITEA_API_URL" >&2
    return 1
  fi
  local prs_json
  prs_json="$(curl --fail-with-body -sS \
    -H "Authorization: token ${GITEA_TOKEN}" \
    -H "Accept: application/json" \
    "${GITEA_API_URL}/repos/${REPO}/pulls?state=open&base=main&limit=50" \
    2>/dev/null)" || {
    echo "::error::Failed to fetch PRs from ${GITEA_API_URL}/repos/${REPO}/pulls" >&2
    return 1
  }

  # Filter to head=staging (the auto-promote shape) and synthesize
  # mergeStateStatus + reviewDecision per PR. Approval count via
  # /pulls/{n}/reviews. Errors fall through to 0-approvals (treated
  # as REVIEW_REQUIRED) preserving the existing "fail-safe — alarm if
  # uncertain" semantic.
  local synthesized="[]"
  while IFS= read -r pr; do
    [ -z "$pr" ] && continue
    [ "$pr" = "null" ] && continue
    local num
    num="$(printf '%s' "$pr" | jq -r '.number')"
    [ -z "$num" ] && continue
    [ "$num" = "null" ] && continue
    local approved_count
    approved_count="$(curl --fail-with-body -sS \
      -H "Authorization: token ${GITEA_TOKEN}" \
      -H "Accept: application/json" \
      "${GITEA_API_URL}/repos/${REPO}/pulls/${num}/reviews" 2>/dev/null \
      | jq '[.[] | select(.state == "APPROVED" and (.dismissed // false) == false)] | length' \
      2>/dev/null || echo 0)"
    local mergeable
    mergeable="$(printf '%s' "$pr" | jq -r '.mergeable')"
    local merge_state="UNKNOWN"
    local review_decision="REVIEW_REQUIRED"
    if [ "$mergeable" = "true" ]; then
      if [ "$approved_count" -ge 1 ]; then
        merge_state="CLEAN"
        review_decision="APPROVED"
      else
        # mergeable but no approving review — exactly the wedge state
        # the alarm targets.
        merge_state="BLOCKED"
        review_decision="REVIEW_REQUIRED"
      fi
    else
      # not mergeable (conflicts, behind, failed checks) — different
      # failure mode, the author owns the fix; the alarm doesn't fire.
      merge_state="DIRTY"
      review_decision="REVIEW_REQUIRED"
    fi
    synthesized="$(printf '%s' "$synthesized" \
      | jq -c --argjson pr "$pr" \
              --arg ms "$merge_state" \
              --arg rd "$review_decision" \
              '. + [{
                 number: $pr.number,
                 title: $pr.title,
                 createdAt: $pr.created_at,
                 mergeStateStatus: $ms,
                 reviewDecision: $rd,
                 url: $pr.html_url
              }]')"
  done < <(printf '%s' "$prs_json" \
    | jq -c '.[] | select(.head.ref == "staging")' 2>/dev/null)

  printf '%s\n' "$synthesized"
}

# -----------------------------------------------------------------------------
# Stale detection
# -----------------------------------------------------------------------------

# Read PR list from stdin, emit one TSV line per stale PR:
#   <num>\t<age_hours>\t<url>\t<title>
# Caller decides what to do (warn, comment, escalate).
detect_stale() {
  local now_ts
  now_ts="$(now_epoch)"
  local stale_seconds=$((STALE_HOURS * 3600))

  jq -r '.[] | [.number, .createdAt, .mergeStateStatus, .reviewDecision, .url, .title] | @tsv' \
    | while IFS=$'\t' read -r num created_at merge_state review_decision url title; do
        # Only alarm on the specific failure mode: BLOCKED + REVIEW_REQUIRED.
        # Other BLOCKED reasons (DIRTY, BEHIND, failed checks) are the
        # author's signal-to-fix; this script targets the silent
        # "no human reviewed yet" wedge specifically.
        [ "$merge_state" = "BLOCKED" ] || continue
        [ "$review_decision" = "REVIEW_REQUIRED" ] || continue

        local created_ts
        created_ts="$(to_epoch "$created_at")" || continue
        local age=$((now_ts - created_ts))
        if [ "$age" -ge "$stale_seconds" ]; then
          local age_h=$((age / 3600))
          printf '%s\t%d\t%s\t%s\n' "$num" "$age_h" "$url" "$title"
        fi
      done
}

# -----------------------------------------------------------------------------
# Reporting
# -----------------------------------------------------------------------------

# Comment body — kept short; the issue body has the full design.
comment_body() {
  local age_h="$1"
  cat <<EOF
⚠️ This auto-promote PR has been BLOCKED on \`REVIEW_REQUIRED\` for **${age_h}h**.

Auto-merge is armed, but main's branch protection requires 1 review and no human has approved. Until someone reviews, the staging→main promote chain is wedged and downstream consumers (canvas builds, tenant redeploys) won't see new code.

**Action**: a human reviewer on \`@Molecule-AI/maintainers\` should approve this PR (or mark it as not ready and close).

Detected by \`scripts/check-stale-promote-pr.sh\` per issue #2975.
EOF
}

post_comment() {
  local pr_num="$1"
  local age_h="$2"
  if [ "$POST_COMMENT" != "true" ]; then
    return 0
  fi
  if [ -z "$GITEA_TOKEN" ]; then
    echo "::warning::GITEA_TOKEN unset — cannot post stale-alarm comment on PR #$pr_num" >&2
    return 0
  fi
  # Idempotency: only one alarm comment per PR. Look for the marker
  # string in existing comments before posting a new one. Gitea's
  # /repos/{owner}/{repo}/issues/{n}/comments returns the same shape
  # for issues + PRs (PRs are issues internally on Gitea, same as
  # GitHub's REST).
  local existing
  existing="$(curl --fail-with-body -sS \
    -H "Authorization: token ${GITEA_TOKEN}" \
    -H "Accept: application/json" \
    "${GITEA_API_URL}/repos/${REPO}/issues/${pr_num}/comments?limit=50" 2>/dev/null \
    | jq -r '.[] | select(.body | test("scripts/check-stale-promote-pr.sh per issue #2975")) | .id' \
    | head -n1)"
  if [ -n "$existing" ]; then
    echo "::notice::PR #$pr_num already has a stale-alarm comment ($existing) — not re-posting"
    return 0
  fi
  local body
  body="$(comment_body "$age_h")"
  if curl --fail-with-body -sS \
      -X POST \
      -H "Authorization: token ${GITEA_TOKEN}" \
      -H "Accept: application/json" \
      -H "Content-Type: application/json" \
      "${GITEA_API_URL}/repos/${REPO}/issues/${pr_num}/comments" \
      -d "$(jq -nc --arg b "$body" '{body: $b}')" \
      >/dev/null 2>&1; then
    echo "::notice::Posted stale-alarm comment on PR #$pr_num (age=${age_h}h)"
  else
    echo "::warning::Failed to POST stale-alarm comment on PR #$pr_num" >&2
  fi
}

# -----------------------------------------------------------------------------
# Main
# -----------------------------------------------------------------------------

stale_count=0
while IFS=$'\t' read -r num age_h url title; do
  [ -n "$num" ] || continue
  stale_count=$((stale_count + 1))
  echo "::warning title=Stale auto-promote PR::PR #$num — BLOCKED on REVIEW_REQUIRED for ${age_h}h. $url"
  {
    echo "## ⚠️ Stale auto-promote PR detected"
    echo
    echo "- PR: #$num — \`$title\`"
    echo "- Age: ${age_h}h"
    echo "- State: BLOCKED on REVIEW_REQUIRED"
    echo "- URL: $url"
    echo
    echo "Auto-merge is armed but waiting on a human review. See issue #2975."
  } >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
  post_comment "$num" "$age_h"
done < <(fetch_prs | detect_stale)

if [ "$stale_count" -eq 0 ]; then
  echo "::notice::No stale auto-promote PRs detected (threshold: ${STALE_HOURS}h)"
fi

# Cap exit code so we don't accidentally break shells that interpret
# >125 as signal-style. 1..N maps to "1..N stale PRs".
exit $(( stale_count > 125 ? 125 : stale_count ))
