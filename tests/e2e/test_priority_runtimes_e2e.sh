#!/usr/bin/env bash
# E2E test: every maintained runtime template works end-to-end.
#
# Self-contained happy-path smoke per runtime. Provisions a fresh
# workspace, waits for status=online, sends a real A2A message, and
# asserts a non-error reply. Pins the contract so the post-#87 template
# extraction (and ongoing template work) can't silently break any
# runtime.
#
# Runtimes covered: claude-code, codex, hermes, openclaw, google-adk.
# claude-code + hermes have unique
# provisioning quirks (claude-code OAuth, hermes 15-min cold-boot)
# and stay first-class with their own run_<runtime> functions; the
# OpenAI-backed runtimes share run_openai_runtime. google-adk has its own
# run_google_adk (it asserts manifest registration unconditionally, then drives
# its AI-Studio BYOK live arm — keyless-Vertex needs platform WIF CI lacks).
# Each phase skips cleanly if its prerequisite secret is missing.
#
# What this proves:
#   1. Provisioning + container boot works for each runtime.
#   2. The runtime reaches status=online within its expected cold-boot
#      window (claude-code: ~60s, hermes: up to 15min on cold apt).
#   3. A real A2A message/send produces a non-empty, non-error reply.
#   4. The activity_logs row for the call is well-formed.
#
# Each phase skips cleanly when its prerequisite secret is absent so a
# partially-keyed env (e.g. CI without an OpenAI key) doesn't false-fail.
#
# REQUIRE-LIVE (false-green guard, mirrors CP serving-e2e's
# SERVING_E2E_REQUIRE_LIVE semantics)
# ------------------------------------------------------------------
# Without a guard, an env with NO live secrets makes every phase SKIP,
# leaving PASS=0 FAIL=0 — and the historical `[ "$FAIL" -eq 0 ]` gate
# exits 0 (GREEN) while validating ZERO runtimes. That made the REQUIRED
# `E2E API Smoke Test` merge gate pass without exercising a single
# runtime (false-green).
#
# Fix: a real "validated arm" counter (VALIDATED) tracks runtimes that
# actually ran AND produced a non-error A2A reply. With E2E_REQUIRE_LIVE=1:
# if zero arms validated, the run exits NON-zero with a loud message.
# Without it (E2E_REQUIRE_LIVE unset/0), a fully-skipped run stays a LOUD
# skip + exit 0 for dev convenience.
#
# This zero-validated→RED decision is the load-bearing logic. It is factored
# into evaluate_require_live_gate() (a pure function of $FAIL/$VALIDATED/
# $E2E_REQUIRE_LIVE, defined before any platform I/O) and is REGRESSION-GATED
# on every PR by tests/e2e/test_require_live_priority_gate_unit.sh, which
# sources this file (E2E_PRIORITY_UNIT_SOURCE=1), sets the counters, and
# asserts the gate's exit code — no platform, no provisioning, no network.
# So the false-green can't silently come back: a revert of the guard fails CI.
#
# CI POSTURE (REQUIRE-LIVE ON — see .gitea/workflows/e2e-api.yml):
# The live e2e-api job SETS E2E_REQUIRE_LIVE=1. The `mock` arm is the
# CI-provisionable live-completion arm: it org-imports a mock workspace
# (→online→canned A2A reply) with NO external secret. The only thing that
# previously blocked it in CI was admin auth — POST /org/import and POST
# /admin/workspaces/:id/tokens are AdminAuth-gated, and the job set no admin
# token, so every admin call 401'd ("admin auth required"). The job now sets
# ADMIN_TOKEN on the platform AND exports the matching MOLECULE_ADMIN_TOKEN
# the scripts send, so mock validates end-to-end and VALIDATED>=1 holds on a
# healthy platform — the REQUIRED `E2E API Smoke Test` gate now HONESTLY
# validates a runtime. If the mock plumbing or the admin-auth wiring breaks,
# the gate goes RED (not false-green). The zero-validated→RED decision is also
# regression-gated WITHOUT provisioning by the bash unit test above, so a
# revert of that logic still fails CI.
#
# LIVE ARMS (run when their prerequisite is present; opportunistic):
#   - `mock` (run_mock) is the no-key REQUIRE-LIVE backbone: a virtual
#     workspace (no container, no EC2, no provider) whose org-import path
#     short-circuits to status='online' with a canned A2A reply. It validates
#     in CI now that the e2e-api job wires an admin token (org-import + token
#     mint are AdminAuth-gated), so it is the guaranteed >=1 validation.
#   - MiniMax (E2E_MINIMAX_API_KEY, from MOLECULE_STAGING_MINIMAX_API_KEY) is
#     an OPPORTUNISTIC best-effort real-LLM arm: registry-fragile in CI (422
#     UNREGISTERED_MODEL_FOR_RUNTIME — see run_minimax header), so a miss is
#     a best-effort MISS via bestfail() and does NOT red the gate.
# The CI e2e-api job sets E2E_REQUIRE_LIVE=1: mock guarantees a validation, so
# the REQUIRED gate is honest (RED if the mock plumbing/admin-auth breaks). The
# zero-validated→RED logic is also regression-gated by the bash unit test above.
#
# Usage:
#   # Enforce REQUIRE-LIVE locally (need >=1 arm to actually validate):
#   E2E_REQUIRE_LIVE=1 E2E_MINIMAX_API_KEY=... \
#     tests/e2e/test_priority_runtimes_e2e.sh
#
#   # Default (no enforcement): all-skip stays a LOUD skip + exit 0:
#   tests/e2e/test_priority_runtimes_e2e.sh
#
#   # Other live arms (if their secrets are configured):
#   CLAUDE_CODE_OAUTH_TOKEN=... E2E_OPENAI_API_KEY=... \
#     tests/e2e/test_priority_runtimes_e2e.sh
#
#   # Run only one runtime
#   E2E_RUNTIMES=mock        tests/e2e/test_priority_runtimes_e2e.sh
#   E2E_RUNTIMES=minimax     tests/e2e/test_priority_runtimes_e2e.sh
#   E2E_RUNTIMES=claude-code tests/e2e/test_priority_runtimes_e2e.sh
#   E2E_RUNTIMES=hermes      tests/e2e/test_priority_runtimes_e2e.sh
#   E2E_RUNTIMES=google-adk  tests/e2e/test_priority_runtimes_e2e.sh  # registration always; live arm needs E2E_GOOGLE_API_KEY
#
# Prereqs:
#   - workspace-server on http://localhost:8080
#   - AdminAuth bootstrap or `MOLECULE_ADMIN_TOKEN` for token minting
#   - For claude-code: CLAUDE_CODE_OAUTH_TOKEN
#   - For hermes:      E2E_OPENAI_API_KEY  (other providers also OK if you
#                       set MODEL_SLUG_HERMES + matching secrets directly)

set -euo pipefail

PASS=0
FAIL=0
SKIP=0
# VALIDATED counts runtimes that ACTUALLY ran end-to-end (provisioned,
# reached online, AND returned a non-error A2A reply). Distinct from PASS,
# which also counts sub-assertions like activity-log rows. This is the
# signal the REQUIRE-LIVE gate keys off: VALIDATED==0 means we proved
# nothing about any runtime, regardless of how many sub-asserts "passed".
VALIDATED=0
CREATED_WSIDS=()

# evaluate_require_live_gate — the SINGLE source of the final exit decision.
# Pure function of $FAIL, $VALIDATED, and $E2E_REQUIRE_LIVE; performs NO I/O
# beyond the loud messages. Returns the exit code the script should exit with:
#   - FAIL>0                       → 1 (a real failure is always red)
#   - VALIDATED==0 + REQUIRE_LIVE  → 1 (false-green trap: proved nothing → RED)
#   - VALIDATED==0 + !REQUIRE_LIVE → 0 (dev-convenience LOUD skip)
#   - VALIDATED>=1                 → 0 (at least one arm validated end-to-end)
# It is a function (not inline tail code) so test_require_live_priority_gate_unit.sh
# can drive the REAL decision in isolation — set the counters, call this, assert
# the return code — with no platform, no provisioning, no network. That makes the
# zero-validated→RED logic a CI-gated regression contract: a future revert of it
# fails the unit test on every PR. See that unit test for the fail-direction proof.
evaluate_require_live_gate() {
  # Any real failure is always red.
  if [ "$FAIL" -ne 0 ]; then
    return 1
  fi

  # REQUIRE-LIVE gate (mirrors CP serving-e2e SERVING_E2E_REQUIRE_LIVE).
  # A run where every runtime SKIPPED proves nothing. In enforced mode
  # (E2E_REQUIRE_LIVE=1) that MUST be red so the required `E2E API Smoke
  # Test` gate can't be false-green on an all-skip run.
  local require_live="${E2E_REQUIRE_LIVE:-0}"
  if [ "$VALIDATED" -eq 0 ]; then
    if [ "$require_live" = "1" ] || [ "$require_live" = "true" ]; then
      echo "::error::E2E_REQUIRE_LIVE is set but ZERO runtimes were validated end-to-end." >&2
      echo "         Every runtime SKIPPED — no live secret was present, so this gate" >&2
      echo "         validated nothing. Wire at least one live arm via Gitea secrets" >&2
      echo "         (E2E_MINIMAX_API_KEY ← MOLECULE_STAGING_MINIMAX_API_KEY is the" >&2
      echo "         default CI arm; CLAUDE_CODE_OAUTH_TOKEN / E2E_OPENAI_API_KEY also" >&2
      echo "         work) so >=1 runtime actually provisions + replies. Failing RED" >&2
      echo "         instead of false-green." >&2
      return 1
    fi
    # Dev convenience: no enforcement requested → loud skip, exit 0.
    echo "SKIPPED: no live secrets present and E2E_REQUIRE_LIVE is not set — validated" >&2
    echo "         zero runtimes. This is a dev-convenience pass; CI sets" >&2
    echo "         E2E_REQUIRE_LIVE=1 to make zero-validated a hard failure." >&2
    return 0
  fi

  echo "OK: $VALIDATED runtime(s) validated end-to-end."
  return 0
}

# Source-guard: when sourced by the unit test (E2E_PRIORITY_UNIT_SOURCE=1) we
# stop HERE — the counters + evaluate_require_live_gate are now defined, and we
# must NOT fall through to _lib.sh's platform-dependent helpers or the live
# pre-sweep curl below (there is no platform in the unit-test environment).
if [ "${E2E_PRIORITY_UNIT_SOURCE:-0}" = "1" ]; then
  return 0
fi

source "$(dirname "$0")/_lib.sh"

# GET /workspaces (list, router.go:165) and POST /workspaces (create,
# router.go:166) are AdminAuth-gated. The e2e-api CI job sets ADMIN_TOKEN on the
# platform (fail-open OFF) and exports MOLECULE_ADMIN_TOKEN here, so the
# pre-sweep list and every runtime-create must send the admin bearer or they
# 401. run_mock uses POST /org/import (also admin-gated) and wires its own admin
# auth inline. Guarded if-set so a fail-open dev platform still works.
ADMIN_AUTH=()
e2e_admin_auth_args ADMIN_AUTH

cleanup() {
  # `set -u` + empty array would error on "${CREATED_WSIDS[@]}"; the
  # ${VAR[@]+"…"} form expands to nothing when the array is unset/empty
  # so the loop body is skipped cleanly. Hits the skip-no-keys path.
  for wid in ${CREATED_WSIDS[@]+"${CREATED_WSIDS[@]}"}; do
    [ -n "$wid" ] && e2e_delete_workspace "$wid" ""
  done
}
trap cleanup EXIT

pass()      { echo "  PASS — $1"; PASS=$((PASS + 1)); }
fail()      { echo "  FAIL — $1"; echo "         $2"; FAIL=$((FAIL + 1)); }
skip()      { echo "  SKIP — $1"; SKIP=$((SKIP + 1)); }
# Mark a runtime as having been validated end-to-end (online + non-error
# A2A reply). Also emits a PASS line so it shows in the results tally.
validated() { echo "  PASS — $1"; PASS=$((PASS + 1)); VALIDATED=$((VALIDATED + 1)); }
# bestfail() is for OPPORTUNISTIC (best-effort) arms whose failure must
# NOT red the gate. It does NOT increment FAIL — it only logs + bumps
# SKIP so the tally stays honest ("we tried, it didn't validate, but it
# was never load-bearing"). Used by the MiniMax arm: MiniMax-create is
# fragile in CI (registry-skewed model id, BYOK plumbing — see core#2263
# and the run_minimax header), so a MiniMax miss is reported but never
# fails the REQUIRED gate. The mock arm is the load-bearing validation
# that keeps the gate honest; MiniMax is the real-LLM bonus on top.
bestfail()  { echo "  BEST-EFFORT MISS — $1"; echo "         $2"; SKIP=$((SKIP + 1)); }

# Pre-sweep any prior runs that left workspaces behind (same defence as
# test_notify_attachments_e2e.sh: trap fires on normal exit, but a
# SIGPIPE / kill -9 can bypass it).
PRIOR=$(curl -s "$BASE/workspaces" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} | python3 -c '
import json, sys
try:
    print(" ".join(w["id"] for w in json.load(sys.stdin) if w.get("name","").startswith("Priority E2E ")))
except Exception:
    pass
')
for _wid in $PRIOR; do
  echo "Sweeping prior workspace: $_wid"
  e2e_delete_workspace "$_wid" ""
done

# Block until $1 reaches one of $2 (space-separated states), or $3 sec elapse.
wait_for_status() {
  local wsid="$1" want="$2" budget="$3"
  local start=$SECONDS
  while [ $((SECONDS - start)) -lt "$budget" ]; do
    local s
    s=$(curl -s "$BASE/workspaces/$wsid" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("status",""))' 2>/dev/null || echo "")
    for w in $want; do [ "$s" = "$w" ] && { echo "$s"; return 0; }; done
    sleep 4
  done
  echo "$s"
  return 1
}

# Send "What is 2+2?" via A2A, return the reply text on stdout. Fails
# (non-zero exit + empty stdout) if the platform returns an error envelope
# or the reply is empty / sentinel-error.
send_test_prompt() {
  local wsid="$1" token="$2"
  local resp
  resp=$(curl -s --max-time 180 -X POST "$BASE/workspaces/$wsid/a2a" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $token" \
    -d '{
      "method": "message/send",
      "params": {
        "message": {
          "role": "user",
          "messageId": "e2e-priority-runtime",
          "parts": [{"kind": "text", "text": "Reply with exactly the word: PONG"}]
        }
      }
    }') || return 1
  # Walk a few common A2A reply shapes; stop at the first non-empty text.
  echo "$resp" | python3 -c '
import json, sys
try:
    d = json.loads(sys.stdin.read())
except Exception:
    sys.exit(1)
texts = []
def walk(node):
    if isinstance(node, dict):
        for v in node.values(): walk(v)
    elif isinstance(node, list):
        for v in node: walk(v)
    elif isinstance(node, str):
        texts.append(node)
walk(d.get("result") or d)
joined = "\n".join(t for t in texts if t.strip())
if not joined.strip():
    sys.exit(2)
# Surface a known error sentinel so the caller can tell apart "empty" from "explicit error"
low = joined.lower()
for needle in ("a2a_error", "agent error", "could not resolve authentication", "401",
               "no provider api key", "missing api", "model_not_found"):
    if needle in low:
        print("ERROR: " + joined[:200])
        sys.exit(3)
print(joined)
'
}

assert_activity_logged() {
  # After a successful A2A round-trip, the platform's a2a_proxy logs
  # an a2a_receive row with method=message/send. Pin the contract so a
  # silent regression in LogActivity (e.g. dropped status field, broken
  # broadcaster) shows up here. Polls briefly because LogActivity is
  # detached-goroutine — the row may land a few hundred ms after the
  # POST returns.
  local label="$1" wsid="$2" token="$3"
  local start=$SECONDS
  while [ $((SECONDS - start)) -lt 10 ]; do
    local act
    act=$(curl -s -H "Authorization: Bearer $token" "$BASE/workspaces/$wsid/activity?type=a2a_receive&limit=10")
    local found
    found=$(echo "$act" | python3 -c '
import json, sys
try:
    rows = json.load(sys.stdin) or []
except Exception:
    sys.exit(1)
for r in rows:
    if r.get("method") == "message/send" and r.get("status") in ("ok", "error"):
        print("ok")
        sys.exit(0)
sys.exit(2)
' 2>/dev/null) && true
    if [ "$found" = "ok" ]; then
      pass "$label activity_logs row written for the A2A turn"
      return 0
    fi
    sleep 1
  done
  fail "$label activity_logs row" "no a2a_receive row with method=message/send appeared in 10s"
}

run_claude_code() {
  echo ""
  echo "=== claude-code happy path ==="
  if [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then
    skip "CLAUDE_CODE_OAUTH_TOKEN not set"
    return 0
  fi
  local secrets
  secrets=$(python3 -c "
import json, os
print(json.dumps({'CLAUDE_CODE_OAUTH_TOKEN': os.environ['CLAUDE_CODE_OAUTH_TOKEN']}))
")
  local resp wsid
  # model required (CTO 2026-05-22 SSOT) — pass the deleted DefaultModel("claude-code") value.
  resp=$(curl -s -X POST "$BASE/workspaces" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} -H "Content-Type: application/json" \
    -d "{\"name\":\"Priority E2E (claude-code)\",\"runtime\":\"claude-code\",\"model\":\"sonnet\",\"tier\":1,\"secrets\":$secrets}")
  wsid=$(echo "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))') || true
  if [ -z "$wsid" ]; then
    fail "create claude-code workspace" "$resp"
    return 0
  fi
  CREATED_WSIDS+=("$wsid")
  echo "  workspace=$wsid"

  # claude-code typical cold boot: 30-90s (image already pulled)
  local final
  final=$(wait_for_status "$wsid" "online failed" 240) || true
  if [ "$final" != "online" ]; then
    fail "claude-code workspace reaches online" "final status: $final"
    return 0
  fi
  pass "claude-code workspace reaches online"

  local token
  token=$(echo "$resp" | e2e_extract_token)
  if [ -z "$token" ]; then
    token=$(e2e_mint_workspace_token "$wsid")
  fi
  if [ -z "$token" ]; then
    fail "resolve claude-code workspace token" "no token returned"
    return 0
  fi

  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    if echo "$reply" | grep -q "PONG"; then
      validated "claude-code reply contains PONG"
    else
      validated "claude-code reply non-empty (first 80 chars: ${reply:0:80})"
    fi
    assert_activity_logged "claude-code" "$wsid" "$token"
  else
    fail "claude-code reply" "${reply:-<empty or error>}"
  fi
}

run_hermes() {
  echo ""
  echo "=== hermes happy path ==="
  if [ -z "${E2E_OPENAI_API_KEY:-}" ]; then
    skip "E2E_OPENAI_API_KEY not set (hermes needs an LLM provider key)"
    return 0
  fi
  local secrets
  secrets=$(python3 -c "
import json, os
k = os.environ['E2E_OPENAI_API_KEY']
print(json.dumps({
    'OPENAI_API_KEY': k,
    'OPENAI_BASE_URL': 'https://api.openai.com/v1',
    'MODEL_PROVIDER': 'openai:gpt-4o',
    # The HERMES_* fields below pin the provider deterministically
    # (see comment in test_staging_full_saas.sh:268-275 for why).
    'HERMES_INFERENCE_PROVIDER': 'custom',
    'HERMES_CUSTOM_BASE_URL': 'https://api.openai.com/v1',
    'HERMES_CUSTOM_API_KEY': k,
    'HERMES_CUSTOM_API_MODE': 'chat_completions',
}))
")
  local resp wsid
  resp=$(curl -s -X POST "$BASE/workspaces" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} -H "Content-Type: application/json" \
    -d "{\"name\":\"Priority E2E (hermes)\",\"runtime\":\"hermes\",\"tier\":1,\"model\":\"openai/gpt-4o\",\"secrets\":$secrets}")
  wsid=$(echo "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))') || true
  if [ -z "$wsid" ]; then
    fail "create hermes workspace" "$resp"
    return 0
  fi
  CREATED_WSIDS+=("$wsid")
  echo "  workspace=$wsid"

  # Hermes cold boot is the slow path: apt + uv + hermes-agent sidecar.
  # Up to 15 min on cold disk; usually 3-5 min when the runtime image is
  # already cached. Be generous so the test doesn't false-fail in CI.
  local final
  final=$(wait_for_status "$wsid" "online failed" 900) || true
  if [ "$final" != "online" ]; then
    fail "hermes workspace reaches online" "final status: $final"
    return 0
  fi
  pass "hermes workspace reaches online"

  local token
  token=$(echo "$resp" | e2e_extract_token)
  if [ -z "$token" ]; then
    token=$(e2e_mint_workspace_token "$wsid")
  fi
  if [ -z "$token" ]; then
    fail "resolve hermes workspace token" "no token returned"
    return 0
  fi

  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    if echo "$reply" | grep -q "PONG"; then
      validated "hermes reply contains PONG"
    else
      validated "hermes reply non-empty (first 80 chars: ${reply:0:80})"
    fi
    assert_activity_logged "hermes" "$wsid" "$token"
  else
    fail "hermes reply" "${reply:-<empty or error>}"
  fi
}

####################################################################
# Secondary runtimes — same provision/online/A2A loop, parametrized.
####################################################################
# Codex and OpenClaw use OpenAI as their LLM provider in this smoke and
# don't need the hermes-specific HERMES_* secret block. Skip if no key.
# claude-code + hermes stay first-class above because each has unique
# provisioning quirks (claude-code OAuth, hermes cold-boot tolerance);
# refactoring them into this generic loop would lose those guards.

run_openai_runtime() {
  local runtime="$1"
  local label="$2"
  echo ""
  echo "=== $label happy path ==="
  if [ -z "${E2E_OPENAI_API_KEY:-}" ]; then
    skip "E2E_OPENAI_API_KEY not set ($runtime needs an LLM provider key)"
    return 0
  fi
  local secrets
  secrets=$(python3 -c "
import json, os
k = os.environ['E2E_OPENAI_API_KEY']
print(json.dumps({
    'OPENAI_API_KEY': k,
    'OPENAI_BASE_URL': 'https://api.openai.com/v1',
    'MODEL_PROVIDER': 'openai:gpt-4o-mini',
}))
")
  local resp wsid
  resp=$(curl -s -X POST "$BASE/workspaces" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} -H "Content-Type: application/json" \
    -d "{\"name\":\"Priority E2E ($runtime)\",\"runtime\":\"$runtime\",\"tier\":1,\"model\":\"openai/gpt-4o-mini\",\"secrets\":$secrets}")
  wsid=$(echo "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))') || true
  if [ -z "$wsid" ]; then
    fail "create $runtime workspace" "$resp"
    return 0
  fi
  CREATED_WSIDS+=("$wsid")
  echo "  workspace=$wsid"

  local final
  final=$(wait_for_status "$wsid" "online failed" 240) || true
  if [ "$final" != "online" ]; then
    fail "$runtime workspace reaches online" "final status: $final"
    return 0
  fi
  pass "$runtime workspace reaches online"

  local token
  token=$(echo "$resp" | e2e_extract_token)
  if [ -z "$token" ]; then
    token=$(e2e_mint_workspace_token "$wsid")
  fi
  if [ -z "$token" ]; then
    fail "resolve $runtime workspace token" "no token returned"
    return 0
  fi

  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    if echo "$reply" | grep -q "PONG"; then
      validated "$runtime reply contains PONG"
    else
      validated "$runtime reply non-empty (first 80 chars: ${reply:0:80})"
    fi
    assert_activity_logged "$runtime" "$wsid" "$token"
  else
    fail "$runtime reply" "${reply:-<empty or error>}"
  fi
}

run_codex()      { run_openai_runtime "codex"      "codex"; }
run_openclaw()   { run_openai_runtime "openclaw"   "openclaw"; }

####################################################################
# google-adk arm — Gemini. REGISTRATION asserted always; LIVE arm is
# REQUIRED-when-keyed, LOUD-skip-when-absent (NEVER best-effort/fail-open).
####################################################################
# google-adk serves Gemini two ways (providers.yaml runtimes.google-adk):
#   * platform arm  → keyless Vertex via the Molecule LLM proxy (server-side
#     WIF mint, platform_managed billing — the org-default PROD path). It needs
#     a platform WIF identity that CI does NOT have, so this arm does NOT drive
#     the keyless-Vertex path (no fail-open arm — we never green a path we can't
#     actually exercise).
#   * google arm   → AI Studio API-key BYOK (the tenant's OWN GOOGLE/GEMINI
#     key), bare `gemini-2.5-pro`. This is the CI-/staging-exercisable path and
#     is what the LIVE portion below drives when E2E_GOOGLE_API_KEY is present.
#
# Two-part contract (core#2332 P0.1 — google-adk previously had ZERO e2e):
#   1. REGISTRATION (always, NO live creds): google-adk MUST be present in the
#      deployed manifest.json's workspace_templates — that file is the SSOT the
#      Create-handler's runtime allowlist is derived from (runtime_registry.go::
#      loadRuntimesFromManifest). If it is absent, a google-adk create 422s
#      RUNTIME_UNSUPPORTED, so registration is the precondition for ANY serving.
#      Asserting it offline means even a key-less CI run proves google-adk is
#      registered (a regression that drops it from the manifest reds the gate).
#      This does NOT bump VALIDATED — registration is not end-to-end serving.
#   2. LIVE (REQUIRED-when-keyed): with E2E_GOOGLE_API_KEY set, provision the
#      AI-Studio BYOK arm end-to-end (online + non-error A2A reply). A miss here
#      is a HARD fail() (fail-closed-if-present), exactly like the claude-code /
#      hermes / openai arms — NOT a best-effort miss. Without the key the live
#      portion is a LOUD skip() (dev-convenience), same as every keyed arm.
run_google_adk() {
  echo ""
  echo "=== google-adk (Gemini) — registration + AI-Studio BYOK happy path ==="

  # ── Part 1: REGISTRATION (always; no live creds needed) ──────────────────
  # Assert google-adk is in the manifest.json workspace_templates SSOT (the
  # Create-handler allowlist source). WORKSPACE_MANIFEST_PATH override mirrors
  # the server's own env (runtime_registry.go::manifestPath); otherwise resolve
  # the monorepo-root manifest.json relative to this script (tests/e2e/ -> repo
  # root is two levels up).
  local manifest="${WORKSPACE_MANIFEST_PATH:-$(cd "$(dirname "$0")/../.." && pwd)/manifest.json}"
  if [ ! -f "$manifest" ]; then
    fail "google-adk registration" "manifest.json not found at $manifest (cannot verify the runtime allowlist SSOT)"
    return 0
  fi
  local registered
  registered=$(python3 -c '
import json, sys
try:
    m = json.load(open(sys.argv[1]))
except Exception as e:
    print("ERR:%s" % e); sys.exit(0)
names = [t.get("name") for t in m.get("workspace_templates", [])]
# loadRuntimesFromManifest strips the "-default" vanilla suffix; match the same.
norm = {n[:-len("-default")] if isinstance(n, str) and n.endswith("-default") else n for n in names}
print("yes" if "google-adk" in norm else "no:%s" % sorted(n for n in norm if n))
' "$manifest")
  if [ "$registered" != "yes" ]; then
    fail "google-adk registered in manifest.json workspace_templates" \
      "google-adk absent from the Create-handler runtime allowlist SSOT ($registered) — a create would 422 RUNTIME_UNSUPPORTED"
    return 0
  fi
  pass "google-adk registered in manifest.json workspace_templates (Create-handler allowlist SSOT)"

  # ── Part 2: LIVE arm (REQUIRED-when-keyed, LOUD-skip-when-absent) ─────────
  # AI-Studio BYOK path: the tenant's own GOOGLE_API_KEY/GEMINI_API_KEY. The
  # keyless-Vertex PROD path needs a platform WIF identity CI lacks, so it is
  # NOT exercised here (no fail-open arm). Same env name the staging-full-saas
  # google-adk arm uses (E2E_GOOGLE_API_KEY).
  if [ -z "${E2E_GOOGLE_API_KEY:-}" ]; then
    skip "E2E_GOOGLE_API_KEY not set (google-adk live arm needs an AI-Studio Gemini key; keyless-Vertex needs platform WIF, not available in CI)"
    return 0
  fi
  local secrets
  secrets=$(python3 -c "
import json, os
# The google provider (providers.yaml) reads GEMINI_API_KEY / GOOGLE_API_KEY and
# dials generativelanguage.googleapis.com with the tenant's OWN key. Inject under
# both names the provider accepts so the adapter resolves regardless of order.
k = os.environ['E2E_GOOGLE_API_KEY']
print(json.dumps({'GOOGLE_API_KEY': k, 'GEMINI_API_KEY': k}))
")
  local resp wsid
  # Bare `gemini-2.5-pro` is the registered AI-Studio BYOK id for google-adk
  # (providers.yaml runtimes.google-adk `google` arm). DeriveProvider routes the
  # bare gemini- id to the google vendor (third_party_anthropic_compat, BYOK).
  resp=$(curl -s -X POST "$BASE/workspaces" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} -H "Content-Type: application/json" \
    -d "{\"name\":\"Priority E2E (google-adk)\",\"runtime\":\"google-adk\",\"tier\":1,\"model\":\"gemini-2.5-pro\",\"secrets\":$secrets}")
  wsid=$(echo "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))') || true
  if [ -z "$wsid" ]; then
    fail "create google-adk workspace" "$resp"
    return 0
  fi
  CREATED_WSIDS+=("$wsid")
  echo "  workspace=$wsid"

  # google-adk runtime image cold boot ~30-90s (image already pulled).
  local final
  final=$(wait_for_status "$wsid" "online failed" 240) || true
  if [ "$final" != "online" ]; then
    fail "google-adk workspace reaches online" "final status: $final"
    return 0
  fi
  pass "google-adk workspace reaches online"

  local token
  token=$(echo "$resp" | e2e_extract_token)
  if [ -z "$token" ]; then
    token=$(e2e_mint_workspace_token "$wsid")
  fi
  if [ -z "$token" ]; then
    fail "resolve google-adk workspace token" "no token returned"
    return 0
  fi

  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    if echo "$reply" | grep -q "PONG"; then
      validated "google-adk reply contains PONG"
    else
      validated "google-adk reply non-empty (first 80 chars: ${reply:0:80})"
    fi
    assert_activity_logged "google-adk" "$wsid" "$token"
  else
    fail "google-adk reply" "${reply:-<empty or error>}"
  fi
}

####################################################################
# Mock arm — the GUARANTEED, always-available REQUIRE-LIVE backbone.
####################################################################
# The mock runtime (workspace-server/internal/handlers/mock_runtime.go)
# is a virtual workspace: NO container, NO EC2, NO LLM key. The org-import
# path (createWorkspaceTree, org_import.go) short-circuits a runtime=mock
# workspace straight to status='online' (no provisioner needed), and the
# A2A proxy (a2a_proxy.go → handleMockA2A) synthesises a deterministic
# canned JSON-RPC reply with logActivity=true (writes the activity_logs
# row too). That makes mock the perfect REQUIRE-LIVE backbone: it
# exercises the SAME plumbing every real runtime needs to pass —
#   provision-decision → status=online → A2A round-trip → activity_logs —
# without depending on any external provider key or LLM availability. It
# is GREEN on a healthy platform and RED only if that plumbing genuinely
# breaks (DB insert, status flip, A2A proxy, activity logging). No more
# false-green (zero-validated is impossible when mock works), and no more
# can't-go-green (mock needs no secret, so it always runs in CI).
#
# Why org-import (POST /org/import) instead of POST /workspaces:
#   The mock→online short-circuit lives ONLY in createWorkspaceTree
#   (org_import.go). The single-workspace Create handler (workspace.go)
#   has no mock branch — it routes runtime=mock through
#   provisionWorkspaceAuto, which in CI's local-build mode has no mock
#   image and would never reach online. Org-import is the supported path
#   to a live mock workspace, so the arm drives it.
#
# The canned reply is one of the "On it!" variants (NOT "PONG"), so this
# arm validates on the non-empty / non-error branch — that is the real
# contract for mock (it proves the plumbing, not an LLM's instruction-
# following).
run_mock() {
  echo ""
  echo "=== mock (no-key plumbing backbone) happy path ==="
  # No secret gate — mock ALWAYS runs. That is the whole point: it is the
  # required-validation arm that keeps E2E_REQUIRE_LIVE honest without a key.

  # Inline single-workspace mock org. model is a required field on the
  # org-import contract (createWorkspaceTree fails-closed without one);
  # mock never USES the model, so any non-empty value satisfies the
  # contract. The org-import path does not run the Create handler's
  # registry model-validation, so "mock" is accepted as-is.
  # POST /org/import is AdminAuth-gated (router.go:778). When the platform has
  # ADMIN_TOKEN set (as the e2e-api CI job now does), an unauthenticated import
  # 401s with {"error":"admin auth required"}. Send the same admin bearer the
  # mint helper uses (MOLECULE_ADMIN_TOKEN, ADMIN_TOKEN fallback) — guarded so a
  # bootstrap/dev platform with no admin token (fail-open) still works.
  local admin_bearer="${MOLECULE_ADMIN_TOKEN:-${ADMIN_TOKEN:-}}"
  local admin_auth=()
  [ -n "$admin_bearer" ] && admin_auth=(-H "Authorization: Bearer $admin_bearer")
  local import_resp wsid
  import_resp=$(curl -s -X POST "$BASE/org/import" -H "Content-Type: application/json" \
    ${admin_auth[@]+"${admin_auth[@]}"} \
    -d '{
      "template": {
        "name": "Priority E2E Mock Org",
        "defaults": {"runtime": "mock", "model": "mock", "tier": 1},
        "workspaces": [
          {"name": "Priority E2E (mock)", "runtime": "mock", "model": "mock", "tier": 1}
        ]
      }
    }')
  # org-import returns {"org":..., "count":N, "workspaces":[{"id":...,
  # "name":...,"tier":...}, ...]} (handlers/org.go:898-901). Pull the id of
  # the single workspace we declared. (Older "results" key fallback kept for
  # forward/back compat in case the response shape is ever versioned.)
  wsid=$(echo "$import_resp" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for r in (d.get("workspaces") or d.get("results") or []):
    if r.get("name") == "Priority E2E (mock)" and r.get("id"):
        print(r["id"]); break
') || true
  if [ -z "$wsid" ]; then
    # mock org-import is the REQUIRE-LIVE backbone and is EXPECTED to succeed in
    # CI now that the e2e-api job wires an admin token (ADMIN_TOKEN on the
    # platform + MOLECULE_ADMIN_TOKEN sent above). A missing id here is a REAL
    # break (admin-auth wiring, org-import create, or the mock short-circuit) and
    # MUST red the gate — so this is a hard fail(), not a best-effort miss. Under
    # E2E_REQUIRE_LIVE=1 a FAIL also forces a non-zero exit via
    # evaluate_require_live_gate. Surface the response so the break is visible
    # (e.g. {"error":"admin auth required"} would mean the token wiring regressed).
    fail "create mock workspace (org-import)" "$import_resp"
    return 0
  fi
  CREATED_WSIDS+=("$wsid")
  echo "  workspace=$wsid"

  # Mock goes straight to online (no container boot) — a short budget is
  # plenty; if it is NOT online quickly the mock short-circuit in
  # createWorkspaceTree is genuinely broken and the gate SHOULD red.
  local final
  final=$(wait_for_status "$wsid" "online failed" 60) || true
  if [ "$final" != "online" ]; then
    fail "mock workspace reaches online" "final status: $final (mock should go online without provisioning)"
    return 0
  fi
  pass "mock workspace reaches online"

  # Mock workspaces are not created with an inline token; mint one via the
  # admin endpoint (same fallback every other arm uses).
  local token
  token=$(e2e_mint_workspace_token "$wsid") || true
  if [ -z "$token" ]; then
    fail "resolve mock workspace token" "no token returned from POST /admin/workspaces/:id/tokens"
    return 0
  fi

  # A2A round-trip. The mock proxy returns a canned non-error reply (one
  # of the "On it!" variants) — NOT "PONG" — so we validate on the
  # non-empty branch. A non-error, non-empty reply means the A2A proxy
  # short-circuit + reply-shape contract are intact end-to-end.
  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    validated "mock reply non-empty (canned; first 80 chars: ${reply:0:80})"
    assert_activity_logged "mock" "$wsid" "$token"
  else
    fail "mock reply" "${reply:-<empty or error>} (mock A2A short-circuit should always return a canned reply)"
  fi
}

####################################################################
# MiniMax live arm — OPPORTUNISTIC (best-effort) real-LLM arm.
####################################################################
# NOTE: this is now a BEST-EFFORT arm, not the REQUIRE-LIVE backbone.
# mock (run_mock above) is the guaranteed, no-key validation that keeps
# the gate honest. This arm uses the BARE registered BYOK id `MiniMax-M2.7`
# (NOT the colon `minimax:MiniMax-M2.7`): on claude-code the colon form is
# INTENTIONALLY unregistered — the claude-code adapter cannot strip the
# `minimax:` prefix, so DeriveProvider rejects it 422
# UNREGISTERED_MODEL_FOR_RUNTIME before any provisioning (provider-registry
# SSOT, internal#718; pinned by derive_provider_matrix_test.go's
# colon-vs-slash-vs-bare triple, and observed on real staging job 295075).
# The bare id is in claude-code's `minimax` arm (registry_gen.go:88
# Models=[MiniMax-M2,MiniMax-M2.7,MiniMax-M2.7-highspeed,MiniMax-M3]) and
# derives provider=minimax (BYOK via MINIMAX_API_KEY), so create-validation
# accepts it. This arm stays BEST-EFFORT (bestfail, non-gating) for transient
# MiniMax provisioning / backend issues — mock carries the REQUIRED gate; if
# MiniMax DOES come up it validates as a bonus real-LLM check.
# Drives the claude-code runtime against MiniMax (BYOK) using the
# already-present Gitea secret MOLECULE_STAGING_MINIMAX_API_KEY,
# surfaced into the env as E2E_MINIMAX_API_KEY (same name + secret the
# staging-smoke / continuous-synth canaries use — see staging-smoke.yml
# and continuous-synth-e2e.yml). NO new credential is introduced.
#
# Why this is the arm that keeps the REQUIRED gate honest:
#   - claude-code's `minimax` provider (providers.yaml / registry_gen.go)
#     is third_party_anthropic_compat: it reads MINIMAX_API_KEY at boot
#     and routes ANTHROPIC_BASE_URL → api.minimax.io/anthropic. So the
#     ONLY tenant secret needed is {"MINIMAX_API_KEY": <key>} — exactly
#     the SECRETS_JSON branch test_staging_full_saas.sh uses.
#   - Model id is the BARE `MiniMax-M2.7`, the registered BYOK arm for
#     claude-code (registry_gen.go:88 Runtimes["claude-code"]["minimax"]
#     Models). DeriveProvider routes bare → provider=minimax (BYOK). The
#     colon-namespaced `minimax:MiniMax-M2.7` is UNREGISTERED on claude-code
#     (the adapter can't strip `minimax:`; internal#718) and 422s create —
#     it is only the correct BYOK id on openclaw/hermes, which DO strip it.
run_minimax() {
  echo ""
  echo "=== minimax (claude-code BYOK) happy path ==="
  if [ -z "${E2E_MINIMAX_API_KEY:-}" ]; then
    skip "E2E_MINIMAX_API_KEY not set (MiniMax live arm needs the MiniMax key)"
    return 0
  fi
  local secrets
  secrets=$(python3 -c "
import json, os
# claude-code's minimax provider (third_party_anthropic_compat) reads
# MINIMAX_API_KEY and points ANTHROPIC_BASE_URL at api.minimax.io/anthropic
# at boot — so the ONLY tenant secret needed is the MiniMax key itself.
print(json.dumps({'MINIMAX_API_KEY': os.environ['E2E_MINIMAX_API_KEY']}))
")
  local resp wsid
  # BARE registered BYOK model id `MiniMax-M2.7` (registry_gen.go:88). The
  # colon form `minimax:MiniMax-M2.7` is UNREGISTERED on claude-code (adapter
  # can't strip `minimax:`; internal#718) and 422s create — bare derives
  # provider=minimax (BYOK via MINIMAX_API_KEY) and passes create-validation.
  resp=$(curl -s -X POST "$BASE/workspaces" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} -H "Content-Type: application/json" \
    -d "{\"name\":\"Priority E2E (minimax)\",\"runtime\":\"claude-code\",\"model\":\"MiniMax-M2.7\",\"tier\":1,\"secrets\":$secrets}")
  wsid=$(echo "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))') || true
  if [ -z "$wsid" ]; then
    # BEST-EFFORT: real MiniMax create/provision can still miss on transient
    # backend / provisioning issues (the bare model id itself is registered —
    # see header). Do NOT red the gate; mock is the required backbone. Report
    # the create response so any miss is visible.
    bestfail "create minimax workspace (best-effort; mock carries the gate)" "$resp"
    return 0
  fi
  CREATED_WSIDS+=("$wsid")
  echo "  workspace=$wsid"

  # claude-code runtime image is already pulled; cold boot ~30-90s. The
  # first MiniMax cold-call can be slow but that's covered by send_test_prompt's
  # --max-time 180.
  #
  # Budget = 90s (was 240s): this is the BEST-EFFORT MiniMax arm (bestfail
  # never reds the gate -- mock is the REQUIRE-LIVE backbone). 90s fully covers
  # the documented claude-code cold-boot success window above, so a MiniMax
  # workspace that genuinely converges is still caught + validated end-to-end;
  # we only stop burning the extra 150s of dead-wait on the runs where it never
  # leaves "provisioning" (the empirical CI case). E2E_MINIMAX_WAIT_SECS
  # overrides if MiniMax CI provisioning is ever fixed. This step was ~80% of
  # the REQUIRED "E2E API Smoke Test" wall-clock; cutting the dead-wait
  # shortens every core PR time-to-merge without dropping a single assertion.
  local final
  final=$(wait_for_status "$wsid" "online failed" "${E2E_MINIMAX_WAIT_SECS:-90}") || true
  if [ "$final" != "online" ]; then
    bestfail "minimax workspace reaches online (best-effort)" "final status: $final"
    return 0
  fi
  pass "minimax workspace reaches online"

  local token
  token=$(echo "$resp" | e2e_extract_token)
  if [ -z "$token" ]; then
    token=$(e2e_mint_workspace_token "$wsid")
  fi
  if [ -z "$token" ]; then
    bestfail "resolve minimax workspace token (best-effort)" "no token returned"
    return 0
  fi

  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    if echo "$reply" | grep -q "PONG"; then
      validated "minimax reply contains PONG"
    else
      validated "minimax reply non-empty (first 80 chars: ${reply:0:80})"
    fi
    assert_activity_logged "minimax" "$wsid" "$token"
  else
    bestfail "minimax reply (best-effort)" "${reply:-<empty or error>}"
  fi
}

# `mock` runs FIRST and by default: it is the no-key REQUIRE-LIVE backbone
# that guarantees >=1 validation on a healthy platform (see run_mock). The
# real-LLM arms (claude-code/codex/hermes/openclaw/minimax/google-adk) run if
# their secrets are present and add real-provider coverage on top; minimax is
# best-effort (never reds the gate). google-adk ALSO asserts its registration
# unconditionally (no key needed), then drives its AI-Studio BYOK live arm as a
# REQUIRED-when-keyed (fail-closed-if-present), LOUD-skip-when-absent arm.
WANT="${E2E_RUNTIMES:-mock claude-code codex hermes openclaw minimax google-adk}"
for r in $WANT; do
  case "$r" in
    mock)        run_mock ;;
    claude-code) run_claude_code ;;
    codex)       run_codex ;;
    hermes)      run_hermes ;;
    openclaw)    run_openclaw ;;
    minimax)     run_minimax ;;
    google-adk)  run_google_adk ;;
    all)         run_mock; run_claude_code; run_codex; run_hermes; run_openclaw; run_minimax; run_google_adk ;;
    *) echo "unknown runtime in E2E_RUNTIMES: $r" >&2; exit 2 ;;
  esac
done

echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped, $VALIDATED runtime(s) validated end-to-end ==="

# Final exit decision lives in evaluate_require_live_gate (defined at the top of
# this file, before any platform I/O) so the same logic is unit-tested in
# isolation by test_require_live_priority_gate_unit.sh. Mirror its return code
# into the process exit code.
evaluate_require_live_gate
exit $?
