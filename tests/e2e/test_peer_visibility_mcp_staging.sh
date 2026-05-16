#!/usr/bin/env bash
# Staging E2E — fresh-provision peer-visibility gate via the LITERAL MCP path.
#
# WHY THIS EXISTS
# ---------------
# Hermes and OpenClaw were repeatedly reported "fleet-verified / cascade-
# complete" because the *proxy* signals were green:
#   - registry-registration + heartbeat (Hermes), and
#   - model round-trip 200 (OpenClaw).
# But a freshly-provisioned workspace, asked on canvas "can you see your
# peers", actually FAILS:
#   - Hermes: 401 on the molecule MCP `list_peers` call,
#   - OpenClaw: falls back to native `sessions_list`, sees no platform peers.
# Tasks #142/#159 were even marked "completed" under this same proxy flaw.
#
# This script codifies the LITERAL user-facing path so it can never silently
# regress: it provisions a brand-new throwaway org + sibling workspaces via
# the real control-plane provisioning path, then for each runtime that should
# have platform peer-visibility it drives the EXACT MCP call the canvas agent
# makes — `POST /workspaces/:id/mcp` JSON-RPC tools/call name=list_peers,
# authenticated by that workspace's own bearer token through the real
# WorkspaceAuth + MCPRateLimiter middleware chain. It then asserts:
#   (1) HTTP 200,
#   (2) JSON-RPC `result` present (NOT an `error` object — a -32000
#       "tool call failed" or a 401 from WorkspaceAuth fails here),
#   (3) the returned peer set CONTAINS the other provisioned sibling
#       workspace IDs — not an empty list, not a native-sessions fallback.
#
# This is NOT a proxy. It does not look at a registry row, /health, the
# heartbeat table, or `GET /registry/:id/peers`. It drives the byte-for-byte
# JSON-RPC envelope that mcp_molecule_list_peers issues from a real agent.
#
# It is written to FAIL on today's broken Hermes/OpenClaw behavior and go
# green only when the in-flight root-cause fixes (Hermes-401, OpenClaw MCP
# wiring) actually land. That is the point: it is the objective proof gate.
#
# AUTH MODEL (mirrors tests/e2e/test_staging_full_saas.sh)
# --------------------------------------------------------
#   Single MOLECULE_ADMIN_TOKEN (= CP_ADMIN_API_TOKEN on Railway staging)
#   drives: POST /cp/admin/orgs (provision), GET
#   /cp/admin/orgs/:slug/admin-token (per-tenant token), DELETE
#   /cp/admin/tenants/:slug (teardown). The per-tenant admin token drives
#   tenant workspace creation; each workspace's OWN auth_token (returned by
#   POST /workspaces) drives its MCP call.
#
# Required env:
#   MOLECULE_ADMIN_TOKEN   CP admin bearer — Railway staging CP_ADMIN_API_TOKEN
# Optional env:
#   MOLECULE_CP_URL        default https://staging-api.moleculesai.app
#   E2E_RUN_ID             slug suffix; CI passes ${GITHUB_RUN_ID}
#   PV_RUNTIMES            space list; default "hermes openclaw claude-code"
#   E2E_PROVISION_TIMEOUT_SECS  default 1800 (hermes/openclaw cold EC2 budget)
#   E2E_MINIMAX_API_KEY / E2E_ANTHROPIC_API_KEY / E2E_OPENAI_API_KEY
#                          LLM provider key injected so the runtime can boot
#   E2E_KEEP_ORG           1 → skip teardown (local debugging only)
#
# Exit codes:
#   0  every runtime saw its peers via the literal MCP call
#   1  generic failure
#   2  missing required env
#   3  provisioning timed out
#   4  teardown left orphan resources
#   10 peer-visibility regression reproduced (the gate firing as designed)

set -uo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — Railway staging CP_ADMIN_API_TOKEN}"
RUN_ID_SUFFIX="${E2E_RUN_ID:-$(date +%H%M%S)-$$}"
PV_RUNTIMES="${PV_RUNTIMES:-hermes openclaw claude-code}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-1800}"

# Slug MUST start with 'e2e-' so the sweep-stale-e2e-orgs safety net
# (EPHEMERAL_PREFIXES) catches any leak this run fails to tear down.
SLUG="e2e-pv-$(date +%Y%m%d)-${RUN_ID_SUFFIX}"
SLUG=$(echo "$SLUG" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-' | head -c 32)

ORG_ID=""
TENANT_URL=""
TENANT_TOKEN=""

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

admin_call() {
  local method="$1" path="$2"; shift 2
  curl -sS -X "$method" "$CP_URL$path" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" "$@"
}
tenant_call() {
  local method="$1" path="$2"; shift 2
  curl -sS -X "$method" "$TENANT_URL$path" \
    -H "Authorization: Bearer $TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    -H "Content-Type: application/json" "$@"
}

# ─── Scoped teardown ───────────────────────────────────────────────────
# Deletes ONLY the org this run created (DELETE /cp/admin/tenants/$SLUG
# with the {"confirm":$SLUG} fat-finger guard). Never a cluster-wide
# sweep — honors feedback_cleanup_after_each_test and
# feedback_never_run_cluster_cleanup_tests_on_live_platform. The
# workflow's always() step + sweep-stale-e2e-orgs are the outer nets.
teardown() {
  local rc=$?
  set +e
  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    echo ""
    log "[teardown] E2E_KEEP_ORG=1 — leaving $SLUG for debugging (REMEMBER TO DELETE)"
    exit $rc
  fi
  echo ""
  log "[teardown] DELETE /cp/admin/tenants/$SLUG (scoped to this run only)"
  admin_call DELETE "/cp/admin/tenants/$SLUG" --max-time 120 \
    -d "{\"confirm\":\"$SLUG\"}" >/dev/null 2>&1
  for j in $(seq 1 24); do
    LIST=$(admin_call GET "/cp/admin/orgs?limit=500" 2>/dev/null)
    LEAK=$(echo "$LIST" | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: print(1); sys.exit(0)
orgs = d if isinstance(d, list) else d.get('orgs', [])
print(sum(1 for o in orgs if o.get('slug') == '$SLUG' and o.get('instance_status') not in ('purged',) and o.get('status') != 'purged'))
" 2>/dev/null || echo 1)
    if [ "$LEAK" = "0" ]; then
      log "[teardown] ✓ $SLUG purged (after ${j}x5s)"
      exit $rc
    fi
    sleep 5
  done
  echo "::warning::[teardown] $SLUG still present after 120s — sweep-stale-e2e-orgs will catch it within MAX_AGE_MINUTES" >&2
  [ $rc -eq 0 ] && rc=4
  exit $rc
}
trap teardown EXIT INT TERM

# ─── 1. Provision the throwaway org ────────────────────────────────────
log "1/6 POST /cp/admin/orgs — slug=$SLUG"
CREATE=$(admin_call POST /cp/admin/orgs \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E peer-visibility $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
ORG_ID=$(echo "$CREATE" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$ORG_ID" ] || fail "org creation failed: $(echo "$CREATE" | head -c 300)"
log "    ORG_ID=$ORG_ID"

# ─── 2. Wait for tenant EC2 + DNS ──────────────────────────────────────
log "2/6 waiting for tenant instance_status=running (cold EC2 + cloudflared)..."
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
while true; do
  [ "$(date +%s)" -gt "$DEADLINE" ] && fail "tenant never came up within ${PROVISION_TIMEOUT_SECS}s"
  STATUS=$(admin_call GET "/cp/admin/orgs?limit=500" 2>/dev/null | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: sys.exit(0)
orgs = d if isinstance(d, list) else d.get('orgs', [])
for o in orgs:
    if o.get('slug') == '$SLUG':
        print(o.get('instance_status') or o.get('status') or 'unknown'); break
" 2>/dev/null)
  case "$STATUS" in running|online|ready) break ;; esac
  sleep 10
done
log "    tenant status=$STATUS"

# ─── 3. Per-tenant admin token + tenant URL ────────────────────────────
log "3/6 fetching per-tenant admin token..."
TT_RESP=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token")
TENANT_TOKEN=$(echo "$TT_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null)
[ -n "$TENANT_TOKEN" ] || fail "tenant token fetch failed: $(echo "$TT_RESP" | head -c 200)"

CP_HOST=$(echo "$CP_URL" | sed -E 's#^https?://##; s#/.*$##')
case "$CP_HOST" in
  api.*)         DERIVED_DOMAIN="${CP_HOST#api.}" ;;
  staging-api.*) DERIVED_DOMAIN="staging.${CP_HOST#staging-api.}" ;;
  *)             DERIVED_DOMAIN="$CP_HOST" ;;
esac
TENANT_URL="https://${SLUG}.${DERIVED_DOMAIN}"
log "    tenant url: $TENANT_URL"

log "3b. waiting for tenant /health (TLS/DNS, up to 10min)..."
for i in $(seq 1 120); do
  curl -fsS "$TENANT_URL/health" -m 5 -k >/dev/null 2>&1 && { log "    /health ok (attempt $i)"; break; }
  sleep 5
done

# ─── 4. Provision the parent + one sibling per runtime under test ──────
# Inject the LLM provider key so each runtime can authenticate at boot.
# Priority: MiniMax → direct-Anthropic → OpenAI (mirrors
# test_staging_full_saas.sh's secrets-injection chain).
SECRETS_JSON='{}'
if [ -n "${E2E_MINIMAX_API_KEY:-}" ]; then
  SECRETS_JSON=$(python3 -c "import json,os;k=os.environ['E2E_MINIMAX_API_KEY'];print(json.dumps({'ANTHROPIC_BASE_URL':'https://api.minimax.io/anthropic','ANTHROPIC_AUTH_TOKEN':k,'MINIMAX_API_KEY':k}))")
elif [ -n "${E2E_ANTHROPIC_API_KEY:-}" ]; then
  SECRETS_JSON=$(python3 -c "import json,os;k=os.environ['E2E_ANTHROPIC_API_KEY'];print(json.dumps({'ANTHROPIC_API_KEY':k}))")
elif [ -n "${E2E_OPENAI_API_KEY:-}" ]; then
  SECRETS_JSON=$(python3 -c "import json,os;k=os.environ['E2E_OPENAI_API_KEY'];print(json.dumps({'OPENAI_API_KEY':k,'OPENAI_BASE_URL':'https://api.openai.com/v1','MODEL_PROVIDER':'openai:gpt-4o','HERMES_INFERENCE_PROVIDER':'custom','HERMES_CUSTOM_BASE_URL':'https://api.openai.com/v1','HERMES_CUSTOM_API_KEY':k,'HERMES_CUSTOM_API_MODE':'chat_completions'}))")
fi

log "4/6 provisioning parent (claude-code) + one sibling per runtime under test..."
P_RESP=$(tenant_call POST /workspaces \
  -d "{\"name\":\"pv-parent\",\"runtime\":\"claude-code\",\"tier\":3,\"secrets\":$SECRETS_JSON}")
PARENT_ID=$(echo "$P_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$PARENT_ID" ] || fail "parent create failed: $(echo "$P_RESP" | head -c 300)"
log "    PARENT_ID=$PARENT_ID"

# WS_IDS[runtime]=id ; WS_TOKENS[runtime]=auth_token (the MCP bearer)
declare -A WS_IDS WS_TOKENS
ALL_WS_IDS="$PARENT_ID"
for rt in $PV_RUNTIMES; do
  R=$(tenant_call POST /workspaces \
    -d "{\"name\":\"pv-$rt\",\"runtime\":\"$rt\",\"tier\":2,\"parent_id\":\"$PARENT_ID\",\"secrets\":$SECRETS_JSON}")
  WID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
  # auth_token is top-level for container runtimes; external-like nest it
  # under connection.auth_token (verified vs staging response shape).
  WTOK=$(echo "$R" | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
print(d.get('auth_token') or d.get('connection', {}).get('auth_token') or '')
" 2>/dev/null)
  [ -n "$WID" ] || fail "$rt workspace create failed: $(echo "$R" | head -c 300)"
  [ -n "$WTOK" ] || fail "$rt workspace did not return an auth_token — cannot drive its MCP call (resp: $(echo "$R" | head -c 300))"
  WS_IDS[$rt]="$WID"
  WS_TOKENS[$rt]="$WTOK"
  ALL_WS_IDS="$ALL_WS_IDS $WID"
  log "    $rt → $WID"
done

# ─── 5. Wait for every sibling online ──────────────────────────────────
log "5/6 waiting for all workspaces status=online (up to ${PROVISION_TIMEOUT_SECS}s — cold boot)..."
WS_DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
for rt in $PV_RUNTIMES; do
  wid="${WS_IDS[$rt]}"
  LAST=""
  while true; do
    [ "$(date +%s)" -gt "$WS_DEADLINE" ] && fail "$rt ($wid) never reached online (last=$LAST)"
    S=$(tenant_call GET "/workspaces/$wid" 2>/dev/null | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: sys.exit(0)
w = d.get('workspace') if isinstance(d.get('workspace'), dict) else d
print(w.get('status') or '')
" 2>/dev/null)
    [ "$S" != "$LAST" ] && { log "    $rt → $S"; LAST="$S"; }
    case "$S" in
      online) break ;;
      failed) sleep 10 ;;   # transient: bootstrap-watcher 5-min deadline, heartbeat recovers
      *)      sleep 10 ;;
    esac
  done
  ok "    $rt online"
done

# ─── 6. THE GATE — literal mcp_molecule_list_peers via POST /:id/mcp ────
# This is the byte-for-byte user-facing call. NOT GET /registry/:id/peers,
# NOT /health, NOT the heartbeat table. JSON-RPC 2.0 tools/call,
# name=list_peers, authenticated by the workspace's OWN bearer token
# through WorkspaceAuth + MCPRateLimiter.
log "6/6 driving the LITERAL list_peers MCP call per runtime..."
echo ""
RPC_BODY='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_peers","arguments":{}}}'
REGRESSED=0
declare -A VERDICT

for rt in $PV_RUNTIMES; do
  wid="${WS_IDS[$rt]}"
  wtok="${WS_TOKENS[$rt]}"
  # The expected peer set = every OTHER provisioned workspace (parent +
  # the sibling runtimes), excluding the caller itself.
  EXPECT_IDS=$(echo "$ALL_WS_IDS" | tr ' ' '\n' | grep -v "^${wid}$" | grep -v '^$')

  set +e
  RESP=$(curl -sS -X POST "$TENANT_URL/workspaces/$wid/mcp" \
    -H "Authorization: Bearer $wtok" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    -H "Content-Type: application/json" \
    -d "$RPC_BODY" \
    -o /tmp/pv_mcp_body.json -w "%{http_code}" 2>/dev/null)
  set -e
  HTTP_CODE="$RESP"
  BODY=$(cat /tmp/pv_mcp_body.json 2>/dev/null || echo '')

  echo "--- $rt (ws=$wid) ---"
  echo "    HTTP $HTTP_CODE"
  echo "    body: $(echo "$BODY" | head -c 600)"

  # (1) HTTP 200 — a 401 (WorkspaceAuth reject, the Hermes symptom) fails here.
  if [ "$HTTP_CODE" != "200" ]; then
    echo "  ✗ $rt: list_peers MCP call returned HTTP $HTTP_CODE (expected 200)"
    VERDICT[$rt]="FAIL(http=$HTTP_CODE)"
    REGRESSED=1
    continue
  fi

  # (2) JSON-RPC result present, not an error object.
  PARSE=$(echo "$BODY" | python3 -c "
import sys, json
expect = set(filter(None, '''$EXPECT_IDS'''.split()))
try:
    d = json.load(sys.stdin)
except Exception as e:
    print('PARSE_ERROR:' + str(e)); sys.exit(0)
if isinstance(d, dict) and d.get('error') is not None:
    print('RPC_ERROR:' + json.dumps(d['error'])[:200]); sys.exit(0)
res = d.get('result') if isinstance(d, dict) else None
if res is None:
    print('NO_RESULT'); sys.exit(0)
# MCP tools/call result shape: {content:[{type:text,text:'<json or prose>'}]}
text = ''
if isinstance(res, dict):
    for c in res.get('content', []):
        if c.get('type') == 'text':
            text += c.get('text', '')
text_l = text.lower()
# Native-sessions fallback signature (the OpenClaw symptom): the agent
# answered from its own runtime session list, not the platform peer set.
if 'sessions_list' in text_l or 'no platform peers' in text_l or 'native session' in text_l:
    print('NATIVE_FALLBACK:' + text[:200]); sys.exit(0)
# The expected sibling IDs must literally appear in the returned peer text.
found = sorted(i for i in expect if i in text)
missing = sorted(expect - set(found))
if not expect:
    print('NO_EXPECTED_PEERS_CONFIGURED'); sys.exit(0)
if missing:
    print('MISSING_PEERS:found=%d/%d missing=%s' % (len(found), len(expect), ','.join(m[:8] for m in missing)))
    sys.exit(0)
print('OK:found=%d/%d' % (len(found), len(expect)))
" 2>/dev/null)

  case "$PARSE" in
    OK:*)
      echo "  ✓ $rt: list_peers returned 200 and contains all expected peers ($PARSE)"
      VERDICT[$rt]="OK"
      ;;
    NATIVE_FALLBACK:*)
      echo "  ✗ $rt: list_peers fell back to NATIVE sessions — sees no platform peers ($PARSE)"
      VERDICT[$rt]="FAIL(native-fallback)"
      REGRESSED=1
      ;;
    RPC_ERROR:*|NO_RESULT|PARSE_ERROR:*)
      echo "  ✗ $rt: list_peers MCP call did not return a usable result ($PARSE)"
      VERDICT[$rt]="FAIL(rpc=$PARSE)"
      REGRESSED=1
      ;;
    MISSING_PEERS:*)
      echo "  ✗ $rt: list_peers returned 200 but peer set is wrong/empty ($PARSE)"
      VERDICT[$rt]="FAIL(peers=$PARSE)"
      REGRESSED=1
      ;;
    *)
      echo "  ✗ $rt: unexpected verdict '$PARSE'"
      VERDICT[$rt]="FAIL(unknown)"
      REGRESSED=1
      ;;
  esac
  echo ""
done

echo "=== SUMMARY — fresh-provision peer-visibility (literal MCP list_peers) ==="
for rt in $PV_RUNTIMES; do
  printf '  %-14s %s\n' "$rt" "${VERDICT[$rt]:-NO_RUN}"
done
echo ""

if [ "$REGRESSED" -ne 0 ]; then
  echo "✗ GATE FAILED — at least one runtime cannot see its peers via the"
  echo "  literal mcp_molecule_list_peers call. This is the real user-facing"
  echo "  failure the proxy signals (registry row / heartbeat / model 200)"
  echo "  were hiding. Expected RED until the Hermes-401 + OpenClaw-MCP-wiring"
  echo "  root-cause fixes land; goes green only when they actually do."
  exit 10
fi

ok "GATE PASSED — every runtime under test sees its platform peers via the literal MCP call."
exit 0
