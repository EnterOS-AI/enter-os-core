#!/usr/bin/env bash
# Regression test for the SECRETS_JSON branching in
# tests/e2e/test_staging_full_saas.sh (lines ~322-368).
#
# The synth-E2E canary picks one of two LLM auth paths based on which
# E2E_*_API_KEY is set. The branch order is load-bearing:
#
#   E2E_MINIMAX_API_KEY first   → claude-code MiniMax path (cheap canary
#                                  default since 2026-05-03; routes via
#                                  workspace-configs-templates/claude-
#                                  code-default/config.yaml's `minimax`
#                                  provider entry).
#
#   E2E_OPENAI_API_KEY second  → langgraph + hermes legacy path (kept
#                                  as fallback for operator dispatches
#                                  that need the OpenAI-shaped
#                                  HERMES_CUSTOM_* env block).
#
# Without this gate, a future "tidy up the if/elif" refactor could
# silently flip the precedence (OpenAI wins when both are set →
# claude-code workspace boots without MINIMAX_API_KEY → 401 at first
# turn → canary red without any signal that the wrong key shape was
# selected). The 2026-05-03 OpenAI-quota incident took ~16h to
# diagnose for exactly this class of "looks like an LLM problem,
# was actually a wiring problem" failure.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SAAS_SCRIPT="$SCRIPT_DIR/test_staging_full_saas.sh"

if [ ! -f "$SAAS_SCRIPT" ]; then
  echo "FATAL: cannot locate test_staging_full_saas.sh at $SAAS_SCRIPT" >&2
  exit 2
fi

PASS=0
FAIL=0

assert_eq() {
  local label="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    echo "  ✓ $label"
    PASS=$((PASS+1))
  else
    echo "  ✗ $label" >&2
    echo "      got:  $got" >&2
    echo "      want: $want" >&2
    FAIL=$((FAIL+1))
  fi
}

# Extract just the SECRETS_JSON block from the saas script and source
# it into a sub-shell so we can run the branching logic in isolation.
# Anchor on the comment header so a structural refactor that moves the
# block fails this test loudly rather than silently sourcing nothing.
extract_block() {
  awk '
    /^# ─── 5\. Provision parent workspace/ {capture=1; next}
    capture && /^MODEL_SLUG=/ {exit}
    capture {print}
  ' "$SAAS_SCRIPT"
}

BLOCK=$(extract_block)
if [ -z "$BLOCK" ]; then
  echo "FATAL: SECRETS_JSON block not found in $SAAS_SCRIPT — refactor anchor changed?" >&2
  exit 2
fi

# Run the extracted block in a clean env, capturing SECRETS_JSON.
run_block() {
  # Caller passes vars on the command line, e.g.
  #   run_block E2E_MINIMAX_API_KEY=mx-test
  env -i PATH="$PATH" "$@" bash -c "
    set -uo pipefail
    $BLOCK
    echo \"\$SECRETS_JSON\"
  " 2>/dev/null | tail -1
}

# Resolve a JSON key from the captured payload using python3 (already
# a hard dep of the saas script). Returns empty string on missing key.
get_json_key() {
  local payload="$1" key="$2"
  python3 -c "
import json, sys
p = json.loads(sys.argv[1])
print(p.get(sys.argv[2], ''))
" "$payload" "$key"
}

list_json_keys() {
  python3 -c "
import json, sys
p = json.loads(sys.argv[1])
print(','.join(sorted(p.keys())))
" "$1"
}

echo "Test: SECRETS_JSON branching in test_staging_full_saas.sh"
echo

# ── Branch 1: MiniMax wins when set ──
SECRETS_JSON=$(run_block E2E_MINIMAX_API_KEY=mx-test)
assert_eq "MiniMax key set → MINIMAX_API_KEY in payload" \
  "$(get_json_key "$SECRETS_JSON" MINIMAX_API_KEY)" "mx-test"
assert_eq "MiniMax-only payload contains exactly MINIMAX_API_KEY" \
  "$(list_json_keys "$SECRETS_JSON")" "MINIMAX_API_KEY"

# ── Branch 1 precedence: MiniMax beats OpenAI when both set ──
# Critical: the 2026-05-03 incident shape was "two paths exist, wrong
# one wins". The bash if/elif must keep MiniMax above OpenAI so the
# claude-code default canary doesn't accidentally use the (more
# expensive, quota-burnt) OpenAI key.
SECRETS_JSON=$(run_block E2E_MINIMAX_API_KEY=mx-priority E2E_OPENAI_API_KEY=oai-loser)
assert_eq "Both keys set → MiniMax wins" \
  "$(get_json_key "$SECRETS_JSON" MINIMAX_API_KEY)" "mx-priority"
assert_eq "Both keys set → OpenAI block NOT emitted" \
  "$(get_json_key "$SECRETS_JSON" OPENAI_API_KEY)" ""
assert_eq "Both keys set → no HERMES_* leakage from OpenAI branch" \
  "$(get_json_key "$SECRETS_JSON" HERMES_INFERENCE_PROVIDER)" ""

# ── Branch 2: OpenAI used when MiniMax absent ──
SECRETS_JSON=$(run_block E2E_OPENAI_API_KEY=oai-test)
assert_eq "Only OpenAI set → OPENAI_API_KEY in payload" \
  "$(get_json_key "$SECRETS_JSON" OPENAI_API_KEY)" "oai-test"
assert_eq "Only OpenAI set → HERMES_CUSTOM_API_KEY mirrors OpenAI key" \
  "$(get_json_key "$SECRETS_JSON" HERMES_CUSTOM_API_KEY)" "oai-test"
assert_eq "Only OpenAI set → MODEL_PROVIDER pinned to colon-form" \
  "$(get_json_key "$SECRETS_JSON" MODEL_PROVIDER)" "openai:gpt-4o"
assert_eq "Only OpenAI set → MINIMAX_API_KEY NOT emitted" \
  "$(get_json_key "$SECRETS_JSON" MINIMAX_API_KEY)" ""

# ── No keys: empty payload ──
SECRETS_JSON=$(run_block)
assert_eq "No keys set → SECRETS_JSON is empty object" \
  "$SECRETS_JSON" "{}"

echo
echo "─────────────────────────────────────────────────"
echo "PASSED: $PASS"
echo "FAILED: $FAIL"
echo "─────────────────────────────────────────────────"
[ "$FAIL" -eq 0 ]
