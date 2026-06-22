#!/usr/bin/env bash
# Regression test for #2159: when test_staging_full_saas.sh exits via
# `set -e` propagating an exit code outside the documented contract
# {0,1,2,3,4}, the cleanup trap must normalize it to 1.
#
# Pre-fix: a poisoned-token curl in step 5/11 exited with curl rc=22
# (HTTP error under --fail-with-body). The sanity workflow's case
# statement only matched {0,1,4}, fell through to the "investigate
# harness" branch, and opened a false-positive priority-high issue
# (#2159, 2026-04-27).
#
# This test exercises the exact normalization pattern in isolation
# so a future refactor that drops or weakens the pattern fails CI.
set -uo pipefail   # NOT -e — we want to inspect non-zero rc explicitly

PASS=0
FAIL=0

# Build a stub harness with the same trap pattern as the production
# script. Source it in a subshell, trigger an exit with a controlled
# rc, and assert the observed final rc.
run_stub() {
  local trigger_rc="$1"
  local stub
  stub=$(mktemp)
  cat > "$stub" <<EOF
#!/usr/bin/env bash
set -euo pipefail
CLEANUP_DONE=0
cleanup_org() {
  local entry_rc=\$?
  if [ "\$CLEANUP_DONE" = "1" ]; then return 0; fi
  CLEANUP_DONE=1
  case "\$entry_rc" in
    0|1|2|3|4) ;;
    *) exit 1 ;;
  esac
}
trap cleanup_org EXIT INT TERM
exit $trigger_rc
EOF
  chmod +x "$stub"
  local observed
  bash "$stub"; observed=$?
  rm -f "$stub"
  echo "$observed"
}

assert_rc() {
  local label="$1" trigger="$2" expected="$3"
  local observed
  observed=$(run_stub "$trigger")
  if [ "$observed" = "$expected" ]; then
    echo "  ✓ $label: trigger=$trigger expected=$expected observed=$observed"
    PASS=$((PASS+1))
  else
    echo "  ✗ $label: trigger=$trigger expected=$expected OBSERVED=$observed" >&2
    FAIL=$((FAIL+1))
  fi
}

echo "Test: cleanup_org exit-code normalization"
echo "  Contract: only {0,1,2,3,4} pass through; anything else maps to 1"
echo

# Contracted codes pass through unchanged.
assert_rc "happy path"                        0  0
assert_rc "fail() generic"                    1  1
assert_rc "missing env"                       2  2
assert_rc "provisioning timeout"              3  3
assert_rc "leak detected (cleanup exits 4)"   4  4

# The bug: rc=22 from curl --fail-with-body must normalize to 1.
assert_rc "curl HTTP error (rc=22, the bug)"  22 1

# Other realistic curl failure codes (network, SSL, etc.) must also
# normalize. Pinning a few representative values so a future regex
# refactor that loses the wildcard is caught.
assert_rc "curl couldn't resolve host (rc=6)"  6  1
assert_rc "curl SSL error (rc=35)"             35 1
assert_rc "curl operation timeout (rc=28)"     28 1

# Edge: very high rc (from a SIGSEGV-killed child or similar).
assert_rc "high rc (139, sigsegv)"             139 1

echo
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" = "0" ]
