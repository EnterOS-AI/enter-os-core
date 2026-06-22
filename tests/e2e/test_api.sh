#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/_lib.sh"  # sets BASE default
PASS=0
FAIL=0

# Phase 30.1: tokens issued on first /registry/register must be echoed
# back on every subsequent /registry/heartbeat + /registry/update-card
# as `Authorization: Bearer <token>`. Capture them here.
ECHO_TOKEN=""
SUM_TOKEN=""
ECHO_AUTH=()
SUM_AUTH=()
ECHO_URL="https://example.com/echo-agent"
SUM_URL="https://example.com/summarizer-agent"

# AdminAuth-gated calls (GET/POST/DELETE /workspaces, /events, /bundles)
# require the platform admin bearer once ADMIN_TOKEN is set on the server.
# Tier-2b (wsauth_middleware.go:250) REJECTS workspace bearer tokens on admin
# routes when ADMIN_TOKEN is set, so admin calls MUST send the exact ADMIN_TOKEN
# value — which the e2e-api CI job exports here as MOLECULE_ADMIN_TOKEN. acurl =
# "admin curl": it always sends the platform admin bearer (if one is set).
#
# Guarded if-set: a fresh self-hosted/dev platform with no ADMIN_TOKEN fail-opens
# (devmode.go:50), so sending no bearer still works there.
ADMIN_BEARER="${MOLECULE_ADMIN_TOKEN:-${ADMIN_TOKEN:-}}"
ADMIN_AUTH=()
[ -n "$ADMIN_BEARER" ] && ADMIN_AUTH=(-H "Authorization: Bearer $ADMIN_BEARER")
acurl() {
  curl -s ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} "$@"
}

# WORKSPACE_TOKEN holds a per-workspace bearer for the WorkspaceAuth-gated
# routes (PATCH /workspaces/:id, /activity, …). It is set after the first
# create+mint and is NOT interchangeable with the admin bearer.
WORKSPACE_TOKEN=""

# Pre-test cleanup: remove any workspaces left over from prior runs so
# count-based assertions ("empty", "count=2") are reproducible.
e2e_cleanup_all_workspaces

check() {
  local desc="$1"
  local expected="$2"
  local actual="$3"
  if echo "$actual" | grep -qF "$expected"; then
    echo "PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $desc"
    echo "  expected to contain: $expected"
    echo "  got: $actual"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== API Integration Tests ==="
echo ""

# Test 1: Health
R=$(curl -s "$BASE/health")
check "GET /health" '"status":"ok"' "$R"

# Test 2: Empty list
R=$(acurl "$BASE/workspaces")
check "GET /workspaces (empty)" '[]' "$R"

# Test 3: Create workspace A. POST /workspaces is AdminAuth-gated (router.go:166);
# send the admin bearer (acurl). On a fail-open dev platform acurl sends nothing
# and the create still works.
R=$(acurl -X POST "$BASE/workspaces" -H "Content-Type: application/json" -d '{"name":"Echo Agent","tier":1,"runtime":"external","external":true}')
check "POST /workspaces (create echo)" '"status":"awaiting_agent"' "$R"
ECHO_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

# Per-workspace token for Echo, for the WorkspaceAuth-gated routes below.
WORKSPACE_TOKEN=$(echo "$R" | e2e_extract_token)
if [ -z "$WORKSPACE_TOKEN" ]; then
  WORKSPACE_TOKEN=$(e2e_mint_workspace_token "$ECHO_ID" 2>/dev/null || echo "")
fi
if [ -n "$WORKSPACE_TOKEN" ]; then
  echo "  (acquired Echo workspace token: ${WORKSPACE_TOKEN:0:8}...)"
else
  echo "  WARNING: no Echo workspace token acquired — WorkspaceAuth calls will fail"
fi

# Test 4: Create workspace B (needs bearer — tokens now exist in DB)
# #1953 cross-tenant isolation: Summarizer is created as a CHILD of Echo so the
# two live in the SAME org (Echo is the org root; Summarizer hangs off it via
# parent_id). The peer-discovery tests below assert same-org peer enumeration
# (Echo sees its child, the child sees its parent). Previously both were created
# parent_id=NULL — two DISTINCT org roots — and "peers" only listed each other
# via the `WHERE parent_id IS NULL` branch that returned every tenant's org root.
# That branch WAS the cross-tenant leak (#1953) and is now removed, so two org
# roots no longer see each other; the assertions must run inside one org.
R=$(acurl -X POST "$BASE/workspaces" -H "Content-Type: application/json" -d "{\"name\":\"Summarizer Agent\",\"tier\":1,\"runtime\":\"external\",\"external\":true,\"parent_id\":\"$ECHO_ID\"}")
check "POST /workspaces (create summarizer)" '"status":"awaiting_agent"' "$R"
SUM_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

# Test 5: List has 2
R=$(acurl "$BASE/workspaces")
COUNT=$(echo "$R" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
check "GET /workspaces (count=2)" "2" "$COUNT"

# Test 6: Get single
R=$(acurl "$BASE/workspaces/$ECHO_ID")
check "GET /workspaces/:id" '"name":"Echo Agent"' "$R"
check "GET /workspaces/:id (agent_card null)" '"agent_card":null' "$R"

# Test 7: Register echo — use workspace-specific token (from real admin
# endpoint), not the admin token. C18 requires a token issued TO THIS
# workspace, not just any valid token.
ECHO_WS_TOKEN="$WORKSPACE_TOKEN"
[ -n "$ECHO_WS_TOKEN" ] && ECHO_AUTH=(-H "Authorization: Bearer $ECHO_WS_TOKEN")
R=$(curl -s -X POST "$BASE/registry/register" -H "Content-Type: application/json" \
  "${ECHO_AUTH[@]}" \
  -d "{\"id\":\"$ECHO_ID\",\"url\":\"$ECHO_URL\",\"agent_card\":{\"name\":\"Echo Agent\",\"skills\":[{\"id\":\"echo\",\"name\":\"Echo\"}]}}")
check "POST /registry/register (echo)" '"status":"registered"' "$R"
# Extract token from register response; fall back to the workspace token we
# already minted (register may not return a new token on re-registration).
ECHO_TOKEN=$(echo "$R" | e2e_extract_token)
if [ -z "$ECHO_TOKEN" ]; then ECHO_TOKEN="$ECHO_WS_TOKEN"; fi

# Test 8: Register summarizer — same pattern: workspace-specific token
SUM_WS_TOKEN=$(echo "$R" | e2e_extract_token)
if [ -z "$SUM_WS_TOKEN" ]; then
  SUM_WS_TOKEN=$(e2e_mint_workspace_token "$SUM_ID" 2>/dev/null || echo "")
fi
[ -n "$SUM_WS_TOKEN" ] && SUM_AUTH=(-H "Authorization: Bearer $SUM_WS_TOKEN")
R=$(curl -s -X POST "$BASE/registry/register" -H "Content-Type: application/json" \
  "${SUM_AUTH[@]}" \
  -d "{\"id\":\"$SUM_ID\",\"url\":\"$SUM_URL\",\"agent_card\":{\"name\":\"Summarizer\",\"skills\":[{\"id\":\"summarize\",\"name\":\"Summarize\"}]}}")
check "POST /registry/register (summarizer)" '"status":"registered"' "$R"
SUM_TOKEN=$(echo "$R" | e2e_extract_token)
if [ -z "$SUM_TOKEN" ]; then SUM_TOKEN="$SUM_WS_TOKEN"; fi

# Test 9: Both online
R=$(acurl "$BASE/workspaces/$ECHO_ID")
check "Echo is online" '"status":"online"' "$R"
check "Echo has agent_card" '"skills"' "$R"
check "Echo has url" "\"url\":\"$ECHO_URL\"" "$R"

# Test 10: Heartbeat
R=$(curl -s -X POST "$BASE/registry/heartbeat" -H "Content-Type: application/json" -H "Authorization: Bearer $ECHO_TOKEN" \
  -d "{\"workspace_id\":\"$ECHO_ID\",\"error_rate\":0.0,\"sample_error\":\"\",\"active_tasks\":2,\"uptime_seconds\":120}")
check "POST /registry/heartbeat" '"status":"ok"' "$R"

R=$(acurl "$BASE/workspaces/$ECHO_ID")
check "Heartbeat updated active_tasks" '"active_tasks":2' "$R"
check "Heartbeat updated uptime" '"uptime_seconds":120' "$R"

# Test 11: Discover without X-Workspace-ID — Phase 30.6 requires it
R=$(curl -s "$BASE/registry/discover/$ECHO_ID")
check "GET /registry/discover/:id (missing caller rejected)" 'X-Workspace-ID header is required' "$R"

# Test 12: Discover (from same-org child — allowed)
R=$(curl -s "$BASE/registry/discover/$ECHO_ID" -H "X-Workspace-ID: $SUM_ID" -H "Authorization: Bearer $SUM_TOKEN")
check "GET /registry/discover/:id (same-org)" '"url"' "$R"

# Test 13: Peers — same-org parent/child see each other (#1953). Echo is the org
# root and lists its child Summarizer; Summarizer lists its parent Echo. A
# cross-org workspace would NOT appear here (see cross_tenant_isolation_test.go).
R=$(curl -s "$BASE/registry/$ECHO_ID/peers" -H "Authorization: Bearer $ECHO_TOKEN")
check "GET /registry/:id/peers (has summarizer)" '"Summarizer' "$R"

R=$(curl -s "$BASE/registry/$SUM_ID/peers" -H "Authorization: Bearer $SUM_TOKEN")
check "GET /registry/:id/peers (has echo)" '"Echo Agent"' "$R"

# Test 14: Check access (same-org parent↔child — allowed)
R=$(curl -s -X POST "$BASE/registry/check-access" -H "Content-Type: application/json" \
  -d "{\"caller_id\":\"$ECHO_ID\",\"target_id\":\"$SUM_ID\"}")
check "POST /registry/check-access (same-org allowed)" '"allowed":true' "$R"

# Test 15: PATCH workspace (update position). PATCH /workspaces/:id is
# WorkspaceAuth-gated (router.go:227 — #680 IDOR fix), so it needs Echo's OWN
# bearer, NOT the admin bearer (WorkspaceAuth rejects the admin token).
R=$(curl -s "${ECHO_AUTH[@]}" -X PATCH "$BASE/workspaces/$ECHO_ID" -H "Content-Type: application/json" -d '{"x":100,"y":200}')
check "PATCH /workspaces/:id (position)" '"status":"updated"' "$R"

R=$(acurl "$BASE/workspaces/$ECHO_ID")
check "Position saved (x=100)" '"x":100' "$R"
check "Position saved (y=200)" '"y":200' "$R"

# Test 16: PATCH workspace (update name) — WorkspaceAuth-gated; use Echo's token.
R=$(curl -s "${ECHO_AUTH[@]}" -X PATCH "$BASE/workspaces/$ECHO_ID" -H "Content-Type: application/json" -d '{"name":"Echo Agent v2"}')
check "PATCH /workspaces/:id (name)" '"status":"updated"' "$R"

R=$(acurl "$BASE/workspaces/$ECHO_ID")
check "Name updated" '"name":"Echo Agent v2"' "$R"

# Test 17: Events (#165 / PR #167 — admin-gated; the admin bearer is required,
# and Tier-2b rejects a workspace bearer here, so use acurl's admin token alone).
R=$(acurl "$BASE/events")
check "GET /events (has events)" 'WORKSPACE_ONLINE' "$R"

R=$(acurl "$BASE/events/$ECHO_ID")
check "GET /events/:id (has events for echo)" 'WORKSPACE_ONLINE' "$R"

# Test 18: Update card
R=$(curl -s -X POST "$BASE/registry/update-card" -H "Content-Type: application/json" -H "Authorization: Bearer $ECHO_TOKEN" \
  -d "{\"workspace_id\":\"$ECHO_ID\",\"agent_card\":{\"name\":\"Echo Agent v2\",\"skills\":[{\"id\":\"echo\",\"name\":\"Echo\"},{\"id\":\"repeat\",\"name\":\"Repeat\"}]}}")
check "POST /registry/update-card" '"status":"updated"' "$R"

# Test 19: Degraded status transition
# First, ensure workspace is online (Redis TTL may have expired during test)
curl -s -X POST "$BASE/registry/heartbeat" -H "Content-Type: application/json" -H "Authorization: Bearer $ECHO_TOKEN" \
  -d "{\"workspace_id\":\"$ECHO_ID\",\"error_rate\":0.0,\"sample_error\":\"\",\"active_tasks\":0,\"uptime_seconds\":180}" > /dev/null

# Re-register to force online status in case liveness expired
curl -s -X POST "$BASE/registry/register" -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ECHO_TOKEN" \
  -d "{\"id\":\"$ECHO_ID\",\"url\":\"$ECHO_URL\",\"agent_card\":{\"name\":\"Echo Agent v2\",\"skills\":[{\"id\":\"echo\",\"name\":\"Echo\"},{\"id\":\"repeat\",\"name\":\"Repeat\"}]}}" > /dev/null

# Now send high error rate to trigger degraded
R=$(curl -s -X POST "$BASE/registry/heartbeat" -H "Content-Type: application/json" -H "Authorization: Bearer $ECHO_TOKEN" \
  -d "{\"workspace_id\":\"$ECHO_ID\",\"error_rate\":0.8,\"sample_error\":\"API rate limit\",\"active_tasks\":0,\"uptime_seconds\":200}")
check "Heartbeat (high error_rate)" '"status":"ok"' "$R"

R=$(acurl "$BASE/workspaces/$ECHO_ID")
check "Status degraded" '"status":"degraded"' "$R"

# Test 20: Recovery
R=$(curl -s -X POST "$BASE/registry/heartbeat" -H "Content-Type: application/json" -H "Authorization: Bearer $ECHO_TOKEN" \
  -d "{\"workspace_id\":\"$ECHO_ID\",\"error_rate\":0.0,\"sample_error\":\"\",\"active_tasks\":0,\"uptime_seconds\":300}")
check "Heartbeat (recovered)" '"status":"ok"' "$R"

R=$(acurl "$BASE/workspaces/$ECHO_ID")
check "Status back online" '"status":"online"' "$R"

# ---------- Activity Log Tests ----------
echo ""
echo "--- Activity Log Tests ---"

# Test: Report activity log
R=$(curl -s -X POST "$BASE/workspaces/$ECHO_ID/activity" -H "Content-Type: application/json" -H "Authorization: Bearer $ECHO_TOKEN" \
  -d '{"activity_type":"agent_log","method":"inference","summary":"Processing user query"}')
check "POST /workspaces/:id/activity (report)" '"status":"logged"' "$R"

# Test: Report A2A activity
R=$(curl -s -X POST "$BASE/workspaces/$ECHO_ID/activity" -H "Content-Type: application/json" -H "Authorization: Bearer $ECHO_TOKEN" \
  -d "{\"activity_type\":\"a2a_send\",\"method\":\"message/send\",\"summary\":\"Sent to summarizer\",\"target_id\":\"$SUM_ID\",\"duration_ms\":150}")
check "POST activity (a2a_send)" '"status":"logged"' "$R"

# Test: Report error activity
R=$(curl -s -X POST "$BASE/workspaces/$ECHO_ID/activity" -H "Content-Type: application/json" -H "Authorization: Bearer $ECHO_TOKEN" \
  -d '{"activity_type":"error","summary":"Connection timeout","status":"error","error_detail":"dial tcp: timeout after 30s"}')
check "POST activity (error)" '"status":"logged"' "$R"

# Test: Report task update
R=$(curl -s -X POST "$BASE/workspaces/$ECHO_ID/activity" -H "Content-Type: application/json" -H "Authorization: Bearer $ECHO_TOKEN" \
  -d '{"activity_type":"task_update","method":"start","summary":"Started data analysis"}')
check "POST activity (task_update)" '"status":"logged"' "$R"

# Test: Invalid activity type rejected
R=$(curl -s -X POST "$BASE/workspaces/$ECHO_ID/activity" -H "Content-Type: application/json" -H "Authorization: Bearer $ECHO_TOKEN" \
  -d '{"activity_type":"bad_type","summary":"test"}')
check "POST activity (invalid type → 400)" 'invalid activity_type' "$R"

# Test: List all activities
R=$(curl -s "$BASE/workspaces/$ECHO_ID/activity" -H "Authorization: Bearer $ECHO_TOKEN")
COUNT=$(echo "$R" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
check "GET /workspaces/:id/activity (has entries)" "4" "$COUNT"

# Test: List activities filtered by type
R=$(curl -s "$BASE/workspaces/$ECHO_ID/activity?type=error" -H "Authorization: Bearer $ECHO_TOKEN")
COUNT=$(echo "$R" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
check "GET activity?type=error (count=1)" "1" "$COUNT"
check "GET activity?type=error (has error_detail)" 'dial tcp' "$R"

R=$(curl -s "$BASE/workspaces/$ECHO_ID/activity?type=a2a_send" -H "Authorization: Bearer $ECHO_TOKEN")
COUNT=$(echo "$R" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
check "GET activity?type=a2a_send (count=1)" "1" "$COUNT"
check "GET activity?type=a2a_send (has target_id)" "$SUM_ID" "$R"

# Test: List with custom limit
R=$(curl -s "$BASE/workspaces/$ECHO_ID/activity?limit=2" -H "Authorization: Bearer $ECHO_TOKEN")
COUNT=$(echo "$R" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
check "GET activity?limit=2 (capped)" "2" "$COUNT"

# Test: Empty activity list for other workspace
R=$(curl -s "$BASE/workspaces/$SUM_ID/activity" -H "Authorization: Bearer $SUM_TOKEN")
check "GET activity (empty for summarizer)" '[]' "$R"

# ---------- Current Task Tests ----------
echo ""
echo "--- Current Task Tests ---"

# Test: Heartbeat with current_task
R=$(curl -s -X POST "$BASE/registry/heartbeat" -H "Content-Type: application/json" -H "Authorization: Bearer $ECHO_TOKEN" \
  -d "{\"workspace_id\":\"$ECHO_ID\",\"error_rate\":0.0,\"sample_error\":\"\",\"active_tasks\":1,\"uptime_seconds\":400,\"current_task\":\"Analyzing document\"}")
check "Heartbeat with current_task" '"status":"ok"' "$R"

# Test: Verify state updates are observable in GET /workspaces/:id.
# current_task itself is stripped from this endpoint as of #966 to avoid
# leaking task bodies via the public-facing GET; active_tasks is still
# the canonical "is it busy" signal here. The list endpoint below covers
# the admin-only current_task visibility path.
R=$(acurl "$BASE/workspaces/$ECHO_ID")
check "active_tasks updated" '"active_tasks":1' "$R"

# Test: Clear current_task via heartbeat
R=$(curl -s -X POST "$BASE/registry/heartbeat" -H "Content-Type: application/json" -H "Authorization: Bearer $ECHO_TOKEN" \
  -d "{\"workspace_id\":\"$ECHO_ID\",\"error_rate\":0.0,\"sample_error\":\"\",\"active_tasks\":0,\"uptime_seconds\":500,\"current_task\":\"\"}")
check "Heartbeat clear current_task" '"status":"ok"' "$R"

R=$(acurl "$BASE/workspaces/$ECHO_ID")
check "active_tasks cleared" '"active_tasks":0' "$R"

# Test: current_task IS visible in the admin workspace list — the list
# endpoint is admin-auth gated and keeps the full record, so operators
# can still see task progress from the dashboard without exposing it
# over the public per-workspace GET.
R=$(acurl "$BASE/workspaces")
check "current_task in list response" '"current_task"' "$R"

# Test 21: Delete
# #1953: Summarizer is now a CHILD of Echo (same-org, for the peer-discovery
# tests above). DELETE on the *parent* (Echo) cascade-removes its descendants
# (CascadeDelete walks the recursive `parent_id` CTE), so deleting Echo first
# would also remove Summarizer and the "one survives" assertion would see 0.
# Delete the CHILD (Summarizer) here instead: a child delete does NOT cascade
# upward, so the parent Echo survives and count=1 holds. The bundle round-trip
# below needs Summarizer's exported config, so capture it BEFORE this delete.
# GET /bundles/export/:id is admin-gated (router.go:741) — use the admin bearer.
BUNDLE=$(acurl "$BASE/bundles/export/$SUM_ID")
check "GET /bundles/export/:id" '"name":"Summarizer Agent"' "$BUNDLE"
ORIG_NAME=$(echo "$BUNDLE" | python3 -c "import sys,json; print(json.load(sys.stdin)['name'])")
ORIG_TIER=$(echo "$BUNDLE" | python3 -c "import sys,json; print(json.load(sys.stdin)['tier'])")

# DELETE /workspaces/:id is admin-gated (router.go:167) AND now also
# approvals-gated for admin-token callers (CR2 RC 10818 — the admin-token
# gate is always-on; delete_workspace is in the gated map). X-Confirm-Name
# must still match the workspace name. e2e_gated_admin_op auto-approves
# the pending approval + retries the DELETE (so the harness sees the
# real 200/'status:removed' result).
R=$(e2e_gated_admin_op "$SUM_ID" acurl -X DELETE "$BASE/workspaces/$SUM_ID?confirm=true" \
  -H "X-Confirm-Name: Summarizer Agent")
check "DELETE /workspaces/:id" '"status":"removed"' "$R"

# Parent Echo must survive a child delete — list (admin) and expect count=1.
R=$(acurl "$BASE/workspaces")
COUNT=$(echo "$R" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
check "List after delete (count=1)" "1" "$COUNT"

# Test 22: Bundle round-trip — export → delete → import → verify same config.
# Summarizer's bundle was captured above; now delete the parent Echo (the only
# remaining workspace) so the import lands in a clean org, then re-import the
# Summarizer bundle.
echo ""
echo "--- Bundle Round-Trip Test ---"

# Delete the remaining parent Echo — DELETE is admin-gated (router.go:167)
# AND approvals-gated for admin-token callers (CR2 RC 10818). Use
# e2e_gated_admin_op to auto-approve the pending approval + retry.
R=$(e2e_gated_admin_op "$ECHO_ID" acurl -X DELETE "$BASE/workspaces/$ECHO_ID?confirm=true" \
  -H "X-Confirm-Name: Echo Agent v2")
check "Delete before re-import" '"status":"removed"' "$R"

# Both workspaces are now deleted. The platform-level ADMIN_TOKEN env is still
# set, so admin routes still require the admin bearer (fail-open does NOT
# re-engage just because the token table emptied) — keep using acurl's bearer.
R=$(acurl "$BASE/workspaces")
COUNT=$(echo "$R" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
check "All workspaces deleted (count=0)" "0" "$COUNT"

# Re-import from the exported bundle. POST /bundles/import is admin-gated
# (router.go:742) — acurl sends the admin bearer.
R=$(acurl -X POST "$BASE/bundles/import" -H "Content-Type: application/json" -d "$BUNDLE")
check "POST /bundles/import" '"status":"provisioning"' "$R"
NEW_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['workspace_id'])")

# Verify new ID is different from old
if [ "$NEW_ID" != "$SUM_ID" ]; then
  echo "PASS: New workspace has different ID"
  PASS=$((PASS + 1))
else
  echo "FAIL: New workspace should have a new ID"
  FAIL=$((FAIL + 1))
fi

# Verify re-imported workspace exists by name — status may be "provisioning",
# "online", or "failed" depending on runtime availability in the environment
# (CI has no Docker, so autogen/langgraph containers never come up). The
# round-trip assertion is about bundle fidelity, not provisioning success.
R=$(curl -s "$BASE/workspaces/$NEW_ID")
check "Re-imported workspace exists" "\"id\":\"$NEW_ID\"" "$R"

REIMPORT_NAME=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['name'])")
REIMPORT_TIER=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)['tier'])")

if [ "$REIMPORT_NAME" = "$ORIG_NAME" ]; then
  echo "PASS: Name matches after round-trip ($ORIG_NAME)"
  PASS=$((PASS + 1))
else
  echo "FAIL: Name mismatch — expected '$ORIG_NAME', got '$REIMPORT_NAME'"
  FAIL=$((FAIL + 1))
fi

if [ "$REIMPORT_TIER" = "$ORIG_TIER" ]; then
  echo "PASS: Tier matches after round-trip ($ORIG_TIER)"
  PASS=$((PASS + 1))
else
  echo "FAIL: Tier mismatch — expected '$ORIG_TIER', got '$REIMPORT_TIER'"
  FAIL=$((FAIL + 1))
fi

# Register the re-imported workspace to verify agent_card round-trips
NEW_TOKEN=$(echo "$R" | e2e_extract_token)
if [ -z "$NEW_TOKEN" ]; then
  NEW_TOKEN=$(e2e_mint_workspace_token "$NEW_ID" 2>/dev/null || echo "")
fi
NEW_AUTH=()
[ -n "$NEW_TOKEN" ] && NEW_AUTH=(-H "Authorization: Bearer $NEW_TOKEN")
R=$(curl -s -X POST "$BASE/registry/register" -H "Content-Type: application/json" \
  "${NEW_AUTH[@]}" \
  -d "{\"id\":\"$NEW_ID\",\"url\":\"$SUM_URL\",\"agent_card\":{\"name\":\"Summarizer\",\"skills\":[{\"id\":\"summarize\",\"name\":\"Summarize\"}]}}")
check "Register re-imported workspace" '"status":"registered"' "$R"
# Capture the fresh token issued to the re-imported workspace.  SUM_TOKEN was
# revoked when SUM_ID was deleted above — use this one for cleanup instead.
REG_NEW_TOKEN=$(echo "$R" | e2e_extract_token)
[ -n "$REG_NEW_TOKEN" ] && NEW_TOKEN="$REG_NEW_TOKEN"

# Re-export and verify agent_card survives the round-trip (#165 / PR #167 —
# GET /bundles/export/:id is admin-gated; use the admin bearer).
REBUNDLE=$(acurl "$BASE/bundles/export/$NEW_ID")
check "Re-exported bundle has agent_card" '"agent_card"' "$REBUNDLE"

# Clean up — DELETE /workspaces/:id is admin-gated; pass no per-call auth so
# e2e_delete_workspace falls back to the platform admin bearer (a workspace
# bearer would be rejected by Tier-2b).
e2e_delete_workspace "$NEW_ID" "$ORIG_NAME"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
exit $FAIL
