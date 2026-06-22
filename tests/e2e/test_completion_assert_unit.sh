#!/usr/bin/env bash
# Fail-direction / load-bearing proof for lib/completion_assert.sh.
#
# This is the watch-it-FAIL counterpart the dev-SOP Phase 3 requires: it
# proves the new real-completion + byok gates actually CATCH a broken agent,
# not just pass on a good one. It runs entirely offline (no LLM, no network,
# no provisioning) — pure assertion logic — so it can run on every PR in the
# fast lane (e2e-api.yml unit-shell step) and locally via `bash`.
#
# The decisive case is `error-as-text payload MUST FAIL`: that is the exact
# trap (#1994) the historical shape-only check missed. If a refactor weakens
# a2a_assert_real_completion to a substring/shape check, THIS test goes red.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PASS=0
FAIL=0

# Minimal stand-ins for the host script's helpers. fail() must NOT exit the
# whole harness here — we want to assert that it WAS called. We trap it by
# running the assertion in a subshell and checking the subshell's exit code:
# the real fail() exits 1, ok() exits 0 implicitly.
log()  { echo "[unit] $*"; }
ok()   { echo "[unit] OK: $*"; }
fail() { echo "[unit] FAIL-CALLED: $*" >&2; exit 1; }

# shellcheck source=lib/completion_assert.sh
source "$HERE/lib/completion_assert.sh"

expect_pass() {
  local desc="$1"; shift
  if ( "$@" ) >/dev/null 2>&1; then
    echo "PASS: $desc (assertion accepted, as expected)"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $desc — expected the assertion to ACCEPT, but it rejected"
    FAIL=$((FAIL + 1))
  fi
}

expect_fail() {
  local desc="$1"; shift
  if ( "$@" ) >/dev/null 2>&1; then
    echo "FAIL: $desc — expected the assertion to REJECT, but it accepted (gate NOT load-bearing!)"
    FAIL=$((FAIL + 1))
  else
    echo "PASS: $desc (assertion rejected, as expected)"
    PASS=$((PASS + 1))
  fi
}

echo "=== completion_assert.sh fail-direction proof ==="

# ---- a2a_assert_real_completion ----
# Good: real known-answer reply passes.
expect_pass "real PINEAPPLE reply passes" \
  a2a_assert_real_completion "PINEAPPLE" "PINEAPPLE" "unit"
expect_pass "case-insensitive known answer passes" \
  a2a_assert_real_completion "pineapple" "PINEAPPLE" "unit"
expect_pass "known answer with minor wrapping passes" \
  a2a_assert_real_completion "Sure: PINEAPPLE" "PINEAPPLE" "unit"

# DECISIVE: the error-as-text trap. Each MUST fail — these are the payloads a
# broken agent returns that the old shape-only `"kind":"text"` check passed.
expect_fail "Agent error as text payload MUST fail" \
  a2a_assert_real_completion "Agent error (Exception) — see workspace logs for details." "PINEAPPLE" "unit"
expect_fail "bare Exception as text MUST fail" \
  a2a_assert_real_completion "Traceback ... Exception: boom" "PINEAPPLE" "unit"
expect_fail "error result as text MUST fail" \
  a2a_assert_real_completion "tool returned error result" "PINEAPPLE" "unit"
expect_fail "MISSING_BYOK_CREDENTIAL as text MUST fail" \
  a2a_assert_real_completion "MISSING_BYOK_CREDENTIAL: set your own key" "PINEAPPLE" "unit"
# Error-as-text that ALSO happens to contain the token still fails (error
# marker takes precedence — a real completion never carries these markers).
expect_fail "error-as-text containing the token still fails" \
  a2a_assert_real_completion "Agent error: could not produce PINEAPPLE" "PINEAPPLE" "unit"
# Empty text fails.
expect_fail "empty text fails" \
  a2a_assert_real_completion "" "PINEAPPLE" "unit"
# Wrong/echoed content (no token, no error) fails — shape-OK but not a real
# completion.
expect_fail "wrong content without token fails" \
  a2a_assert_real_completion "Reply with exactly the word PINEAPPLE and nothing else." "BANANA" "unit"

# ---- assert_byok_not_platform_proxy (#1994 guard) ----
expect_pass "byok resolution passes the guard" \
  assert_byok_not_platform_proxy '{"resolved_mode":"byok","provider_selection":"minimax","source":"derived_provider"}' "unit"
# DECISIVE: a platform_managed resolution on a byok workspace = the #1994
# regression. MUST fail.
expect_fail "platform_managed resolution trips the #1994 guard" \
  assert_byok_not_platform_proxy '{"resolved_mode":"platform_managed","provider_selection":"platform","source":"derived_provider"}' "unit"
expect_fail "missing resolved_mode trips the guard" \
  assert_byok_not_platform_proxy '{"provider_selection":"x"}' "unit"
expect_fail "disabled mode trips the guard (not byok)" \
  assert_byok_not_platform_proxy '{"resolved_mode":"disabled"}' "unit"

# ---- a2a_completion_error_marker (the scanner under the gate) ----
if hit=$(a2a_completion_error_marker "all good PINEAPPLE"); then
  echo "FAIL: clean text wrongly flagged as error marker ($hit)"; FAIL=$((FAIL + 1))
else
  echo "PASS: clean text has no error marker"; PASS=$((PASS + 1))
fi
if hit=$(a2a_completion_error_marker "An Exception occurred"); then
  echo "PASS: error marker detected ($hit)"; PASS=$((PASS + 1))
else
  echo "FAIL: error marker NOT detected in 'An Exception occurred'"; FAIL=$((FAIL + 1))
fi

# ---- redact_secrets (diagnostic-output safety) ----
redact_check() {
  local desc="$1"
  local input="$2"
  local must_not_contain="$3"
  local output
  output=$(printf '%s' "$input" | redact_secrets)
  if printf '%s' "$output" | grep -qF "$must_not_contain"; then
    echo "FAIL: $desc — secret/token leaked in redacted output"
    FAIL=$((FAIL + 1))
  else
    echo "PASS: $desc (secret redacted)"
    PASS=$((PASS + 1))
  fi
}

redact_check "Authorization header value redacted" \
  "Authorization: Bearer sk-ant-abc123XYZ" \
  "sk-ant-abc123XYZ"
redact_check "known API key redacted" \
  '{"ANTHROPIC_API_KEY":"sk-ant-abc123","status":"ok"}' \
  "sk-ant-abc123"
redact_check "generic *_TOKEN redacted" \
  'MINIMAX_API_KEY=mini-max-secret-token' \
  "mini-max-secret-token"
redact_check "URL query token redacted" \
  "https://api.example.com/v1?token=supersecrettoken&status=400" \
  "supersecrettoken"
# _ResultError diagnostic path: the runtime surfaces upstream errors as text,
# and that text can embed Authorization headers or API keys. Redaction must
# scrub them without removing the useful failure classification/status.
redact_check "_ResultError payload with embedded token redacted" \
  'Agent error (_ResultError): HTTP 401 {\"error\":\"invalid auth\", \"Authorization\":\"Bearer sk-ant-leaked\"}' \
  "sk-ant-leaked"
if printf '%s' 'Agent error (_ResultError): HTTP 401 {"error":"invalid auth"}' | redact_secrets | grep -qF 'HTTP 401'; then
  echo "PASS: _ResultError redaction preserves HTTP status context"
  PASS=$((PASS + 1))
else
  echo "FAIL: _ResultError redaction stripped useful HTTP status context"
  FAIL=$((FAIL + 1))
fi
# Positive: non-secret context (HTTP status, error message) must survive.
if printf '%s' '{"status":401,"error":"invalid key"}' | redact_secrets | grep -qF '"status":401'; then
  echo "PASS: redaction preserves non-secret HTTP status context"
  PASS=$((PASS + 1))
else
  echo "FAIL: redaction stripped useful non-secret context"
  FAIL=$((FAIL + 1))
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
