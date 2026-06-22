#!/usr/bin/env bash
# E2E test: Agents talk TO EACH OTHER via A2A delegation
# Tests cross-runtime peer-to-peer communication, not just user→agent
set -euo pipefail

PLATFORM="${1:-http://localhost:8080}"
OPENAI_KEY="${OPENAI_API_KEY:?Set OPENAI_API_KEY env var}"
PASS=0
FAIL=0

check() {
  local label="$1" expected="$2" actual="$3"
  if echo "$actual" | grep -qi "$expected"; then
    echo "PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $label"
    echo "  expected: $expected"
    echo "  got: $(echo "$actual" | head -3)"
    FAIL=$((FAIL + 1))
  fi
}

wait_online() {
  local id="$1" name="$2" max="${3:-60}"
  for i in $(seq 1 "$max"); do
    local s
    s=$(curl -s "$PLATFORM/workspaces/$id" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
    [ "$s" = "online" ] && return 0
    [ "$s" = "failed" ] && return 1
    [ $((i % 10)) -eq 0 ] && echo "  [$name] $((i*5))s..."
    sleep 5
  done
  return 1
}

a2a_send() {
  local id="$1" message="$2"
  curl -s -X POST "$PLATFORM/workspaces/$id/a2a" \
    -H 'Content-Type: application/json' \
    -d "{\"method\":\"message/send\",\"params\":{\"message\":{\"role\":\"user\",\"parts\":[{\"kind\":\"text\",\"text\":\"$message\"}]}}}" | \
    python3 -c "import sys,json; r=json.load(sys.stdin); print(r.get('result',{}).get('parts',[{}])[0].get('text','ERROR'))" 2>/dev/null
}

echo "============================================"
echo "  Cross-Agent Chat: Agents Talk to Each Other"
echo "============================================"
echo ""

# --- Create 3 agents: PM (Claude Code), Developer (OpenClaw), Researcher (Codex) ---
echo "--- Creating 3 agents ---"

R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"PM","role":"Project Manager","tier":2,"template":"claude-code-default"}')
PM=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
echo "PM (Claude Code): $PM"

R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"Developer","role":"Code implementation","tier":2,"template":"openclaw"}')
DEV=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
echo "Developer (OpenClaw): $DEV"

R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"Researcher","role":"Research and analysis","tier":2,"template":"codex"}')
RES=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
echo "Researcher (Codex): $RES"

# --- Set hierarchy: PM -> Developer, Researcher ---
echo ""
echo "--- Setting hierarchy ---"
curl -s -X PATCH "$PLATFORM/workspaces/$DEV" -H 'Content-Type: application/json' \
  -d "{\"parent_id\":\"$PM\"}" > /dev/null
curl -s -X PATCH "$PLATFORM/workspaces/$RES" -H 'Content-Type: application/json' \
  -d "{\"parent_id\":\"$PM\"}" > /dev/null
echo "PM → Developer, Researcher"

# --- Set API keys ---
echo ""
echo "--- Setting API keys ---"
for ID in $PM $DEV $RES; do
  curl -s -X POST "$PLATFORM/workspaces/$ID/secrets" \
    -H 'Content-Type: application/json' \
    -d "{\"key\":\"OPENAI_API_KEY\",\"value\":\"$OPENAI_KEY\"}" > /dev/null
done

# Restart to pick up keys
for ID in $PM $DEV $RES; do
  curl -s -X POST "$PLATFORM/workspaces/$ID/restart" > /dev/null
done
echo "Set keys and restarting..."

# --- Wait for all online ---
echo ""
echo "--- Waiting for agents ---"
wait_online "$PM" "PM" 60 && check "PM online" "ok" "ok" || check "PM online" "online" "timeout"
wait_online "$DEV" "Developer" 120 && check "Developer online" "ok" "ok" || check "Developer online" "online" "timeout"
wait_online "$RES" "Researcher" 120 && check "Researcher online" "ok" "ok" || check "Researcher online" "online" "timeout"

# --- Set prompts with delegation instructions ---
echo ""
echo "--- Setting prompts with peer info ---"

curl -s -X PUT "$PLATFORM/workspaces/$PM/files/system-prompt.md" \
  -H 'Content-Type: application/json' \
  -d "{\"content\":\"You are the PM. You coordinate Developer and Researcher. When asked to research something, delegate to the Researcher (workspace ID: $RES) using the delegate_to_workspace tool. When asked to build something, delegate to the Developer (workspace ID: $DEV). Always include the peer's response in your answer.\"}" > /dev/null

curl -s -X PUT "$PLATFORM/workspaces/$DEV/files/system-prompt.md" \
  -H 'Content-Type: application/json' \
  -d '{"content":"You are the Developer. When asked to code or build something, describe the approach briefly. Keep responses under 30 words."}' > /dev/null

curl -s -X PUT "$PLATFORM/workspaces/$RES/files/system-prompt.md" \
  -H 'Content-Type: application/json' \
  -d '{"content":"You are the Researcher. When asked to research something, provide a brief 1-sentence finding. Keep responses under 30 words."}' > /dev/null

# Restart PM to pick up new prompt with peer IDs
curl -s -X POST "$PLATFORM/workspaces/$PM/restart" > /dev/null
sleep 5
wait_online "$PM" "PM" 60 || true
check "Prompts set" "ok" "ok"

# --- Test 1: Direct agent responses ---
echo ""
echo "--- Test 1: Direct responses (no delegation) ---"

echo "  Asking Developer directly..."
RESP=$(a2a_send "$DEV" "how would you implement a REST API?")
echo "  Developer: $RESP"
check "Developer responds directly" "API" "$RESP"

echo "  Asking Researcher directly..."
RESP=$(a2a_send "$RES" "what is the latest trend in AI agents?")
echo "  Researcher: $RESP"
check "Researcher responds directly" "agent" "$RESP"

# --- Test 2: PM delegates to Researcher ---
echo ""
echo "--- Test 2: PM delegates to Researcher (cross-runtime A2A) ---"
echo "  Asking PM to research something (should delegate to Researcher)..."
RESP=$(a2a_send "$PM" "Please ask the Researcher to briefly explain what Claude Code is.")
echo "  PM says: $RESP"
# The response should contain info from the Researcher
check "PM got Researcher's response" "graph\|agent\|lang\|workflow" "$RESP"

# --- Test 3: PM delegates to Developer ---
echo ""
echo "--- Test 3: PM delegates to Developer (cross-runtime A2A) ---"
echo "  Asking PM to get dev advice (should delegate to Developer)..."
RESP=$(a2a_send "$PM" "Ask the Developer how to build a WebSocket server.")
echo "  PM says: $RESP"
check "PM got Developer's response" "WebSocket\|socket\|server\|connect" "$RESP"

# --- Cleanup ---
echo ""
echo "--- Cleanup ---"
curl -s -X DELETE "$PLATFORM/workspaces/$PM" > /dev/null 2>&1
check "Cleanup" "ok" "ok"

echo ""
echo "============================================"
echo "  Results: $PASS passed, $FAIL failed"
echo "============================================"
exit $FAIL
