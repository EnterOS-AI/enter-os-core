#!/usr/bin/env bash
# Regression test for tests/e2e/lib/model_slug.sh.
#
# PR #2571 fixed a synth-E2E masking bug where MODEL_SLUG was hardcoded
# to "openai/gpt-4o" (slash-form). Without this regression test, dropping
# any branch of the case (or flipping a slug format) would silently revert
# behavior — the E2E only fails as "Could not resolve authentication method"
# at the very first message, after a successful tenant + workspace provision.
#
# Each branch must FAIL the test if the dispatch behavior changes, not
# just produce some non-empty string.
set -uo pipefail

# Resolve to the lib relative to this test file so the test runs from
# any cwd (CI, local invocation, repo root).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/e2e/lib/model_slug.sh
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
run_test "codex → slash-form fallback"                             codex       "openai/gpt-4o"
run_test "claude-code → OAuth/default alias"                      claude-code "sonnet"

# BARE registered BYOK id (registry_gen.go:88), NOT colon `minimax:…`. On
# claude-code the colon form is intentionally UNREGISTERED (the adapter can't
# strip `minimax:`) and 422s create-validation (internal#718, job 295075);
# bare resolves to provider=minimax BYOK. Pinned by the matrix test's
# colon-vs-slash-vs-bare triple in derive_provider_matrix_test.go.
got=$(unset E2E_MODEL_SLUG E2E_ANTHROPIC_API_KEY; E2E_MINIMAX_API_KEY="mx-test" pick_model_slug claude-code)
assert_eq "claude-code + MiniMax key → bare registered MiniMax model" "$got" "MiniMax-M2.7"

got=$(unset E2E_MODEL_SLUG E2E_MINIMAX_API_KEY; E2E_ANTHROPIC_API_KEY="sk-ant-test" pick_model_slug claude-code)
assert_eq "claude-code + Anthropic API key → Anthropic API model" "$got" "claude-sonnet-4-6"

got=$(unset E2E_MODEL_SLUG; E2E_MINIMAX_API_KEY="mx-priority" E2E_ANTHROPIC_API_KEY="sk-ant-loser" pick_model_slug claude-code)
assert_eq "claude-code + both keys → MiniMax priority (bare)"     "$got" "MiniMax-M2.7"

# ── seo-agent (claude-code-adapter template variant) ──
# seo-agent shares the claude-code dispatch branch (it reuses the claude-code
# adapter + the same copied providers block). Pin that it resolves IDENTICALLY
# to claude-code for every key path so a future refactor can't accidentally
# fork seo-agent's model selection from claude-code's.
run_test "seo-agent → claude-code default alias"                  seo-agent   "sonnet"

got=$(unset E2E_MODEL_SLUG E2E_ANTHROPIC_API_KEY; E2E_MINIMAX_API_KEY="mx-test" pick_model_slug seo-agent)
assert_eq "seo-agent + MiniMax key → bare MiniMax model (==claude-code)" "$got" "MiniMax-M2.7"

got=$(unset E2E_MODEL_SLUG E2E_MINIMAX_API_KEY; E2E_ANTHROPIC_API_KEY="sk-ant-test" pick_model_slug seo-agent)
assert_eq "seo-agent + Anthropic key → Anthropic model (==claude-code)" "$got" "claude-sonnet-4-6"

# ── google-adk (Gemini) ──
# AI-Studio BYOK arm → bare gemini-2.5-pro (providers.yaml runtimes.google-adk
# `google` arm). The platform/Vertex arm is selected via E2E_LLM_PATH=platform
# (a platform: id), not this dispatch. Pin the bare form so a drift to the
# platform id (which would change billing/route) is caught.
run_test "google-adk → AI-Studio bare gemini id"                  google-adk  "gemini-2.5-pro"

got=$(E2E_MODEL_SLUG="google_genai:gemini-2.5-pro" pick_model_slug google-adk)
assert_eq "google-adk + E2E_MODEL_SLUG override (adapter spelling)" "$got" "google_genai:gemini-2.5-pro"

# ── Fallback for unknown runtime ──
# Picks slash-form (hermes-shaped) since hermes is the historical
# default and most third-party runtimes behave hermes-like. Pinning
# this so a future "smarter" fallback (e.g., empty string, error) is
# a deliberate choice, not silent drift.
run_test "unknown runtime → slash-form fallback"                   gemini      "openai/gpt-4o"
run_test "empty runtime → slash-form fallback"                     ""          "openai/gpt-4o"

# ── Platform-managed path (E2E_LLM_PATH=platform) ──
# The moonshot/kimi NOT_CONFIGURED regression path (RFC#340 Fix A #2187).
# Selects the slash-namespaced platform model (default moonshot/kimi-k2.6),
# takes precedence over the per-key BYOK branches, and is itself overridden by
# E2E_MODEL_SLUG. These pins guard the harness's ability to drive the platform
# arm — the one the prod bug shipped on.
echo
echo "Test: pick_model_slug — platform-managed path (E2E_LLM_PATH=platform)"
echo

got=$(unset E2E_MODEL_SLUG E2E_DEFAULT_PLATFORM_MODEL; E2E_LLM_PATH=platform pick_model_slug claude-code)
assert_eq "claude-code + platform path → headline kimi model"      "$got" "moonshot/kimi-k2.6"

got=$(unset E2E_MODEL_SLUG E2E_DEFAULT_PLATFORM_MODEL; E2E_LLM_PATH=platform E2E_MINIMAX_API_KEY="mx-stray" pick_model_slug claude-code)
assert_eq "platform path beats a stray BYOK key (no mask)"         "$got" "moonshot/kimi-k2.6"

got=$(unset E2E_MODEL_SLUG; E2E_LLM_PATH=platform E2E_DEFAULT_PLATFORM_MODEL="minimax/MiniMax-M3" pick_model_slug claude-code)
assert_eq "platform path honours E2E_DEFAULT_PLATFORM_MODEL"        "$got" "minimax/MiniMax-M3"

got=$(unset E2E_DEFAULT_PLATFORM_MODEL; E2E_MODEL_SLUG="anthropic/claude-opus-4-7" E2E_LLM_PATH=platform pick_model_slug claude-code)
assert_eq "E2E_MODEL_SLUG still wins over platform path"            "$got" "anthropic/claude-opus-4-7"

# ── Override via E2E_MODEL_SLUG ──
# When the operator sets E2E_MODEL_SLUG, the per-runtime dispatch is
# bypassed. Used during workflow_dispatch to A/B specific slugs.
echo
echo "Test: pick_model_slug — E2E_MODEL_SLUG override"
echo

got=$(E2E_MODEL_SLUG="anthropic:claude-opus-4-7" pick_model_slug codex)
assert_eq "override beats codex default"                          "$got" "anthropic:claude-opus-4-7"

got=$(E2E_MODEL_SLUG="custom/whatever" pick_model_slug hermes)
assert_eq "override beats hermes default"                         "$got" "custom/whatever"

got=$(E2E_MODEL_SLUG="some-bare-id" pick_model_slug claude-code)
assert_eq "override beats claude-code default"                    "$got" "some-bare-id"

# Empty-string override does NOT activate (falls through to dispatch).
# This is the historical bash idiom: -n "" → false → no override. Pin
# it because changing this behavior (e.g. via -v test) would silently
# break the dispatch when an operator passes "" to clear an inherited
# env var.
got=$(E2E_MODEL_SLUG="" pick_model_slug codex)
assert_eq "empty-string override falls through to dispatch"       "$got" "openai/gpt-4o"

echo
echo "─────────────────────────────────────────────────"
echo "PASSED: $PASS"
echo "FAILED: $FAIL"
echo "─────────────────────────────────────────────────"
[ "$FAIL" -eq 0 ]
