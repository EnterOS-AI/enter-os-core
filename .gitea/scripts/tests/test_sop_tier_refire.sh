#!/usr/bin/env bash
# Tests for sop-tier-refire.{yml,sh} — internal#292.
#
# Behavior matrix:
#
#   T1: PR open + APPROVED via tier:low → script invokes sop-tier-check
#       and POSTs status=success.
#   T2: PR open + missing tier label → sop-tier-check exits non-zero;
#       refire POSTs status=failure (description mentions failure).
#   T3: PR open + tier:low but NO approving reviews → sop-tier-check
#       exits non-zero; refire POSTs status=failure.
#   T4: PR CLOSED → refire exits 0 with no status POST (no-op on closed).
#   T5: Rate-limit — recent status update within 30s → refire skips,
#       no new POST.
#   T6 (yaml-lint): workflow `if:` expression contains author_association
#       gate + slash-command-trigger gate + PR-not-issue gate.
#   T7 (yaml-lint): workflow file is parseable YAML.
#
# Tests T1-T5 run the real script against a local-fixture HTTP server
# (python http.server with a stub handler — `tests/_refire_fixture.py`)
# so the script's Gitea API calls hit the fixture, not the real Gitea.
#
# Tests T6/T7 are pure YAML checks against the workflow file.
#
# Hostile-self-review (per feedback_assert_exact_not_substring):
# this test MUST FAIL if the workflow or script is absent. Verified by
# running the test before the files exist (covered in the PR body).

set -euo pipefail

THIS_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_DIR="$(cd "$THIS_DIR/.." && pwd)"
WORKFLOW_DIR="$(cd "$THIS_DIR/../../workflows" && pwd)"
WORKFLOW="$WORKFLOW_DIR/sop-tier-refire.yml"
DISPATCH_WORKFLOW="$WORKFLOW_DIR/review-refire-comments.yml"
SCRIPT="$SCRIPT_DIR/sop-tier-refire.sh"

PASS=0
FAIL=0
FAILED_TESTS=""

assert_eq() {
  local label="$1"
  local expected="$2"
  local got="$3"
  if [ "$expected" = "$got" ]; then
    echo "  PASS  $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL  $label"
    echo "        expected: <$expected>"
    echo "        got:      <$got>"
    FAIL=$((FAIL + 1))
    FAILED_TESTS="${FAILED_TESTS} ${label}"
  fi
}

assert_contains() {
  local label="$1"
  local needle="$2"
  local haystack="$3"
  if printf '%s' "$haystack" | grep -qF "$needle"; then
    echo "  PASS  $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL  $label"
    echo "        needle:    <$needle>"
    echo "        haystack:  <$(printf '%s' "$haystack" | head -c 400)>"
    FAIL=$((FAIL + 1))
    FAILED_TESTS="${FAILED_TESTS} ${label}"
  fi
}

assert_file_exists() {
  local label="$1"
  local path="$2"
  if [ -f "$path" ]; then
    echo "  PASS  $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL  $label (not found: $path)"
    FAIL=$((FAIL + 1))
    FAILED_TESTS="${FAILED_TESTS} ${label}"
  fi
}

# Existence (foundation — every other test depends on these)
echo
echo "== existence =="
assert_file_exists "workflow file exists"  "$WORKFLOW"
assert_file_exists "dispatcher workflow file exists" "$DISPATCH_WORKFLOW"
assert_file_exists "script file exists"    "$SCRIPT"
if [ "$FAIL" -gt 0 ]; then
  echo
  echo "------"
  echo "PASS=$PASS FAIL=$FAIL (existence)"
  echo "Cannot proceed without these files."
  exit 1
fi

# T6 / T7 — workflow YAML structure
echo
echo "== T6/T7 workflow yaml =="

# YAML parseability
PARSE_OUT=$(python3 -c 'import sys,yaml;yaml.safe_load(open(sys.argv[1]).read());print("ok")' "$WORKFLOW" 2>&1 || true)
assert_eq "T7 workflow parses as YAML" "ok" "$PARSE_OUT"

# The old per-workflow issue_comment listener caused queue storms because
# Gitea queues jobs before evaluating job-level `if:`. The script remains,
# but comment-triggered refires route through the single dispatcher.
WORKFLOW_CONTENT=$(cat "$WORKFLOW")
if printf '%s' "$WORKFLOW_CONTENT" | grep -q '^  issue_comment:'; then
  echo "  FAIL  T6a manual fallback workflow must not listen on issue_comment"
  FAIL=$((FAIL + 1))
  FAILED_TESTS="${FAILED_TESTS} T6a"
else
  echo "  PASS  T6a manual fallback workflow does not listen on issue_comment"
  PASS=$((PASS + 1))
fi
assert_contains "T6b workflow exposes workflow_dispatch" \
  "workflow_dispatch" "$WORKFLOW_CONTENT"
assert_contains "T6c workflow documents unsupported manual inputs" \
  "workflow_dispatch inputs" "$WORKFLOW_CONTENT"
# Does NOT check out PR HEAD (security)
if grep -q 'ref: \${{ github.event.pull_request.head' "$WORKFLOW"; then
  echo "  FAIL  T6d workflow MUST NOT check out PR head (security)"
  FAIL=$((FAIL + 1))
  FAILED_TESTS="${FAILED_TESTS} T6d"
else
  echo "  PASS  T6d workflow does not check out PR head"
  PASS=$((PASS + 1))
fi

DISPATCH_PARSE_OUT=$(python3 -c 'import sys,yaml;yaml.safe_load(open(sys.argv[1]).read());print("ok")' "$DISPATCH_WORKFLOW" 2>&1 || true)
assert_eq "T6e dispatcher workflow parses as YAML" "ok" "$DISPATCH_PARSE_OUT"
DISPATCH_CONTENT=$(cat "$DISPATCH_WORKFLOW")
assert_contains "T6f dispatcher listens on issue_comment" \
  "issue_comment" "$DISPATCH_CONTENT"
assert_contains "T6g dispatcher handles /qa-recheck" \
  "/qa-recheck" "$DISPATCH_CONTENT"
assert_contains "T6h dispatcher handles /security-recheck" \
  "/security-recheck" "$DISPATCH_CONTENT"
assert_contains "T6i dispatcher handles /refire-tier-check" \
  "/refire-tier-check" "$DISPATCH_CONTENT"

# T1-T5 — script behavior against a local Gitea-fixture
echo
echo "== T1-T5 script behavior (vs local fixture) =="

# Spin up the fixture HTTP server.
FIXTURE_DIR=$(mktemp -d)
trap 'rm -rf "$FIXTURE_DIR"; [ -n "${FIX_PID:-}" ] && kill "$FIX_PID" 2>/dev/null || true' EXIT
FIXTURE_PY="$THIS_DIR/_refire_fixture.py"
if [ ! -f "$FIXTURE_PY" ]; then
  echo "::error::fixture server $FIXTURE_PY missing"
  exit 1
fi

FIX_LOG="$FIXTURE_DIR/fixture.log"
FIX_STATE_DIR="$FIXTURE_DIR/state"
mkdir -p "$FIX_STATE_DIR"

# Find an unused port.
FIX_PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')

FIXTURE_STATE_DIR="$FIX_STATE_DIR" python3 "$FIXTURE_PY" "$FIX_PORT" \
  >"$FIX_LOG" 2>&1 &
FIX_PID=$!

# Wait for fixture readiness.
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:${FIX_PORT}/_ping" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
if ! curl -fsS "http://127.0.0.1:${FIX_PORT}/_ping" >/dev/null 2>&1; then
  echo "::error::fixture server failed to start. Log:"
  cat "$FIX_LOG"
  exit 1
fi

# Helper: set fixture state for a scenario, then run the script.
# tier_result is one of: pass | fail_no_label | fail_no_approvals.
# The refire script's tier-check invocation is mocked because the real
# sop-tier-check.sh uses bash 4+ associative arrays — incompatible with
# the macOS bash 3.2 dev shell. Linux Gitea runners use bash 4/5 so
# production runs the real script. The mock exercises the success +
# failure branches of refire's status-POST glue.
run_scenario() {
  local scenario="$1"
  local tier_result="${2:-pass}"
  echo "$scenario" >"$FIX_STATE_DIR/scenario"
  : >"$FIX_STATE_DIR/posted_statuses.jsonl"  # clear status log

  local out
  set +e
  out=$(
    PATH="$FIXTURE_DIR/bin:$PATH" \
    GITEA_TOKEN="fixture-token" \
    GITEA_HOST="fixture.local" \
    REPO="molecule-ai/molecule-core" \
    PR_NUMBER="999" \
    COMMENT_AUTHOR="test-runner" \
    SOP_REFIRE_DISABLE_RATE_LIMIT="1" \
    SOP_REFIRE_TIER_CHECK_SCRIPT="$THIS_DIR/_mock_tier_check.sh" \
    MOCK_TIER_RESULT="$tier_result" \
    FIXTURE_PORT="$FIX_PORT" \
    bash "$SCRIPT" 2>&1
  )
  local rc=$?
  set -e
  echo "$out" >"$FIX_STATE_DIR/last_run.log"
  echo "$rc" >"$FIX_STATE_DIR/last_rc"
}

# Install a curl shim that rewrites https://fixture.local → http://127.0.0.1:$PORT
# Use bash prefix-strip (${var#prefix}) — it sidesteps the `/` delimiter
# confusion of ${var/pattern/replacement}.
mkdir -p "$FIXTURE_DIR/bin"
cat >"$FIXTURE_DIR/bin/curl" <<SHIM
#!/usr/bin/env bash
# Test shim: rewrite https://fixture.local/* -> http://127.0.0.1:${FIX_PORT}/*
# The fixture doesn't authenticate; -H Authorization passes through harmlessly.
new_args=()
for a in "\$@"; do
  if [[ "\$a" == https://fixture.local/* ]]; then
    rest="\${a#https://fixture.local}"
    a="http://127.0.0.1:${FIX_PORT}\${rest}"
  fi
  new_args+=("\$a")
done
exec /usr/bin/curl "\${new_args[@]}"
SHIM
chmod +x "$FIXTURE_DIR/bin/curl"

# T1: tier:low + 1 APPROVED + author is in engineers team → success
run_scenario "T1_success" "pass"
RC=$(cat "$FIX_STATE_DIR/last_rc")
POSTED=$(cat "$FIX_STATE_DIR/posted_statuses.jsonl" 2>/dev/null || true)
assert_eq "T1 exit code 0 (success)" "0" "$RC"
assert_contains "T1 POSTed state=success" '"state": "success"' "$POSTED"
assert_contains "T1 POST context is sop-tier-check / tier-check" \
  '"context": "sop-tier-check / tier-check (pull_request)"' "$POSTED"
assert_contains "T1 description names commenter" "test-runner" "$POSTED"

# T2: missing tier label → tier-check fails → failure status POSTed
run_scenario "T2_no_tier_label" "fail_no_label"
RC=$(cat "$FIX_STATE_DIR/last_rc")
POSTED=$(cat "$FIX_STATE_DIR/posted_statuses.jsonl" 2>/dev/null || true)
# tier-check.sh exits 1; refire script forwards that exit, so RC != 0
if [ "$RC" -ne 0 ]; then
  echo "  PASS  T2 exit code non-zero (got $RC)"
  PASS=$((PASS + 1))
else
  echo "  FAIL  T2 exit code should be non-zero, got 0"
  FAIL=$((FAIL + 1))
  FAILED_TESTS="${FAILED_TESTS} T2_rc"
fi
assert_contains "T2 POSTed state=failure" '"state": "failure"' "$POSTED"

# T3: tier:low present but ZERO approving reviews → failure
run_scenario "T3_no_approvals" "fail_no_approvals"
RC=$(cat "$FIX_STATE_DIR/last_rc")
POSTED=$(cat "$FIX_STATE_DIR/posted_statuses.jsonl" 2>/dev/null || true)
if [ "$RC" -ne 0 ]; then
  echo "  PASS  T3 exit code non-zero (got $RC)"
  PASS=$((PASS + 1))
else
  echo "  FAIL  T3 exit code should be non-zero, got 0"
  FAIL=$((FAIL + 1))
  FAILED_TESTS="${FAILED_TESTS} T3_rc"
fi
assert_contains "T3 POSTed state=failure" '"state": "failure"' "$POSTED"

# T4: closed PR — refire is a no-op (no POST, exit 0)
run_scenario "T4_closed" "pass"
RC=$(cat "$FIX_STATE_DIR/last_rc")
POSTED=$(cat "$FIX_STATE_DIR/posted_statuses.jsonl" 2>/dev/null || true)
assert_eq "T4 closed PR exits 0" "0" "$RC"
assert_eq "T4 closed PR posts no status" "" "$POSTED"

# T5: rate-limit — disable the env override and let scenario set a
# recent statuses entry. Re-enable rate-limit for this scenario by NOT
# passing SOP_REFIRE_DISABLE_RATE_LIMIT.
echo "T5_rate_limited" >"$FIX_STATE_DIR/scenario"
: >"$FIX_STATE_DIR/posted_statuses.jsonl"
set +e
T5_OUT=$(
  PATH="$FIXTURE_DIR/bin:$PATH" \
  GITEA_TOKEN="fixture-token" \
  GITEA_HOST="fixture.local" \
  REPO="molecule-ai/molecule-core" \
  PR_NUMBER="999" \
  COMMENT_AUTHOR="test-runner" \
  FIXTURE_PORT="$FIX_PORT" \
  bash "$SCRIPT" 2>&1
)
T5_RC=$?
set -e
POSTED=$(cat "$FIX_STATE_DIR/posted_statuses.jsonl" 2>/dev/null || true)
assert_eq "T5 rate-limited exits 0" "0" "$T5_RC"
assert_contains "T5 rate-limited log says skipped" "rate-limited" "$T5_OUT"
assert_eq "T5 rate-limited posts no status" "" "$POSTED"

echo
echo "------"
echo "PASS=$PASS FAIL=$FAIL"
if [ "$FAIL" -gt 0 ]; then
  echo "Failed:$FAILED_TESTS"
fi
[ "$FAIL" -eq 0 ]
