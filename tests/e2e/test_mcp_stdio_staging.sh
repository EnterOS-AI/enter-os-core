#!/usr/bin/env bash
# Staging E2E for MCP stdio transport (runtime#61 regression).
#
# Verifies that the MCP server in the claude-code workspace image
# handles stdout redirected to a regular file — the exact failure
# mode openclaw hits when capturing MCP output.
#
# Required env:
#   MOLECULE_CP_URL        default: https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   CP admin bearer (Railway CP_ADMIN_API_TOKEN)
#
# Optional env:
#   E2E_KEEP_ORG           1 → skip teardown (debugging only)
#   E2E_RUN_ID             Slug suffix; CI: ${GITHUB_RUN_ID}

set -euo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLEC…OKEN required — Railway staging CP_ADMIN_API_TOKEN}"
RUN_ID_SUFFIX="${E2E_RUN_ID:-$(date +%H%M%S)-$$}"

SLUG="e2e-mcp-$(date +%Y%m%d)-${RUN_ID_SUFFIX}"
SLUG=$(echo "$SLUG" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-' | head -c 32)

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

CURL_COMMON=(-sS --fail-with-body --max-time 30)

# ─── cleanup trap ───────────────────────────────────────────────────────
CLEANUP_DONE=0
cleanup_org() {
  local entry_rc=$?
  if [ "$CLEANUP_DONE" = "1" ]; then return 0; fi
  CLEANUP_DONE=1

  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    log "E2E_KEEP_ORG=1 → leaving $SLUG behind for inspection"
    return 0
  fi

  log "Cleanup: deleting tenant $SLUG..."
  curl "${CURL_COMMON[@]}" --max-time 120 -X DELETE "$CP_URL/cp/admin/tenants/$SLUG" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"confirm\":\"$SLUG\"}" >/dev/null 2>&1 \
    && ok "Teardown request accepted" \
    || log "Teardown returned non-2xx (may already be gone)"
}
trap cleanup_org EXIT

# ─── provision tenant ───────────────────────────────────────────────────
log "Provisioning tenant $SLUG..."
TENANT=$(curl "${CURL_COMMON[@]}" -X POST "$CP_URL/cp/admin/orgs" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"slug\":\"$SLUG\",\"name\":\"MCP Stdio E2E $SLUG\"}")
ok "Tenant provisioned"

# ─── get tenant admin token ─────────────────────────────────────────────
log "Fetching tenant admin token..."
for _ in $(seq 1 30); do
  TOKEN_RESP=$(curl -sS --max-time 10 "$CP_URL/cp/admin/orgs/$SLUG/admin-token" \
    -H "Authorization: Bearer $ADMIN_TOKEN" 2>/dev/null || echo '{}')
  TOKEN=$(echo "$TOKEN_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null || echo "")
  [ -n "$TOKEN" ] && break
  sleep 2
done
[ -n "$TOKEN" ] || fail "Could not retrieve tenant admin token"
ok "Tenant admin token obtained"

# ─── create claude-code workspace ───────────────────────────────────────
log "Creating claude-code workspace..."
WS=$(curl "${CURL_COMMON[@]}" -X POST "$CP_URL/workspaces" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"MCP Stdio Test","role":"Test","runtime":"claude-code","tier":1}')
WS_ID=$(echo "$WS" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
ok "Workspace created: $WS_ID"

# ─── wait for online ────────────────────────────────────────────────────
log "Waiting for workspace to come online (up to 120s)..."
for _ in $(seq 1 24); do
  STATUS=$(curl -sS --max-time 10 "$CP_URL/workspaces/$WS_ID" \
    -H "Authorization: Bearer $TOKEN" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
  [ "$STATUS" = "online" ] && break
  sleep 5
done
[ "$STATUS" = "online" ] || fail "Workspace did not come online (status=$STATUS)"
ok "Workspace online"

# ─── get workspace container info ───────────────────────────────────────
log "Fetching workspace runtime info..."
RUNTIME_INFO=$(curl -sS --max-time 10 "$CP_URL/workspaces/$WS_ID" \
  -H "Authorization: Bearer $TOKEN" 2>/dev/null)
CONTAINER_ID=$(echo "$RUNTIME_INFO" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('container_id',''))" 2>/dev/null || echo "")
[ -n "$CONTAINER_ID" ] || fail "No container_id in workspace response"
ok "Container ID: $CONTAINER_ID"

# ─── MCP stdio transport test ───────────────────────────────────────────
log "Testing MCP stdio transport with regular-file stdout..."

OUTPUT=$(mktemp)
trap 'rm -f "$OUTPUT"; cleanup_org' EXIT

# Send initialize + tools/list via stdin, capture stdout to regular file
{
  echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'
  echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
} | docker exec -i -e WORKSPACE_ID="$WS_ID" "$CONTAINER_ID" \
  python -m molecule_runtime.a2a_mcp_server > "$OUTPUT" 2>&1 || {
  RC=$?
  log "MCP server exited with code $RC (expected for stdin EOF)"
}

if grep -q '"result"' "$OUTPUT"; then
  ok "MCP server handles regular-file stdout"
else
  fail "MCP server did not produce JSON-RPC result. Output:\n$(head -20 "$OUTPUT")"
fi

if grep -q '"tools"' "$OUTPUT"; then
  ok "MCP tools/list returns tools"
else
  fail "MCP tools/list did not return tools. Output:\n$(head -20 "$OUTPUT")"
fi

# ─── summary ────────────────────────────────────────────────────────────
log "All tests passed ✅"
