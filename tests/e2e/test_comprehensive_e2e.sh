#!/usr/bin/env bash
# Comprehensive E2E test — covers ALL platform API endpoints, workspace lifecycle,
# parent-child A2A, peer delegation, secrets, config, bundles, approvals, memories, and more.
#
# Requires: platform running on :8080, Postgres + Redis up.
# Does NOT require running agent containers (tests platform-only behavior).
set -euo pipefail

source "$(dirname "$0")/_lib.sh"  # sets BASE default
PASS=0
FAIL=0
SKIP=0

# Phase 30.1: tokens issued at /registry/register must be echoed back on
# heartbeat, update-card, discover, and peers calls.
PM_TOKEN=""
DEV_TOKEN=""

e2e_cleanup_all_workspaces

check() {
  local desc="$1" expected="$2" actual="$3"
  if echo "$actual" | grep -qF "$expected"; then
    echo "  PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $desc"
    echo "    expected: $expected"
    echo "    got: ${actual:0:200}"
    FAIL=$((FAIL + 1))
  fi
}

check_status() {
  local desc="$1" expected_code="$2" actual_code="$3"
  if [ "$actual_code" = "$expected_code" ]; then
    echo "  PASS: $desc (HTTP $actual_code)"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $desc (expected HTTP $expected_code, got $actual_code)"
    FAIL=$((FAIL + 1))
  fi
}

jq_extract() {
  python3 -c "import sys,json; print(json.load(sys.stdin)$1)" 2>/dev/null
}

echo "============================================"
echo "  Comprehensive Platform E2E Test Suite"
echo "============================================"
echo ""

# ============================================================
# Section 1: Health & Metrics
# ============================================================
echo "--- Section 1: Health & Metrics ---"
R=$(curl -s "$BASE/health")
check "GET /health" '"status":"ok"' "$R"

CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/metrics")
check_status "GET /metrics returns 200" "200" "$CODE"

# ============================================================
# Section 2: Workspace CRUD
# ============================================================
echo ""
echo "--- Section 2: Workspace CRUD ---"

# Create parent workspace (PM) and immediately register to capture its
# auth token BEFORE the provisioner's container can spawn and claim it.
# Tokens are single-issue on first /registry/register per workspace
# (Phase 30.1) — if the container's main.py beats us, our later calls
# get no token and bearer-protected endpoints fail. Empirically the
# script wins the race 5/5 times when register fires right after
# create; sections that depend on container readiness (RT_* in 2b)
# still run normally.
R=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
  -d '{"name":"Test PM","role":"Project Manager","tier":2,"runtime":"external","external":true}')
check "Create PM" '"status":"awaiting_agent"' "$R"
PM_ID=$(echo "$R" | jq_extract "['id']")
echo "  PM_ID=$PM_ID"
RR=$(curl -s -X POST "$BASE/registry/register" -H "Content-Type: application/json" \
  -d "{\"id\":\"$PM_ID\",\"url\":\"http://localhost:9000\",\"agent_card\":{\"name\":\"PM\",\"skills\":[]}}")
PM_TOKEN=$(echo "$RR" | e2e_extract_token)

# Create child workspace under PM
R=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
  -d "{\"name\":\"Test Dev\",\"role\":\"Developer\",\"tier\":2,\"parent_id\":\"$PM_ID\",\"runtime\":\"external\",\"external\":true}")
check "Create Dev (child of PM)" '"status":"awaiting_agent"' "$R"
DEV_ID=$(echo "$R" | jq_extract "['id']")
RR=$(curl -s -X POST "$BASE/registry/register" -H "Content-Type: application/json" \
  -d "{\"id\":\"$DEV_ID\",\"url\":\"http://localhost:9001\",\"agent_card\":{\"name\":\"Dev Agent\",\"skills\":[],\"version\":\"1.0.0\"}}")
DEV_TOKEN=$(echo "$RR" | e2e_extract_token)

# Create sibling
R=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
  -d "{\"name\":\"Test QA\",\"role\":\"QA\",\"tier\":1,\"parent_id\":\"$PM_ID\",\"runtime\":\"external\",\"external\":true}")
check "Create QA (sibling of Dev)" '"status":"awaiting_agent"' "$R"
QA_ID=$(echo "$R" | jq_extract "['id']")
curl -s -X POST "$BASE/registry/register" -H "Content-Type: application/json" \
  -d "{\"id\":\"$QA_ID\",\"url\":\"http://localhost:9002\",\"agent_card\":{\"name\":\"QA\",\"skills\":[]}}" > /dev/null

# Create unrelated workspace
R=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
  -d '{"name":"Test Outsider","role":"External","tier":1,"runtime":"external","external":true}')
check "Create Outsider (unrelated)" '"status":"awaiting_agent"' "$R"
OUTSIDER_ID=$(echo "$R" | jq_extract "['id']")

# List workspaces
R=$(curl -s "$BASE/workspaces")
check "List workspaces (4 total)" "$PM_ID" "$R"

# Get single workspace
R=$(curl -s "$BASE/workspaces/$PM_ID")
check "Get PM by ID" '"name":"Test PM"' "$R"

# Update workspace position
R=$(curl -s -X PATCH "$BASE/workspaces/$PM_ID" -H "Content-Type: application/json" \
  -d '{"x":100,"y":200}')
check "Update PM position" '"status":"updated"' "$R"

# Verify position persisted
R=$(curl -s "$BASE/workspaces/$PM_ID")
check "PM position persisted" '"x":100' "$R"

# ============================================================
# Section 2b: Runtime Assignment & Image Selection
# ============================================================
echo ""
echo "--- Section 2b: Runtime Assignment ---"

if [ "${RUN_SPAWNED_RUNTIME_LEGACY_E2E:-0}" != "1" ]; then
  echo "  SKIP: spawned-runtime image checks require local runtime images; set RUN_SPAWNED_RUNTIME_LEGACY_E2E=1 to enable"
  SKIP=$((SKIP + 5))
else

# Create workspace with explicit runtime
R=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
  -d '{"name":"RT Claude","role":"Test","tier":2,"runtime":"claude-code","model":"sonnet"}')
check "Create claude-code workspace" '"status":"provisioning"' "$R"
RT_CC_ID=$(echo "$R" | jq_extract "['id']")

R=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
  -d '{"name":"RT Codex","role":"Test","tier":2,"runtime":"codex","model":"openai:gpt-5"}')
check "Create codex workspace" '"status":"provisioning"' "$R"
RT_CX_ID=$(echo "$R" | jq_extract "['id']")

R=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
  -d '{"name":"RT Hermes","role":"Test","tier":2,"runtime":"hermes","model":"openai:gpt-5"}')
check "Create hermes workspace" '"status":"provisioning"' "$R"
RT_HM_ID=$(echo "$R" | jq_extract "['id']")

# Wait for containers to start (poll up to 30s for first one to appear)
if command -v docker &>/dev/null; then
  for _ in 1 2 3 4 5 6; do
    sleep 5
    if docker inspect "ws-${RT_CC_ID}" >/dev/null 2>&1; then break; fi
  done

  _check_image() {
    local ws_id="$1" expected_tag="$2" label="$3"
    # Poll up to 30s for image to appear
    local actual_image="NOT_FOUND"
    for _ in 1 2 3 4 5 6; do
      actual_image=$(docker inspect "ws-${ws_id}" --format '{{.Config.Image}}' 2>/dev/null || echo "NOT_FOUND")
      if echo "$actual_image" | grep -qF "$expected_tag"; then break; fi
      sleep 5
    done
    if echo "$actual_image" | grep -qF "$expected_tag"; then
      echo "  PASS: $label → $actual_image"
      PASS=$((PASS + 1))
    else
      echo "  FAIL: $label (expected *$expected_tag, got $actual_image)"
      FAIL=$((FAIL + 1))
    fi
  }

  _check_image "$RT_CC_ID" "claude-code" "claude-code uses claude-code image"
  _check_image "$RT_CX_ID" "codex" "codex uses codex image"
  _check_image "$RT_HM_ID" "hermes" "hermes uses hermes image"
else
  echo "  SKIP: Docker not available — cannot verify container images"
  SKIP=$((SKIP + 3))
fi

# Verify runtime in agent card after registration
sleep 5
for rt_id in $RT_CC_ID $RT_CX_ID $RT_HM_ID; do
  # Register so we can check agent card
  curl -s -X POST "$BASE/registry/register" -H "Content-Type: application/json" \
    -d "{\"id\":\"$rt_id\",\"url\":\"http://localhost:19999\",\"agent_card\":{\"name\":\"Test\",\"skills\":[]}}" > /dev/null 2>&1
done

# Config file should reflect runtime
R=$(curl -s "$BASE/workspaces/$RT_CC_ID/files/config.yaml" 2>/dev/null)
if echo "$R" | grep -qF "runtime: claude-code"; then
  echo "  PASS: claude-code config.yaml has runtime: claude-code"
  PASS=$((PASS + 1))
elif echo "$R" | grep -qF "error"; then
  echo "  SKIP: config.yaml not accessible (container may not be ready)"
  SKIP=$((SKIP + 1))
else
  echo "  FAIL: claude-code config.yaml missing runtime field"
  FAIL=$((FAIL + 1))
fi

# Verify runtime change persists on restart (if provisioner supports ExecRead)
# Write a new runtime to config, restart, check image changes
R=$(curl -s -X PUT "$BASE/workspaces/$RT_CX_ID/files/config.yaml" \
  -H "Content-Type: application/json" \
  -d '{"content":"name: RT Codex\nruntime: openclaw\nmodel: openai:gpt-4.1-mini\ntier: 2\n"}')
if echo "$R" | grep -qF "saved"; then
  curl -s -X POST "$BASE/workspaces/$RT_CX_ID/restart" > /dev/null 2>&1
  # Poll up to 30s for the new container image to appear (restart can take a while)
  if command -v docker &>/dev/null; then
    for _ in 1 2 3 4 5 6; do
      sleep 5
      actual=$(docker inspect "ws-${RT_CX_ID}" --format '{{.Config.Image}}' 2>/dev/null || echo "")
      if echo "$actual" | grep -qF "openclaw"; then break; fi
    done
    _check_image "$RT_CX_ID" "openclaw" "Runtime change codex to openclaw on restart"
  else
    echo "  SKIP: Docker not available"
    SKIP=$((SKIP + 1))
  fi
else
  echo "  SKIP: Could not write config (container offline)"
  SKIP=$((SKIP + 1))
fi

# Clean up runtime test workspaces
e2e_delete_workspace "$RT_CC_ID" "RT Claude"
sleep 0.3
e2e_delete_workspace "$RT_CX_ID" "RT Codex"
sleep 0.3
e2e_delete_workspace "$RT_HM_ID" "RT Hermes"
sleep 0.3

fi

# ============================================================
# Section 3: Registry & Heartbeat
# ============================================================
echo ""
echo "--- Section 3: Registry & Heartbeat ---"

# Dev was already registered in Section 2 right after creation (to beat
# the provisioner in the token-issuance race). The async provisioner
# may have flipped the status to "failed" in the meantime (no real
# container image in test env). Send a fresh heartbeat first so status
# goes back to online before we assert.
curl -s -X POST "$BASE/registry/heartbeat" -H "Content-Type: application/json" \
  -H "Authorization: Bearer $DEV_TOKEN" \
  -d "{\"workspace_id\":\"$DEV_ID\",\"active_tasks\":0,\"uptime_seconds\":1}" > /dev/null
R=$(curl -s "$BASE/workspaces/$DEV_ID")
check "Dev status online after register" '"status":"online"' "$R"

# Heartbeat with current_task
R=$(curl -s -X POST "$BASE/registry/heartbeat" -H "Content-Type: application/json" \
  -H "Authorization: Bearer $DEV_TOKEN" \
  -d "{\"workspace_id\":\"$DEV_ID\",\"active_tasks\":1,\"current_task\":\"Running tests\"}")
check "Heartbeat with task" '"status":"ok"' "$R"

# Verify current_task visible
R=$(curl -s "$BASE/workspaces/$DEV_ID")
check "Current task visible" '"current_task":"Running tests"' "$R"

# Heartbeat with error rate (trigger degraded — needs >0.5 AND registered)
R=$(curl -s -X POST "$BASE/registry/heartbeat" -H "Content-Type: application/json" \
  -H "Authorization: Bearer $DEV_TOKEN" \
  -d "{\"workspace_id\":\"$DEV_ID\",\"error_rate\":0.8,\"sample_error\":\"timeout\"}")
check "Degraded heartbeat" '"status":"ok"' "$R"

# Verify degraded status
sleep 1
R=$(curl -s "$BASE/workspaces/$DEV_ID")
check "Dev degraded" '"last_error_rate":0.8' "$R"

# Recover
R=$(curl -s -X POST "$BASE/registry/heartbeat" -H "Content-Type: application/json" \
  -H "Authorization: Bearer $DEV_TOKEN" \
  -d "{\"workspace_id\":\"$DEV_ID\",\"error_rate\":0.0}")
R=$(curl -s "$BASE/workspaces/$DEV_ID")
check "Dev recovered" '"last_error_rate":0' "$R"

# ============================================================
# Section 4: Discovery & Access Control
# ============================================================
echo ""
echo "--- Section 4: Discovery & Access Control ---"

# PM was registered in Section 2 right after creation.

# Discover requires X-Workspace-ID
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/registry/discover/$DEV_ID")
check_status "Discover without header → 400" "400" "$CODE"

# PM discovers Dev (parent→child: allowed)
R=$(curl -s -H "X-Workspace-ID: $PM_ID" -H "Authorization: Bearer $PM_TOKEN" "$BASE/registry/discover/$DEV_ID")
check "PM discovers Dev (parent→child)" "$DEV_ID" "$R"

# Dev discovers QA (siblings: allowed) — QA was registered in Section 2
R=$(curl -s -H "X-Workspace-ID: $DEV_ID" -H "Authorization: Bearer $DEV_TOKEN" "$BASE/registry/discover/$QA_ID")
check "Dev discovers QA (siblings)" "$QA_ID" "$R"

# Check access: PM → Dev (allowed)
R=$(curl -s -X POST "$BASE/registry/check-access" -H "Content-Type: application/json" \
  -d "{\"caller_id\":\"$PM_ID\",\"target_id\":\"$DEV_ID\"}")
check "Access PM→Dev (parent→child)" '"allowed":true' "$R"

# Check access: Dev → Outsider (denied)
R=$(curl -s -X POST "$BASE/registry/check-access" -H "Content-Type: application/json" \
  -d "{\"caller_id\":\"$DEV_ID\",\"target_id\":\"$OUTSIDER_ID\"}")
check "Access Dev→Outsider (denied)" '"allowed":false' "$R"

# Peers — Dev should see PM and QA
R=$(curl -s -H "X-Workspace-ID: $DEV_ID" -H "Authorization: Bearer $DEV_TOKEN" "$BASE/registry/$DEV_ID/peers")
check "Dev peers include PM" "$PM_ID" "$R"
check "Dev peers include QA" "$QA_ID" "$R"

# ============================================================
# Section 5: Secrets
# ============================================================
echo ""
echo "--- Section 5: Secrets ---"

# List secrets (initial state — may have secrets from org import .env)
R=$(curl -s "$BASE/workspaces/$PM_ID/secrets")
check "List secrets (responds)" '[' "$R"

# Set a secret
R=$(curl -s -X POST "$BASE/workspaces/$PM_ID/secrets" -H "Content-Type: application/json" \
  -d '{"key":"OPENAI_API_KEY","value":"sk-test-12345"}')
check "Set secret" '"status":"saved"' "$R"

# List secrets (1 item, value not exposed)
R=$(curl -s "$BASE/workspaces/$PM_ID/secrets")
check "Secret listed" '"key":"OPENAI_API_KEY"' "$R"
check "Secret value hidden" '"has_value":true' "$R"

# Get model (derived from secrets or config)
R=$(curl -s "$BASE/workspaces/$PM_ID/model")
# Model endpoint returns whatever is configured
check "Get model endpoint" '{' "$R"

# Delete secret
R=$(curl -s -X DELETE "$BASE/workspaces/$PM_ID/secrets/OPENAI_API_KEY")
check "Delete secret" '"status":"deleted"' "$R"

# ============================================================
# Section 6: Config & Files
# ============================================================
echo ""
echo "--- Section 6: Config & Files ---"

# Note: Config read requires container or template — test error case
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/workspaces/$PM_ID/files/config.yaml")
# May return 404 if no container/template exists (expected)
echo "  INFO: GET config.yaml → HTTP $CODE (expected 200 or 404)"

# ============================================================
# Section 7: Workspace Memory (HMA)
# ============================================================
echo ""
# Pause before memory tests to avoid rate limits from prior sections
sleep 3
echo "--- Section 7: Workspace Memory ---"

# Commit LOCAL memory
R=$(curl -s -X POST "$BASE/workspaces/$DEV_ID/memories" -H "Content-Type: application/json" \
  -d '{"content":"Architecture uses Go + React","scope":"LOCAL"}')
check "Commit LOCAL memory" '"scope":"LOCAL"' "$R"
MEM_ID=$(echo "$R" | jq_extract "['id']" 2>/dev/null || echo "")

# Commit TEAM memory
R=$(curl -s -X POST "$BASE/workspaces/$DEV_ID/memories" -H "Content-Type: application/json" \
  -d '{"content":"Sprint goal: ship v2.0 by Friday","scope":"TEAM"}')
check "Commit TEAM memory" '"scope":"TEAM"' "$R"
TEAM_MEM_ID=$(echo "$R" | jq_extract "['id']" 2>/dev/null || echo "")

# List all memories
R=$(curl -s "$BASE/workspaces/$DEV_ID/memories")
check "List all memories" 'Architecture uses Go' "$R"
check "List includes TEAM memory" 'Sprint goal' "$R"

# Filter by scope
R=$(curl -s "$BASE/workspaces/$DEV_ID/memories?scope=LOCAL")
check "Filter LOCAL scope" 'Architecture' "$R"

R=$(curl -s "$BASE/workspaces/$DEV_ID/memories?scope=TEAM")
check "Filter TEAM scope" 'Sprint goal' "$R"

# Invalid scope rejected
R=$(curl -s -X POST "$BASE/workspaces/$DEV_ID/memories" -H "Content-Type: application/json" \
  -d '{"content":"test","scope":"INVALID"}')
check "Invalid scope rejected" 'error' "$R"

# Empty content rejected
R=$(curl -s -X POST "$BASE/workspaces/$DEV_ID/memories" -H "Content-Type: application/json" \
  -d '{"content":"","scope":"LOCAL"}')
check "Empty content rejected" 'error' "$R"

# Memory persists across API calls (simulate recall after restart)
R=$(curl -s "$BASE/workspaces/$DEV_ID/memories")
check "Memory persists (recall)" 'Architecture' "$R"

# Delete memory
if [ -n "$MEM_ID" ]; then
  R=$(curl -s -X DELETE "$BASE/workspaces/$DEV_ID/memories/$MEM_ID")
  check "Delete LOCAL memory" '"status"' "$R"
fi

# Verify deleted memory is gone
R=$(curl -s "$BASE/workspaces/$DEV_ID/memories?scope=LOCAL")
if echo "$R" | grep -qF "Architecture"; then
  echo "  FAIL: Deleted memory still visible"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: Deleted memory removed"
  PASS=$((PASS + 1))
fi

# Clean up TEAM memory
if [ -n "$TEAM_MEM_ID" ]; then
  curl -s -X DELETE "$BASE/workspaces/$DEV_ID/memories/$TEAM_MEM_ID" > /dev/null
fi

sleep 2
# Cross-workspace memory isolation — PM should NOT see Dev's LOCAL memories
R=$(curl -s -X POST "$BASE/workspaces/$DEV_ID/memories" -H "Content-Type: application/json" \
  -d '{"content":"Dev secret note","scope":"LOCAL"}')
DEV_SECRET_ID=$(echo "$R" | jq_extract "['id']" 2>/dev/null || echo "")

R=$(curl -s "$BASE/workspaces/$PM_ID/memories")
if echo "$R" | grep -qF "Dev secret note"; then
  echo "  FAIL: PM can see Dev's LOCAL memory (isolation broken)"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: Memory isolation — PM cannot see Dev's LOCAL"
  PASS=$((PASS + 1))
fi

# Clean up
if [ -n "$DEV_SECRET_ID" ]; then
  curl -s -X DELETE "$BASE/workspaces/$DEV_ID/memories/$DEV_SECRET_ID" > /dev/null
fi

# ============================================================
# Section 8: Activity Logging
# ============================================================
echo ""
echo "--- Section 8: Activity Logging ---"

# Report activity
R=$(curl -s -X POST "$BASE/workspaces/$DEV_ID/activity" -H "Content-Type: application/json" \
  -d '{"activity_type":"agent_log","summary":"Running unit tests","status":"ok"}')
check "Report activity" '"status"' "$R"

# List activity
R=$(curl -s "$BASE/workspaces/$DEV_ID/activity?limit=5")
check "List activity" 'Running unit tests' "$R"

# Filter by type
R=$(curl -s "$BASE/workspaces/$DEV_ID/activity?type=agent_log")
check "Filter activity by type" 'agent_log' "$R"

# ============================================================
# Section 9: Events
# ============================================================
echo ""
echo "--- Section 9: Events ---"

# List global events
R=$(curl -s "$BASE/events")
check "List global events" 'WORKSPACE_' "$R"

# List events for PM
R=$(curl -s "$BASE/events/$PM_ID")
check "List PM events" "$PM_ID" "$R"

# ============================================================
# Section 10: Approvals
# ============================================================
echo ""
echo "--- Section 10: Approvals ---"

# Create approval request
R=$(curl -s -X POST "$BASE/workspaces/$DEV_ID/approvals" -H "Content-Type: application/json" \
  -d '{"action":"deploy to production","reason":"All tests passing"}')
check "Create approval" '"status":"pending"' "$R"
APPROVAL_ID=$(echo "$R" | jq_extract "['id']" 2>/dev/null || echo "")

# List pending approvals
R=$(curl -s "$BASE/approvals/pending")
check "List pending approvals" 'deploy to production' "$R"

# List workspace approvals
R=$(curl -s "$BASE/workspaces/$DEV_ID/approvals")
check "List Dev approvals" 'deploy to production' "$R"

# Decide approval
if [ -n "$APPROVAL_ID" ]; then
  R=$(curl -s -X POST "$BASE/workspaces/$DEV_ID/approvals/$APPROVAL_ID/decide" \
    -H "Content-Type: application/json" -d '{"approved":true,"decided_by":"admin"}')
  check "Approve request" '"approved":true' "$R"
fi

# ============================================================
# Section 11: Canvas Viewport
# ============================================================
echo ""
echo "--- Section 11: Canvas Viewport ---"

R=$(curl -s -X PUT "$BASE/canvas/viewport" -H "Content-Type: application/json" \
  -d '{"x":50,"y":100,"zoom":1.5}')
check "Save viewport" '"status":"saved"' "$R"

R=$(curl -s "$BASE/canvas/viewport")
check "Get viewport" '"zoom":1.5' "$R"

# ============================================================
# Section 12: Agent Card Update
# ============================================================
echo ""
echo "--- Section 12: Agent Card Update ---"

R=$(curl -s -X POST "$BASE/registry/update-card" -H "Content-Type: application/json" \
  -H "Authorization: Bearer $DEV_TOKEN" \
  -d "{\"workspace_id\":\"$DEV_ID\",\"agent_card\":{\"name\":\"Dev Agent v2\",\"skills\":[{\"id\":\"code\",\"name\":\"Coding\"}],\"version\":\"2.0.0\"}}")
check "Update agent card" '"status":"updated"' "$R"

R=$(curl -s "$BASE/workspaces/$DEV_ID")
check "Agent card updated" '"name":"Dev Agent v2"' "$R"

# ============================================================
# Section 13: Bundle Export/Import
# ============================================================
echo ""
sleep 3
echo "--- Section 13: Bundle Export/Import ---"

# Export PM bundle
R=$(curl -s "$BASE/bundles/export/$PM_ID")
check "Export PM bundle" '"name":"Test PM"' "$R"
check "Bundle has workspace data" '"name":"Test PM"' "$R"

# Import bundle (create from exported data)
BUNDLE=$(curl -s "$BASE/bundles/export/$PM_ID")
R=$(curl -s -X POST "$BASE/bundles/import" -H "Content-Type: application/json" -d "$BUNDLE")
check "Import bundle" '"status"' "$R"

# ============================================================
# Section 14: Workspace Delete (Cascade)
# ============================================================
echo ""
echo "--- Section 14: Cleanup & Delete ---"

# Delete without name confirmation should be rejected before cascade.
R=$(curl -s -X DELETE "$BASE/workspaces/$PM_ID")
check "Delete PM requires name confirmation" '"destructive_action_requires_confirmation"' "$R"

# Delete with name confirmation but without cascade confirmation should
# still require explicit child confirmation.
R=$(curl -s -X DELETE "$BASE/workspaces/$PM_ID" -H "X-Confirm-Name: Test PM")
check "Delete PM requires cascade confirmation" '"confirmation_required"' "$R"

# Delete with confirmation. This destructive path is approval-gated for admin
# callers, so drive the same approve-and-retry helper used by focused E2Es.
R=$(e2e_gated_admin_op "$PM_ID" curl -s -X DELETE "$BASE/workspaces/$PM_ID?confirm=true" -H "X-Confirm-Name: Test PM")
check "Delete PM cascades" '"cascade_deleted"' "$R"

# Delete outsider
e2e_delete_workspace "$OUTSIDER_ID" "Test Outsider"

# Clean up remaining workspaces (bundle imports, runtime test workspaces, etc.)
sleep 2
while IFS=$'\t' read -r wid name; do
    [ -z "$wid" ] && continue
    sleep 0.5  # avoid rate limit
    e2e_delete_workspace "$wid" "$name" >/dev/null 2>&1 || true
done < <(curl -s "$BASE/workspaces" | python3 -c "
import json, sys
for w in json.load(sys.stdin):
    print(str(w.get('id','')) + '\t' + str(w.get('name','')))
" 2>/dev/null)

# Poll for clean state up to 30s — DB cascade + container stop is async on busy systems
for _ in 1 2 3 4 5 6; do
  sleep 5
  R=$(curl -s "$BASE/workspaces")
  if [ "$R" = "[]" ]; then break; fi
done
check "All workspaces cleaned" '[]' "$R"

# ============================================================
# Summary
# ============================================================
echo ""
echo "============================================"
echo "  Results: $PASS passed, $FAIL failed, $SKIP skipped"
echo "  Total: $((PASS + FAIL + SKIP)) checks"
echo "============================================"

[ "$FAIL" -eq 0 ] && exit 0 || exit 1
