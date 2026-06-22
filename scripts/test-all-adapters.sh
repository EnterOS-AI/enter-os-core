#!/usr/bin/env bash
# E2E test: all maintained adapters — create one agent per runtime, test A2A
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
    echo "  got: $(echo "$actual" | head -2)"
    FAIL=$((FAIL + 1))
  fi
}

wait_online() {
  local id="$1" name="$2" max="${3:-60}"
  for i in $(seq 1 "$max"); do
    local s
    s=$(curl -s "$PLATFORM/workspaces/$id" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
    [ "$s" = "online" ] && return 0
    [ "$s" = "failed" ] && echo "  $name FAILED" && return 1
    [ $((i % 10)) -eq 0 ] && echo "  [$name] $((i*5))s... ($s)"
    sleep 5
  done
  echo "  $name timed out after $((max*5))s"
  return 1
}

a2a_send() {
  local id="$1" message="$2" max_retries="${3:-3}"
  for attempt in $(seq 1 "$max_retries"); do
    local resp text
    resp=$(curl -s -X POST "$PLATFORM/workspaces/$id/a2a" \
      -H 'Content-Type: application/json' \
      -d "{\"method\":\"message/send\",\"params\":{\"message\":{\"role\":\"user\",\"parts\":[{\"kind\":\"text\",\"text\":\"$message\"}]}}}" 2>/dev/null)
    text=$(echo "$resp" | python3 -c "import sys,json; r=json.load(sys.stdin); print(r.get('result',{}).get('parts',[{}])[0].get('text',''))" 2>/dev/null)
    if echo "$text" | grep -qi "rate\|billing\|limit\|429"; then
      [ "$attempt" -lt "$max_retries" ] && echo "  Rate limited, waiting 60s ($attempt/$max_retries)..." && sleep 60 && continue
    fi
    echo "$text"
    return 0
  done
  echo "ERROR: retries exhausted"
}

echo "============================================"
echo "  All-Adapters E2E Test (4 runtimes)"
echo "============================================"
echo ""

# --- Create workspaces ---
echo "--- Step 1: Create 4 workspaces ---"

R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"Alice-Claude","role":"claude-code test","tier":2,"template":"claude-code-default"}')
ALICE=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "Create Alice (claude-code)" "provisioning" "$R"

R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"Bob-Codex","role":"codex test","tier":2,"template":"codex"}')
BOB=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "Create Bob (codex)" "provisioning" "$R"

R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"Carol-OpenClaw","role":"openclaw test","tier":2,"template":"openclaw"}')
CAROL=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "Create Carol (openclaw)" "provisioning" "$R"

R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"Dave-Hermes","role":"hermes test","tier":2,"template":"hermes"}')
DAVE=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "Create Dave (hermes)" "provisioning" "$R"

# --- Set API keys (skip Claude which uses OAuth) ---
echo ""
echo "--- Step 2: Set API keys ---"
for ID in $BOB $CAROL $DAVE; do
  curl -s -X POST "$PLATFORM/workspaces/$ID/secrets" \
    -H 'Content-Type: application/json' \
    -d "{\"key\":\"OPENAI_API_KEY\",\"value\":\"$OPENAI_KEY\"}" > /dev/null
done
echo "Set OPENAI_API_KEY on 3 agents"

# Auto-restart happens automatically when secrets are set
echo "Secrets trigger auto-restart — waiting for agents to come back..."
sleep 15

# --- Wait for all online ---
echo ""
echo "--- Step 3: Wait for agents (OpenClaw ~3min, Hermes may take longer) ---"

wait_online "$ALICE" "Alice-Claude" 20 && check "Alice online" "ok" "ok" || check "Alice online" "online" "timeout"
wait_online "$BOB" "Bob-Codex" 60 && check "Bob online" "ok" "ok" || check "Bob online" "online" "timeout"
wait_online "$DAVE" "Dave-Hermes" 180 && check "Dave online" "ok" "ok" || check "Dave online" "online" "timeout"
wait_online "$CAROL" "Carol-OpenClaw" 360 && check "Carol online" "ok" "ok" || check "Carol online" "online" "timeout"

# --- Test A2A messages ---
echo ""
echo "--- Step 4: A2A direct messages ---"

echo "  Talking to Alice (Claude Code)..."
RESP=$(a2a_send "$ALICE" "say hello in one word")
echo "    -> $RESP"
check "Alice responds" "hello" "$RESP"

echo "  Talking to Bob (Codex)..."
RESP=$(a2a_send "$BOB" "say hello in one word")
echo "    -> $RESP"
check "Bob responds" "hello" "$RESP"

echo "  Talking to Carol (OpenClaw)..."
RESP=$(a2a_send "$CAROL" "say hello in one word")
echo "    -> $RESP"
check "Carol responds" "hello" "$RESP"

echo "  Talking to Dave (Hermes)..."
RESP=$(a2a_send "$DAVE" "say hello in one word")
echo "    -> $RESP"
check "Dave responds" "hello" "$RESP"

# --- Peer discovery ---
echo ""
echo "--- Step 5: Peer discovery ---"
R=$(curl -s "$PLATFORM/registry/$ALICE/peers" | python3 -c "
import sys,json
peers = json.load(sys.stdin)
print(f'{len(peers)} peers: {\" \".join(p.get(\"name\",\"\") for p in peers)}')
" 2>/dev/null)
echo "  Alice sees: $R"
check "Alice sees 3 peers" "3 peers" "$R"

# --- Isolation ---
echo ""
echo "--- Step 6: Verify isolation ---"
HOST_WS=$(find /Users/hongming/Documents/GitHub/molecule-monorepo/workspace-configs-templates -maxdepth 1 -name 'ws-*' -type d 2>/dev/null | wc -l | tr -d ' ')
check "No ws-* dirs on host" "0" "$HOST_WS"

# --- Cleanup ---
echo ""
echo "--- Step 7: Cleanup ---"
for ID in $ALICE $BOB $CAROL $DAVE; do
  curl -s -X DELETE "$PLATFORM/workspaces/$ID" > /dev/null 2>&1
done
check "Cleanup" "ok" "ok"

echo ""
echo "============================================"
echo "  Results: $PASS passed, $FAIL failed"
echo "============================================"
exit $FAIL
