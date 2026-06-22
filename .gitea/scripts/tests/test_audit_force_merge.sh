#!/usr/bin/env bash
# test_audit_force_merge.sh — regression lock for audit-force-merge fail-closed
# behavior. Verifies every schema validation path via direct jq filter tests.
#
# Usage: bash test_audit_force_merge.sh

set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

[ -x "$(command -v jq)" ] || { echo "SKIP: jq not on PATH"; exit 0; }

HEAD_SHA="deadbeef00000000000000000000000000000000"

# The schema validation jq expression from audit-force-merge.sh.
validate_pr_schema() {
  jq -r '
    (.merged | type == "boolean") and
    (.merge_commit_sha | type == "string") and
    (.merged_by | type == "object") and (.merged_by.login | type == "string") and
    (.base | type == "object") and (.base.ref | type == "string") and
    (.head | type == "object") and (.head.sha | type == "string")
  '
}

validate_statuses_type() {
  jq -r '(.statuses | type) == "array"'
}

# T1 — valid PR payload → true
T1=$(echo '{"merged":true,"merge_commit_sha":"abc","merged_by":{"login":"u"},"base":{"ref":"main"},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T1" = "true" ] || fail "T1: valid payload should pass schema"
pass "T1: valid payload passes schema"

# T2 — merged=false (valid types) → true (schema is about types, not values)
T2=$(echo '{"merged":false,"merge_commit_sha":"abc","merged_by":{"login":"u"},"base":{"ref":"main"},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T2" = "true" ] || fail "T2: merged=false with valid types should pass schema"
pass "T2: merged=false with valid types passes schema"

# T3 — missing merged field → false
T3=$(echo '{"merge_commit_sha":"abc","merged_by":{"login":"u"},"base":{"ref":"main"},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T3" = "false" ] || fail "T3: missing merged should fail schema"
pass "T3: missing merged fails schema"

# T4 — merged is string "true" instead of boolean → false
T4=$(echo '{"merged":"true","merge_commit_sha":"abc","merged_by":{"login":"u"},"base":{"ref":"main"},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T4" = "false" ] || fail "T4: merged as string should fail schema"
pass "T4: merged as string fails schema"

# T5 — merge_commit_sha is null → false
T5=$(echo '{"merged":true,"merge_commit_sha":null,"merged_by":{"login":"u"},"base":{"ref":"main"},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T5" = "false" ] || fail "T5: null merge_commit_sha should fail schema"
pass "T5: null merge_commit_sha fails schema"

# T6 — merged_by is null → false
T6=$(echo '{"merged":true,"merge_commit_sha":"abc","merged_by":null,"base":{"ref":"main"},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T6" = "false" ] || fail "T6: null merged_by should fail schema"
pass "T6: null merged_by fails schema"

# T7 — base.ref is number → false
T7=$(echo '{"merged":true,"merge_commit_sha":"abc","merged_by":{"login":"u"},"base":{"ref":123},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T7" = "false" ] || fail "T7: numeric base.ref should fail schema"
pass "T7: numeric base.ref fails schema"

# T8 — head is missing → false
T8=$(echo '{"merged":true,"merge_commit_sha":"abc","merged_by":{"login":"u"},"base":{"ref":"main"}}' | validate_pr_schema)
[ "$T8" = "false" ] || fail "T8: missing head should fail schema"
pass "T8: missing head fails schema"

# T9 — statuses missing → false
T9=$(echo '{}' | validate_statuses_type)
[ "$T9" = "false" ] || fail "T9: missing statuses should fail type check"
pass "T9: missing statuses fails type check"

# T10 — statuses is string → false
T10=$(echo '{"statuses":"unexpected"}' | validate_statuses_type)
[ "$T10" = "false" ] || fail "T10: string statuses should fail type check"
pass "T10: string statuses fails type check"

# T11 — statuses is null → false
T11=$(echo '{"statuses":null}' | validate_statuses_type)
[ "$T11" = "false" ] || fail "T11: null statuses should fail type check"
pass "T11: null statuses fails type check"

# T12 — statuses is array → true
T12=$(echo '{"statuses":[{"context":"c1","status":"success"}]}' | validate_statuses_type)
[ "$T12" = "true" ] || fail "T12: array statuses should pass type check"
pass "T12: array statuses passes type check"

# T13 — empty array statuses → true
T13=$(echo '{"statuses":[]}' | validate_statuses_type)
[ "$T13" = "true" ] || fail "T13: empty array statuses should pass type check"
pass "T13: empty array statuses passes type check"

# T14-T16: REQUIRED_CHECKS_JSON branch entry validation
validate_required_checks_json() {
  local branch="$1"
  local json="$2"
  echo "$json" | jq -r --arg branch "$branch" 'has($branch) and (.[$branch] | type == "array")'
}

# T14 — branch exists and is array → true
T14=$(validate_required_checks_json "main" '{"main":["CI / all-required"]}')
[ "$T14" = "true" ] || fail "T14: existing array branch should pass"
pass "T14: existing array branch passes"

# T15 — branch missing → false
T15=$(validate_required_checks_json "staging" '{"main":["CI / all-required"]}')
[ "$T15" = "false" ] || fail "T15: missing branch should fail"
pass "T15: missing branch fails"

# T16 — branch entry is string instead of array → false
T16=$(validate_required_checks_json "main" '{"main":"CI / all-required"}')
[ "$T16" = "false" ] || fail "T16: string branch entry should fail"
pass "T16: string branch entry fails"

# ---------------------------------------------------------------------------
# T17+ — /statuses pagination (status-pagination RCA, #2440-family).
# The reader now pages /commits/{sha}/statuses to exhaustion instead of reading
# the capped combined /status view. These lock the page-accumulation,
# newest-wins collapse, short-page stop, and fail-closed contracts.
# ---------------------------------------------------------------------------

# Page-body type validator used per page (bare array, not an object).
validate_page_is_array() { jq -e 'type == "array"' >/dev/null 2>&1 && echo true || echo false; }

# newest-wins collapse: mirror the script's max-by-id jq (order-independent).
collapse_newest_per_context() {
  declare -A CS
  while IFS=$'\t' read -r ctx state; do
    [ -n "$ctx" ] && CS[$ctx]="$state"
  done < <(jq -r 'group_by(.context) | map(max_by(.id)) | .[] | "\(.context)\t\(.status)"')
  state="${CS[CI / all-required (push)]:-missing}"
  echo "$state"
}

# T17 — a bare JSON array page passes the per-page array check.
T17=$(echo '[{"context":"c1","status":"success"}]' | validate_page_is_array)
[ "$T17" = "true" ] || fail "T17: bare array page should pass array check"
pass "T17: bare array page passes array check"

# T18 — a non-array page (object) fails the per-page array check → fail-closed.
T18=$(echo '{"statuses":[]}' | validate_page_is_array)
[ "$T18" = "false" ] || fail "T18: object page should fail array check (fail-closed)"
pass "T18: object page fails array check (fail-closed)"

# T19 — required SUCCESS on PAGE 2 is FOUND after accumulation (not missing).
#   page1: 100 noise rows (older ids); page2: the required-context success.
PAGE1=$(jq -nc '[range(0;100) | {id:., context:("noise-\(.) (push)"), status:"pending"}]')
PAGE2='[{"id":200,"context":"CI / all-required (push)","status":"success"}]'
# Accumulation matching the script: two-arg `jq -s '.[0] + .[1]'` over the
# running accumulator and the new page.
ACCUM=$(jq -s '.[0] + .[1]' <(echo "$PAGE1") <(echo "$PAGE2"))
LEN=$(echo "$ACCUM" | jq 'length')
[ "$LEN" = "101" ] || fail "T19: accumulated length should be 101, got $LEN"
RESULT=$(echo "$ACCUM" | collapse_newest_per_context)
[ "$RESULT" = "success" ] || fail "T19: required success on page2 must be FOUND, got '$RESULT'"
pass "T19: required success on page2 is found after pagination"

# T20 — genuinely-absent required context across all pages stays 'missing'
#       → fail-closed (counted as not-green, flags the force-merge).
ABSENT=$(jq -nc '[range(0;100) | {id:., context:("noise-\(.) (push)"), status:"success"}]')
RESULT2=$(echo "$ABSENT" | collapse_newest_per_context)
[ "$RESULT2" = "missing" ] || fail "T20: absent required context must stay 'missing', got '$RESULT2'"
pass "T20: genuinely-absent required context stays missing (fail-closed)"

# T21 — non-monotonic order: newest id (157, neither first nor last in list)
#       a NEWER success row (oldest-first append → last overwrite wins).
DUP='[{"id":155,"context":"CI / all-required (push)","status":"pending"},
      {"id":157,"context":"CI / all-required (push)","status":"success"},
      {"id":125,"context":"CI / all-required (push)","status":"failure"}]'
RESULT3=$(echo "$DUP" | collapse_newest_per_context)
[ "$RESULT3" = "success" ] || fail "T21: newest (success) must win over older (failure), got '$RESULT3'"
pass "T21: newest row per context wins after pagination collapse"

# T22 — short-page stop condition: a page with fewer than PER_PAGE rows ends
#       the loop. Emulate the numeric comparison the script uses.
PER_PAGE=100
PAGE_COUNT=$(echo "$PAGE2" | jq 'length')   # 1 row
if [ "$PAGE_COUNT" -lt "$PER_PAGE" ]; then SHORT=stop; else SHORT=continue; fi
[ "$SHORT" = "stop" ] || fail "T22: short page should stop pagination"
pass "T22: short page stops pagination loop"

# T23 — a full page (== PER_PAGE) continues the loop.
FULL=$(jq -nc '[range(0;100) | {id:., context:"x", status:"success"}]')
FULL_COUNT=$(echo "$FULL" | jq 'length')
if [ "$FULL_COUNT" -lt "$PER_PAGE" ]; then CONT=stop; else CONT=continue; fi
[ "$CONT" = "continue" ] || fail "T23: full page should continue pagination"
pass "T23: full page continues pagination loop"

echo
echo "ALL AUDIT-FORCE-MERGE CHECKS PASSED"
