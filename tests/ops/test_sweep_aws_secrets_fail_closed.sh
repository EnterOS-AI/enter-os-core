#!/usr/bin/env bash
# Regression test for scripts/ops/sweep-aws-secrets.sh — verifies the
# live-org fetch is fail-closed. A non-2xx response, invalid JSON, or a
# response missing the 'orgs' array must abort the sweep BEFORE any secrets
# are classified as orphans. This is especially critical under
# SWEEP_ALLOW_BULK=1, where an empty live-org set would otherwise delete
# every old managed secret.
set -uo pipefail

SCRIPT="${SCRIPT:-scripts/ops/sweep-aws-secrets.sh}"

PASS=0
FAIL=0

run_case() {
  local name="$1" curl_exit="$2" curl_body="$3" bulk="${4:-0}"
  local expect_abort="${5:-true}"   # true = must stop before AWS/orphan classification
  local tmp
  tmp=$(mktemp -d -t sweep-fail-closed-XXXXXX)
  local sentinel="$tmp/aws_reached"

  # Generate a mock curl script. We use Python to write the body so that
  # JSON quotes/brackets are not mangled by shell quoting in a heredoc.
  export curl_body curl_exit tmp
  python3 -c "
import os, shlex
body = os.environ['curl_body']
exit_code = os.environ['curl_exit']
path = os.path.join(os.environ['tmp'], 'curl')
with open(path, 'w') as f:
    f.write('#!/usr/bin/env bash\n')
    f.write(f'echo {shlex.quote(body)}\n')
    f.write(f'exit {exit_code}\n')
"
  chmod +x "$tmp/curl"

  # Mock aws cli: writes a sentinel file and exits with a distinctive code
  # so we can prove whether the sweep reached AWS/classification.
  cat > "$tmp/aws" <<'MOCK'
#!/usr/bin/env bash
echo "reached" > "$AWS_SENTINEL"
exit 99
MOCK
  chmod +x "$tmp/aws"

  local out="$tmp/out" err="$tmp/err"
  PATH="$tmp:$PATH" \
    CP_ADMIN_API_TOKEN=tok-prod \
    CP_STAGING_ADMIN_API_TOKEN=tok-staging \
    AWS_ACCESS_KEY_ID=ak \
    AWS_SECRET_ACCESS_KEY=sk \
    AWS_SENTINEL="$sentinel" \
    SWEEP_ALLOW_BULK="$bulk" \
    bash "$SCRIPT" --execute > "$out" 2> "$err"
  local actual_exit=$?
  local case_fail=0

  if [ "$expect_abort" = "true" ]; then
    # Fail-closed cases: script must abort before AWS and before orphan
    # classification. Exit code should be the fetch/validation failure (1),
    # NOT the aws mock's distinctive 99.
    if [ "$actual_exit" -eq 99 ]; then
      echo "  ✗ $name: reached aws mock (exit 99) instead of aborting at fetch" >&2
      case_fail=1
    elif [ "$actual_exit" -eq 0 ]; then
      echo "  ✗ $name: exited 0 instead of aborting" >&2
      case_fail=1
    fi
    if [ -f "$sentinel" ]; then
      echo "  ✗ $name: aws sentinel exists — sweep reached AWS/classification" >&2
      case_fail=1
    fi
    if grep -qE '== Sweep plan ==|would delete:|orphan-(tenant|workspace)' "$out" "$err" 2>/dev/null; then
      echo "  ✗ $name: output contains sweep plan / orphan classification" >&2
      case_fail=1
    fi
  else
    # Happy-path control: valid live-org fetch must allow the sweep to proceed past
    # the live-org fetch and reach AWS/classification. We use an empty orgs list
    # so no real delete work happens; the aws mock proves the boundary was crossed.
    if [ ! -f "$sentinel" ]; then
      echo "  ✗ $name: aws sentinel missing — sweep did not reach AWS/classification" >&2
      case_fail=1
    fi
    if [ "$actual_exit" -ne 99 ]; then
      echo "  ✗ $name: expected aws mock exit 99, got $actual_exit" >&2
      case_fail=1
    fi
  fi

  if [ "$case_fail" -eq 0 ]; then
    echo "  ✓ $name"
    PASS=$((PASS + 1))
  else
    echo "    stdout:" >&2
    sed 's/^/      /' "$out" >&2
    echo "    stderr:" >&2
    sed 's/^/      /' "$err" >&2
    FAIL=$((FAIL + 1))
  fi

  rm -rf "$tmp"
  unset curl_body curl_exit
}

echo "Test: sweep-aws-secrets live-org fetch fail-closed"
echo

# Non-2xx from CP admin API (curl -f exits 22).
run_case "prod API returns 500"                          22 '{"error":"internal"}'        0 true
run_case "prod API returns 500 with SWEEP_ALLOW_BULK=1"  22 '{"error":"internal"}'        1 true

# Valid HTTP but invalid JSON body.
run_case "prod API returns malformed JSON"               0  'this is not json'             0 true
run_case "prod API returns malformed JSON with SWEEP_ALLOW_BULK=1" 0 'this is not json'  1 true

# Valid JSON but missing 'orgs' key.
run_case "prod API returns JSON without orgs"            0  '{"foo":"bar"}'                0 true
run_case "prod API returns JSON without orgs with SWEEP_ALLOW_BULK=1" 0 '{"foo":"bar"}'  1 true

# Valid JSON but 'orgs' is not an array.
run_case "prod API returns orgs as string"               0  '{"orgs":"not-an-array"}'      0 true

# Happy-path control: valid orgs array must allow the sweep to proceed past
# the live-org fetch and reach AWS/classification. We use an empty orgs list
# so no real delete work happens; the aws mock proves the boundary was crossed.
run_case "prod API returns valid empty orgs (reaches AWS)" 0 '{"orgs":[]}'                 0 false

echo
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
