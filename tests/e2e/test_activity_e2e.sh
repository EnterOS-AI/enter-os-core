#!/usr/bin/env bash
# E2E tests for activity logging, A2A communication tracking, and current task visibility.
# Requires: platform running on localhost:8080 with at least one online agent.
set -euo pipefail

source "$(dirname "$0")/_lib.sh"  # sets BASE default
PASS=0
FAIL=0
TIMEOUT="${A2A_TIMEOUT:-120}"

# Phase 30.1: heartbeats require a bearer token. Re-register the
# detected online agent to obtain one for our test-harness heartbeats.
AGENT_TOKEN=""

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

check_not() {
  local desc="$1"
  local unexpected="$2"
  local actual="$3"
  if echo "$actual" | grep -qF -- "$unexpected"; then
    echo "FAIL: $desc"
    echo "  should NOT contain: $unexpected"
    FAIL=$((FAIL + 1))
  else
    echo "PASS: $desc"
    PASS=$((PASS + 1))
  fi
}

echo "=== Activity Logging E2E Tests ==="
echo ""

# --- Setup: find an online agent ---
AGENT_ID=$(curl -s "$BASE/workspaces" | python3 -c "
import sys, json
ws = json.load(sys.stdin)
for w in ws:
    if w['status'] == 'online':
        print(w['id']); break
else:
    print('')
")

if [ -z "$AGENT_ID" ]; then
  echo "SKIP: No online agent found. Start an agent first."
  echo "=== Results: 0 passed, 0 failed (skipped) ==="
  exit 0
fi

AGENT_NAME=$(curl -s "$BASE/workspaces/$AGENT_ID" | python3 -c "import sys,json; print(json.load(sys.stdin)['name'])")
AGENT_URL=$(curl -s "$BASE/workspaces/$AGENT_ID" | python3 -c "import sys,json; print(json.load(sys.stdin).get('url') or 'http://localhost:9999')")
echo "Using agent: $AGENT_NAME ($AGENT_ID)"
echo ""

# Re-register to capture a bearer token for heartbeat tests (Phase 30.1).
# Re-registration is idempotent; the agent's own token continues to work
# alongside this one.
RREG=$(curl -s -X POST "$BASE/registry/register" -H "Content-Type: application/json" \
  -d "{\"id\":\"$AGENT_ID\",\"url\":\"$AGENT_URL\",\"agent_card\":{\"name\":\"$AGENT_NAME\",\"skills\":[]}}")
AGENT_TOKEN=$(echo "$RREG" | e2e_extract_token)

# ---------- A2A Communication Logging ----------
echo "--- A2A Communication Logging ---"

# Test 1: Send A2A message and verify activity is logged
R=$(curl -s --max-time "$TIMEOUT" -X POST "$BASE/workspaces/$AGENT_ID/a2a" \
  -H "Content-Type: application/json" \
  -d '{
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "parts": [{"type": "text", "text": "Say hello in one word"}]
      }
    }
  }')
check "A2A message/send returns response" 'result' "$R"

# Test 2: Activity log should have a new a2a_receive entry
# Retry up to 3s for the async LogActivity goroutine to complete
AFTER_COUNT=0
for _ in 1 2 3 4 5 6; do
  R=$(curl -s "$BASE/workspaces/$AGENT_ID/activity?type=a2a_receive")
  AFTER_COUNT=$(echo "$R" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
  [ "$AFTER_COUNT" -gt "0" ] && break
  sleep 0.5
done
if [ "$AFTER_COUNT" -gt "0" ]; then
  echo "PASS: A2A activity log created (count=$AFTER_COUNT)"
  PASS=$((PASS + 1))
else
  echo "FAIL: Expected at least 1 a2a_receive activity, got $AFTER_COUNT"
  FAIL=$((FAIL + 1))
fi

# Test 3: Activity log contains method and duration
check "Activity log has method" 'message/send' "$R"
check "Activity log has duration_ms" 'duration_ms' "$R"
check "Activity log has status ok" '"status":"ok"' "$R"

# Test 4: Activity log contains request and response bodies
FIRST_ID=$(echo "$R" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d[0]['id'] if d else '')")
if [ -n "$FIRST_ID" ]; then
  check "Activity has request_body" 'request_body' "$R"
  check "Activity has response_body" 'response_body' "$R"
  echo "PASS: Activity entry ID: $FIRST_ID"
  PASS=$((PASS + 1))
fi

# Test 5: Activity log has target_id pointing to this workspace
check "Activity target_id is workspace" "$AGENT_ID" "$R"

# ---------- Agent Self-Reported Activity ----------
echo ""
echo "--- Agent Self-Reported Activity ---"

# Test 6: Agent reports a task_update
R=$(curl -s -X POST "$BASE/workspaces/$AGENT_ID/activity" -H "Content-Type: application/json" \
  -d '{"activity_type":"task_update","method":"start","summary":"Started document analysis","duration_ms":0}')
check "Agent reports task_update" '"status":"logged"' "$R"

# Test 7: Agent reports an error
R=$(curl -s -X POST "$BASE/workspaces/$AGENT_ID/activity" -H "Content-Type: application/json" \
  -d '{"activity_type":"error","summary":"Failed to parse input","status":"error","error_detail":"JSON decode error at line 42"}')
check "Agent reports error" '"status":"logged"' "$R"

# Test 8: Agent reports generic log
R=$(curl -s -X POST "$BASE/workspaces/$AGENT_ID/activity" -H "Content-Type: application/json" \
  -d '{"activity_type":"agent_log","method":"inference","summary":"Generated response using gpt-4","duration_ms":2500,"metadata":{"model":"gpt-4","tokens":1500}}')
check "Agent reports agent_log with metadata" '"status":"logged"' "$R"

# ---------- Activity Filtering ----------
echo ""
echo "--- Activity Filtering ---"

# Test 9: Filter by error type
R=$(curl -s "$BASE/workspaces/$AGENT_ID/activity?type=error")
check "Filter error activities" 'JSON decode error' "$R"
check "Error has error_detail" 'error_detail' "$R"

# Test 10: Filter by task_update
R=$(curl -s "$BASE/workspaces/$AGENT_ID/activity?type=task_update")
check "Filter task_update activities" 'document analysis' "$R"

# Test 11: Filter by agent_log
R=$(curl -s "$BASE/workspaces/$AGENT_ID/activity?type=agent_log")
check "Filter agent_log activities" 'inference' "$R"

# Test 12: Total count includes all types
R=$(curl -s "$BASE/workspaces/$AGENT_ID/activity")
TOTAL=$(echo "$R" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
if [ "$TOTAL" -ge "4" ]; then
  echo "PASS: Total activities >= 4 (got $TOTAL)"
  PASS=$((PASS + 1))
else
  echo "FAIL: Expected >= 4 total activities, got $TOTAL"
  FAIL=$((FAIL + 1))
fi

# Test 13: Limit parameter works
R=$(curl -s "$BASE/workspaces/$AGENT_ID/activity?limit=2")
LIMITED=$(echo "$R" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
check "Limit=2 returns at most 2" "2" "$LIMITED"

# ---------- Current Task Visibility ----------
echo ""
echo "--- Current Task Visibility ---"

# Test 14: Set current_task via heartbeat
R=$(curl -s -X POST "$BASE/registry/heartbeat" -H "Content-Type: application/json" -H "Authorization: Bearer $AGENT_TOKEN" \
  -d "{\"workspace_id\":\"$AGENT_ID\",\"error_rate\":0.0,\"sample_error\":\"\",\"active_tasks\":2,\"uptime_seconds\":600,\"current_task\":\"Analyzing quarterly report\"}")
check "Heartbeat with current_task" '"status":"ok"' "$R"

# Test 15: current_task visible in GET /workspaces/:id
R=$(curl -s "$BASE/workspaces/$AGENT_ID")
check "current_task in workspace detail" '"current_task":"Analyzing quarterly report"' "$R"

# Test 16: current_task visible in GET /workspaces list
R=$(curl -s "$BASE/workspaces")
check "current_task in workspace list" 'Analyzing quarterly report' "$R"

# Test 17: Update current_task to new value
R=$(curl -s -X POST "$BASE/registry/heartbeat" -H "Content-Type: application/json" -H "Authorization: Bearer $AGENT_TOKEN" \
  -d "{\"workspace_id\":\"$AGENT_ID\",\"error_rate\":0.0,\"sample_error\":\"\",\"active_tasks\":1,\"uptime_seconds\":700,\"current_task\":\"Generating summary\"}")
check "Heartbeat update task" '"status":"ok"' "$R"

R=$(curl -s "$BASE/workspaces/$AGENT_ID")
check "current_task updated" '"current_task":"Generating summary"' "$R"
check_not "old task cleared" 'quarterly report' "$(curl -s "$BASE/workspaces/$AGENT_ID" | python3 -c "import sys,json; print(json.load(sys.stdin)['current_task'])")"

# Test 18: Clear current_task
R=$(curl -s -X POST "$BASE/registry/heartbeat" -H "Content-Type: application/json" -H "Authorization: Bearer $AGENT_TOKEN" \
  -d "{\"workspace_id\":\"$AGENT_ID\",\"error_rate\":0.0,\"sample_error\":\"\",\"active_tasks\":0,\"uptime_seconds\":800,\"current_task\":\"\"}")
check "Heartbeat clear task" '"status":"ok"' "$R"

R=$(curl -s "$BASE/workspaces/$AGENT_ID")
check "current_task is empty" '"current_task":""' "$R"

# ---------- Cross-Workspace Activity Isolation ----------
echo ""
echo "--- Activity Isolation ---"

# Test 19: Create a second workspace to verify isolation
R=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" -d '{"name":"Activity Test Workspace","tier":1,"runtime":"external","external":true}')
TEMP_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

# Test 20: New workspace has empty activity
R=$(curl -s "$BASE/workspaces/$TEMP_ID/activity")
check "New workspace has no activity" '[]' "$R"

# Test 21: Report activity to temp workspace
curl -s -X POST "$BASE/workspaces/$TEMP_ID/activity" -H "Content-Type: application/json" \
  -d '{"activity_type":"agent_log","summary":"Temp workspace log"}' > /dev/null

# Test 22: Activity does NOT leak to agent workspace
R=$(curl -s "$BASE/workspaces/$AGENT_ID/activity?type=agent_log")
check_not "No cross-workspace leak" 'Temp workspace log' "$R"

# Test 23: Activity shows in correct workspace
R=$(curl -s "$BASE/workspaces/$TEMP_ID/activity")
check "Activity in correct workspace" 'Temp workspace log' "$R"

# Cleanup
e2e_delete_workspace "$TEMP_ID" "Activity Test Workspace"

# ---------- Edge Cases ----------
echo ""
echo "--- Edge Cases ---"

# Test 24: Activity on non-existent workspace returns empty
R=$(curl -s "$BASE/workspaces/00000000-0000-0000-0000-000000000000/activity")
check "Activity on missing workspace returns empty" '[]' "$R"

# Test 25: Report requires activity_type
R=$(curl -s -X POST "$BASE/workspaces/$AGENT_ID/activity" -H "Content-Type: application/json" \
  -d '{"summary":"missing type"}')
check "Missing activity_type → 400" 'ActivityType' "$R"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
exit $FAIL
