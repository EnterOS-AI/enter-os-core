#!/usr/bin/env bash
# Per-runtime model slug dispatch for E2E provisioning.
#
# Different runtimes parse the model slug differently (PR #2571 incident,
# 2026-05-03):
#
#   hermes      → "openai/gpt-4o"  (slash-form: derive-provider.sh splits
#                                    on the prefix to set
#                                    HERMES_INFERENCE_PROVIDER. Bare
#                                    "gpt-4o" falls through to Anthropic
#                                    default + 401, see PR #1714.)
#
#   langgraph   → "openai:gpt-4o"  (colon-form: langchain init_chat_model
#                                    requires "<provider>:<model>".
#                                    Slash-form was misinterpreted as
#                                    OpenRouter routing → fell through
#                                    without auth, surfaced 2026-05-03
#                                    after the a2a-sdk v1 contract bugs
#                                    PR #2558+#2563+#2567 cleared the
#                                    masking layers.)
#
#   claude-code → "sonnet"         (entry-id form: claude-code template's
#                                    config.yaml uses bare model names,
#                                    auth comes via CLAUDE_CODE_OAUTH_TOKEN
#                                    or ANTHROPIC_API_KEY rather than the
#                                    slug.)
#
# When E2E_MODEL_SLUG is set, it overrides this dispatch — useful when an
# operator dispatches the workflow to test a specific slug.
#
# Unit tested by tests/e2e/test_model_slug.sh — every branch must stay
# pinned because regressions silently mask as "Could not resolve
# authentication method" + the synth-E2E gate goes red without naming
# the slug-format mismatch.

# Usage: pick_model_slug <runtime>
#   stdout: the slug string
#   E2E_MODEL_SLUG (env): if set + non-empty, used as-is (operator override)
pick_model_slug() {
  local runtime="${1:-}"
  if [ -n "${E2E_MODEL_SLUG:-}" ]; then
    printf '%s' "$E2E_MODEL_SLUG"
    return 0
  fi
  case "$runtime" in
    hermes)      printf 'openai/gpt-4o' ;;
    langgraph)   printf 'openai:gpt-4o' ;;
    claude-code) printf 'sonnet' ;;
    *)           printf 'openai/gpt-4o' ;;  # safest fallback (matches hermes)
  esac
}
