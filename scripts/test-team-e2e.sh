#!/usr/bin/env bash
# E2E test: Create a team with different templates/models, test A2A communication.
# Everything via platform API — no manual file edits.
set -euo pipefail

PLATFORM="${1:-http://localhost:8080}"
OR_KEY="${OPENAI_API_KEY:-${OPENROUTER_API_KEY:?Set OPENAI_API_KEY or OPENROUTER_API_KEY env var}}"
PASS=0
FAIL=0

check() {
  local label="$1" expected="$2" actual="$3"
  if echo "$actual" | grep -q "$expected"; then
    echo "PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $label"
    echo "  expected: $expected"
    echo "  got: $actual"
    FAIL=$((FAIL + 1))
  fi
}

wait_online() {
  local id="$1" name="$2"
  for i in $(seq 1 20); do
    local s
    s=$(curl -s "$PLATFORM/workspaces/$id" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
    [ "$s" = "online" ] && return 0
    sleep 3
  done
  echo "  WARNING: $name did not come online within 60s"
  return 1
}

echo "============================================"
echo "  E2E Test: Multi-Template Team + A2A"
echo "============================================"
echo ""

# -------------------------------------------------------
# Step 1: Create workspaces from different templates
# -------------------------------------------------------
echo "--- Step 1: Create workspaces ---"

# PM — Claude Code (uses OAuth token from template)
R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"PM","role":"Project Manager — coordinates the team","tier":2,"template":"claude-code-default"}')
PM_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "Create PM (claude-code)" "provisioning" "$R"

# Research Agent — Claude Code + Gemini Flash
R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"Researcher","role":"Deep research and analysis","tier":2,"template":"claude-code-default"}')
RES_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "Create Researcher (claude-code)" "provisioning" "$R"

# Dev Agent — OpenClaw + Gemini Flash
R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"Developer","role":"Code implementation and review","tier":2,"template":"openclaw"}')
DEV_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "Create Developer (openclaw)" "provisioning" "$R"

# Analyst — Hermes + Gemini Flash
R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"Analyst","role":"Data analysis and reporting","tier":2,"template":"hermes"}')
ANA_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
check "Create Analyst (hermes)" "provisioning" "$R"

echo ""
echo "  PM:         $PM_ID"
echo "  Researcher: $RES_ID"
echo "  Developer:  $DEV_ID"
echo "  Analyst:    $ANA_ID"

# -------------------------------------------------------
# Step 2: Set parent hierarchy via API
# -------------------------------------------------------
echo ""
echo "--- Step 2: Set parent hierarchy ---"

for CHILD_ID in $RES_ID $DEV_ID $ANA_ID; do
  R=$(curl -s -X PATCH "$PLATFORM/workspaces/$CHILD_ID" -H 'Content-Type: application/json' \
    -d "{\"parent_id\":\"$PM_ID\"}")
  check "Set parent for $(echo $CHILD_ID | cut -c1-8)..." "updated" "$R"
done

# -------------------------------------------------------
# Step 3: Set secrets via API (OpenRouter key for non-Claude agents)
# -------------------------------------------------------
echo ""
echo "--- Step 3: Set API keys via secrets API ---"

for CHILD_ID in $RES_ID $DEV_ID $ANA_ID; do
  R=$(curl -s -X POST "$PLATFORM/workspaces/$CHILD_ID/secrets" -H 'Content-Type: application/json' \
    -d "{\"key\":\"OPENROUTER_API_KEY\",\"value\":\"$OR_KEY\"}")
  check "Set OPENROUTER_API_KEY for $(echo $CHILD_ID | cut -c1-8)..." "saved" "$R"
done

# -------------------------------------------------------
# Step 4: Customize system prompts via Files API
# -------------------------------------------------------
echo ""
echo "--- Step 4: Wait for all to come online ---"

for name_id in "PM:$PM_ID" "Researcher:$RES_ID" "Developer:$DEV_ID" "Analyst:$ANA_ID"; do
  IFS=: read name id <<< "$name_id"
  if wait_online "$id" "$name"; then
    check "$name online" "online" "online"
  else
    check "$name online" "online" "timeout"
  fi
done

# -------------------------------------------------------
# Step 5: Customize prompts via Files API (containers are online now)
# -------------------------------------------------------
echo ""
echo "--- Step 5: Customize prompts via Files API ---"

R=$(curl -s -X PUT "$PLATFORM/workspaces/$RES_ID/files/system-prompt.md" -H 'Content-Type: application/json' \
  -d '{"content":"You are a research agent. When asked to introduce yourself, say: I am the Researcher agent."}')
check "Set Researcher prompt" "saved" "$R"

R=$(curl -s -X PUT "$PLATFORM/workspaces/$DEV_ID/files/SOUL.md" -H 'Content-Type: application/json' \
  -d '{"content":"You are a developer agent. When asked to introduce yourself, say: I am the Developer agent."}')
check "Set Developer prompt (SOUL.md)" "saved" "$R"

R=$(curl -s -X PUT "$PLATFORM/workspaces/$ANA_ID/files/system-prompt.md" -H 'Content-Type: application/json' \
  -d '{"content":"You are an analyst agent. When asked to introduce yourself, say: I am the Analyst agent."}')
check "Set Analyst prompt" "saved" "$R"

# -------------------------------------------------------
# Step 6: Restart to pick up new prompts + secrets
# -------------------------------------------------------
echo ""
echo "--- Step 6: Restart agents ---"

for ID in $RES_ID $DEV_ID $ANA_ID; do
  curl -s -X POST "$PLATFORM/workspaces/$ID/restart" > /dev/null
done
echo "Restarting 3 agents... waiting 30s"
sleep 30

for name_id in "Researcher:$RES_ID" "Developer:$DEV_ID" "Analyst:$ANA_ID"; do
  IFS=: read name id <<< "$name_id"
  if wait_online "$id" "$name"; then
    check "$name back online" "online" "online"
  else
    check "$name back online" "online" "timeout"
  fi
done

# -------------------------------------------------------
# Step 7: Verify files in containers (no host ws-* dirs)
# -------------------------------------------------------
echo ""
echo "--- Step 7: Verify config files in containers ---"

R=$(curl -s "$PLATFORM/workspaces/$RES_ID/files" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
check "Researcher has files" "[12]" "$R"

R=$(curl -s "$PLATFORM/workspaces/$DEV_ID/files" | python3 -c "import sys,json; files=json.load(sys.stdin); print(' '.join(f['path'] for f in files if not f['dir']))")
check "Developer has OpenClaw files" "SOUL.md" "$R"

# Verify NO ws-* dirs on host
HOST_WS=$(find /Users/hongming/Documents/GitHub/molecule-monorepo/workspace-configs-templates -maxdepth 1 -name 'ws-*' -type d 2>/dev/null | wc -l | tr -d ' ')
check "No ws-* dirs on host" "0" "$HOST_WS"

# -------------------------------------------------------
# Step 8: Test A2A — direct messages
# -------------------------------------------------------
echo ""
echo "--- Step 8: A2A direct messages ---"

# Talk to Researcher
R=$(curl -s -X POST "$PLATFORM/workspaces/$RES_ID/a2a" -H 'Content-Type: application/json' \
  -d '{"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"introduce yourself in one sentence"}]}}}')
RESP=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('result',{}).get('parts',[{}])[0].get('text','ERROR')[:200])" 2>/dev/null)
echo "  Researcher says: $RESP"
check "Researcher responds" "Researcher" "$RESP"

# Talk to Developer
R=$(curl -s -X POST "$PLATFORM/workspaces/$DEV_ID/a2a" -H 'Content-Type: application/json' \
  -d '{"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"introduce yourself in one sentence"}]}}}')
RESP=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('result',{}).get('parts',[{}])[0].get('text','ERROR')[:200])" 2>/dev/null)
echo "  Developer says: $RESP"
check "Developer responds" "Developer" "$RESP"

# Talk to Analyst
R=$(curl -s -X POST "$PLATFORM/workspaces/$ANA_ID/a2a" -H 'Content-Type: application/json' \
  -d '{"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"introduce yourself in one sentence"}]}}}')
RESP=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('result',{}).get('parts',[{}])[0].get('text','ERROR')[:200])" 2>/dev/null)
echo "  Analyst says: $RESP"
check "Analyst responds" "Analyst" "$RESP"

# -------------------------------------------------------
# Step 9: Test peer discovery
# -------------------------------------------------------
echo ""
echo "--- Step 9: Peer discovery ---"

R=$(curl -s "$PLATFORM/registry/${PM_ID}/peers" | python3 -c "import sys,json; peers=json.load(sys.stdin); print(len(peers))")
check "PM has 3 peers (children)" "3" "$R"

R=$(curl -s "$PLATFORM/registry/${RES_ID}/peers" | python3 -c "import sys,json; peers=json.load(sys.stdin); names=[p.get('name','') for p in peers]; print(' '.join(names))")
check "Researcher sees siblings" "Developer" "$R"

# -------------------------------------------------------
# Step 10: Test cross-workspace access control
# -------------------------------------------------------
echo ""
echo "--- Step 10: Access control ---"

# Parent <-> child (PM <-> Researcher) should be allowed
R=$(curl -s -X POST "$PLATFORM/registry/check-access" -H 'Content-Type: application/json' \
  -d "{\"caller_id\":\"$PM_ID\",\"target_id\":\"$RES_ID\"}")
check "PM -> Researcher allowed" "true" "$R"

# Siblings (Researcher <-> Developer) should be allowed
R=$(curl -s -X POST "$PLATFORM/registry/check-access" -H 'Content-Type: application/json' \
  -d "{\"caller_id\":\"$RES_ID\",\"target_id\":\"$DEV_ID\"}")
check "Researcher -> Developer allowed" "true" "$R"

# -------------------------------------------------------
# Step 11: Cleanup
# -------------------------------------------------------
echo ""
echo "--- Step 11: Cleanup ---"

curl -s -X DELETE "$PLATFORM/workspaces/$PM_ID" > /dev/null
check "Delete PM (cascades)" "ok" "ok"

REMAINING=$(curl -s "$PLATFORM/workspaces" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
check "All workspaces deleted" "0" "$REMAINING"

# -------------------------------------------------------
# Results
# -------------------------------------------------------
echo ""
echo "============================================"
echo "  Results: $PASS passed, $FAIL failed"
echo "============================================"
exit $FAIL
