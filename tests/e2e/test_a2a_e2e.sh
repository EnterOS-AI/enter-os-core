#!/usr/bin/env bash
set -euo pipefail

BASE="${BASE:-http://localhost:8080}"
PASS=0
FAIL=0
TIMEOUT="${A2A_TIMEOUT:-120}"  # seconds per A2A call (override via A2A_TIMEOUT env var)

# shellcheck source=_lib.sh
source "$(dirname "$0")/_lib.sh"
# molecule-core#1995 (#1994 follow-on): real-completion assertion helpers.
# Adds a NEGATIVE error-as-text check on top of the shape checks below, so a
# broken agent that returns its error AS a text part
# ({"kind":"text","text":"Agent error (Exception) ..."}) — which STILL
# matches the shape check `"kind":"text"` — now FAILS instead of passing.
# shellcheck source=lib/completion_assert.sh
source "$(dirname "$0")/lib/completion_assert.sh"

# check_no_error_as_text <desc> <agent_text>
# Additive negative gate: PASS only if the agent text carries NO
# error-as-text marker (Agent error / Exception / error result /
# MISSING_BYOK_CREDENTIAL). Uses the same scanner as the staging
# real-completion gate so the trap is closed consistently across lanes.
check_no_error_as_text() {
  local desc="$1"
  local text="$2"
  local hit
  if hit=$(a2a_completion_error_marker "$text"); then
    echo "FAIL: $desc"
    echo "  agent returned an error-AS-text payload (matched '$hit') — a broken"
    echo "  agent that surfaces its error as a text part is NOT a real reply."
    echo "  got: $(echo "$text" | head -3)"
    FAIL=$((FAIL + 1))
  else
    echo "PASS: $desc"
    PASS=$((PASS + 1))
  fi
}

check() {
  local desc="$1"
  local expected="$2"
  local actual="$3"
  if echo "$actual" | grep -qF -- "$expected"; then
    echo "PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $desc"
    echo "  expected to contain: $expected"
    echo "  got: $(echo "$actual" | head -5)"
    FAIL=$((FAIL + 1))
  fi
}

a2a_send() {
  local ws_id="$1"
  local text="$2"
  curl -s --max-time "$TIMEOUT" -X POST "$BASE/workspaces/$ws_id/a2a" \
    -H "Content-Type: application/json" \
    -d "{
      \"method\": \"message/send\",
      \"params\": {
        \"message\": {
          \"role\": \"user\",
          \"parts\": [{\"type\": \"text\", \"text\": \"$text\"}]
        }
      }
    }"
}

echo "=== A2A End-to-End Tests (Free Model: google/gemini-2.5-flash) ==="
echo ""

# --- Setup: find or create workspaces ---
ECHO_ID=$(curl -s "$BASE/workspaces" | python3 -c "
import sys, json
ws = json.load(sys.stdin)
for w in ws:
    if w['name'] == 'Echo Agent' and w['status'] == 'online':
        print(w['id']); break
else:
    print('')
")
SEO_ID=$(curl -s "$BASE/workspaces" | python3 -c "
import sys, json
ws = json.load(sys.stdin)
for w in ws:
    if w['name'] == 'SEO Agent' and w['status'] == 'online':
        print(w['id']); break
else:
    print('')
")

if [ -z "$ECHO_ID" ] || [ -z "$SEO_ID" ]; then
  echo "ERROR: Need both Echo Agent and SEO Agent online. Found echo=$ECHO_ID seo=$SEO_ID"
  exit 1
fi

echo "Echo Agent: $ECHO_ID"
echo "SEO Agent:  $SEO_ID"
echo ""

# ========================================
# Test 1: Basic message/send — Echo Agent
# ========================================
echo "--- Test 1: Basic message/send ---"
R=$(a2a_send "$ECHO_ID" "Say hello back")
check "JSON-RPC response has result" '"result"' "$R"
check "Response has agent role" '"role":"agent"' "$R"
check "Response has text part" '"kind":"text"' "$R"
TEXT=$(echo "$R" | python3 -c "import sys,json; r=json.load(sys.stdin); print(r['result']['parts'][0]['text'][:200])" 2>/dev/null || echo "PARSE_ERROR")
# Negative gate (#1994): the text part must not BE an error.
check_no_error_as_text "Echo reply is not an error-as-text payload" "$TEXT"
echo "  Agent said: $TEXT"
echo ""

# ========================================
# Test 2: Basic message/send — SEO Agent
# ========================================
echo "--- Test 2: SEO Agent responds ---"
R=$(a2a_send "$SEO_ID" "What SEO skills do you have?")
check "SEO agent responds" '"result"' "$R"
check "SEO response has text" '"kind":"text"' "$R"
TEXT=$(echo "$R" | python3 -c "import sys,json; r=json.load(sys.stdin); print(r['result']['parts'][0]['text'][:200])" 2>/dev/null || echo "PARSE_ERROR")
# Negative gate (#1994): a broken SEO agent that returns "Agent error
# (Exception) ..." AS text still matches the `"kind":"text"` shape check
# above — THAT is the gap that let drained-key/byok-misroute failures pass
# CI. This makes that case FAIL.
check_no_error_as_text "SEO reply is not an error-as-text payload" "$TEXT"
echo "  SEO Agent said: $TEXT"
echo ""

# ========================================
# Test 3: JSON-RPC envelope wrapping
# ========================================
echo "--- Test 3: Auto JSON-RPC envelope wrapping ---"
# Send bare method+params (no jsonrpc/id fields) — proxy should wrap it
R=$(curl -s --max-time "$TIMEOUT" -X POST "$BASE/workspaces/$ECHO_ID/a2a" \
  -H "Content-Type: application/json" \
  -d '{"method":"message/send","params":{"message":{"role":"user","parts":[{"type":"text","text":"Bare request test"}]}}}')
check "Bare request gets wrapped and works" '"result"' "$R"
echo ""

# ========================================
# Test 4: Full JSON-RPC 2.0 envelope
# ========================================
echo "--- Test 4: Full JSON-RPC 2.0 envelope ---"
R=$(curl -s --max-time "$TIMEOUT" -X POST "$BASE/workspaces/$ECHO_ID/a2a" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":"custom-id-123","method":"message/send","params":{"message":{"role":"user","parts":[{"type":"text","text":"Full envelope test"}]}}}')
check "Full envelope returns result" '"result"' "$R"
check "Preserves custom request ID" '"id":"custom-id-123"' "$R"
echo ""

# ========================================
# Test 5: Invalid method returns error
# ========================================
echo "--- Test 5: Invalid method ---"
R=$(curl -s --max-time 10 -X POST "$BASE/workspaces/$ECHO_ID/a2a" \
  -H "Content-Type: application/json" \
  -d '{"method":"nonexistent/method","params":{}}')
check "Invalid method returns JSON-RPC error" '"error"' "$R"
check "Error code is method not found" '-32601' "$R"
echo ""

# ========================================
# Test 6: Offline workspace returns error
# ========================================
echo "--- Test 6: Offline workspace ---"
# Create a workspace but don't provision it
R=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" -d '{"name":"Offline Test","tier":1,"runtime":"external","external":true}')
OFFLINE_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
R=$(curl -s --max-time 10 -X POST "$BASE/workspaces/$OFFLINE_ID/a2a" \
  -H "Content-Type: application/json" \
  -d '{"method":"message/send","params":{"message":{"role":"user","parts":[{"type":"text","text":"test"}]}}}')
check "Offline workspace returns error" '"error"' "$R"
# Clean up
e2e_delete_workspace "$OFFLINE_ID" "Offline Test"
echo ""

# ========================================
# Test 7: Nonexistent workspace returns 404
# ========================================
echo "--- Test 7: Nonexistent workspace ---"
R=$(curl -s -o /dev/null -w "%{http_code}" --max-time 10 -X POST "$BASE/workspaces/00000000-0000-0000-0000-000000000000/a2a" \
  -H "Content-Type: application/json" \
  -d '{"method":"message/send","params":{"message":{"role":"user","parts":[{"type":"text","text":"test"}]}}}')
check "Nonexistent workspace returns 404" "404" "$R"
echo ""

# ========================================
# Test 8: Multi-turn conversation
# ========================================
echo "--- Test 8: Multi-turn conversation ---"
R1=$(a2a_send "$ECHO_ID" "My name is Alice. Remember that.")
check "Turn 1 succeeds" '"result"' "$R1"
R2=$(a2a_send "$ECHO_ID" "What is my name?")
check "Turn 2 succeeds" '"result"' "$R2"
TEXT2=$(echo "$R2" | python3 -c "import sys,json; r=json.load(sys.stdin); print(r['result']['parts'][0]['text'])" 2>/dev/null || echo "PARSE_ERROR")
echo "  Turn 2 response: $(echo "$TEXT2" | head -3)"
echo ""

# ========================================
# Test 9: Long input handling
# ========================================
echo "--- Test 9: Long input ---"
LONG_TEXT=$(python3 -c "print('This is a test sentence. ' * 50)")
R=$(a2a_send "$ECHO_ID" "$LONG_TEXT")
check "Long input returns result" '"result"' "$R"
echo ""

# ========================================
# Test 10: Peers can discover each other
# ========================================
echo "--- Test 10: Peer discovery ---"
R=$(curl -s "$BASE/registry/$ECHO_ID/peers")
check "Echo sees SEO as peer" 'SEO Agent' "$R"
R=$(curl -s "$BASE/registry/$SEO_ID/peers")
check "SEO sees Echo as peer" 'Echo Agent' "$R"
echo ""

# ========================================
# Test 11: Agent card reflects skills
# ========================================
echo "--- Test 11: Agent cards ---"
R=$(curl -s "$BASE/workspaces/$ECHO_ID")
check "Echo agent has agent_card" '"agent_card"' "$R"
check "Echo has skills" '"skills"' "$R"

R=$(curl -s "$BASE/workspaces/$SEO_ID")
check "SEO agent has agent_card" '"agent_card"' "$R"
check "SEO has skills" '"skills"' "$R"
echo ""

# ========================================
# Test 12: Heartbeat updates
# ========================================
echo "--- Test 12: Heartbeat ---"
sleep 2
R=$(curl -s "$BASE/workspaces/$ECHO_ID")
UPTIME=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['uptime_seconds'])")
check "Echo has uptime > 0" "true" "$([ "$UPTIME" -gt 0 ] 2>/dev/null && echo true || echo false)"
echo "  Echo uptime: ${UPTIME}s"
echo ""

# ========================================
# Summary
# ========================================
echo "=== Results: $PASS passed, $FAIL failed ==="
exit $FAIL
