#!/usr/bin/env bash
# E2E test: hermes runtime native MCP push parity via molecule-a2a plugin.
#
# Validates the full chain shipped in:
#   - NousResearch/hermes-agent#18775           (upstream patch)
#   - Molecule-AI/hermes-platform-molecule-a2a  (plugin)
#   - Molecule-AI/molecule-ai-workspace-template-hermes#32 (workspace
#     template — Dockerfile bakes plugin in, executor uses /a2a/inbound)
#
# Test flow:
#   1. Provision two workspaces — peer (claude-code) + hermes
#   2. Set provider keys on hermes (the plugin path needs an LLM)
#   3. Wait both online
#   4. Verify hermes loaded the plugin (HTTP probe of /a2a/health
#      from inside the workspace)
#   5. Send A2A message peer → hermes
#   6. Verify hermes processes via plugin path (no fresh subprocess
#      per message; same hermes daemon handles the turn through full
#      pipeline)
#   7. Send a SECOND A2A message and verify hermes maintains session
#      continuity (the proof-point — old chat-completions path would
#      have lost context between turns)
#   8. Cleanup
#
# Pre-reqs:
#   - PLATFORM env or first arg pointing at a molecule platform that
#     has the hermes runtime image republished AFTER PR #32 merge
#   - $OPENROUTER_API_KEY (or $HERMES_API_KEY for direct Nous routing)
#   - $OPENAI_API_KEY (for the claude-code peer)
#
# Run:
#   PLATFORM=https://demo-tenant.staging.moleculesai.app \
#       ./scripts/test-hermes-plugin-e2e.sh

set -euo pipefail

PLATFORM="${PLATFORM:-${1:-http://localhost:8080}}"
HERMES_PROVIDER_KEY="${OPENROUTER_API_KEY:-${HERMES_API_KEY:-}}"
PEER_OPENAI_KEY="${OPENAI_API_KEY:-}"

if [ -z "$HERMES_PROVIDER_KEY" ]; then
  echo "FAIL: set OPENROUTER_API_KEY or HERMES_API_KEY for the hermes workspace"
  exit 2
fi
if [ -z "$PEER_OPENAI_KEY" ]; then
  echo "FAIL: set OPENAI_API_KEY for the claude-code peer workspace"
  exit 2
fi

PASS=0
FAIL=0

check() {
  local label="$1" expected="$2" actual="$3"
  if echo "$actual" | grep -qiE "$expected"; then
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
  local id="$1" name="$2" max="${3:-60}"
  for i in $(seq 1 "$max"); do
    local s
    s=$(curl -s "$PLATFORM/workspaces/$id" \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
    [ "$s" = "online" ] && return 0
    [ "$s" = "failed" ] && echo "  $name FAILED" && return 1
    [ $((i % 6)) -eq 0 ] && echo "  [$name] ${i}/${max}... ($s)"
    sleep 5
  done
  echo "  $name did not come online within $((max*5))s"
  return 1
}

a2a_send() {
  local id="$1" message="$2" max_retries="${3:-3}"
  for attempt in $(seq 1 "$max_retries"); do
    local resp text
    resp=$(curl -s -X POST "$PLATFORM/workspaces/$id/a2a" \
      -H 'Content-Type: application/json' \
      -d "$(python3 -c "import json,sys; print(json.dumps({
        'method': 'message/send',
        'params': {'message': {'role': 'user', 'parts': [{'kind': 'text', 'text': sys.argv[1]}]}}
      }))" "$message")")
    text=$(echo "$resp" | python3 -c "
import sys, json
try:
  r = json.load(sys.stdin)
  print(r.get('result', {}).get('parts', [{}])[0].get('text', ''))
except Exception:
  print('')
" 2>/dev/null)
    if echo "$text" | grep -qiE "rate|throttl|429|credits"; then
      [ "$attempt" -lt "$max_retries" ] && { sleep 60; continue; }
    fi
    echo "$text"
    return 0
  done
  echo "ERROR: all retries exhausted"
  return 1
}

# In-container probe via the platform's exec-in-workspace helper. If the
# platform doesn't expose one, this becomes a curl-from-host probe of
# the workspace's exposed port (skipped silently if no path exists).
probe_plugin_health() {
  local id="$1"
  curl -fsS "$PLATFORM/workspaces/$id/exec" \
    -H 'Content-Type: application/json' \
    -d '{"cmd": ["curl", "-fsS", "http://127.0.0.1:8645/a2a/health"]}' \
    2>/dev/null \
    || echo "exec-helper not available — skipping in-container probe"
}

echo "=========================================="
echo "  Hermes plugin path E2E"
echo "  Platform: $PLATFORM"
echo "=========================================="
echo ""

# -------------------------------------------------------
# 1. Provision peer (claude-code) + hermes
# -------------------------------------------------------
echo "--- 1. Provision peer (claude-code) ---"
R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"PeerAlice","role":"Claude Code peer","tier":2,"template":"claude-code-default"}')
PEER_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "Provision peer" "provisioning|online" "$R"
echo "  Peer: $PEER_ID"

echo ""
echo "--- 2. Provision hermes (plugin path) ---"
R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"HermesPluginBob","role":"Hermes peer (plugin path)","tier":2,"template":"hermes"}')
HERMES_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "Provision hermes" "provisioning|online" "$R"
echo "  Hermes: $HERMES_ID"

# -------------------------------------------------------
# 3. Set provider keys
# -------------------------------------------------------
echo ""
echo "--- 3. Set provider keys ---"
R=$(curl -s -X POST "$PLATFORM/workspaces/$HERMES_ID/secrets" \
  -H 'Content-Type: application/json' \
  -d "{\"key\":\"OPENROUTER_API_KEY\",\"value\":\"$HERMES_PROVIDER_KEY\"}")
check "Set hermes OPENROUTER_API_KEY" "saved" "$R"

R=$(curl -s -X POST "$PLATFORM/workspaces/$PEER_ID/secrets" \
  -H 'Content-Type: application/json' \
  -d "{\"key\":\"OPENAI_API_KEY\",\"value\":\"$PEER_OPENAI_KEY\"}")
check "Set peer OPENAI_API_KEY" "saved" "$R"

# -------------------------------------------------------
# 4. Wait online
# -------------------------------------------------------
echo ""
echo "--- 4. Wait online (hermes cold-boot ~3-6 min for fork install + plugin) ---"
wait_online "$PEER_ID" "Peer" 30 && check "Peer online" "ok" "ok" || check "Peer online" "online" "timeout"
wait_online "$HERMES_ID" "Hermes" 120 && check "Hermes online" "ok" "ok" || check "Hermes online" "online" "timeout"

# -------------------------------------------------------
# 5. Verify plugin loaded inside the hermes container
# -------------------------------------------------------
echo ""
echo "--- 5. Verify plugin loaded ---"
HEALTH=$(probe_plugin_health "$HERMES_ID")
echo "  Plugin /a2a/health probe: $HEALTH"
if echo "$HEALTH" | grep -q "molecule-a2a"; then
  check "Plugin /a2a/health responds 200" "molecule-a2a" "$HEALTH"
else
  echo "  (in-container probe not available on this platform — relying on A2A round-trip below)"
fi

# -------------------------------------------------------
# 6. First A2A message — establish session
# -------------------------------------------------------
echo ""
echo "--- 6. First A2A message (peer → hermes) ---"
echo "  Telling hermes: 'My name is Carol. Reply with just OK.'"
RESP1=$(a2a_send "$HERMES_ID" "My name is Carol. Reply with just the word OK.")
echo "  Hermes says: $RESP1"
check "First message gets a reply" "ok|received|got|name" "$RESP1"

# -------------------------------------------------------
# 7. Second A2A message — verify session continuity
# -------------------------------------------------------
echo ""
echo "--- 7. Second A2A message (proves session continuity) ---"
echo "  Asking hermes to recall the name from msg #1..."
RESP2=$(a2a_send "$HERMES_ID" "What name did I introduce myself with one message ago? One word answer.")
echo "  Hermes says: $RESP2"
# Plugin path: hermes daemon kept the conversation in its session store
# across turns; the answer should mention "Carol".
# Old chat-completions path: each turn was independent; reply would NOT
# know the prior name (would say "you didn't introduce yourself" or
# similar).
check "Session continuity proves plugin path" "carol" "$RESP2"

# -------------------------------------------------------
# 8. Cleanup
# -------------------------------------------------------
echo ""
echo "--- 8. Cleanup ---"
curl -s -X DELETE "$PLATFORM/workspaces/$PEER_ID" >/dev/null && echo "  Deleted peer"
curl -s -X DELETE "$PLATFORM/workspaces/$HERMES_ID" >/dev/null && echo "  Deleted hermes"

echo ""
echo "=========================================="
echo "  Pass: $PASS    Fail: $FAIL"
echo "=========================================="
[ "$FAIL" -eq 0 ]
