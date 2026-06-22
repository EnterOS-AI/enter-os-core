#!/usr/bin/env bash
# Real-completion + per-provider liveness + byok-routing assertion helpers
# for the staging full-SaaS E2E (tests/e2e/test_staging_full_saas.sh).
#
# WHY THIS LIB EXISTS (molecule-core#1995 / #1994 follow-on):
# The A2A e2e historically asserted only response SHAPE — e.g.
# test_a2a_e2e.sh:`check "SEO response has text" '"kind":"text"'`. A fully
# BROKEN agent returns its error AS a text part:
#     {"kind":"text","text":"Agent error (Exception) — see workspace logs..."}
# which STILL matches `"kind":"text"` → the shape check PASSES on a broken
# agent. That is exactly why the 2026-05-2x drained-key / byok-misroute
# failures (agents-team PM + reno marketing erroring on every LLM call)
# sailed through CI. "Channel returns text shape" != "agent actually
# completed an LLM round-trip".
#
# These helpers add three load-bearing gates ON TOP of (never replacing) the
# existing shape + PONG checks:
#   1. a2a_assert_real_completion  — deterministic known-answer round-trip
#      (CONTAINS the expected token AND NOT an error-as-text payload).
#   2. provider_liveness_matrix    — per-offered-provider cheap completion
#      probe, providers sourced from the providers.yaml SSOT runtimes block.
#   3. assert_byok_not_platform_proxy — #1994 regression guard: a
#      byok-resolving workspace must NOT resolve to platform_managed.
#
# Conventions: reuses the host script's fail()/ok()/log() + tenant_call().
# Source this AFTER those are defined. BASH 4+.

# Error-as-text trap markers. If the agent's text part contains ANY of
# these, the "round-trip" did not really complete — the agent surfaced an
# error AS text. This is the negative assertion that makes a broken agent
# FAIL instead of slipping through the shape check.
#
# Kept as an array (not a single regex) so a new failure signature is a
# one-line append + the failure message can name which marker matched.
A2A_ERROR_AS_TEXT_MARKERS=(
  "Agent error"
  "Exception"
  "error result"
  "MISSING_BYOK_CREDENTIAL"
)

# a2a_completion_error_marker <agent_text>
#   Echoes the first error-as-text marker found in <agent_text> (case-
#   insensitive), or nothing if clean. Exit 0 if a marker matched, 1 if not.
#   Pure string scan — no LLM, no network — so it is deterministic and is the
#   unit under the fail-direction proof in test_completion_assert_unit.sh.
a2a_completion_error_marker() {
  local text="$1"
  local upper marker
  upper=$(printf '%s' "$text" | tr '[:lower:]' '[:upper:]')
  for marker in "${A2A_ERROR_AS_TEXT_MARKERS[@]}"; do
    if printf '%s' "$upper" | grep -qF -- "$(printf '%s' "$marker" | tr '[:lower:]' '[:upper:]')"; then
      printf '%s' "$marker"
      return 0
    fi
  done
  return 1
}

# redact_secrets
#   Reads stdin, writes stdout with credential-looking values replaced by
#   <REDACTED>. Used by diagnostic emitters so run logs stay secret-safe.
#   Covers Authorization/Bearer headers, common key names, generic *_API_KEY /
#   *_TOKEN / *_SECRET values, URL query credential params, and claude-code
#   SDK-style credential keys. Preserves HTTP status codes and non-secret
#   error context.
redact_secrets() {
  python3 "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/redact_secrets.py"
}

# diagnose_staging_result_error <workspace_id> <a2a_response> <context_label>
#   Diagnostic-only helper for molecule-core#2712. When the canary agent
#   returns a _ResultError / error-as-text payload, the RUN OUTPUT must show
#   WHY the LLM/backend/runtime call failed, not just the wrapped error string.
#   Emits (via redact_secrets):
#     - the full A2A response JSON (so upstream HTTP status/body can be read)
#     - the workspace's status, runtime_state, and last_sample_error
#     - recent activity_logs rows (error_detail, status, summary)
#   This does NOT change pass/fail semantics — the caller still fail()s.
diagnose_staging_result_error() {
  local ws_id="$1"
  local a2a_resp="$2"
  local ctx="${3:-A2A}"

  log "── DIAGNOSTIC BURST ($ctx — staging LLM/backend/runtime failure) ──"

  log "Full A2A response (redacted JSON):"
  {
    printf '%s\n' "$a2a_resp" | python3 -m json.tool 2>/dev/null || printf '%s\n' "$a2a_resp"
  } | redact_secrets

  if [ -n "$ws_id" ]; then
    log "Workspace $ws_id snapshot:"
    local ws_json
    ws_json=$(tenant_call GET "/workspaces/$ws_id" 2>/dev/null || echo '{}')
    {
      printf '%s\n' "$ws_json" | python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
    print('  status          :', d.get('status', '?'))
    print('  runtime_state   :', d.get('runtime_state', '?'))
    print('  url             :', d.get('url', '?'))
    print('  last_sample_error:', (d.get('last_sample_error') or '')[:500])
except Exception as e:
    print('  (workspace JSON parse error:', e, ')')
"
    } | redact_secrets 2>/dev/null || true

    log "Recent activity logs for $ws_id:"
    local activity_json
    activity_json=$(tenant_call GET "/activity?workspace_id=$ws_id&limit=20" 2>/dev/null || echo '[]')
    {
      printf '%s\n' "$activity_json" | python3 -c "
import json, sys
try:
    rows = json.load(sys.stdin)
    for r in rows[:10]:
        ts = r.get('created_at', '?')
        typ = r.get('activity_type', '?')
        st = r.get('status', '?')
        summ = (r.get('summary') or '')[:120]
        print(f'  - {ts} {typ} status={st} {summ}')
        ed = r.get('error_detail')
        if ed:
            print('    error_detail:', str(ed)[:300])
except Exception as e:
    print('  (activity JSON parse error:', e, ')')
"
    } | redact_secrets 2>/dev/null || true
  fi

  log "── END DIAGNOSTIC ──"
}

# a2a_assert_real_completion <agent_text> <expected_token> <context_label>
#   The CORE gate. Asserts the agent text:
#     (a) does NOT contain any error-as-text marker (broken-agent trap), AND
#     (b) CONTAINS <expected_token> (case-insensitive) — proving a real LLM
#         round-trip produced the deterministic known answer.
#   Calls fail() (which exits) on either violation. This MUST fail on an
#   error-as-text payload — that is the property test_completion_assert_unit.sh
#   pins.
a2a_assert_real_completion() {
  local text="$1"
  local expected="$2"
  local ctx="${3:-A2A}"

  if [ -z "$text" ]; then
    fail "$ctx — real-completion gate: agent returned EMPTY text (no round-trip)."
  fi

  local hit
  if hit=$(a2a_completion_error_marker "$text"); then
    fail "$ctx — real-completion gate: agent returned an ERROR-AS-TEXT payload (matched '$hit'). A broken agent that surfaces its error as a text part is NOT a completed round-trip. This is the trap the shape-only check missed (#1994). Raw: ${text:0:200}"
  fi

  # Known-answer: real LLM round-trip yields the deterministic token. A
  # prompt-echo / truncated-context / wrong-auth pipeline won't.
  if ! printf '%s' "$text" | tr '[:lower:]' '[:upper:]' | grep -qF -- "$(printf '%s' "$expected" | tr '[:lower:]' '[:upper:]')"; then
    fail "$ctx — real-completion gate: reply did NOT contain expected known-answer token '$expected'. The channel returned a text shape but no real completion. Raw: ${text:0:200}"
  fi

  ok "$ctx — real completion verified (contains '$expected', no error-as-text). Reply: \"${text:0:80}\""
}

# offered_platform_models_for_runtime <runtime>
#   Emits, one per line, the platform-servable model ids the providers.yaml
#   SSOT (runtimes.<runtime>.providers[name=platform].models) declares for
#   <runtime>. This is the SSOT-driven offered/platform-servable matrix — NOT
#   a hardcoded provider list — so a provider added/removed in providers.yaml
#   automatically changes the matrix this probe exercises.
#
#   Reads the embedded copy at workspace-server/internal/providers/providers.yaml
#   (the same file go:embed compiles into the binary). Requires python3 +
#   PyYAML (already a test-harness dep). On parse failure, emits nothing and
#   returns 1 so the caller can fail loud rather than silently skip.
offered_platform_models_for_runtime() {
  local runtime="$1"
  local yaml_path="${PROVIDERS_YAML_PATH:-}"
  if [ -z "$yaml_path" ]; then
    # This lib lives at tests/e2e/lib/ -> repo root is three dirs up
    # (lib -> e2e -> tests -> repo-root).
    yaml_path="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)/workspace-server/internal/providers/providers.yaml"
  fi
  if [ ! -f "$yaml_path" ]; then
    log "    [provider-matrix] providers.yaml SSOT not found at $yaml_path"
    return 1
  fi
  RUNTIME_REF="$runtime" python3 - "$yaml_path" <<'PY'
import os, sys
try:
    import yaml
except Exception as e:  # PyYAML missing — fail loud, do not silently skip.
    sys.stderr.write(f"PyYAML required for provider-matrix SSOT read: {e}\n")
    sys.exit(2)
rt = os.environ["RUNTIME_REF"]
with open(sys.argv[1]) as f:
    doc = yaml.safe_load(f)
native = (doc.get("runtimes") or {}).get(rt) or {}
for pref in native.get("providers", []) or []:
    if pref.get("name") == "platform":
        for m in pref.get("models", []) or []:
            print(m)
PY
}

# provider_liveness_matrix <runtime> <probe_fn>
#   For each platform-servable model the SSOT lists for <runtime>, calls
#   <probe_fn> <model_id> which must echo the agent text (or empty) and return
#   0 on a non-error completion, non-zero otherwise. Logs a per-model pass/fail
#   matrix. Returns 0 only if EVERY probed model produced a non-error
#   completion; non-zero (and a recorded matrix) otherwise.
#
#   Purpose: exercise each offered provider's AUTH + ROUTING path so a drained
#   key / wrong base-URL / byok-misroute fails the gate (the #1994 class). The
#   probe_fn is expected to use minimal max_tokens.
#
#   This helper does the SSOT read + matrix bookkeeping; the host script
#   supplies probe_fn (it owns workspace ids + tenant_call wiring).
provider_liveness_matrix() {
  local runtime="$1"
  local probe_fn="$2"
  local models model rc total=0 passed=0
  local -a results=()

  models=$(offered_platform_models_for_runtime "$runtime") || {
    fail "provider-liveness: could not read offered-provider matrix from providers.yaml SSOT for runtime=$runtime"
  }
  if [ -z "$models" ]; then
    log "    [provider-matrix] runtime=$runtime offers no platform-servable models in the SSOT — nothing to probe (not a failure)."
    return 0
  fi

  log "    [provider-matrix] SSOT offered platform models for $runtime:"
  while IFS= read -r model; do
    [ -z "$model" ] && continue
    log "      - $model"
  done <<<"$models"

  while IFS= read -r model; do
    [ -z "$model" ] && continue
    total=$((total + 1))
    set +e
    "$probe_fn" "$model"
    rc=$?
    set -e
    if [ "$rc" = "0" ]; then
      passed=$((passed + 1))
      results+=("PASS  $model")
    elif [ "$rc" = "75" ]; then
      # 75 (EX_TEMPFAIL convention) = probe skipped (key/runtime not
      # available in this lane). Not counted toward pass/fail — logged.
      total=$((total - 1))
      results+=("SKIP  $model (probe unavailable in this lane)")
    else
      results+=("FAIL  $model")
    fi
  done <<<"$models"

  log "    [provider-matrix] result matrix (runtime=$runtime):"
  local line
  for line in "${results[@]}"; do
    log "      $line"
  done
  log "    [provider-matrix] $passed/$total probed providers completed without error"

  if [ "$passed" != "$total" ]; then
    return 1
  fi
  return 0
}

# assert_byok_not_platform_proxy <billing_mode_json> <context_label>
#   #1994 regression guard. Given the JSON body from
#   GET /admin/workspaces/:id/llm-billing-mode (same derived resolver the
#   provision-time strip gate uses), asserts the workspace resolves to BYOK
#   and NOT platform_managed. A regression of #1994 (byok workspace baked to
#   platform_managed → routed through the platform proxy → platform LLM key
#   drained) flips resolved_mode to "platform_managed" and trips this gate.
#   Calls fail() (exits) on violation.
assert_byok_not_platform_proxy() {
  local body="$1"
  local ctx="${2:-byok-guard}"
  local mode prov
  mode=$(printf '%s' "$body" | python3 -c "import json,sys
try: print(json.load(sys.stdin).get('resolved_mode',''))
except Exception: print('')" 2>/dev/null || echo "")
  prov=$(printf '%s' "$body" | python3 -c "import json,sys
try:
    d=json.load(sys.stdin); v=d.get('provider_selection')
    print(v if v is not None else '')
except Exception: print('')" 2>/dev/null || echo "")

  if [ -z "$mode" ]; then
    fail "$ctx — byok-routing guard: could not read resolved_mode from billing-mode response. Raw: ${body:0:200}"
  fi
  if [ "$mode" = "platform_managed" ]; then
    fail "$ctx — byok-routing guard TRIPPED (#1994 regression): a byok-configured workspace resolved to 'platform_managed' (provider_selection=$prov) → it would route through the platform proxy and drain the platform LLM key. Expected resolved_mode=byok. Raw: ${body:0:200}"
  fi
  if [ "$mode" != "byok" ]; then
    fail "$ctx — byok-routing guard: unexpected resolved_mode='$mode' (expected 'byok'). provider_selection=$prov. Raw: ${body:0:200}"
  fi
  ok "$ctx — byok-routing guard: workspace resolves byok (provider_selection=$prov), NOT platform-proxy. #1994 stays fixed."
}
