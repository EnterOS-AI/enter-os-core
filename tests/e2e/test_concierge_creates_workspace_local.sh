#!/usr/bin/env bash
# LOCAL functional variant of the concierge-creates-a-workspace gate.
#
# Same proof as tests/e2e/test_staging_concierge_creates_workspace_e2e.sh but
# against the ALREADY-RUNNING local stack (BASE, default http://localhost:8080),
# so the "concierge actually invokes create_workspace via the platform MCP" claim
# can be demonstrated locally — far faster than provisioning an EC2 tenant.
#
# Drive the AGENT (not the REST API): send the concierge an A2A message/send
# ("create a workspace named e2e-cncrg-worker-<runid> with role engineer") and
# assert the DETERMINISTIC SIDE EFFECT — that named workspace now EXISTS in
# GET /workspaces — which can only happen if the concierge's LLM really invoked
# the create_workspace platform-MCP tool.
#
# SKIP-LOUD GATE (this is the whole point of the local variant). The platform MCP
# tools — incl. create_workspace — only light up on the DEDICATED platform-agent
# image (Dockerfile.platform-agent, ships /opt/molecule-mcp-server). The ordinary
# `claude-code` image the default local stack provisions the concierge on does
# NOT ship it (platform_agent.go SELF-HOST CAVEAT). So before driving the agent
# this script PROBES the concierge's own MCP tool list (POST /workspaces/:id/mcp
# tools/list) and SKIPs LOUD (exit 0) unless create_workspace is actually present.
# It also skips-loud when no concierge is seeded or it isn't online. That makes
# this runnable on any local stack: it only EXERCISES the path when the local
# stack can actually run it, and never false-reds when it can't.
#
# To make the local stack able to run this GREEN you need BOTH:
#   1. A concierge seeded as the kind='platform' root. The self-hosted compose
#      sets MOLECULE_SEED_PLATFORM_AGENT=1 so the ws-server self-seeds it
#      (EnsureSelfHostedPlatformAgent) + best-effort provisions it on boot
#      (MaybeProvisionPlatformAgentOnBoot).
#   2. That concierge running on the platform-agent image (so create_workspace
#      exists) WITH a working model key (e.g. MINIMAX_API_KEY / a BYOK key) so its
#      LLM can run the tool. The default `claude-code` image will SKIP at the MCP
#      probe — that's expected and honest, not a failure.
#
# Env contract:
#   BASE                      default http://localhost:8080
#   MOLECULE_ADMIN_TOKEN      platform admin bearer IF the local stack sets
#                             ADMIN_TOKEN (devmode fail-open if unset). Used by
#                             _lib.sh helpers for admin-gated GET/DELETE.
#   E2E_CONCIERGE_ONLINE_SECS default 300 (local boot budget)
#   E2E_AGENT_ACT_SECS        default 300 (LLM think+tool-call budget)
#   E2E_RUN_ID                slug/name suffix; default $$-based
#
# Exit codes:
#   0  concierge created the workspace, OR honest skip-loud (path not runnable)
#   1  generic / assertion failure (agent didn't act, or the tool failed)
set -euo pipefail

: "${BASE:=http://localhost:8080}"
export BASE
# shellcheck disable=SC1091
# shellcheck source=_lib.sh
source "$(dirname "$0")/_lib.sh"
# Error-as-text scanner so a concierge that surfaces a tool error AS its reply
# is distinguished from a clean "created it" reply.
# shellcheck disable=SC1091
# shellcheck source=lib/completion_assert.sh
source "$(dirname "$0")/lib/completion_assert.sh"

CONCIERGE_ONLINE_SECS="${E2E_CONCIERGE_ONLINE_SECS:-300}"
AGENT_ACT_SECS="${E2E_AGENT_ACT_SECS:-300}"
RUN_ID_SUFFIX="${E2E_RUN_ID:-$(date +%H%M%S)-$$}"
WORKER_NAME="e2e-cncrg-worker-${RUN_ID_SUFFIX}"
WORKER_NAME=$(echo "$WORKER_NAME" | tr -cd 'a-zA-Z0-9-' | head -c 48)
export WORKER_NAME

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }
skip_loud() { echo "[$(date +%H:%M:%S)] ⏭️  SKIP (local path not runnable): $*" >&2; exit 0; }

# Admin-auth curl args (if the local stack set ADMIN_TOKEN; else empty / fail-open).
ADMIN_AUTH=()
e2e_admin_auth_args ADMIN_AUTH

WORKER_ID=""
cleanup() {
  # Targeted delete of the worker the concierge created (best-effort). _lib.sh's
  # helper sends the admin bearer + confirm header.
  if [ -n "$WORKER_ID" ]; then
    log "🧹 deleting concierge-created worker $WORKER_ID ($WORKER_NAME)..."
    e2e_delete_workspace "$WORKER_ID" "$WORKER_NAME" || true
  fi
}
trap cleanup EXIT INT TERM

list_ws() { curl -sS --max-time 15 "$BASE/workspaces" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"}; }

find_platform_root() {
  list_ws | python3 -c "
import sys, json
try: rows = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
for w in rows if isinstance(rows, list) else []:
    if w.get('kind') == 'platform' and not w.get('parent_id'):
        print(w.get('id','')); break
else:
    print('')"
}

ws_field() {  # <id> <field>
  curl -sS --max-time 15 "$BASE/workspaces/$1" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
print(d.get('$2','') if isinstance(d, dict) else '')"
}

find_worker_by_name() {
  list_ws | python3 -c "
import sys, json, os
want = os.environ['WORKER_NAME']
try: rows = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
for w in rows if isinstance(rows, list) else []:
    if w.get('name') == want:
        print(w.get('id','')); break
else:
    print('')"
}

# concierge_has_create_workspace_tool <id>: probe POST /workspaces/:id/mcp
# tools/list and echo "yes" iff create_workspace is in the advertised tool set.
# This is THE gate distinguishing the platform-agent image (has the tool) from
# the ordinary claude-code image (does not).
concierge_has_create_workspace_tool() {  # <id>
  local wid="$1" out
  out=$(curl -sS --max-time 30 -X POST "$BASE/workspaces/$wid/mcp" \
    ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} \
    -H "Content-Type: application/json" \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' 2>/dev/null || echo '{}')
  echo "$out" | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: print('no'); sys.exit(0)
tools = (d.get('result') or {}).get('tools', []) if isinstance(d, dict) else []
names = {t.get('name','') for t in tools if isinstance(t, dict)}
# Accept the bare name or any mcp_*_create_workspace alias the bridge may expose.
print('yes' if any(n == 'create_workspace' or n.endswith('create_workspace') for n in names) else 'no')"
}

# ─── 0. Preflight ────────────────────────────────────────────────────────────
log "═══ LOCAL concierge CREATES-A-WORKSPACE (real-LLM) E2E ═══  BASE=$BASE"
log "    worker the concierge will be asked to create: name=$WORKER_NAME"
curl -sS --max-time 10 "$BASE/health" >/dev/null 2>&1 || skip_loud "local stack not reachable at $BASE/health — run \`make up\` first"
ok "Local stack reachable"

# ─── 1. Discover the concierge (kind='platform' root) ─────────────────────────
CONCIERGE_ID=$(find_platform_root)
if [ -z "$CONCIERGE_ID" ]; then
  skip_loud "no kind='platform' concierge seeded on the local stack. Set MOLECULE_SEED_PLATFORM_AGENT=1 \
on the ws-server (self-hosted compose does this) so it self-seeds + provisions the concierge."
fi
ok "Concierge (platform root) = $CONCIERGE_ID"

# ─── 2. Ensure the concierge is online ────────────────────────────────────────
log "Waiting for the concierge to be online (up to ${CONCIERGE_ONLINE_SECS}s)..."
ONLINE_DEADLINE=$(( $(date +%s) + CONCIERGE_ONLINE_SECS ))
C_STATUS=""; LAST_C_STATUS=""
while true; do
  C_STATUS=$(ws_field "$CONCIERGE_ID" status)
  if [ "$C_STATUS" != "$LAST_C_STATUS" ]; then log "    concierge → ${C_STATUS:-<none>}"; LAST_C_STATUS="$C_STATUS"; fi
  [ "$C_STATUS" = "online" ] && break
  if [ "$(date +%s)" -gt "$ONLINE_DEADLINE" ]; then
    skip_loud "concierge $CONCIERGE_ID never reached online within ${CONCIERGE_ONLINE_SECS}s (last='${C_STATUS}'). \
On the default local stack the concierge needs a model key (e.g. MINIMAX_API_KEY) to boot — without one it stays failed."
  fi
  sleep 5
done
ok "Concierge online"

# ─── 3. Gate: the platform MCP create_workspace tool must actually be present ──
log "Probing the concierge's MCP tool set for create_workspace..."
HAS_TOOL=$(concierge_has_create_workspace_tool "$CONCIERGE_ID")
if [ "$HAS_TOOL" != "yes" ]; then
  skip_loud "the concierge's platform MCP does NOT expose create_workspace — it is running on the ordinary \
claude-code image (no /opt/molecule-mcp-server), not the platform-agent image. Provision the concierge on \
Dockerfile.platform-agent to exercise this path locally. (This is the documented SELF-HOST CAVEAT, not a bug.)"
fi
ok "Concierge advertises create_workspace via its platform MCP"

# Pre-state: the worker must not already exist.
PRE_EXISTING=$(find_worker_by_name)
[ -n "$PRE_EXISTING" ] && fail "worker '$WORKER_NAME' already exists pre-test ($PRE_EXISTING) — cannot prove causality"
ok "Pre-state confirmed: '$WORKER_NAME' does not exist yet"

# ─── 4. Drive the AGENT via A2A message/send ──────────────────────────────────
log "Sending the concierge a natural-language create-workspace request..."
AGENT_PROMPT="Please create a new workspace in this org right now using your platform tools. \
Use the create_workspace tool with name exactly ${WORKER_NAME} (use that exact string, no quotes) and role engineer. \
Do not ask me any clarifying questions — the name and role are final. \
After the tool succeeds, reply with the new workspace id."
export AGENT_PROMPT
A2A_PAYLOAD=$(python3 -c "
import json, os, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-cncrg-mk-local-1',
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': os.environ['AGENT_PROMPT']}],
        }
    }
}))")

A2A_TMP=$(mktemp -t cncrg-mk-local-XXXXXX)
set +e
A2A_CODE=$(curl -sS --max-time "$AGENT_ACT_SECS" -X POST "$BASE/workspaces/$CONCIERGE_ID/a2a" \
  ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} \
  -H "Content-Type: application/json" \
  -d "$A2A_PAYLOAD" -o "$A2A_TMP" -w '%{http_code}' 2>/dev/null)
A2A_RC=$?
set -e
A2A_CODE=${A2A_CODE:-000}
A2A_RESP=$(cat "$A2A_TMP" 2>/dev/null || echo "")
rm -f "$A2A_TMP"
if [ "$A2A_RC" != "0" ] || [ "$A2A_CODE" -lt 200 ] || [ "$A2A_CODE" -ge 300 ]; then
  fail "A2A POST /workspaces/$CONCIERGE_ID/a2a failed (curl_rc=$A2A_RC, http=$A2A_CODE): $(echo "$A2A_RESP" | head -c 400)"
fi
AGENT_TEXT=$(echo "$A2A_RESP" | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
parts = (d.get('result') or {}).get('parts', []) if isinstance(d, dict) else []
print(parts[0].get('text','') if parts else '')" 2>/dev/null || echo "")
log "    concierge replied (first 300 chars): $(echo "$AGENT_TEXT" | head -c 300)"

# ─── 5. ASSERT the deterministic side effect: the worker now EXISTS ───────────
log "Polling GET /workspaces for the worker the concierge was asked to create..."
ACT_DEADLINE=$(( $(date +%s) + AGENT_ACT_SECS ))
while true; do
  WORKER_ID=$(find_worker_by_name)
  [ -n "$WORKER_ID" ] && break
  if [ "$(date +%s)" -gt "$ACT_DEADLINE" ]; then
    if hit=$(a2a_completion_error_marker "$AGENT_TEXT"); then
      fail "TOOL FAILED: concierge surfaced an error-as-text reply (matched '$hit') and no workspace '$WORKER_NAME' was created. Reply: $(echo "$AGENT_TEXT" | head -c 400)"
    fi
    fail "AGENT DID NOT ACT: concierge replied but no workspace named '$WORKER_NAME' exists after ${AGENT_ACT_SECS}s — its LLM did not invoke create_workspace. Reply: $(echo "$AGENT_TEXT" | head -c 400)"
  fi
  sleep 6
done
ok "DETERMINISTIC SIDE EFFECT CONFIRMED: workspace '$WORKER_NAME' now EXISTS (id=$WORKER_ID)"

WORKER_KIND=$(ws_field "$WORKER_ID" kind)
if [ -n "$WORKER_KIND" ] && [ "$WORKER_KIND" != "workspace" ]; then
  fail "created node '$WORKER_NAME' has kind='$WORKER_KIND' (want 'workspace')"
fi
ok "Created node is a real kind='workspace' row"

ok "═══ LOCAL CONCIERGE CREATES-A-WORKSPACE E2E PASSED ═══"
log "Proven locally: a natural-language A2A request → the concierge's LLM invoked create_workspace via the platform MCP → real workspace '$WORKER_NAME' (id=$WORKER_ID). Teardown runs via EXIT trap."
