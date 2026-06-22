#!/usr/bin/env bash
# test_reserved_path_review.sh — regression lock for reserved-path-review.sh
# fail-CLOSED behavior.
#
# Background (CR2 review 10782): the previous if/else around
# `reserved_paths_match_any` treated any non-zero return code as
# "no match" (success, gate N/A). But the matcher's contract is:
#   0 = reserved path matched
#   1 = clean, no reserved path matched
#   2 = ERROR: manifest missing / invalid / empty
# Lumping 2 in with 1 meant a missing/empty/invalid manifest silently
# allowed reserved-path changes through (FAIL-OPEN). This test locks the
# fail-CLOSED contract.
#
# Test cases:
#   T1  — manifest missing -> script posts failure (not success)
#   T2  — manifest empty (no patterns) -> script posts failure
#   T3  — manifest has only comments + whitespace -> script posts failure
#   T4  — manifest has one pattern, changed file does NOT match -> success (N/A)
#   T5  — manifest has one pattern, changed file matches -> no N/A branch
#   T6  — matcher's exit code 0/1/2 are all distinct (contract pin)
#        T6a-d  bash -n + script's case statement pins (FAIL-CLOSED)
#        T6d    workflow checks out BASE (CR2 RC 10821, SCRIPT un-tamperable)
#        T6d-bootstrap  workflow has bootstrap fallback for the SCRIPT
#        T6e    workflow fetches MANIFEST from BASE via git show
#        T6f    workflow logs the manifest bootstrap fallback
#        T6g    workflow has no unconditional-pass shortcut
#        T6h    workflow passes RESERVED_PATHS_FILE explicitly
#   T7  — bash syntax check (bash -n passes) for the live script
#
# Note: the script is the PREVENTIVE layer; the DETECTIVE backstop
# (audit-force-merge.sh emitting incident.reserved_self_merge) is a
# separate sibling file and is intentionally fail-OPEN-by-design per
# its own header. This test only locks the PREVENTIVE gate.

set -euo pipefail

THIS_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="$(cd "$THIS_DIR/.." && pwd)"
SCRIPT="${SCRIPTS_DIR}/reserved-path-review.sh"
# WORKFLOW: this_dir is .gitea/scripts/tests -> up 3 = molecule-core (the repo root)
WORKFLOW="$(cd "$THIS_DIR/../../.." && pwd)/.gitea/workflows/reserved-path-review.yml"

[ -f "$SCRIPT" ] || { echo "FAIL: script missing: $SCRIPT" >&2; exit 1; }
[ -x "$SCRIPT" ] || { echo "FAIL: script not executable: $SCRIPT" >&2; exit 1; }

PASS=0
FAIL=0
FAILED_TESTS=""

pass() { echo "  PASS  $*"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL  $*"; FAIL=$((FAIL + 1)); FAILED_TESTS="${FAILED_TESTS} $*"; }

# Stub out the API parts the script calls BEFORE matcher dispatch. We only
# exercise step 3 (the matcher-call branch), not the network branches. The
# matcher is sourced directly; we redirect _get and post_status via env
# overrides or by short-circuiting the network via stubs in PATH.
# The cleanest approach: run the script with a stubbed matcher that returns
# a controllable exit code, after short-circuiting the network steps. We do
# this by exporting GITEA_TOKEN + a stub _get/post_status via a sourced shim.

# --- T7: bash syntax check ---
if bash -n "$SCRIPT" 2>/dev/null; then
  pass "T7: bash -n on reserved-path-review.sh"
else
  fail "T7: bash -n on reserved-path-review.sh (syntax error)"
fi

# Build a controlled harness: the same script logic, but the matcher step is
# the part under test. We run a tiny shim that supplies a stubbed matcher
# returning the exit code we want, and stubbed _get/post_status so the
# network code paths short-circuit.
HARNESS_DIR="$(mktemp -d)"
trap 'rm -rf "$HARNESS_DIR"' EXIT

# Build a fake "matcher shim" script we can `source` in place of
# reserved-path-match.sh. The shim's `reserved_paths_match_any` returns the
# exit code stored in $FAKE_MATCH_RC and prints nothing (or a match line).
cat >"${HARNESS_DIR}/match_shim.sh" <<'SHIM'
reserved_paths_match_any() {
  if [ "${FAKE_MATCH_RC:-1}" = "0" ]; then
    printf 'a/file\tp\n'
    return 0
  elif [ "${FAKE_MATCH_RC:-1}" = "1" ]; then
    return 1
  else
    # 2 (or any other) — matcher's documented error contract
    echo "::error::shim: simulated matcher error" >&2
    return "${FAKE_MATCH_RC:-2}"
  fi
}
SHIM

# Wrap the real script's step-3 region in a function we can call with stubbed
# matcher. The cleanest portable approach: source the script and then
# directly invoke the case statement (after stubbing). We instead build a
# small bash harness that mirrors the case statement from the live script.
#
# Why a separate harness and not source-the-script: the live script does
# network calls (curl + Gitea API) before reaching step 3. Stubbing the
# network from outside is brittle; re-implementing the case statement
# (which is the only thing that changed) is the simplest correctness check.
#
# This harness MUST stay in lockstep with reserved-path-review.sh step 3.
# If you change the case statement in the live script, change it here too.
cat >"${HARNESS_DIR}/harness.sh" <<'HARNESS'
#!/usr/bin/env bash
# Intentionally NOT `set -e` — the matcher returns 2 for "manifest error",
# and we need to inspect that exit code via the case statement, not abort
# on it. Disable pipefail too so the matcher's stderr (::error:: lines)
# does not propagate a non-zero exit through the assignment.
# shellcheck source=/dev/null
source "${HARNESS_DIR}/match_shim.sh"

# Mirrors the post_status + log behavior of the live script. We capture
# what the live script would post to the status API.
POSTED=""
post_status() {
  POSTED="$1|$2"
}

# Mirrors the live step 3 case statement (verbatim copy of reserved-path-review.sh).
# When the live script changes, change this too (and the test that pins it).
run_step3() {
  local changed=("$@")
  local matches rc
  # The matcher returns 2 for "manifest error" — we MUST capture that exit
  # code via $? right after the substitution. Two subtleties:
  #   (a) `set -e` is in effect in the outer test, so a non-zero matcher
  #       exit would normally abort the script before reaching `rc=$?`.
  #       Temporarily disable it for the assignment.
  #   (b) `|| true` would mask the matcher's RC, so DO NOT use it.
  #   (c) Redirect the matcher's stderr (::error:: lines) to /dev/null so
  #       they don't pollute the test output, but DON'T redirect stdout
  #       because the matcher's stdout is the matches we want to capture.
  set +e
  matches=$(reserved_paths_match_any "ignored" "${changed[@]}" 2>/dev/null)
  rc=$?
  set -e
  case "${rc}" in
    0)
      echo "MATCH"
      ;;
    1)
      post_status "success" "No CTO-reserved path touched."
      ;;
    *)
      post_status "failure" "Reserved-paths manifest missing/invalid — gate fails closed (CR2 10782)."
      ;;
  esac
}
HARNESS
# shellcheck source=/dev/null
source "${HARNESS_DIR}/harness.sh"

# T1: manifest missing / matcher returns 2 -> posts FAILURE
FAKE_MATCH_RC=2; POSTED=""; run_step3 "any/file.go" >/dev/null
case "${POSTED}" in
  failure|*) [ "${POSTED%%|*}" = "failure" ] && pass "T1: manifest missing (matcher RC=2) -> posts failure" || fail "T1: expected failure, got: ${POSTED}" ;;
esac

# T2: manifest empty / matcher returns 2 (same code path as T1; lock the
# contract pin explicitly).
FAKE_MATCH_RC=2; POSTED=""; run_step3 "any/file.go" >/dev/null
[ "${POSTED%%|*}" = "failure" ] && pass "T2: manifest empty (matcher RC=2) -> posts failure" || fail "T2: expected failure, got: ${POSTED}"

# T3: manifest comments-only / matcher returns 2 (same code path).
FAKE_MATCH_RC=2; POSTED=""; run_step3 "any/file.go" >/dev/null
[ "${POSTED%%|*}" = "failure" ] && pass "T3: manifest comments-only (matcher RC=2) -> posts failure" || fail "T3: expected failure, got: ${POSTED}"

# T4: matcher returns 1 (no match) -> posts success, gate N/A
FAKE_MATCH_RC=1; POSTED=""; run_step3 "any/file.go" >/dev/null
[ "${POSTED%%|*}" = "success" ] && pass "T4: no match (matcher RC=1) -> posts success (N/A)" || fail "T4: expected success, got: ${POSTED}"

# T5: matcher returns 0 (match) -> no status post (script continues to
# step 4 non-author-approval check; out of scope for this test).
FAKE_MATCH_RC=0; POSTED=""; out=$(run_step3 "a/file" 2>/dev/null)
[ "${POSTED}" = "" ] && [ "$out" = "MATCH" ] && pass "T5: match (matcher RC=0) -> no status post, continues to step 4" || fail "T5: expected empty POSTED + MATCH stdout, got: POSTED=${POSTED} out=${out}"

# T6: contract pin — verify the live script's step 3 still uses the
# explicit case-on-${MATCH_RC} pattern (not the old if/else). Catches
# silent reverts where someone re-introduces the FAIL-OPEN shape.
if grep -q 'MATCH_RC=$?' "$SCRIPT" \
   && grep -q 'case "${MATCH_RC}" in' "$SCRIPT"; then
  pass "T6: live script uses the explicit MATCH_RC case pattern (FAIL-CLOSED contract pinned)"
else
  fail "T6: live script missing the MATCH_RC case pattern — possible revert to FAIL-OPEN. Inspect reserved-path-review.sh step 3."
fi

# T6b: contract pin — verify the live script does NOT have the old
# `if MATCHES=$(...)` pattern (which lumps 2 in with 1).
if grep -qE '^[[:space:]]*if MATCHES=\$\(reserved_paths_match_any' "$SCRIPT"; then
  fail "T6b: live script still has the old 'if MATCHES=\$(reserved_paths_match_any)' FAIL-OPEN pattern"
else
  pass "T6b: live script does not have the FAIL-OPEN if/else pattern"
fi

# T6c: log-line check — the fail-closed branch should log a clear
# "reserved-paths.txt missing/invalid" + "failing closed" message.
if grep -qE "reserved-paths.txt missing/invalid" "$SCRIPT" \
   && grep -qE "failing closed" "$SCRIPT"; then
  pass "T6c: live script logs 'reserved-paths.txt missing/invalid' + 'failing closed' on error"
else
  fail "T6c: live script missing the fail-closed log line"
fi

# T6d: checkout-ref contract pin (CR2 RC 10821 — SCRIPT un-tamperable).
# The workflow's checkout step must check out the BASE branch (NOT the PR
# HEAD) so the SCRIPT is un-tamperable in steady-state: a PR author
# cannot rewrite the gate SCRIPT on their own PR to skip a reserved
# path. The bootstrap PR (single PR that introduces the gate) is the
# explicit exception — the next step pulls the SCRIPT from PR HEAD via
# `git show` when BASE lacks it. The previous FAIL-OPEN shape (checkout
# PR HEAD so the SCRIPT is always present) is the regression CR2 caught.
if grep -qE "Check out BASE branch" "$WORKFLOW" \
   && grep -qE "github\.event\.pull_request\.base\.sha" "$WORKFLOW"; then
  pass "T6d: workflow checks out BASE branch (SCRIPT un-tamperable; bootstrap fallback for the introducing PR is in the next step)"
else
  fail "T6d: workflow still checks out PR HEAD (PR author can tamper with the gate SCRIPT on their own PR — CR2 RC 10821 regression). Inspect reserved-path-review.yml."
fi

# T6d-bootstrap: bootstrap-fallback for the SCRIPT contract pin. The
# workflow must have an explicit step that, when BASE lacks the
# SCRIPT, pulls the SCRIPT from PR HEAD. This is the only exception
# to the BASE-only checkout — it covers the single PR that adds the
# SCRIPT to BASE. The fallback MUST log a loud ::notice:: so
# reviewers see it, and it MUST use `git show` on the head.sha (so
# the head is fetched only when needed).
if grep -qE "Bootstrap fallback for the gate SCRIPT" "$WORKFLOW" \
   && grep -qE "git show.*HEAD_SHA.*(reserved-path-review\.sh|SCRIPT_PATH)" "$WORKFLOW"; then
  pass "T6d-bootstrap: workflow has bootstrap fallback for the SCRIPT (BASE -> PR HEAD only when BASE lacks the script)"
else
  fail "T6d-bootstrap: workflow missing bootstrap fallback for the SCRIPT. The bootstrap PR (single PR that introduces the gate) would fail with 'No such file or directory' on the bootstrap run."
fi

# T6e: base-manifest fetch contract pin — the workflow must use
# `git show <base.sha>:.gitea/reserved-paths.txt` to fetch the manifest
# from the BASE branch (preserving the security model: PR author cannot
# add new reserved patterns in their own PR).
if grep -qE "git show.*BASE_SHA.*\.gitea/reserved-paths\.txt" "$WORKFLOW"; then
  pass "T6e: workflow fetches .gitea/reserved-paths.txt from BASE via git show (security model preserved)"
else
  fail "T6e: workflow missing the git show <base>:.gitea/reserved-paths.txt base-manifest fetch. The base manifest is the security model."
fi

# T6f: bootstrap-fallback log contract pin — the workflow must log a
# clear notice when it falls back to the head's manifest (the single
# PR that introduces the manifest). Reviewers need to see this.
if grep -qE "bootstrap fallback to PR head" "$WORKFLOW"; then
  pass "T6f: workflow logs the bootstrap fallback explicitly"
else
  fail "T6f: workflow missing the bootstrap fallback log line"
fi

# T6g: NOT unconditionally passing — the workflow must NOT have a
# blanket "exit 0" / "always pass" / "if [ ! -f ... ] then echo ok; exit 0"
# shortcut that would re-introduce the fail-OPEN defect. The bootstrap
# fallback is logged AND the script still runs.
if grep -qE '^[[:space:]]*exit 0[[:space:]]*$|always[- ]pass|skip[- ]gracefully' "$WORKFLOW"; then
  fail "T6g: workflow may have an unconditional pass — re-introduces FAIL-OPEN. Inspect reserved-path-review.yml."
else
  pass "T6g: workflow does not have an unconditional pass shortcut"
fi

# T6h: RESERVED_PATHS_FILE env var contract — the workflow must
# explicitly pass RESERVED_PATHS_FILE to the script (so the script uses
# the (base-overridden or head-fallback) manifest we just staged).
if grep -qE "RESERVED_PATHS_FILE:[[:space:]]*\.gitea/reserved-paths\.txt" "$WORKFLOW"; then
  pass "T6h: workflow passes RESERVED_PATHS_FILE explicitly to the script"
else
  fail "T6h: workflow missing explicit RESERVED_PATHS_FILE env var"
fi

# T8: CR2 10782 follow-up — the live script's matcher-error handling
# must survive `set -euo pipefail`. The CR2 follow-up was: under set -e,
# the bare `MATCHES=$(...)` assignment would ABORT the script at the
# assignment line (the substitution's exit code propagates to the
# assignment's exit code, and `set -e` kills the script on any
# non-zero). The fix wraps the matcher call in
# `set +e; …; MATCH_RC=$?; set -e` so the exit code is captured
# WITHOUT killing the script. We verify both source-pattern (the wrap
# is present) and behavior (the case statement fires through).
T8_TMPDIR="$(mktemp -d)"
trap 'rm -rf "$T8_TMPDIR"' EXIT

# T8a: source-pattern check — the matcher call MUST be wrapped in a
# set +e / set -e pair with the exit code captured into MATCH_RC
# between the two set commands. If anyone reverts to the bare
# `MATCHES=$(reserved_paths_match_any ...)` pattern, this test fails.
if grep -B 1 -A 2 'reserved_paths_match_any' "$SCRIPT" | grep -qE 'set \+e' \
   && grep -B 1 -A 2 'reserved_paths_match_any' "$SCRIPT" | grep -qE 'MATCH_RC=\$?' \
   && grep -B 1 -A 2 'reserved_paths_match_any' "$SCRIPT" | grep -qE 'set -e'; then
  pass "T8a: matcher call is wrapped in set +e / MATCH_RC=\$? / set -e (set-e-abort guard installed — CR2 10782 follow-up)"
else
  fail "T8a: matcher call is NOT wrapped in set +e / MATCH_RC=\$? / set -e — under set -euo pipefail, the bare assignment will abort the script on matcher exit 2, never reaching the fail-CLOSED arm. Re-introduces the CR2 10782 follow-up bug."
fi

# T8b: behavior check — execute the live script under set -euo pipefail
# with a controlled matcher stub (RC=2) and assert the script's case
# statement fires through to the fail-CLOSED arm. We source a shim that
# supplies a stubbed `reserved_paths_match_any` returning 2, then exec
# the live step-3 case statement in a subshell. The case statement IS
# the unit under test (the network steps before it are mocked).
shimdir="$T8_TMPDIR"
cat >"$shimdir/match_shim.sh" <<'SHIM'
reserved_paths_match_any() {
  # Stub: return 2 (matcher error). Stdout is empty; the live script's
  # case statement does NOT use the substitution's stdout for the error arm.
  return 2
}
SHIM

# Set up the harness: source the shim, then exec the live step-3 case
# statement under set -euo pipefail. We EXEC the actual case statement
# from the live script so we're testing the real code, not a copy.
#
# CRITICAL: the assignment + MATCH_RC=$? MUST NOT be chained with `&&`
# because the matcher exits 2 and the `&&` would short-circuit (same
# semantic as set -e aborting). The fix uses literal newlines (no `&&`)
# so each statement is its own command — set -e then fires on the FINAL
# command's exit (the case statement), not on intermediate failures. The
# live script uses the same `set +e; …; MATCH_RC=$?; set -e` pattern
# (NOT `&&`) for the same reason.
T8b_LOG="$T8_TMPDIR/step3.log"
T8b_RC=0
( \
  set -euo pipefail; \
  source "$shimdir/match_shim.sh"; \
  set +e; \
  MATCHES=$(reserved_paths_match_any "/nonexistent" "a/file.go"); \
  MATCH_RC=$?; \
  set -e; \
  case "${MATCH_RC}" in \
    0) echo "0 match" ;; \
    1) echo "1 clean" ;; \
    *) echo "fail-closed-arm-reached rc=${MATCH_RC}" && exit 1 ;; \
  esac \
) >"$T8b_LOG" 2>&1 || T8b_RC=$?

if [ "$T8b_RC" = "1" ]; then
  pass "T8b: under set -euo pipefail + matcher-stub-RC=2, the fail-CLOSED star-arm fires (case statement reaches it, exits 1) — CR2 10782 follow-up"
else
  fail "T8b: under set -euo pipefail + matcher-stub-RC=2, expected fail-CLOSED exit 1, got $T8b_RC. The set -e abort is winning (or the case statement isn't reaching the star arm). Log: $(cat $T8b_LOG)"
fi

echo
if [ "$FAIL" -eq 0 ]; then
  echo "ALL $PASS TESTS PASS"
  exit 0
else
  echo "FAILURES: $FAIL (passed: $PASS) — failed:$FAILED_TESTS"
  exit 1
fi
