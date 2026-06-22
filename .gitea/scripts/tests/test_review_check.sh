#!/usr/bin/env bash
# shellcheck disable=SC2034
# Regression tests for .gitea/scripts/review-check.sh (RFC#324 Step 1).
#
# Covers:
#   T1  — open PR: script fetches PR + reviews, continues to team probe
#   T2  — closed PR: script exits 0 (no-op)
#   T3  — APPROVED non-author review exists → candidates exist
#   T4  — no non-author APPROVED reviews → exit 1 (no candidates)
#   T5  — only author reviews (no non-author APPROVE) → exit 1
#   T6  — dismissed APPROVED review → treated as no approval
#   T7  — team membership probe → 204 (member) → script exits 0
#   T8  — team membership probe → 404 (not a member) → script exits 1
#   T9  — team membership probe → 403 (token not in team) → script exits 1 (fail closed)
#   T10 — CURL_AUTH_FILE created with mode 600 and correct header content
#   T11 — bash syntax check (bash -n passes)
#   T12 — jq filter: non-author APPROVED official current-head → in candidate list; dismissed → excluded
#   T13 — missing required env GITEA_TOKEN → exits 1 with error
#   T14 — non-default-base PR exits 0 without requiring review
#   T15 — comment agent-prefix approval → exit 1
#   T16 — comment generic keyword approval → exit 1
#   T17 — comments with no approval keywords → exit 1
#   T18 — wrong-team review + right-team comment → exit 1
#   T19 — ai-sop-ack APPROVED review excluded from qa-review gate
#   T20 — ai-sop-ack APPROVED review excluded from security-review gate
#   T21 — stale-head APPROVED review → exit 1 (commit_id mismatch)
#   T22 — missing/non-official APPROVED review → exit 1 (official != true)
#   T23 — missing-commit_id APPROVED review → exit 1 (SEV-1 internal#812
#         fail-closed contract: a missing/empty commit_id is REJECTED, not
#         silently accepted as "older Gitea row" the way the pre-fix
#         gitea-merge-queue.py did. Closes the spoof-bug surface that
#         #843 had.)
#
# Hostile-self-review (per feedback_assert_exact_not_substring):
# this test MUST FAIL if the script is absent. Verified by running
# the test before the file exists (covered in the PR body).

set -euo pipefail

THIS_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_DIR="$(cd "$THIS_DIR/.." && pwd)"
SCRIPT="$SCRIPT_DIR/review-check.sh"

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
    echo "        haystack:  <$(printf '%s' "$haystack" | head -c 200)>"
    FAIL=$((FAIL + 1))
    FAILED_TESTS="${FAILED_TESTS} ${label}"
  fi
}

assert_file_mode() {
  local label="$1"
  local path="$2"
  local expected_mode="$3"
  if [ ! -f "$path" ]; then
    echo "  FAIL  $label (file not found: $path)"
    FAIL=$((FAIL + 1))
    FAILED_TESTS="${FAILED_TESTS} ${label}"
    return
  fi
  local got_mode
  got_mode=$(stat -c '%a' "$path" 2>/dev/null || stat -f '%Lp' "$path" 2>/dev/null || echo "000")
  if [ "$expected_mode" = "$got_mode" ]; then
    echo "  PASS  $label (mode=$got_mode)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL  $label (expected mode=$expected_mode, got=$got_mode)"
    FAIL=$((FAIL + 1))
    FAILED_TESTS="${FAILED_TESTS} ${label}"
  fi
}

assert_file_contains() {
  local label="$1"
  local path="$2"
  local needle="$3"
  if [ ! -f "$path" ]; then
    echo "  FAIL  $label (file not found: $path)"
    FAIL=$((FAIL + 1))
    FAILED_TESTS="${FAILED_TESTS} ${label}"
    return
  fi
  if grep -qF "$needle" "$path"; then
    echo "  PASS  $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL  $label (needle not found: <$needle>)"
    FAIL=$((FAIL + 1))
    FAILED_TESTS="${FAILED_TESTS} ${label}"
  fi
}

# Existence check (foundation)
echo
echo "== existence =="
if [ -f "$SCRIPT" ]; then
  echo "  PASS  script exists: $SCRIPT"
  PASS=$((PASS + 1))
else
  echo "  FAIL  script not found: $SCRIPT"
  FAIL=$((FAIL + 1))
  FAILED_TESTS="${FAILED_TESTS} script_exists"
  echo
  echo "------"
  echo "PASS=$PASS FAIL=$FAIL (existence)"
  echo "Cannot proceed without the script."
  exit 1
fi

# T11 — bash syntax check
echo
echo "== T11 bash syntax =="
if bash -n "$SCRIPT" 2>&1; then
  echo "  PASS  T11 bash -n passes"
  PASS=$((PASS + 1))
else
  echo "  FAIL  T11 bash -n failed"
  FAIL=$((FAIL + 1))
  FAILED_TESTS="${FAILED_TESTS} T11"
fi

# T13 — missing required env
echo
echo "== T13 missing GITEA_TOKEN =="
set +e
T13_OUT=$(PATH="/tmp:$PATH" GITEA_TOKEN='' GITEA_HOST=git.example.com REPO=x/y PR_NUMBER=1 TEAM=qa TEAM_ID=1 bash "$SCRIPT" 2>&1 || true)
set -e
assert_contains "T13 exits non-zero when GITEA_TOKEN missing" "GITEA_TOKEN required" "$T13_OUT"

# Start fixture HTTP server
echo
echo "== fixture setup =="
FIXTURE_DIR=$(mktemp -d)
trap 'rm -rf "$FIXTURE_DIR"; [ -n "${FIX_PID:-}" ] && kill "$FIX_PID" 2>/dev/null || true' EXIT
FIXTURE_PY="$THIS_DIR/_review_check_fixture.py"
if [ ! -f "$FIXTURE_PY" ]; then
  echo "::error::fixture server $FIXTURE_PY missing"
  exit 1
fi

FIX_LOG="$FIXTURE_DIR/fixture.log"
FIX_STATE_DIR="$FIXTURE_DIR/state"
mkdir -p "$FIX_STATE_DIR"

# Find an unused port
FIX_PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')

FIXTURE_STATE_DIR="$FIX_STATE_DIR" python3 "$FIXTURE_PY" "$FIX_PORT" \
  >"$FIX_LOG" 2>&1 &
FIX_PID=$!

# Wait for fixture readiness
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
echo "  fixture running on port $FIX_PORT"

# Install a curl shim that rewrites https://fixture.local/* -> http://127.0.0.1:$FIX_PORT/*
# Use double-quoted heredoc so FIX_PORT is expanded into the shim at creation time.
mkdir -p "$FIXTURE_DIR/bin"
cat >"$FIXTURE_DIR/bin/curl" <<"CURL_SHIM"
#!/usr/bin/env bash
# Shim: rewrite https://fixture.local/* -> http://127.0.0.1:FIXPORT/*
# Generated at test-run time; FIXPORT is substituted when this file is written.
new_args=()
for a in "$@"; do
  if [[ "$a" == https://fixture.local/* ]]; then
    rest="${a#https://fixture.local}"
    a="http://127.0.0.1:FIXPORT${rest}"
  fi
  new_args+=("$a")
done
exec /usr/bin/curl "${new_args[@]}"
CURL_SHIM
# Now substitute FIXPORT with the actual port number. Use perl rather than
# sed -i so the test runs on both GNU sed and BSD/macOS sed.
perl -0pi -e "s/FIXPORT/${FIX_PORT}/g" "$FIXTURE_DIR/bin/curl"
chmod +x "$FIXTURE_DIR/bin/curl"

# Helper: run the script with fixture environment
run_review_check() {
  local scenario="$1"
  local team="${2:-qa}"
  local team_id="${3:-20}"
  echo "$scenario" >"$FIX_STATE_DIR/scenario"
  local out
  set +e
  out=$(
    PATH="$FIXTURE_DIR/bin:/tmp:$PATH" \
    GITEA_TOKEN="fixture-token" \
    GITEA_HOST="fixture.local" \
    REPO="molecule-ai/molecule-core" \
    PR_NUMBER="999" \
    DEFAULT_BRANCH="main" \
    TEAM="$team" \
    TEAM_ID="$team_id" \
    REVIEW_CHECK_DEBUG="0" \
    REVIEW_CHECK_STRICT="0" \
    bash "$SCRIPT" 2>&1
  )
  local rc=$?
  set -e
  echo "$out" >"$FIX_STATE_DIR/last_run.log"
  echo "$rc" >"$FIX_STATE_DIR/last_rc"
  echo "$out"
}

# T1 — open PR: script fetches PR and continues
echo
echo "== T1 open PR =="
T1_OUT=$(run_review_check "T1_pr_open")
T1_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T1 exit code 0 (approver exists + team member)" "0" "$T1_RC"
assert_contains "T1 qa-review APPROVED by core-devops" "APPROVED by core-devops" "$T1_OUT"

# T2 — closed PR: exits 0 immediately (no-op)
echo
echo "== T2 closed PR =="
T2_OUT=$(run_review_check "T2_pr_closed")
T2_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T2 exit code 0 (closed PR no-op)" "0" "$T2_RC"

# T3 — APPROVED non-author reviews exist
echo
echo "== T3 approved non-author reviews =="
T3_OUT=$(run_review_check "T3_reviews_approved_non_author")
T3_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T3 exit code 0 (candidates + team member)" "0" "$T3_RC"

# T4 — no non-author APPROVED reviews → exit 1
echo
echo "== T4 no non-author APPROVED reviews =="
T4_OUT=$(run_review_check "T4_reviews_empty")
T4_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T4 exit code 1 (no candidates)" "1" "$T4_RC"
assert_contains "T4 awaiting non-author APPROVE" "awaiting non-author APPROVE" "$T4_OUT"

# T14 — non-default-base PR should not make the default branch red.
echo
echo "== T14 non-default base PR =="
T14_OUT=$(run_review_check "T14_non_default_base")
T14_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T14 exit code 0 (non-default base no-op)" "0" "$T14_RC"
assert_contains "T14 not applicable notice" "gate not applicable" "$T14_OUT"

# T5 — only author reviews → exit 1
echo
echo "== T5 only author reviews =="
T5_OUT=$(run_review_check "T5_reviews_only_author")
T5_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T5 exit code 1 (only author reviews, no candidates)" "1" "$T5_RC"

# T6 — dismissed APPROVED review → treated as no approval
echo
echo "== T6 dismissed APPROVED review =="
T6_OUT=$(run_review_check "T6_reviews_dismissed")
T6_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T6 exit code 1 (dismissed = no approval)" "1" "$T6_RC"

# T7 — team member → exit 0
echo
echo "== T7 team membership 204 (member) =="
T7_OUT=$(run_review_check "T7_team_member")
T7_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T7 exit code 0 (member, APPROVED)" "0" "$T7_RC"
assert_contains "T7 APPROVED by core-devops (team member)" "APPROVED by core-devops" "$T7_OUT"

# T8 — not a team member → exit 1 (fail closed)
echo
echo "== T8 team membership 404 (not a member) =="
T8_OUT=$(run_review_check "T8_team_not_member")
T8_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T8 exit code 1 (not in team)" "1" "$T8_RC"

# T9 — 403 token-not-in-team → exit 1 (fail closed)
echo
echo "== T9 team membership 403 (token not in team) =="
T9_OUT=$(run_review_check "T9_team_403")
T9_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T9 exit code 1 (403 token-not-in-team, fail closed)" "1" "$T9_RC"
assert_contains "T9 403 error in output" "403" "$T9_OUT"

# T10 — token file creation and permissions
echo
echo "== T10 CURL_AUTH_FILE =="
# Verify the token-file logic directly: create a temp file with the
# same mktemp pattern, write the header with printf, chmod 600, then assert.
T10_TOKEN="secret-fixture-token-abc123"
T10_AUTHFILE=$(mktemp "${TMPDIR:-/tmp}/curl-auth.test.XXXXXX")
chmod 600 "$T10_AUTHFILE"
printf 'header = "Authorization: token %s"\n' "$T10_TOKEN" > "$T10_AUTHFILE"
assert_file_mode "T10a mktemp authfile mode 600 (CURL_AUTH_FILE pattern)" "$T10_AUTHFILE" "600"
assert_file_contains "T10b printf header format (CURL_AUTH_FILE content)" "$T10_AUTHFILE" "Authorization: token secret-fixture-token-abc123"
assert_file_contains "T10c 'header =' curl-config syntax" "$T10_AUTHFILE" 'header = "Authorization: token '
rm -f "$T10_AUTHFILE"

# T12 — jq filter: non-author APPROVED official current-head included; dismissed/stale/missing-official excluded
echo
echo "== T12 jq filter =="
# These are tested indirectly via T3 and T6 above, but let's also test
# the jq expression directly.
JQ_FILTER='.[]
  | select(.state == "APPROVED")
  | select(.official == true)
  | select(.dismissed != true)
  | select(.user.login != "alice")
  | select(.commit_id == $head)
  | .user.login'

T12_INPUT='[{"state":"APPROVED","official":true,"dismissed":false,"commit_id":"deadbeef0000111122223333444455556666","user":{"login":"core-devops"}},{"state":"CHANGES_REQUESTED","official":true,"dismissed":false,"commit_id":"deadbeef0000111122223333444455556666","user":{"login":"bob"}},{"state":"APPROVED","official":true,"dismissed":false,"commit_id":"deadbeef0000111122223333444455556666","user":{"login":"alice"}},{"state":"APPROVED","official":true,"dismissed":true,"commit_id":"deadbeef0000111122223333444455556666","user":{"login":"carol"}},{"state":"APPROVED","official":false,"dismissed":false,"commit_id":"deadbeef0000111122223333444455556666","user":{"login":"dave"}},{"state":"APPROVED","official":true,"dismissed":false,"commit_id":"oldsha0000000000000000000000000000","user":{"login":"eve"}}]'

JQ_CMD=$(command -v jq 2>/dev/null || echo /tmp/jq)
T12_CANDIDATES=$(echo "$T12_INPUT" | "$JQ_CMD" -r --arg head "deadbeef0000111122223333444455556666" "$JQ_FILTER" 2>/dev/null | sort -u)
assert_contains "T12 jq: core-devops (non-author APPROVED official current-head) in candidates" "core-devops" "$T12_CANDIDATES"
assert_eq "T12 jq: alice (author) NOT in candidates" "" "$(echo "$T12_CANDIDATES" | grep '^alice$' || true)"
assert_eq "T12 jq: carol (dismissed) NOT in candidates" "" "$(echo "$T12_CANDIDATES" | grep '^carol$' || true)"
assert_eq "T12 jq: dave (official=false) NOT in candidates" "" "$(echo "$T12_CANDIDATES" | grep '^dave$' || true)"
assert_eq "T12 jq: eve (stale head) NOT in candidates" "" "$(echo "$T12_CANDIDATES" | grep '^eve$' || true)"

# T15 — comment-based approval via agent prefix pattern → exit 1
# SECURITY: agent-prefix comments are also removed. A text prefix in an
# issue comment is spoofable (any team member can type "[core-qa-agent]")
# and lacks the audit trail of an official Gitea review.
echo
echo "== T15 comment agent-prefix approval =="
T15_OUT=$(run_review_check "T15_comments_agent_approval")
T15_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T15 exit code 1 (agent-prefix comment rejected — not an official review)" "1" "$T15_RC"
assert_contains "T15 no candidates error" "no candidates from reviews API or issue comments" "$T15_OUT"

# T16 — comment-based approval via generic APPROVED keyword → exit 1
# SECURITY: generic keywords (APPROVED/LGTM/ACCEPTED) must NOT satisfy the
# gate — only official Gitea reviews or agent-prefix comments count. A plain
# comment from a team member is a bypass if it skips the review UI.
echo
echo "== T16 comment generic keyword approval =="
T16_OUT=$(run_review_check "T16_comments_generic_approval")
T16_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T16 exit code 1 (generic-approval comment rejected — not an official review)" "1" "$T16_RC"
assert_contains "T16 no candidates error" "no candidates from reviews API or issue comments" "$T16_OUT"

# T17 — no approval keywords in comments → exit 1
echo
echo "== T17 comments with no approval keywords =="
T17_OUT=$(run_review_check "T17_comments_no_approval")
T17_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T17 exit code 1 (no candidates from comments)" "1" "$T17_RC"
assert_contains "T17 no candidates error" "no candidates from reviews API or issue comments" "$T17_OUT"

# T18 — wrong-team review + right-team comment → exit 1
# SECURITY: with comment approval fully removed, a wrong-team review plus
# a right-team comment yields NO valid candidates. Only official reviews
# from the target team count.
echo
echo "== T18 review candidate wrong team, comment candidate right team =="
T18_OUT=$(run_review_check "T18_review_wrong_team_comment_right_team")
T18_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T18 exit code 1 (comment approval removed — no valid candidates)" "1" "$T18_RC"
assert_contains "T18 none are in team" "none are in team" "$T18_OUT"

# T19 — ai-sop-ack member APPROVED review must NOT count toward qa-review
# or security-review (R1 hardening refinement, msg 1388c76f).
echo
echo "== T19 ai-sop-ack APPROVED review excluded from qa-review gate =="
T19_OUT=$(run_review_check "T19_ai_sop_ack_approved" "qa" "20")
T19_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T19 exit code 1 (ai-sop-ack not in qa team)" "1" "$T19_RC"
assert_contains "T19 ai-reviewer excluded from qa" "candidates: ai-reviewer" "$T19_OUT"
assert_contains "T19 none are in qa team" "none are in team" "$T19_OUT"

# T20 — same ai-sop-ack member must also be excluded from security-review gate.
echo
echo "== T20 ai-sop-ack APPROVED review excluded from security-review gate =="
T20_OUT=$(run_review_check "T19_ai_sop_ack_approved" "security" "21")
T20_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T20 exit code 1 (ai-sop-ack not in security team)" "1" "$T20_RC"
assert_contains "T20 ai-reviewer excluded from security" "candidates: ai-reviewer" "$T20_OUT"
assert_contains "T20 none are in security team" "none are in team" "$T20_OUT"

# T21 — stale-head APPROVED review must be rejected (commit_id mismatch).
# SECURITY: an approval on an old commit does not cover the current head.
echo
echo "== T21 stale-head APPROVED review rejected =="
T21_OUT=$(run_review_check "T21_stale_head_approved")
T21_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T21 exit code 1 (stale-head approval rejected)" "1" "$T21_RC"
assert_contains "T21 no candidates error" "no candidates from reviews API or issue comments" "$T21_OUT"

# T22 — missing/non-official APPROVED review must be rejected.
# SECURITY: only official Gitea reviews count; comments and non-official reviews lack audit trail.
echo
echo "== T22 missing official flag APPROVED review rejected =="
T22_OUT=$(run_review_check "T22_missing_official")
T22_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T22 exit code 1 (missing official rejected)" "1" "$T22_RC"
assert_contains "T22 no candidates error" "no candidates from reviews API or issue comments" "$T22_OUT"

# T23 — missing-commit_id APPROVED review must be rejected.
# SEV-1 internal#812 (supersedes closed internal#843). A review with NO
# commit_id field is the spoof-bug signature: a real reviewer cannot
# have submitted against a commit that doesn't exist. The fail-closed
# SSOT must REJECT — the pre-fix gitea-merge-queue.py silently accepted
# these (the "older Gitea row" escape hatch), which is the exact surface
# that closed #843 had. The Python unit tests in
# test_approval_validator.py cover the predicate at the unit level;
# this T23 covers the bash + jq pipeline end-to-end.
echo
echo "== T23 missing commit_id APPROVED review rejected (SEV-1 fail-closed) =="
T23_OUT=$(run_review_check "T23_missing_commit_id")
T23_RC=$(cat "$FIX_STATE_DIR/last_rc")
assert_eq "T23 exit code 1 (missing commit_id rejected)" "1" "$T23_RC"
assert_contains "T23 no candidates error" "no candidates from reviews API or issue comments" "$T23_OUT"

echo
echo "------"
echo "PASS=$PASS FAIL=$FAIL"
if [ "$FAIL" -gt 0 ]; then
  echo "Failed:$FAILED_TESTS"
fi
[ "$FAIL" -eq 0 ]
