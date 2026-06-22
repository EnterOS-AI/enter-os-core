#!/usr/bin/env bash
# scripts/test-check-stale-promote-pr.sh
#
# Exhaustive bash unit tests for check-stale-promote-pr.sh.
# Goal: 100% branch coverage on the detector logic.
#
# Each case writes a fixture JSON, freezes the clock with NOW_OVERRIDE,
# runs the script with --fixture + --no-comment (so we don't try to
# actually call `gh pr comment`), and asserts on stdout/exit code.
#
# Run: bash scripts/test-check-stale-promote-pr.sh
# Expected: "All N tests passed" + exit 0.

set -euo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/check-stale-promote-pr.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0

# ─────────────────────────────────────────────────────────────────────────────
# Helpers
# ─────────────────────────────────────────────────────────────────────────────

# Frozen "now" — 2026-05-06T05:00:00Z. Compute dynamically so the
# tests stay correct regardless of platform-specific date semantics
# (gnu vs bsd) and any author math errors on the epoch.
if FROZEN_NOW="$(date -u -d '2026-05-06T05:00:00Z' +%s 2>/dev/null)"; then
  :  # gnu-date worked
elif FROZEN_NOW="$(date -u -j -f '%Y-%m-%dT%H:%M:%SZ' '2026-05-06T05:00:00Z' +%s 2>/dev/null)"; then
  :  # bsd-date worked
else
  echo "FATAL: cannot compute FROZEN_NOW on this platform" >&2
  exit 1
fi

run_script() {
  # Args: <fixture-file>
  # Returns stdout + exit code via a known marker.
  local fixture="$1"
  shift
  set +e
  NOW_OVERRIDE="$FROZEN_NOW" \
    POST_COMMENT="false" \
    bash "$SCRIPT" --fixture "$fixture" "$@" 2>&1
  local rc=$?
  set -e
  echo "EXIT_CODE=$rc"
}

assert_pass() {
  local name="$1"
  local got="$2"
  local want_pattern="$3"
  if printf '%s' "$got" | grep -qE "$want_pattern"; then
    PASS=$((PASS + 1))
    printf '  ✓ %s\n' "$name"
  else
    FAIL=$((FAIL + 1))
    printf '  ✗ %s\n    want pattern: %s\n    got:\n%s\n' "$name" "$want_pattern" "$got"
  fi
}

assert_no_match() {
  local name="$1"
  local got="$2"
  local bad_pattern="$3"
  if printf '%s' "$got" | grep -qE "$bad_pattern"; then
    FAIL=$((FAIL + 1))
    printf '  ✗ %s\n    bad pattern matched: %s\n    got:\n%s\n' "$name" "$bad_pattern" "$got"
  else
    PASS=$((PASS + 1))
    printf '  ✓ %s\n' "$name"
  fi
}

# ─────────────────────────────────────────────────────────────────────────────
# Test cases
# ─────────────────────────────────────────────────────────────────────────────

echo "1. Empty PR list — clean exit"
echo '[]' > "$TMP/empty.json"
got=$(run_script "$TMP/empty.json")
assert_pass "empty-no-warning" "$got" "No stale auto-promote PRs detected"
assert_pass "empty-exit-zero" "$got" "EXIT_CODE=0"

echo
echo "2. Single PR, BLOCKED+REVIEW_REQUIRED, 5h old — fires alarm"
cat > "$TMP/stale1.json" <<EOF
[{
  "number": 2963,
  "title": "staging → main",
  "createdAt": "2026-05-06T00:00:00Z",
  "mergeStateStatus": "BLOCKED",
  "reviewDecision": "REVIEW_REQUIRED",
  "url": "https://github.com/test/test/pull/2963"
}]
EOF
got=$(run_script "$TMP/stale1.json")
assert_pass "stale1-warning" "$got" "Stale auto-promote PR"
assert_pass "stale1-pr-number" "$got" "PR #2963"
assert_pass "stale1-age" "$got" "for 5h"
assert_pass "stale1-exit-1" "$got" "EXIT_CODE=1"

echo
echo "3. Same PR but only 3h old — under threshold, NO alarm"
cat > "$TMP/young.json" <<EOF
[{
  "number": 100,
  "title": "fresh promote",
  "createdAt": "2026-05-06T02:00:00Z",
  "mergeStateStatus": "BLOCKED",
  "reviewDecision": "REVIEW_REQUIRED",
  "url": "https://github.com/test/test/pull/100"
}]
EOF
got=$(run_script "$TMP/young.json")
assert_pass "young-no-alarm" "$got" "No stale auto-promote PRs"
assert_pass "young-exit-zero" "$got" "EXIT_CODE=0"
assert_no_match "young-no-warning" "$got" "Stale auto-promote PR"

echo
echo "4. PR is BLOCKED but for the wrong reason (DIRTY, not REVIEW_REQUIRED)"
cat > "$TMP/dirty.json" <<EOF
[{
  "number": 200,
  "title": "needs rebase",
  "createdAt": "2026-05-06T00:00:00Z",
  "mergeStateStatus": "BLOCKED",
  "reviewDecision": "APPROVED",
  "url": "https://github.com/test/test/pull/200"
}]
EOF
got=$(run_script "$TMP/dirty.json")
assert_pass "dirty-no-alarm" "$got" "No stale auto-promote PRs"
assert_pass "dirty-exit-zero" "$got" "EXIT_CODE=0"

echo
echo "5. PR is APPROVED but mergeStateStatus is CLEAN — NOT alarming"
cat > "$TMP/clean.json" <<EOF
[{
  "number": 300,
  "title": "all green",
  "createdAt": "2026-05-06T00:00:00Z",
  "mergeStateStatus": "CLEAN",
  "reviewDecision": "APPROVED",
  "url": "https://github.com/test/test/pull/300"
}]
EOF
got=$(run_script "$TMP/clean.json")
assert_pass "clean-no-alarm" "$got" "No stale auto-promote PRs"

echo
echo "6. Multiple PRs — only the BLOCKED+REVIEW_REQUIRED+old one alarms"
cat > "$TMP/mixed.json" <<EOF
[
  {
    "number": 100,
    "title": "fresh",
    "createdAt": "2026-05-06T04:00:00Z",
    "mergeStateStatus": "BLOCKED",
    "reviewDecision": "REVIEW_REQUIRED",
    "url": "https://x/100"
  },
  {
    "number": 200,
    "title": "stale + alarming",
    "createdAt": "2026-05-05T20:00:00Z",
    "mergeStateStatus": "BLOCKED",
    "reviewDecision": "REVIEW_REQUIRED",
    "url": "https://x/200"
  },
  {
    "number": 300,
    "title": "approved + clean",
    "createdAt": "2026-05-05T20:00:00Z",
    "mergeStateStatus": "CLEAN",
    "reviewDecision": "APPROVED",
    "url": "https://x/300"
  }
]
EOF
got=$(run_script "$TMP/mixed.json")
assert_pass "mixed-only-200" "$got" "PR #200"
assert_no_match "mixed-not-100" "$got" "PR #100"
assert_no_match "mixed-not-300" "$got" "PR #300"
assert_pass "mixed-exit-1" "$got" "EXIT_CODE=1"

echo
echo "7. Custom STALE_HOURS via --stale-hours overrides threshold"
got=$(run_script "$TMP/young.json" --stale-hours 1)
assert_pass "custom-threshold-fires" "$got" "PR #100"
assert_pass "custom-threshold-exit-1" "$got" "EXIT_CODE=1"

echo
echo "8. Two stale PRs — exit code reflects count"
cat > "$TMP/two-stale.json" <<EOF
[
  {
    "number": 200,
    "title": "stale-A",
    "createdAt": "2026-05-05T20:00:00Z",
    "mergeStateStatus": "BLOCKED",
    "reviewDecision": "REVIEW_REQUIRED",
    "url": "https://x/200"
  },
  {
    "number": 201,
    "title": "stale-B",
    "createdAt": "2026-05-05T19:00:00Z",
    "mergeStateStatus": "BLOCKED",
    "reviewDecision": "REVIEW_REQUIRED",
    "url": "https://x/201"
  }
]
EOF
got=$(run_script "$TMP/two-stale.json")
assert_pass "two-stale-exit-2" "$got" "EXIT_CODE=2"

echo
echo "9. Help text is shown for --help"
set +e
help_out=$(bash "$SCRIPT" --help 2>&1)
help_rc=$?
set -e
assert_pass "help-exits-zero" "EXIT_CODE=$help_rc" "EXIT_CODE=0"
assert_pass "help-mentions-issue" "$help_out" "issue #2975"

echo
echo "10. Unknown arg exits 64 (EX_USAGE)"
set +e
bad_out=$(bash "$SCRIPT" --bogus 2>&1)
bad_rc=$?
set -e
assert_pass "unknown-arg-rc" "EXIT_CODE=$bad_rc" "EXIT_CODE=64"

echo
echo "11. Missing repo + missing fixture exits 2"
set +e
out=$(REPO="" bash "$SCRIPT" 2>&1)
rc=$?
set -e
assert_pass "no-repo-exit-2" "EXIT_CODE=$rc" "EXIT_CODE=2"

# ─────────────────────────────────────────────────────────────────────────────
# Summary
# ─────────────────────────────────────────────────────────────────────────────

echo
echo "─────────────────────────────────────────────"
echo "Tests:  $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
echo "All tests passed."
