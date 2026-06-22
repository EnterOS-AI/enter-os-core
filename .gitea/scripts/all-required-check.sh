#!/usr/bin/env bash
# all-required-check.sh — fail-closed aggregator for CI / all-required.
#
# Accepts "<job-name> <result>" pairs as positional arguments. Exits 0 only
# when EVERY supplied result is "success". Any other result (failure, skipped,
# cancelled, etc.) fails the gate.
#
# When executed directly, this script is invoked by .gitea/workflows/ci.yml
# with the needs.*.result values from the all-required sentinel. When sourced,
# it exposes the all_required_check() function for unit testing.
#
# Ref: molecule-core#656 (Phase 4 all-required hard-gate), #2615 (coverage).
set -euo pipefail

all_required_check() {
  local fail=0

  if [ $(($# % 2)) -ne 0 ]; then
    echo "::error::all-required-check: odd number of arguments (expected 'name result' pairs)"
    return 1
  fi

  while [ $# -ge 2 ]; do
    local name="$1" result="$2"
    shift 2
    printf 'CI / %s = %s\n' "$name" "$result"
    # `success` is the only green terminal state we accept. A plain
    # `needs:` job is only started when all needs succeed, so reaching
    # this step already implies success — but assert explicitly so a
    # future `if: always()` reintroduction (which WOULD let non-success
    # through) fails loudly instead of silently passing the gate.
    if [ "$result" != "success" ]; then
      echo "::error::aggregated CI job '${name}' did not succeed (result=${result})"
      fail=1
    fi
  done

  if [ "$fail" -ne 0 ]; then
    echo "::error::all-required: one or more aggregated CI jobs did not succeed"
    return 1
  fi

  echo "OK: all aggregated CI jobs succeeded — CI / all-required green."
}

# When executed (not sourced), run with positional args if supplied; otherwise
# fall back to the env vars populated by Gitea Actions. This lets CI call the
# script with no arguments while unit tests drive it with arbitrary pairs.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  if [ $# -gt 0 ]; then
    all_required_check "$@"
  else
    all_required_check \
      "Detect changes"             "${CHANGES_RESULT:-}" \
      "Platform (Go)"              "${PLATFORM_RESULT:-}" \
      "Canvas (Next.js)"           "${CANVAS_RESULT:-}" \
      "Shellcheck (E2E scripts)"   "${SHELLCHECK_RESULT:-}" \
      "Python Lint & Test"         "${PYTHON_LINT_RESULT:-}" \
      "Canvas Deploy Status"       "${CANVAS_DEPLOY_RESULT:-}"
  fi
fi
