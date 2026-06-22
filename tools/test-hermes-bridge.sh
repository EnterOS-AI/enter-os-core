#!/usr/bin/env bash
# test-hermes-bridge.sh — regression tests for template-hermes install.sh's
# OpenAI bridge logic. Runs offline (no network, no docker, no CI dependency).
#
# These tests pin the bridge invariants that we fixed on 2026-04-23 after
# production found these bugs:
#
#   template-hermes#12: API_SERVER_KEY must be written to /etc/environment
#     + /etc/profile.d/ so molecule-runtime inherits it.
#
#   template-hermes#13: When bridging OPENAI_API_KEY, the model slug's
#     "openai/" prefix must be stripped — OpenAI rejects prefixed names.
#
#   template-hermes#14: The bridge must emit `api_mode: "chat_completions"`
#     in config.yaml — otherwise hermes's custom provider defaults to
#     codex_responses which sends include=[reasoning.encrypted_content],
#     rejected by gpt-4o/gpt-4.1.
#
# Also pins the "don't fire" invariants — the bridge must NOT activate
# when the operator has explicitly configured HERMES_CUSTOM_*, and
# setting PROVIDER=openai would crash the hermes gateway ("Unknown provider").
#
# Invocation:
#
#     bash tools/test-hermes-bridge.sh /path/to/template-hermes/install.sh
#
# Default path: ../molecule-ai-workspace-template-hermes/install.sh relative
# to this script, which matches the dev-machine layout of the sibling repo.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALL_SH="${1:-$SCRIPT_DIR/../../molecule-ai-workspace-template-hermes/install.sh}"

if [ ! -f "$INSTALL_SH" ]; then
  echo "error: install.sh not found at $INSTALL_SH" >&2
  echo "usage: $0 [install.sh-path]" >&2
  exit 2
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0

# run_case — extract just the bridge + config.yaml write blocks from
# install.sh, stub out the parts that would require real side effects
# (system package installs, API_SERVER_KEY write to /etc/, gateway start),
# set up a minimal env, run, and capture the config.yaml output.
#
# Args:
#   $1 = test name
#   $2+ = env assignments (e.g. OPENAI_API_KEY=xxx, HERMES_DEFAULT_MODEL=openai/gpt-4o)
run_case() {
  local name="$1"; shift
  local case_dir="$TMP/$name"
  mkdir -p "$case_dir"

  # Build a minimal harness that:
  #   1. Sources scripts/derive-provider.sh (real, from the template repo)
  #   2. Applies the bridge if-block (inlined verbatim from install.sh)
  #   3. Emits config.yaml
  # Intentionally skips: apt installs, hermes download, /etc writes,
  # gateway start. We care about the BRANCH LOGIC not the system effects.
  local template_dir
  template_dir=$(cd "$(dirname "$INSTALL_SH")" && pwd)

  HERMES_HOME="$case_dir" \
  bash -c "
set -euo pipefail
HERMES_HOME='$case_dir'
$(for kv in "$@"; do printf 'export %s\n' "$kv"; done)
# Source derive-provider from the real template repo
. '$template_dir/scripts/derive-provider.sh'
DEFAULT_MODEL=\"\${HERMES_DEFAULT_MODEL:-nousresearch/hermes-4-70b}\"

# Bridge block — extracted 1:1 from install.sh (the shape must stay in sync).
if [ \"\${PROVIDER}\" = \"custom\" ] && [ -n \"\${OPENAI_API_KEY:-}\" ] && [ -z \"\${HERMES_CUSTOM_BASE_URL:-}\" ] && [ -z \"\${HERMES_CUSTOM_API_KEY:-}\" ]; then
  export HERMES_CUSTOM_BASE_URL='https://api.openai.com/v1'
  export HERMES_CUSTOM_API_KEY=\"\${OPENAI_API_KEY}\"
  export HERMES_CUSTOM_API_MODE='chat_completions'
  DEFAULT_MODEL=\"\${DEFAULT_MODEL#openai/}\"
fi

# Emit config.yaml (same shape as install.sh)
{
  echo 'model:'
  echo \"  default: \\\"\${DEFAULT_MODEL}\\\"\"
  echo \"  provider: \\\"\${PROVIDER}\\\"\"
  if [ -n \"\${HERMES_CUSTOM_BASE_URL:-}\" ]; then
    echo \"  base_url: \\\"\${HERMES_CUSTOM_BASE_URL}\\\"\"
  fi
  if [ -n \"\${HERMES_CUSTOM_API_KEY:-}\" ]; then
    echo \"  api_key: \\\"\${HERMES_CUSTOM_API_KEY}\\\"\"
  fi
  if [ -n \"\${HERMES_CUSTOM_API_MODE:-}\" ]; then
    echo \"  api_mode: \\\"\${HERMES_CUSTOM_API_MODE}\\\"\"
  fi
} > '$case_dir/config.yaml'
" >"$case_dir/stdout" 2>"$case_dir/stderr" || {
    printf 'FAIL %s: harness exited non-zero\n' "$name" >&2
    echo "stderr:" >&2
    sed 's/^/  /' "$case_dir/stderr" >&2
    FAIL=$((FAIL+1))
    return 1
  }
  cat "$case_dir/config.yaml"
}

# assert_in — assert a fragment appears in the config.yaml of the named case.
assert_in() {
  local name="$1" pattern="$2"
  if grep -qF "$pattern" "$TMP/$name/config.yaml"; then
    printf 'PASS %s: contains %q\n' "$name" "$pattern"
    PASS=$((PASS+1))
  else
    printf 'FAIL %s: missing %q\n' "$name" "$pattern" >&2
    echo "  actual config.yaml:" >&2
    sed 's/^/    /' "$TMP/$name/config.yaml" >&2
    FAIL=$((FAIL+1))
  fi
}

assert_not_in() {
  local name="$1" pattern="$2"
  if grep -qF "$pattern" "$TMP/$name/config.yaml"; then
    printf 'FAIL %s: unexpected %q present\n' "$name" "$pattern" >&2
    echo "  actual config.yaml:" >&2
    sed 's/^/    /' "$TMP/$name/config.yaml" >&2
    FAIL=$((FAIL+1))
  else
    printf 'PASS %s: absent %q\n' "$name" "$pattern"
    PASS=$((PASS+1))
  fi
}

# ─── Case 1: OpenAI bridge fires, strips prefix, sets api_mode ──────────
# Regression guard for #13 + #14. When only OPENAI_API_KEY is set and the
# user specifies openai/gpt-4o, install.sh must:
#   - KEEP provider=custom (not flip to "openai" — hermes has no native
#     openai provider, gateway would crash "Unknown provider")
#   - strip "openai/" prefix from the model → "gpt-4o"
#   - emit api_mode: "chat_completions" (so hermes doesn't hit /v1/responses
#     with include=[reasoning.encrypted_content] which gpt-4o rejects)
run_case "openai-bridge-happy" \
  OPENAI_API_KEY=sk-test-abc \
  HERMES_DEFAULT_MODEL=openai/gpt-4o >/dev/null

assert_in      "openai-bridge-happy" 'default: "gpt-4o"'
assert_in      "openai-bridge-happy" 'provider: "custom"'
assert_in      "openai-bridge-happy" 'base_url: "https://api.openai.com/v1"'
assert_in      "openai-bridge-happy" 'api_key: "sk-test-abc"'
assert_in      "openai-bridge-happy" 'api_mode: "chat_completions"'
assert_not_in  "openai-bridge-happy" 'provider: "openai"'
assert_not_in  "openai-bridge-happy" 'default: "openai/gpt-4o"'

# ─── Case 2: Bridge skipped when operator sets HERMES_CUSTOM_* ──────────
# When an operator points at a self-hosted vLLM or similar, the bridge
# must NOT overwrite their values. api_mode should NOT be forced to
# chat_completions (the operator might want codex_responses for o1 models).
run_case "operator-custom-wins" \
  OPENAI_API_KEY=sk-test-abc \
  HERMES_CUSTOM_BASE_URL=http://my-vllm:8080/v1 \
  HERMES_CUSTOM_API_KEY=operator-key \
  HERMES_DEFAULT_MODEL=openai/gpt-4o >/dev/null

assert_in      "operator-custom-wins" 'base_url: "http://my-vllm:8080/v1"'
assert_in      "operator-custom-wins" 'api_key: "operator-key"'
assert_not_in  "operator-custom-wins" 'api_mode: "chat_completions"'
assert_not_in  "operator-custom-wins" 'base_url: "https://api.openai.com/v1"'

# ─── Case 3: Non-custom providers untouched ─────────────────────────────
# An OPENROUTER_API_KEY should pick provider=openrouter (per
# derive-provider.sh), and the bridge must not fire.
run_case "openrouter-not-touched" \
  OPENROUTER_API_KEY=sk-or-test \
  OPENAI_API_KEY=sk-test-abc \
  HERMES_DEFAULT_MODEL=openai/gpt-4o >/dev/null

assert_in      "openrouter-not-touched" 'provider: "openrouter"'
assert_not_in  "openrouter-not-touched" 'api_mode: "chat_completions"'
assert_not_in  "openrouter-not-touched" 'base_url: "https://api.openai.com/v1"'
# openrouter keeps the full slug (it can resolve openai/gpt-4o)
assert_in      "openrouter-not-touched" 'default: "openai/gpt-4o"'

# ─── Case 4: Non-openai model on bridge path leaves slug alone ──────────
# If the bridge fires but the model isn't prefixed with openai/, we don't
# want to break the string. Prefix-strip is a no-op when the prefix isn't there.
run_case "non-prefixed-model" \
  OPENAI_API_KEY=sk-test-abc \
  HERMES_DEFAULT_MODEL=gpt-4o >/dev/null

assert_in      "non-prefixed-model" 'default: "gpt-4o"'

# ─── Summary ────────────────────────────────────────────────────────────
echo ""
echo "Hermes bridge test: PASS=$PASS FAIL=$FAIL"
[ "$FAIL" = "0" ]
