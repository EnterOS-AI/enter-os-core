#!/usr/bin/env bash
# Fail-direction / load-bearing proof for the E2E_REQUIRE_LIVE zero-validated
# gate in test_priority_runtimes_e2e.sh (the REQUIRED `E2E API Smoke Test`).
#
# WHY (harden/enforce-ci-gates-core-v2, PR #2286): the priority-runtimes E2E's
# only historical exit gate was `[ "$FAIL" -eq 0 ]`. When every runtime SKIPs
# because no live secret is present — exactly what the CI step did — PASS=0
# FAIL=0 and the script exited 0 (GREEN) while validating ZERO runtimes. The
# REQUIRED merge gate was therefore false-green: passing without exercising a
# single runtime. The fix adds a VALIDATED counter and makes a zero-validated
# run RED when E2E_REQUIRE_LIVE is set.
#
# That zero-validated→RED decision lives in evaluate_require_live_gate() in
# test_priority_runtimes_e2e.sh. CI cannot prove it via a live arm — the CI
# substrate can't provision ANY runtime end-to-end (MiniMax 422, mock org-
# import create fails, claude-code needs a key CI lacks), so the live e2e-api
# job does NOT force E2E_REQUIRE_LIVE (that would red the required gate for
# everyone). This UNIT test is the regression coverage instead: it drives the
# REAL evaluate_require_live_gate() function — not a copy — in isolation by
# sourcing the script with E2E_PRIORITY_UNIT_SOURCE=1 (which stops before any
# platform I/O), setting the counters, and asserting the gate's return code.
#
# Because it exercises the actual function, a future revert of the zero-
# validated→RED logic in test_priority_runtimes_e2e.sh fails THIS test on
# every PR — so the false-green can't silently come back.
#
# Runs entirely offline (no LLM, no network, no provisioning) — pure shell
# logic — so it runs on every PR in the fast lane and locally via `bash`.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE_SCRIPT="$SCRIPT_DIR/test_priority_runtimes_e2e.sh"

if [ ! -f "$GATE_SCRIPT" ]; then
  echo "FATAL: cannot find $GATE_SCRIPT" >&2
  exit 2
fi

PASS=0
FAIL=0

# run_case <E2E_REQUIRE_LIVE value> <VALIDATED count> <FAIL count>
# Sources the REAL test_priority_runtimes_e2e.sh under the unit source-guard
# (E2E_PRIORITY_UNIT_SOURCE=1 → it returns right after defining the counters
# and evaluate_require_live_gate(), before _lib.sh / the live pre-sweep curl),
# sets the counters to the scenario, calls the real gate, and echoes the
# return code. Each case runs in a fresh `bash -c` so set -e/-u inside the
# sourced script can't leak between cases or kill this harness.
run_case() {
  local require_live="$1" validated="$2" failcount="$3"
  local observed
  E2E_PRIORITY_UNIT_SOURCE=1 \
  E2E_REQUIRE_LIVE="$require_live" \
  GATE_SCRIPT="$GATE_SCRIPT" \
  VAL="$validated" \
  FL="$failcount" \
  bash -c '
    set -uo pipefail
    # shellcheck disable=SC1090
    source "$GATE_SCRIPT"      # returns at the source-guard (no platform I/O)
    VALIDATED="$VAL"
    FAIL="$FL"
    evaluate_require_live_gate >/dev/null 2>&1
    exit $?
  '
  observed=$?
  echo "$observed"
}

assert_rc() {
  local label="$1" require_live="$2" validated="$3" failcount="$4" expected="$5"
  local observed
  observed=$(run_case "$require_live" "$validated" "$failcount")
  if [ "$observed" = "$expected" ]; then
    echo "  ✓ $label: REQUIRE_LIVE=$require_live VALIDATED=$validated FAIL=$failcount → rc=$observed"
    PASS=$((PASS + 1))
  else
    echo "  ✗ $label: REQUIRE_LIVE=$require_live VALIDATED=$validated FAIL=$failcount expected=$expected OBSERVED=$observed" >&2
    FAIL=$((FAIL + 1))
  fi
}

echo "=== E2E_REQUIRE_LIVE priority-runtimes zero-validated gate proof ==="
echo "    (drives the REAL evaluate_require_live_gate from $GATE_SCRIPT)"
echo

# (a) DECISIVE false-green trap: REQUIRE_LIVE=1 + zero validated → RED (exit 1).
assert_rc "require-live, zero validated → RED (the false-green trap)" \
  1 0 0 1

# (b) REQUIRE_LIVE=1 + at least one validated → GREEN (exit 0).
assert_rc "require-live, one validated → GREEN" \
  1 1 0 0
assert_rc "require-live, several validated → GREEN" \
  1 3 0 0

# (c) REQUIRE_LIVE unset-equivalent (0) + zero validated → GREEN (loud skip).
assert_rc "no require-live, zero validated → GREEN (dev-convenience loud skip)" \
  0 0 0 0

# REQUIRE_LIVE=true (string form) is also honoured by the gate.
assert_rc "require-live='true', zero validated → RED" \
  true 0 0 1

# A real FAIL is always red, regardless of REQUIRE_LIVE / VALIDATED — the
# zero-validated guard must not mask (nor be masked by) a genuine failure.
assert_rc "real FAIL with validations, no require-live → RED" \
  0 2 1 1
assert_rc "real FAIL, zero validated, no require-live → RED" \
  0 0 1 1

echo
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
