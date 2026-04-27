#!/usr/bin/env bash
# E2E test: every runtime template (8 total) works end-to-end.
#
# Self-contained happy-path smoke per runtime. Provisions a fresh
# workspace, waits for status=online, sends a real A2A message, and
# asserts a non-error reply. Pins the contract so the post-#87 template
# extraction (and ongoing template work) can't silently break any
# runtime.
#
# Runtimes covered: claude-code, hermes, langgraph, crewai, autogen,
# deepagents, openclaw, gemini-cli. claude-code + hermes have unique
# provisioning quirks (claude-code OAuth, hermes 15-min cold-boot)
# and stay first-class with their own run_<runtime> functions; the
# 5 OpenAI-backed runtimes share run_openai_runtime; gemini-cli has
# its own block (Google AI key). Each phase skips cleanly if its
# prerequisite secret is missing.
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
# Usage:
#   CLAUDE_CODE_OAUTH_TOKEN=... E2E_OPENAI_API_KEY=... \
#     tests/e2e/test_priority_runtimes_e2e.sh
#
#   # Run only one runtime
#   E2E_RUNTIMES=claude-code tests/e2e/test_priority_runtimes_e2e.sh
#   E2E_RUNTIMES=hermes      tests/e2e/test_priority_runtimes_e2e.sh
#
# Prereqs:
#   - workspace-server on http://localhost:8080
#   - MOLECULE_ENV != production (required for admin/test-token)
#   - For claude-code: CLAUDE_CODE_OAUTH_TOKEN
#   - For hermes:      E2E_OPENAI_API_KEY  (other providers also OK if you
#                       set MODEL_SLUG_HERMES + matching secrets directly)

set -euo pipefail

source "$(dirname "$0")/_lib.sh"

PASS=0
FAIL=0
SKIP=0
CREATED_WSIDS=()

cleanup() {
  # `set -u` + empty array would error on "${CREATED_WSIDS[@]}"; the
  # ${VAR[@]+"…"} form expands to nothing when the array is unset/empty
  # so the loop body is skipped cleanly. Hits the skip-no-keys path.
  for wid in ${CREATED_WSIDS[@]+"${CREATED_WSIDS[@]}"}; do
    [ -n "$wid" ] && curl -s -X DELETE "$BASE/workspaces/$wid?confirm=true" > /dev/null || true
  done
}
trap cleanup EXIT

pass()  { echo "  PASS — $1"; PASS=$((PASS + 1)); }
fail()  { echo "  FAIL — $1"; echo "         $2"; FAIL=$((FAIL + 1)); }
skip()  { echo "  SKIP — $1"; SKIP=$((SKIP + 1)); }

# Pre-sweep any prior runs that left workspaces behind (same defence as
# test_notify_attachments_e2e.sh: trap fires on normal exit, but a
# SIGPIPE / kill -9 can bypass it).
PRIOR=$(curl -s "$BASE/workspaces" | python3 -c '
import json, sys
try:
    print(" ".join(w["id"] for w in json.load(sys.stdin) if w.get("name","").startswith("Priority E2E ")))
except Exception:
    pass
')
for _wid in $PRIOR; do
  echo "Sweeping prior workspace: $_wid"
  curl -s -X DELETE "$BASE/workspaces/$_wid?confirm=true" > /dev/null || true
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
  resp=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
    -d "{\"name\":\"Priority E2E (claude-code)\",\"runtime\":\"claude-code\",\"tier\":1,\"secrets\":$secrets}")
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
  token=$(e2e_mint_test_token "$wsid")
  if [ -z "$token" ]; then
    fail "mint claude-code test token" "no token returned"
    return 0
  fi

  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    if echo "$reply" | grep -q "PONG"; then
      pass "claude-code reply contains PONG"
    else
      pass "claude-code reply non-empty (first 80 chars: ${reply:0:80})"
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
  resp=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
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
  token=$(e2e_mint_test_token "$wsid")
  if [ -z "$token" ]; then
    fail "mint hermes test token" "no token returned"
    return 0
  fi

  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    if echo "$reply" | grep -q "PONG"; then
      pass "hermes reply contains PONG"
    else
      pass "hermes reply non-empty (first 80 chars: ${reply:0:80})"
    fi
    assert_activity_logged "hermes" "$wsid" "$token"
  else
    fail "hermes reply" "${reply:-<empty or error>}"
  fi
}

####################################################################
# Secondary runtimes — same provision/online/A2A loop, parametrized.
####################################################################
# These 5 templates (langgraph, crewai, autogen, deepagents, openclaw)
# all use OpenAI as their LLM provider in the default config and don't
# need the hermes-specific HERMES_* secret block. Skip if no key.
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
  resp=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
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
  token=$(e2e_mint_test_token "$wsid")
  if [ -z "$token" ]; then
    fail "mint $runtime test token" "no token returned"
    return 0
  fi

  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    if echo "$reply" | grep -q "PONG"; then
      pass "$runtime reply contains PONG"
    else
      pass "$runtime reply non-empty (first 80 chars: ${reply:0:80})"
    fi
    assert_activity_logged "$runtime" "$wsid" "$token"
  else
    fail "$runtime reply" "${reply:-<empty or error>}"
  fi
}

run_langgraph()  { run_openai_runtime "langgraph"  "langgraph"; }
run_crewai()     { run_openai_runtime "crewai"     "crewai"; }
run_autogen()    { run_openai_runtime "autogen"    "autogen"; }
run_deepagents() { run_openai_runtime "deepagents" "deepagents"; }
run_openclaw()   { run_openai_runtime "openclaw"   "openclaw"; }

# gemini-cli wants a Google API key, not OpenAI. Skip if absent.
run_gemini_cli() {
  echo ""
  echo "=== gemini-cli happy path ==="
  if [ -z "${E2E_GEMINI_API_KEY:-}" ]; then
    skip "E2E_GEMINI_API_KEY not set (gemini-cli needs Google AI key)"
    return 0
  fi
  local secrets
  secrets=$(python3 -c "
import json, os
print(json.dumps({'GEMINI_API_KEY': os.environ['E2E_GEMINI_API_KEY']}))
")
  local resp wsid
  resp=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
    -d "{\"name\":\"Priority E2E (gemini-cli)\",\"runtime\":\"gemini-cli\",\"tier\":1,\"secrets\":$secrets}")
  wsid=$(echo "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))') || true
  if [ -z "$wsid" ]; then fail "create gemini-cli workspace" "$resp"; return 0; fi
  CREATED_WSIDS+=("$wsid")
  echo "  workspace=$wsid"
  local final
  final=$(wait_for_status "$wsid" "online failed" 240) || true
  if [ "$final" != "online" ]; then
    fail "gemini-cli workspace reaches online" "final status: $final"
    return 0
  fi
  pass "gemini-cli workspace reaches online"
  local token; token=$(e2e_mint_test_token "$wsid")
  if [ -z "$token" ]; then fail "mint gemini-cli test token" "no token"; return 0; fi
  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    if echo "$reply" | grep -q "PONG"; then
      pass "gemini-cli reply contains PONG"
    else
      pass "gemini-cli reply non-empty (first 80 chars: ${reply:0:80})"
    fi
    assert_activity_logged "gemini-cli" "$wsid" "$token"
  else
    fail "gemini-cli reply" "${reply:-<empty or error>}"
  fi
}

WANT="${E2E_RUNTIMES:-claude-code hermes}"
for r in $WANT; do
  case "$r" in
    claude-code) run_claude_code ;;
    hermes)      run_hermes ;;
    langgraph)   run_langgraph ;;
    crewai)      run_crewai ;;
    autogen)     run_autogen ;;
    deepagents)  run_deepagents ;;
    openclaw)    run_openclaw ;;
    gemini-cli)  run_gemini_cli ;;
    all)         run_claude_code; run_hermes; run_langgraph; run_crewai; run_autogen; run_deepagents; run_openclaw; run_gemini_cli ;;
    *) echo "unknown runtime in E2E_RUNTIMES: $r" >&2; exit 2 ;;
  esac
done

echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped ==="
[ "$FAIL" -eq 0 ]
