#!/usr/bin/env bash
# Regression test for tests/e2e/lib/model_slug.sh.
#
# PR #2571 fixed a synth-E2E masking bug where MODEL_SLUG was hardcoded
# to "openai/gpt-4o" (slash-form) but langgraph's init_chat_model needs
# "openai:gpt-4o" (colon-form). Fix shipped as a per-runtime case
# statement. Without this regression test, dropping any branch of the
# case (or flipping a slug format) would silently revert behavior — the
# E2E only fails as "Could not resolve authentication method" at the
# very first message, after a successful tenant + workspace provision.
#
# Each branch must FAIL the test if the dispatch behavior changes, not
# just produce some non-empty string.
set -uo pipefail

# Resolve to the lib relative to this test file so the test runs from
# any cwd (CI, local invocation, repo root).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/model_slug.sh
source "$SCRIPT_DIR/lib/model_slug.sh"

PASS=0
FAIL=0

assert_eq() {
  local label="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    echo "  ✓ $label"
    PASS=$((PASS+1))
  else
    echo "  ✗ $label: got=$(printf %q "$got")  want=$(printf %q "$want")" >&2
    FAIL=$((FAIL+1))
  fi
}

run_test() {
  local label="$1" runtime="$2" want="$3"
  # Pin per-test isolation: explicitly unset the override so a leaked
  # E2E_MODEL_SLUG from caller env can't poison the dispatch branches.
  local got
  got=$(unset E2E_MODEL_SLUG; pick_model_slug "$runtime")
  assert_eq "$label" "$got" "$want"
}

echo "Test: pick_model_slug — per-runtime dispatch"
echo

# ── Per-runtime branches (the load-bearing ones for synth-E2E) ──
run_test "hermes → slash-form (derive-provider.sh contract)"       hermes      "openai/gpt-4o"
run_test "langgraph → colon-form (init_chat_model contract)"       langgraph   "openai:gpt-4o"
run_test "claude-code → bare model name (entry-id form)"           claude-code "sonnet"

# ── Fallback for unknown runtime ──
# Picks slash-form (hermes-shaped) since hermes is the historical
# default and most third-party runtimes behave hermes-like. Pinning
# this so a future "smarter" fallback (e.g., empty string, error) is
# a deliberate choice, not silent drift.
run_test "unknown runtime → slash-form fallback"                   gemini      "openai/gpt-4o"
run_test "empty runtime → slash-form fallback"                     ""          "openai/gpt-4o"

# ── Override via E2E_MODEL_SLUG ──
# When the operator sets E2E_MODEL_SLUG, the per-runtime dispatch is
# bypassed. Used during workflow_dispatch to A/B specific slugs.
echo
echo "Test: pick_model_slug — E2E_MODEL_SLUG override"
echo

got=$(E2E_MODEL_SLUG="anthropic:claude-opus-4-7" pick_model_slug langgraph)
assert_eq "override beats langgraph default"                      "$got" "anthropic:claude-opus-4-7"

got=$(E2E_MODEL_SLUG="custom/whatever" pick_model_slug hermes)
assert_eq "override beats hermes default"                         "$got" "custom/whatever"

got=$(E2E_MODEL_SLUG="some-bare-id" pick_model_slug claude-code)
assert_eq "override beats claude-code default"                    "$got" "some-bare-id"

# Empty-string override does NOT activate (falls through to dispatch).
# This is the historical bash idiom: -n "" → false → no override. Pin
# it because changing this behavior (e.g. via -v test) would silently
# break the dispatch when an operator passes "" to clear an inherited
# env var.
got=$(E2E_MODEL_SLUG="" pick_model_slug langgraph)
assert_eq "empty-string override falls through to dispatch"       "$got" "openai:gpt-4o"

echo
echo "─────────────────────────────────────────────────"
echo "PASSED: $PASS"
echo "FAILED: $FAIL"
echo "─────────────────────────────────────────────────"
[ "$FAIL" -eq 0 ]
