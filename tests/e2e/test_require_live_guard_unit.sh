#!/usr/bin/env bash
# Fail-direction / load-bearing proof for the E2E_REQUIRE_LIVE
# fail-closed-on-skip guard in test_staging_full_saas.sh.
#
# WHY (harden/e2e-staging-saas-failclosed): the staging SaaS E2E is being
# hardened to become a HARD merge-gate. A gate that can reach its final `ok`
# WITHOUT having actually exercised a provision→online→A2A cycle is a
# false-green — it would let a refactor that short-circuits the lifecycle
# (or a skip path that swallows it) report PASS. require_live_or_die() is the
# guard; this test proves it FAILS (exit 5) when milestones are missing and
# PASSES when all fired — the watch-it-fail counterpart the dev-SOP requires.
#
# Runs entirely offline (no LLM, no network, no provisioning) — pure shell
# logic — so it can run on every PR in the fast lane and locally via `bash`.
set -uo pipefail

# Scratch dir for the generated guard-runner stubs. EXIT trap guarantees
# cleanup even when an assertion exits the test non-zero (lint_cleanup_traps).
TMPDIR_E2E=$(mktemp -d -t require-live-guard-XXXXXX)
trap 'rm -rf "$TMPDIR_E2E"' EXIT INT TERM

PASS=0
FAIL=0

# Reproduce the EXACT guard logic from test_staging_full_saas.sh. Kept in
# lockstep with the host script: if the host logic changes, this test must
# change with it (and a divergence is itself a signal to re-prove the gate).
make_guard_runner() {
  cat <<'EOF'
REQUIRE_LIVE="${E2E_REQUIRE_LIVE:-0}"
LIVE_MILESTONES=""
live_milestone() {
  case " $LIVE_MILESTONES " in
    *" $1 "*) ;;
    *) LIVE_MILESTONES="$LIVE_MILESTONES $1" ;;
  esac
}
require_live_or_die() {
  [ "$REQUIRE_LIVE" = "1" ] || return 0
  local required="provisioned tenant_online workspace_online a2a_roundtrip"
  local m missing=""
  for m in $required; do
    case " $LIVE_MILESTONES " in
      *" $m "*) ;;
      *) missing="$missing $m" ;;
    esac
  done
  if [ -n "$missing" ]; then
    echo "MISSING:${missing}" >&2
    exit 5
  fi
}
EOF
}

# run_case <E2E_REQUIRE_LIVE value> <space-separated milestones to stamp>
# echoes the observed exit code.
run_case() {
  local require_live="$1"; shift
  local milestones="$1"; shift || true
  local stub observed m
  stub=$(mktemp "$TMPDIR_E2E/stub.XXXXXX")
  {
    echo "#!/usr/bin/env bash"
    echo "set -uo pipefail"
    make_guard_runner
    for m in $milestones; do
      echo "live_milestone $m"
    done
    echo "require_live_or_die"
    echo 'echo REACHED_END'
  } > "$stub"
  E2E_REQUIRE_LIVE="$require_live" bash "$stub" >/dev/null 2>&1
  observed=$?
  rm -f "$stub"
  echo "$observed"
}

assert_rc() {
  local label="$1" require_live="$2" milestones="$3" expected="$4"
  local observed
  observed=$(run_case "$require_live" "$milestones")
  if [ "$observed" = "$expected" ]; then
    echo "  ✓ $label: REQUIRE_LIVE=$require_live milestones='$milestones' → rc=$observed"
    PASS=$((PASS+1))
  else
    echo "  ✗ $label: REQUIRE_LIVE=$require_live milestones='$milestones' expected=$expected OBSERVED=$observed" >&2
    FAIL=$((FAIL+1))
  fi
}

echo "=== E2E_REQUIRE_LIVE fail-closed-on-skip guard proof ==="
echo

# DECISIVE (false-green trap): REQUIRE_LIVE=1 but NO lifecycle ran → exit 5.
assert_rc "require-live, nothing ran → exit 5 (the false-green trap)" \
  1 "" 5

# REQUIRE_LIVE=1 with a partial lifecycle (provisioned but no A2A) → exit 5.
assert_rc "require-live, partial lifecycle → exit 5" \
  1 "provisioned tenant_online workspace_online" 5

# REQUIRE_LIVE=1 with every required milestone → pass (rc=0).
assert_rc "require-live, full lifecycle → pass" \
  1 "provisioned tenant_online workspace_online a2a_roundtrip" 0

# Idempotency: duplicate stamps don't break membership; full set still passes.
assert_rc "require-live, duplicate stamps still pass" \
  1 "provisioned provisioned tenant_online workspace_online a2a_roundtrip a2a_roundtrip" 0

# Guard is a no-op when CI did not demand a live run: a non-live local run
# with nothing stamped must NOT exit 5 (we don't break local/debug runs).
assert_rc "no require-live, nothing ran → pass (guard is opt-in)" \
  0 "" 0
assert_rc "require-live unset-equivalent (0), partial → pass" \
  0 "provisioned" 0

# Extra unknown milestone is harmless as long as required set is present.
assert_rc "require-live, extra milestone tolerated" \
  1 "provisioned tenant_online workspace_online a2a_roundtrip extra_thing" 0

echo
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
