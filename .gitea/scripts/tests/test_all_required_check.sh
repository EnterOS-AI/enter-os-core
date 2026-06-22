#!/usr/bin/env bash
# test_all_required_check.sh — regression lock for CI / all-required aggregation.
#
# Covers the fail-closed contract of .gitea/scripts/all-required-check.sh:
#   - all-success  → rc=0
#   - failure      → rc=1
#   - skipped      → rc=1
#   - cancelled    → rc=1
#   - odd arg count → rc=1 (caller bug)
#   - empty input  → rc=0 (vacuously true)
#   - anti-mask    → function body has no `|| true` / `|| echo` swallow patterns
#   - anti-inline  → ci.yml references this script, not inline logic
#
# Ref: molecule-core#656 (Phase 4 all-required hard-gate), #2615 (coverage gap).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
SCRIPT="$ROOT/.gitea/scripts/all-required-check.sh"
CI_YML="$ROOT/.gitea/workflows/ci.yml"

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

[ -f "$SCRIPT" ] || fail "script not found: $SCRIPT"
[ -f "$CI_YML" ] || fail "ci.yml not found: $CI_YML"

# Source the script once so later tests can call all_required_check() directly.
# shellcheck source=.gitea/scripts/all-required-check.sh
. "$SCRIPT"

# --- T1: all-success → rc=0 and OK summary ----------------------------------
set +e
out="$("$SCRIPT" "Job A" success "Job B" success 2>&1)"
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "T1: expected rc=0, got $rc (out=$out)"
[[ "$out" == *"OK: all aggregated CI jobs succeeded"* ]] || fail "T1: missing OK summary in: $out"
[[ "$out" == *"CI / Job A = success"* ]] || fail "T1: missing Job A line in: $out"
[[ "$out" == *"CI / Job B = success"* ]] || fail "T1: missing Job B line in: $out"
pass "T1: all-success → rc=0"

# --- T2: one failure → rc=1, error names the failing job ---------------------
set +e
out="$("$SCRIPT" "Job A" success "Job B" failure 2>&1)"
rc=$?
set -e
[ "$rc" -ne 0 ] || fail "T2: expected rc!=0, got $rc (out=$out)"
[[ "$out" == *"::error::aggregated CI job 'Job B' did not succeed (result=failure)"* ]] || fail "T2: missing per-job error in: $out"
[[ "$out" == *"::error::all-required: one or more aggregated CI jobs did not succeed"* ]] || fail "T2: missing final error in: $out"
pass "T2: one failure → rc=1 with named error"

# --- T3: skipped is not success → rc=1 --------------------------------------
set +e
out="$("$SCRIPT" "Job A" success "Job B" skipped 2>&1)"
rc=$?
set -e
[ "$rc" -ne 0 ] || fail "T3: expected rc!=0 for skipped, got $rc (out=$out)"
[[ "$out" == *"::error::aggregated CI job 'Job B' did not succeed (result=skipped)"* ]] || fail "T3: missing skipped error in: $out"
pass "T3: skipped → rc=1"

# --- T4: cancelled is not success → rc=1 ------------------------------------
set +e
out="$("$SCRIPT" "Job A" success "Job B" cancelled 2>&1)"
rc=$?
set -e
[ "$rc" -ne 0 ] || fail "T4: expected rc!=0 for cancelled, got $rc (out=$out)"
[[ "$out" == *"::error::aggregated CI job 'Job B' did not succeed (result=cancelled)"* ]] || fail "T4: missing cancelled error in: $out"
pass "T4: cancelled → rc=1"

# --- T5: multiple failures report each --------------------------------------
set +e
out="$("$SCRIPT" "Job A" failure "Job B" skipped "Job C" cancelled 2>&1)"
rc=$?
set -e
[ "$rc" -ne 0 ] || fail "T5: expected rc!=0, got $rc"
[[ "$out" == *"'Job A' did not succeed"* ]] || fail "T5: missing Job A error"
[[ "$out" == *"'Job B' did not succeed"* ]] || fail "T5: missing Job B error"
[[ "$out" == *"'Job C' did not succeed"* ]] || fail "T5: missing Job C error"
pass "T5: multiple failures → rc=1, all named"

# --- T6: odd argument count → rc=1 (caller bug) -----------------------------
set +e
out="$("$SCRIPT" "Job A" success "Job B" 2>&1)"
rc=$?
set -e
[ "$rc" -ne 0 ] || fail "T6: expected rc!=0 for odd args, got $rc (out=$out)"
[[ "$out" == *"odd number of arguments"* ]] || fail "T6: missing odd-args error in: $out"
pass "T6: odd args → rc=1"

# --- T7: empty input → rc=0 (vacuously true) --------------------------------
set +e
out="$(all_required_check 2>&1)"
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "T7: expected rc=0 for empty args, got $rc (out=$out)"
[[ "$out" == *"OK: all aggregated CI jobs succeeded"* ]] || fail "T7: missing OK summary in: $out"
pass "T7: empty input → rc=0"

# --- T8: anti-mask — function body has no swallow patterns ------------------
body="$(declare -f all_required_check)"
if echo "$body" | grep -qE '\|\| true|\|\| echo|\|\| exit 0|\|\| :'; then
  fail "T8: all_required_check contains a swallow pattern: $body"
fi
if ! echo "$body" | grep -q 'return 1'; then
  fail "T8: all_required_check missing return 1: $body"
fi
pass "T8: no swallow patterns, has return 1"

# --- T9: anti-inline — ci.yml uses the script, not inline check() -----------
grep -q "all-required-check.sh" "$CI_YML" || fail "T9: ci.yml does not reference all-required-check.sh"
if grep -A5 "Verify all aggregated CI jobs succeeded" "$CI_YML" | grep -q 'check()'; then
  fail "T9: ci.yml still contains inline check() function"
fi
pass "T9: ci.yml references script, no inline check()"

echo ""
echo "ALL TESTS PASSED"
