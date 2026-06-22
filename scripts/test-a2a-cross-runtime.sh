#!/usr/bin/env bash
# E2E test: Claude Code agent ↔ OpenClaw agent A2A communication
# Tests cross-runtime peer messaging between two different agent infras.
set -euo pipefail

PLATFORM="${1:-http://localhost:8080}"
OPENAI_KEY="${OPENAI_API_KEY:?Set OPENAI_API_KEY env var before running this test}"
PASS=0
FAIL=0

check() {
  local label="$1" expected="$2" actual="$3"
  if echo "$actual" | grep -qi "$expected"; then
    echo "PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $label"
    echo "  expected to contain: $expected"
    echo "  got: $actual"
    FAIL=$((FAIL + 1))
  fi
}

wait_online() {
  local id="$1" name="$2" max="${3:-30}"
  for i in $(seq 1 "$max"); do
    local s
    s=$(curl -s "$PLATFORM/workspaces/$id" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
    [ "$s" = "online" ] && return 0
    [ "$s" = "failed" ] && echo "  $name FAILED" && return 1
    [ $((i % 5)) -eq 0 ] && echo "  [$name] ${i}/${max}... ($s)"
    sleep 5
  done
  echo "  $name did not come online within $((max*5))s"
  return 1
}

# Send A2A message with retry (free OpenRouter has rate limits — 1 min cooldown)
a2a_send() {
  local id="$1" message="$2" max_retries="${3:-3}"
  for attempt in $(seq 1 "$max_retries"); do
    local resp
    resp=$(curl -s -X POST "$PLATFORM/workspaces/$id/a2a" \
      -H 'Content-Type: application/json' \
      -d "{\"method\":\"message/send\",\"params\":{\"message\":{\"role\":\"user\",\"parts\":[{\"kind\":\"text\",\"text\":\"$message\"}]}}}")

    local text
    text=$(echo "$resp" | python3 -c "import sys,json; r=json.load(sys.stdin); print(r.get('result',{}).get('parts',[{}])[0].get('text',''))" 2>/dev/null)

    # Check for rate limit / billing errors
    if echo "$text" | grep -qi "rate\|billing\|credits\|limit\|429\|throttl"; then
      if [ "$attempt" -lt "$max_retries" ]; then
        echo "  Rate limited, waiting 60s before retry ($attempt/$max_retries)..."
        sleep 60
        continue
      fi
    fi

    echo "$text"
    return 0
  done
  echo "ERROR: all retries exhausted"
  return 1
}

echo "============================================"
echo "  Cross-Runtime A2A: Claude Code ↔ OpenClaw"
echo "============================================"
echo ""

# -------------------------------------------------------
# Step 1: Create Claude Code agent
# -------------------------------------------------------
echo "--- Step 1: Create Claude Code agent ---"
R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"Alice","role":"Claude Code assistant","tier":2,"template":"claude-code-default"}')
ALICE_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "Create Alice (claude-code)" "provisioning" "$R"
echo "  Alice: $ALICE_ID"

# -------------------------------------------------------
# Step 2: Create OpenClaw agent
# -------------------------------------------------------
echo ""
echo "--- Step 2: Create OpenClaw agent ---"
R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"Bob","role":"OpenClaw research assistant","tier":2,"template":"openclaw"}')
BOB_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "Create Bob (openclaw)" "provisioning" "$R"
echo "  Bob: $BOB_ID"

# -------------------------------------------------------
# Step 3: Set Bob as Alice's peer (same parent = siblings)
# -------------------------------------------------------
echo ""
echo "--- Step 3: Make them peers (root-level siblings) ---"
# Root-level workspaces with no parent are siblings — they can communicate
echo "  Both are root-level → siblings → A2A allowed"

# -------------------------------------------------------
# Step 4: Set API key for Bob (OpenClaw needs OpenRouter key)
# -------------------------------------------------------
echo ""
echo "--- Step 4: Set Bob's API key ---"
R=$(curl -s -X POST "$PLATFORM/workspaces/$BOB_ID/secrets" \
  -H 'Content-Type: application/json' \
  -d "{\"key\":\"OPENAI_API_KEY\",\"value\":\"$OPENAI_KEY\"}")
check "Set Bob's OPENAI_API_KEY" "saved" "$R"

# -------------------------------------------------------
# Step 5: Wait for both to come online
# -------------------------------------------------------
echo ""
echo "--- Step 5: Wait for agents to come online ---"
echo "  (OpenClaw takes ~2 min for npm install + gateway start)"

if wait_online "$ALICE_ID" "Alice" 20; then
  check "Alice online" "ok" "ok"
else
  check "Alice online" "online" "timeout"
fi

if wait_online "$BOB_ID" "Bob" 360; then
  check "Bob online" "ok" "ok"
else
  check "Bob online" "online" "timeout"
fi

# -------------------------------------------------------
# Step 6: Customize prompts
# -------------------------------------------------------
echo ""
echo "--- Step 6: Set agent prompts ---"

# Alice's system prompt (written to container via Files API)
R=$(curl -s -X PUT "$PLATFORM/workspaces/$ALICE_ID/files/system-prompt.md" \
  -H 'Content-Type: application/json' \
  -d '{"content":"You are Alice, a helpful assistant. When asked to introduce yourself, say exactly: I am Alice, running on Claude Code. Keep responses under 20 words."}')
check "Set Alice prompt" "saved" "$R"

# Bob's SOUL.md (OpenClaw convention)
R=$(curl -s -X PUT "$PLATFORM/workspaces/$BOB_ID/files/SOUL.md" \
  -H 'Content-Type: application/json' \
  -d '{"content":"You are Bob, a research assistant. When asked to introduce yourself, say exactly: I am Bob, running on OpenClaw. Keep responses under 20 words."}')
check "Set Bob prompt (SOUL.md)" "saved" "$R"

# Restart both to pick up new prompts
echo "  Restarting agents..."
curl -s -X POST "$PLATFORM/workspaces/$ALICE_ID/restart" > /dev/null
curl -s -X POST "$PLATFORM/workspaces/$BOB_ID/restart" > /dev/null
sleep 5

echo "  Waiting for restart..."
wait_online "$ALICE_ID" "Alice" 20 || true
wait_online "$BOB_ID" "Bob" 360 || true

# -------------------------------------------------------
# Step 7: Test direct A2A messages
# -------------------------------------------------------
echo ""
echo "--- Step 7: Direct A2A messages ---"

echo "  Talking to Alice..."
ALICE_RESP=$(a2a_send "$ALICE_ID" "introduce yourself in one sentence")
echo "  Alice says: $ALICE_RESP"
check "Alice responds" "Alice" "$ALICE_RESP"

echo ""
echo "  Talking to Bob..."
BOB_RESP=$(a2a_send "$BOB_ID" "introduce yourself in one sentence")
echo "  Bob says: $BOB_RESP"
check "Bob responds" "Bob" "$BOB_RESP"

# -------------------------------------------------------
# Step 8: Verify peer discovery
# -------------------------------------------------------
echo ""
echo "--- Step 8: Peer discovery ---"

R=$(curl -s "$PLATFORM/registry/$ALICE_ID/peers" | python3 -c "
import sys,json
peers = json.load(sys.stdin)
names = [p.get('name','') for p in peers]
print(' '.join(names))
" 2>/dev/null)
echo "  Alice's peers: $R"
check "Alice sees Bob" "Bob" "$R"

R=$(curl -s "$PLATFORM/registry/$BOB_ID/peers" | python3 -c "
import sys,json
peers = json.load(sys.stdin)
names = [p.get('name','') for p in peers]
print(' '.join(names))
" 2>/dev/null)
echo "  Bob's peers: $R"
check "Bob sees Alice" "Alice" "$R"

# -------------------------------------------------------
# Step 9: Verify cross-runtime access control
# -------------------------------------------------------
echo ""
echo "--- Step 9: Access control ---"

R=$(curl -s -X POST "$PLATFORM/registry/check-access" -H 'Content-Type: application/json' \
  -d "{\"caller_id\":\"$ALICE_ID\",\"target_id\":\"$BOB_ID\"}")
check "Alice -> Bob allowed" "true" "$R"

R=$(curl -s -X POST "$PLATFORM/registry/check-access" -H 'Content-Type: application/json' \
  -d "{\"caller_id\":\"$BOB_ID\",\"target_id\":\"$ALICE_ID\"}")
check "Bob -> Alice allowed" "true" "$R"

# -------------------------------------------------------
# Step 10: Verify no ws-* dirs on host
# -------------------------------------------------------
echo ""
echo "--- Step 10: Verify isolation ---"

HOST_WS=$(find /Users/hongming/Documents/GitHub/molecule-monorepo/workspace-configs-templates -maxdepth 1 -name 'ws-*' -type d 2>/dev/null | wc -l | tr -d ' ')
check "No ws-* dirs on host" "0" "$HOST_WS"

echo ""
echo "  Alice container: $(docker ps --format '{{.Names}}' | grep "${ALICE_ID:0:12}" || echo 'not found')"
echo "  Bob container: $(docker ps --format '{{.Names}}' | grep "${BOB_ID:0:12}" || echo 'not found')"

# -------------------------------------------------------
# Step 11: Cleanup
# -------------------------------------------------------
echo ""
echo "--- Step 11: Cleanup ---"

curl -s -X DELETE "$PLATFORM/workspaces/$ALICE_ID" > /dev/null
curl -s -X DELETE "$PLATFORM/workspaces/$BOB_ID" > /dev/null
check "Cleanup" "ok" "ok"

# -------------------------------------------------------
# Results
# -------------------------------------------------------
echo ""
echo "============================================"
echo "  Results: $PASS passed, $FAIL failed"
echo "============================================"
exit $FAIL
