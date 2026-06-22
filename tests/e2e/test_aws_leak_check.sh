#!/usr/bin/env bash
set -uo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck disable=SC1091
# shellcheck source=lib/aws_leak_check.sh
source "$SCRIPT_DIR/lib/aws_leak_check.sh"

PASS=0
FAIL=0

TMPDIR_E2E=$(mktemp -d -t aws-leak-check-e2e-XXXXXX)
trap 'rm -rf "$TMPDIR_E2E"' EXIT INT TERM

make_fake_aws() {
  local body="$1"
  mkdir -p "$TMPDIR_E2E/bin"
  cat > "$TMPDIR_E2E/bin/aws" <<EOF
#!/usr/bin/env bash
set -euo pipefail
echo "\$*" >> "$TMPDIR_E2E/aws.calls"
$body
EOF
  chmod +x "$TMPDIR_E2E/bin/aws"
}

reset_env() {
  /bin/rm -f "$TMPDIR_E2E/aws.calls"
  export PATH="$TMPDIR_E2E/bin:$ORIG_PATH"
  export AWS_ACCESS_KEY_ID=test-access
  export AWS_SECRET_ACCESS_KEY=test-secret
  export AWS_DEFAULT_REGION=us-east-2
  export E2E_AWS_LEAK_CHECK=required
  export E2E_AWS_LEAK_CHECK_SECS=0
  export E2E_AWS_LEAK_CHECK_INTERVAL=1
  unset E2E_AWS_TERMINATE_LEAKS
}

assert_rc() {
  local label="$1"
  local expected="$2"
  shift 2
  local observed
  "$@" >/tmp/aws-leak-check.out 2>/tmp/aws-leak-check.err
  observed=$?
  if [ "$observed" = "$expected" ]; then
    echo "  PASS $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL $label: expected rc=$expected observed=$observed" >&2
    echo "  stderr:" >&2
    sed 's/^/    /' /tmp/aws-leak-check.err >&2
    FAIL=$((FAIL + 1))
  fi
}

ORIG_PATH="$PATH"

echo "Test: AWS EC2 leak check helper"

reset_env
/bin/rm -rf "${TMPDIR_E2E:?}/bin"
/bin/mkdir -p "$TMPDIR_E2E/noaws"
export PATH="$TMPDIR_E2E/noaws"
export E2E_AWS_LEAK_CHECK=auto
assert_rc "auto mode skips when aws is unavailable" 0 e2e_verify_no_ec2_leaks_for_slug e2e-smoke-test

reset_env
/bin/rm -rf "${TMPDIR_E2E:?}/bin"
/bin/mkdir -p "$TMPDIR_E2E/noaws"
export PATH="$TMPDIR_E2E/noaws"
export E2E_AWS_LEAK_CHECK=required
assert_rc "required mode fails when aws is unavailable" 2 e2e_verify_no_ec2_leaks_for_slug e2e-smoke-test

reset_env
# shellcheck disable=SC2016
make_fake_aws 'if [ "$1 $2" = "ec2 describe-instances" ]; then exit 0; fi'
assert_rc "no matching EC2 returns clean" 0 e2e_verify_no_ec2_leaks_for_slug e2e-smoke-test

reset_env
# shellcheck disable=SC2016
make_fake_aws 'if [ "$1 $2" = "ec2 describe-instances" ]; then echo "i-123 running ws-tenant-e2e-smoke-test-abc"; exit 0; fi'
assert_rc "persistent matching EC2 is a leak" 4 e2e_verify_no_ec2_leaks_for_slug e2e-smoke-test

reset_env
export E2E_AWS_TERMINATE_LEAKS=1
# shellcheck disable=SC2016
make_fake_aws '
if [ "$1 $2" = "ec2 describe-instances" ]; then
  echo "i-123 running ws-tenant-e2e-smoke-test-abc"
  exit 0
fi
if [ "$1 $2" = "ec2 terminate-instances" ]; then
  echo "terminated" >/dev/null
  exit 0
fi
'
assert_rc "terminate mode attempts cleanup before returning leak" 4 e2e_verify_no_ec2_leaks_for_slug e2e-smoke-test
if grep -q "terminate-instances" "$TMPDIR_E2E/aws.calls"; then
  echo "  PASS terminate-instances was called"
  PASS=$((PASS + 1))
else
  echo "  FAIL terminate-instances was not called" >&2
  FAIL=$((FAIL + 1))
fi

echo
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" = "0" ]
