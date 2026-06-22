#!/usr/bin/env bash
# E2E tests for the user_tasks platform ability — agent → user action
# requests ("asks"). Exercises the FULL contract both surfaces expose:
#
#   REST (WorkspaceAuth unless noted):
#     POST   /workspaces/:id/user-tasks              create an ask
#     GET    /workspaces/:id/user-tasks              this workspace's asks
#     GET    /user-tasks/pending          (AdminAuth) org-wide pending asks
#     PATCH  /workspaces/:id/user-tasks/:taskId      edit (scoped by ws id)
#     DELETE /workspaces/:id/user-tasks/:taskId      remove (scoped by ws id)
#     POST   /workspaces/:id/user-tasks/:taskId/resolve   done|dismissed
#
#   MCP a2a-bridge tools (POST /workspaces/:id/mcp, JSON-RPC tools/call):
#     request_user_action(title, detail?)   list_user_tasks()
#     update_user_task(user_task_id, …)      delete_user_task(user_task_id)
#
# The MCP arm is what proves the agent→user ability END-TO-END: it drives
# the literal `tools/call` envelope through the real WorkspaceAuth chain
# (the exact call a canvas agent makes), then asserts the new task surfaces
# on the admin-gated concierge feed (/user-tasks/pending).
#
# Requires: platform running on $BASE (default http://localhost:8080).
# Env contract (same as its siblings in this dir):
#   BASE                  platform base URL (default http://localhost:8080)
#   ADMIN_TOKEN /         platform admin bearer; MOLECULE_ADMIN_TOKEN wins.
#   MOLECULE_ADMIN_TOKEN  Sent on AdminAuth routes (create/delete ws,
#                         /user-tasks/pending). Fail-open dev platform with
#                         no admin token still works (helpers send nothing).
set -euo pipefail

source "$(dirname "$0")/_lib.sh"  # sets BASE default + admin-auth helpers
PASS=0
FAIL=0

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

# Assert an exact HTTP status. $1 desc, $2 expected code, $3 actual code.
check_code() {
  local desc="$1"
  local expected="$2"
  local actual="$3"
  if [ "$actual" = "$expected" ]; then
    echo "PASS: $desc (HTTP $actual)"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $desc"
    echo "  expected HTTP $expected, got HTTP $actual"
    FAIL=$((FAIL + 1))
  fi
}

# Admin bearer for AdminAuth routes (create/delete workspace, pending feed).
ADMIN_AUTH=()
e2e_admin_auth_args ADMIN_AUTH
acurl() { curl -s ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} "$@"; }

# The local create-workspace response embeds a claude_code_channel_snippet
# whose raw newlines/escapes make the body un-loadable by strict json.load
# (the same reason _extract_token.py can emit empty here). So pull id +
# auth_token with tolerant regexes that don't parse the whole envelope.
extract_field_regex() {  # <field> ; reads body on stdin
  local field="$1"
  python3 -c "
import sys, re
body = sys.stdin.read()
m = re.search(r'\"$field\"\s*:\s*\"([^\"]+)\"', body)
print(m.group(1) if m else '')
"
}
extract_ws_id() { extract_field_regex "id"; }
extract_ws_token() { extract_field_regex "auth_token"; }

# Create an external workspace; echo "<id>\t<token>". Caller registers ids
# in CREATED_WSIDS for the scoped teardown.
create_workspace() {  # <name>
  local name="$1" resp wid tok
  resp=$(acurl -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
    -d "{\"name\":\"$name\",\"tier\":1,\"runtime\":\"external\",\"external\":true}")
  wid=$(printf '%s' "$resp" | extract_ws_id)
  tok=$(printf '%s' "$resp" | extract_ws_token)
  if [ -z "$wid" ]; then
    echo "FATAL: create workspace '$name' returned no id: $(printf '%s' "$resp" | head -c 200)" >&2
    return 1
  fi
  if [ -z "$tok" ]; then
    # External create did not echo a token — mint one via the admin endpoint.
    tok=$(e2e_mint_workspace_token "$wid" 2>/dev/null || echo "")
  fi
  if [ -z "$tok" ]; then
    echo "FATAL: no workspace bearer for '$name' ($wid)" >&2
    return 1
  fi
  printf '%s\t%s\n' "$wid" "$tok"
}

# Issue a JSON-RPC tools/call to a workspace MCP endpoint. Echoes the raw
# HTTP body on stdout and persists the HTTP status to $MCP_CODE_FILE (mcp_call
# runs in a command substitution, so a plain var would be lost in the
# subshell — read the code back via mcp_http_code after the call).
# <wsid> <bearer> <tool> <args-json>
MCP_CODE_FILE="$(mktemp -t ut_mcp_code.XXXXXX)"
MCP_BODY_FILE="$(mktemp -t ut_mcp_body.XXXXXX)"
mcp_call() {
  local wsid="$1" bearer="$2" tool="$3" args="$4" code
  set +e
  code=$(curl -sS -X POST "$BASE/workspaces/$wsid/mcp" \
    -H "Authorization: Bearer $bearer" \
    -H "Content-Type: application/json" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$tool\",\"arguments\":$args}}" \
    -o "$MCP_BODY_FILE" -w "%{http_code}" 2>/dev/null)
  set -e
  printf '%s' "$code" > "$MCP_CODE_FILE"
  cat "$MCP_BODY_FILE" 2>/dev/null || echo ''
}
mcp_http_code() { cat "$MCP_CODE_FILE" 2>/dev/null || echo ''; }

# Extract the `result.content[].text` from an MCP tools/call response.
mcp_result_text() {  # reads body on stdin
  python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print(''); sys.exit(0)
res = d.get('result') if isinstance(d, dict) else None
if not isinstance(res, dict):
    print(''); sys.exit(0)
print(''.join(c.get('text','') for c in res.get('content', []) if c.get('type') == 'text'))
"
}

# ─── Scoped teardown ───────────────────────────────────────────────────
# Deletes ONLY the workspaces THIS run created (CREATED_WSIDS). Deleting a
# workspace cascades its user_tasks rows, so no separate task cleanup is
# needed. NEVER a blanket sweep — a local stack can be shared with other
# concurrent E2E runs.
CREATED_WSIDS=()
teardown() {
  local rc=$?
  set +e
  echo ""
  echo "[teardown] deleting ${#CREATED_WSIDS[@]} workspace(s) this run created (scoped)"
  for wid in ${CREATED_WSIDS[@]+"${CREATED_WSIDS[@]}"}; do
    [ -n "$wid" ] || continue
    e2e_delete_workspace "$wid" "" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"}
  done
  exit $rc
}
trap teardown EXIT INT TERM

echo "=== user_tasks E2E (REST + MCP) ==="
echo ""

# ─── Setup: two sibling workspaces (A raises asks; B probes scoping) ────
IFS=$'\t' read -r WS_A A_TOK < <(create_workspace "UserTasks-A-$$") || true
[ -n "${WS_A:-}" ] || { echo "FATAL: ws-A setup failed"; exit 1; }
CREATED_WSIDS+=("$WS_A")
IFS=$'\t' read -r WS_B B_TOK < <(create_workspace "UserTasks-B-$$") || true
[ -n "${WS_B:-}" ] || { echo "FATAL: ws-B setup failed"; exit 1; }
CREATED_WSIDS+=("$WS_B")
echo "ws-A=$WS_A  ws-B=$WS_B"
echo ""

# ─── 1. Create (REST) on ws-A → 201, status pending ────────────────────
echo "--- 1. Create (REST) ---"
R=$(curl -s -w "\n%{http_code}" -X POST "$BASE/workspaces/$WS_A/user-tasks" \
  -H "Authorization: Bearer $A_TOK" -H "Content-Type: application/json" \
  -d '{"title":"Review the Q3 draft","detail":"Need your sign-off before send"}')
CODE=$(printf '%s' "$R" | tail -n1)
BODY=$(printf '%s' "$R" | sed '$d')
check_code "POST create user-task" "201" "$CODE"
check "create returns status pending" '"status":"pending"' "$BODY"
TASK_ID=$(printf '%s' "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['user_task_id'])")
echo "  TASK_ID=$TASK_ID"
[ -n "$TASK_ID" ] || { echo "FATAL: no user_task_id returned"; }

# ─── 2. Read (REST workspace + admin pending) ──────────────────────────
echo ""
echo "--- 2. Read ---"
R=$(curl -s "$BASE/workspaces/$WS_A/user-tasks" -H "Authorization: Bearer $A_TOK")
check "GET ws-A user-tasks contains the task id" "$TASK_ID" "$R"
check "GET ws-A user-tasks shows title" 'Review the Q3 draft' "$R"
R=$(acurl "$BASE/user-tasks/pending")
check "GET /user-tasks/pending (admin) contains the task" "$TASK_ID" "$R"
check "pending entry carries workspace_name" "UserTasks-A-$$" "$R"

# ─── 3. Update (REST) PATCH title/detail → 200, change applied ─────────
echo ""
echo "--- 3. Update (REST PATCH) ---"
R=$(curl -s -w "\n%{http_code}" -X PATCH "$BASE/workspaces/$WS_A/user-tasks/$TASK_ID" \
  -H "Authorization: Bearer $A_TOK" -H "Content-Type: application/json" \
  -d '{"title":"Review the Q3 draft (URGENT)","detail":"Sign-off needed by EOD"}')
CODE=$(printf '%s' "$R" | tail -n1)
check_code "PATCH update user-task" "200" "$CODE"
R=$(curl -s "$BASE/workspaces/$WS_A/user-tasks" -H "Authorization: Bearer $A_TOK")
check "PATCH applied new title" '(URGENT)' "$R"
check "PATCH applied new detail" 'Sign-off needed by EOD' "$R"

# ─── 4. Resolve (REST) done → 200, gone from pending ───────────────────
echo ""
echo "--- 4. Resolve (REST done) ---"
R=$(curl -s -w "\n%{http_code}" -X POST "$BASE/workspaces/$WS_A/user-tasks/$TASK_ID/resolve" \
  -H "Authorization: Bearer $A_TOK" -H "Content-Type: application/json" \
  -d '{"status":"done","resolved_by":"cto"}')
CODE=$(printf '%s' "$R" | tail -n1)
BODY=$(printf '%s' "$R" | sed '$d')
check_code "POST resolve done" "200" "$CODE"
check "resolve echoes status done" '"status":"done"' "$BODY"
R=$(acurl "$BASE/user-tasks/pending")
check_not "resolved task no longer pending (admin feed)" "$TASK_ID" "$R"

# ─── 5. Create via MCP tool request_user_action → new pending task ─────
# This is the agent→user ability proven end-to-end: the literal tools/call
# the canvas agent makes, surfacing on the admin concierge feed.
echo ""
echo "--- 5. Create via MCP (request_user_action) ---"
BODY=$(mcp_call "$WS_A" "$A_TOK" "request_user_action" '{"title":"Provide the staging API key","detail":"Blocked on it for the deploy"}')
check_code "MCP request_user_action HTTP" "200" "$(mcp_http_code)"
TEXT=$(printf '%s' "$BODY" | mcp_result_text)
check "MCP request_user_action success text" 'Asked the user' "$TEXT"
# A NEW pending task must appear on the admin feed.
R=$(acurl "$BASE/user-tasks/pending")
check "MCP-created ask appears in pending feed" 'Provide the staging API key' "$R"
MCP_TASK_ID=$(printf '%s' "$R" | python3 -c "
import sys, json
d = json.load(sys.stdin)
for t in d:
    if t.get('title') == 'Provide the staging API key':
        print(t['id']); break
")
echo "  MCP_TASK_ID=$MCP_TASK_ID"
[ -n "$MCP_TASK_ID" ] || echo "  (note: could not resolve MCP_TASK_ID — later MCP steps assert by title)"

# ─── 6. list_user_tasks (MCP) returns ws-A's task(s) ───────────────────
echo ""
echo "--- 6. list_user_tasks (MCP) ---"
BODY=$(mcp_call "$WS_A" "$A_TOK" "list_user_tasks" '{}')
check_code "MCP list_user_tasks HTTP" "200" "$(mcp_http_code)"
TEXT=$(printf '%s' "$BODY" | mcp_result_text)
check "list_user_tasks contains the MCP task" 'Provide the staging API key' "$TEXT"
check "list_user_tasks shows it pending" '"status":"pending"' "$TEXT"

# ─── 7. update_user_task (MCP) changes it → verify ─────────────────────
echo ""
echo "--- 7. update_user_task (MCP) ---"
BODY=$(mcp_call "$WS_A" "$A_TOK" "update_user_task" \
  "{\"user_task_id\":\"$MCP_TASK_ID\",\"title\":\"Provide the PROD API key\"}")
check_code "MCP update_user_task HTTP" "200" "$(mcp_http_code)"
TEXT=$(printf '%s' "$BODY" | mcp_result_text)
check "MCP update_user_task success text" 'User task updated' "$TEXT"
BODY=$(mcp_call "$WS_A" "$A_TOK" "list_user_tasks" '{}')
TEXT=$(printf '%s' "$BODY" | mcp_result_text)
check "update applied (new title visible)" 'Provide the PROD API key' "$TEXT"
check_not "update applied (old title gone)" 'staging API key' "$TEXT"

# ─── 8. delete_user_task (MCP) → gone from list ────────────────────────
echo ""
echo "--- 8. delete_user_task (MCP) ---"
BODY=$(mcp_call "$WS_A" "$A_TOK" "delete_user_task" "{\"user_task_id\":\"$MCP_TASK_ID\"}")
check_code "MCP delete_user_task HTTP" "200" "$(mcp_http_code)"
TEXT=$(printf '%s' "$BODY" | mcp_result_text)
check "MCP delete_user_task success text" 'User task deleted' "$TEXT"
BODY=$(mcp_call "$WS_A" "$A_TOK" "list_user_tasks" '{}')
TEXT=$(printf '%s' "$BODY" | mcp_result_text)
check_not "deleted task gone from list" 'Provide the PROD API key' "$TEXT"

# ─── 9. Scoping / authz ────────────────────────────────────────────────
echo ""
echo "--- 9. Scoping / authz ---"
# A fresh ws-A task to attempt cross-workspace mutation against.
SCOPE_ID=$(curl -s -X POST "$BASE/workspaces/$WS_A/user-tasks" \
  -H "Authorization: Bearer $A_TOK" -H "Content-Type: application/json" \
  -d '{"title":"Scope probe task"}' | python3 -c "import sys,json; print(json.load(sys.stdin)['user_task_id'])")
echo "  SCOPE_ID=$SCOPE_ID (owned by ws-A)"
# ws-B PATCHes ws-A's task → 404 (workspace_id scope).
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$BASE/workspaces/$WS_B/user-tasks/$SCOPE_ID" \
  -H "Authorization: Bearer $B_TOK" -H "Content-Type: application/json" -d '{"title":"hijack"}')
check_code "ws-B PATCH of ws-A's task is scoped out" "404" "$CODE"
# ws-B DELETEs ws-A's task → 404.
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$BASE/workspaces/$WS_B/user-tasks/$SCOPE_ID" \
  -H "Authorization: Bearer $B_TOK")
check_code "ws-B DELETE of ws-A's task is scoped out" "404" "$CODE"
# Task survived the cross-workspace attempts (still on ws-A, unchanged).
R=$(curl -s "$BASE/workspaces/$WS_A/user-tasks" -H "Authorization: Bearer $A_TOK")
check "ws-A's task survived cross-ws attempts" "$SCOPE_ID" "$R"
check_not "ws-A's task title was NOT hijacked" 'hijack' "$R"
# /user-tasks/pending is AdminAuth — a workspace bearer must be rejected.
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/user-tasks/pending" -H "Authorization: Bearer $A_TOK")
if [ "$CODE" = "401" ] || [ "$CODE" = "403" ]; then
  echo "PASS: /user-tasks/pending rejects a workspace token (HTTP $CODE)"
  PASS=$((PASS + 1))
else
  echo "FAIL: /user-tasks/pending should reject a workspace token, got HTTP $CODE"
  FAIL=$((FAIL + 1))
fi
# …and reject no auth at all.
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/user-tasks/pending")
if [ "$CODE" = "401" ] || [ "$CODE" = "403" ]; then
  echo "PASS: /user-tasks/pending rejects an unauthenticated caller (HTTP $CODE)"
  PASS=$((PASS + 1))
else
  echo "FAIL: /user-tasks/pending should reject no auth, got HTTP $CODE"
  FAIL=$((FAIL + 1))
fi

# ─── 10. Validation ────────────────────────────────────────────────────
echo ""
echo "--- 10. Validation ---"
# Missing title → 400.
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/workspaces/$WS_A/user-tasks" \
  -H "Authorization: Bearer $A_TOK" -H "Content-Type: application/json" -d '{"detail":"no title here"}')
check_code "create without title → 400" "400" "$CODE"
# Resolve with an invalid status → 400.
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/workspaces/$WS_A/user-tasks/$SCOPE_ID/resolve" \
  -H "Authorization: Bearer $A_TOK" -H "Content-Type: application/json" -d '{"status":"banana"}')
check_code "resolve with invalid status → 400" "400" "$CODE"
# PATCH with an invalid status → 400.
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$BASE/workspaces/$WS_A/user-tasks/$SCOPE_ID" \
  -H "Authorization: Bearer $A_TOK" -H "Content-Type: application/json" -d '{"status":"banana"}')
check_code "PATCH with invalid status → 400" "400" "$CODE"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
exit $FAIL
