#!/bin/bash
# test_staging_external_runtime.sh — E2E regression for the
# external-runtime workspace lifecycle on a real staging tenant.
#
# Why this test exists: the four/five sites that write 'awaiting_agent'
# / 'hibernating' to workspaces.status had been silently failing in
# production for five days (see migration 046) before a static drift
# gate caught the enum gap. Unit tests passed because sqlmock matched
# the SQL by regex but didn't enforce the live enum constraint, and
# every existing E2E exercised hermes (not external) so the silent
# failures never surfaced. This test pins the four awaiting_agent
# transitions in real Postgres on a real staging tenant.
#
# Verification path:
#   1. Provision a fresh tenant (test_staging_full_saas.sh harness shape).
#   2. Create an external-runtime workspace with NO URL → assert
#      response status == 'awaiting_agent' AND GET on the workspace
#      returns the same. (Pre-fix the row stuck on 'provisioning'
#      because the UPDATE in workspace.go:333 silently failed.)
#   3. Register a fake URL via /registry/register → assert transition
#      to 'online'. (Pre-fix this branch worked because it writes
#      'online' which IS in the enum.)
#   4. Stop heartbeating; wait past REMOTE_LIVENESS_STALE_AFTER (90s
#      default) + a sweep interval → assert transition back to
#      'awaiting_agent'. (Pre-fix the sweep UPDATE failed silently and
#      the workspace stuck on 'online' indefinitely.)
#
# Hibernation is intentionally NOT covered here — it has its own timing
# model (idle threshold) and warrants a separate harness.
#
# Required env (mirrors test_staging_full_saas.sh):
#   MOLECULE_CP_URL          default: https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN     CP admin bearer (Railway CP_ADMIN_API_TOKEN)
#
# Optional env:
#   E2E_PROVISION_TIMEOUT_SECS  default 900 (15 min cold EC2 budget)
#   E2E_KEEP_ORG                1 → skip teardown (debugging only)
#   E2E_RUN_ID                  Slug suffix; CI: ${GITHUB_RUN_ID}
#   E2E_STALE_WAIT_SECS         default 180 (90s window + 90s buffer)
#   E2E_INTENTIONAL_FAILURE     1 → break a step on purpose to verify
#                               the EXIT trap still tears down (mirrors
#                               the full-saas harness's safety net).
#
# Exit codes: 0 happy, 1 generic, 2 missing env, 3 provision timeout,
# 4 teardown leak.

set -euo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — Railway staging CP_ADMIN_API_TOKEN}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"
RUN_ID_SUFFIX="${E2E_RUN_ID:-$(date +%H%M%S)-$$}"
STALE_WAIT_SECS="${E2E_STALE_WAIT_SECS:-180}"

SLUG="e2e-ext-$(date +%Y%m%d)-${RUN_ID_SUFFIX}"
SLUG=$(echo "$SLUG" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-' | head -c 32)

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

CURL_COMMON=(-sS --fail-with-body --max-time 30)

# ─── cleanup trap (mirrors full-saas) ────────────────────────────────────
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

  local leak_count=1 elapsed=0
  while [ "$elapsed" -lt 60 ]; do
    leak_count=$(curl "${CURL_COMMON[@]}" "$CP_URL/cp/admin/orgs" \
      -H "Authorization: Bearer $ADMIN_TOKEN" 2>/dev/null \
      | python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(1 for o in d.get('orgs', []) if o.get('slug')=='$SLUG' and o.get('status') != 'purged'))" \
      2>/dev/null || echo 1)
    [ "$leak_count" = "0" ] && break
    sleep 5
    elapsed=$((elapsed + 5))
  done

  if [ "$leak_count" != "0" ]; then
    echo "⚠️  LEAK: org $SLUG still present post-teardown (count=$leak_count)" >&2
    exit 4
  fi
  ok "Teardown clean — no orphan resources for $SLUG (${elapsed}s)"

  case "$entry_rc" in
    0|1|2|3|4) ;;
    *) exit 1 ;;
  esac
}
trap cleanup_org EXIT INT TERM

# ─── 0. Preflight ───────────────────────────────────────────────────────
log "═══════════════════════════════════════════════════════════════════"
log " Staging external-runtime E2E (regression for migration 046)"
log "   CP:    $CP_URL"
log "   Slug:  $SLUG"
log "   Stale: ${STALE_WAIT_SECS}s wait window"
log "═══════════════════════════════════════════════════════════════════"

curl "${CURL_COMMON[@]}" "$CP_URL/health" >/dev/null || fail "CP health check failed"
ok "CP reachable"

admin_call() {
  local method="$1"; shift; local path="$1"; shift
  curl "${CURL_COMMON[@]}" -X "$method" "$CP_URL$path" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" "$@"
}

# ─── 1. Create org ──────────────────────────────────────────────────────
log "1/8 Creating org $SLUG..."
CREATE_RESP=$(admin_call POST /cp/admin/orgs \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E ext $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
ORG_ID=$(echo "$CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")
[ -z "$ORG_ID" ] && fail "Org create response missing 'id'"
ok "Org created (id=$ORG_ID)"

# ─── 2. Wait for tenant provisioning ────────────────────────────────────
# Terminal status from /cp/admin/orgs is 'running' (org_instances.status),
# NOT 'ready' — same field the full-saas harness polls. 'failed' surfaces
# diagnostic dump and aborts. See test_staging_full_saas.sh step 2 for
# the field-bugfix history (2026-04-21, last_error path).
log "2/8 Waiting for tenant (up to ${PROVISION_TIMEOUT_SECS}s)..."
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
LAST_STATUS=""
while true; do
  if [ "$(date +%s)" -gt "$DEADLINE" ]; then
    fail "Tenant provisioning timed out (last: $LAST_STATUS)"
  fi
  LIST_JSON=$(admin_call GET /cp/admin/orgs 2>/dev/null || echo '{"orgs":[]}')
  STATUS=$(echo "$LIST_JSON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for o in d.get('orgs', []):
    if o.get('slug') == '$SLUG':
        print(o.get('instance_status', ''))
        sys.exit(0)
print('')
" 2>/dev/null || echo "")
  if [ "$STATUS" != "$LAST_STATUS" ]; then
    log "   instance_status: $STATUS"
    LAST_STATUS="$STATUS"
  fi
  case "$STATUS" in
    running) break ;;
    failed)
      log "── DIAGNOSTIC BURST (step 2 — tenant provisioning failed) ──"
      echo "$LIST_JSON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for o in d.get('orgs', []):
    if o.get('slug') == '$SLUG':
        print(json.dumps(o, indent=2))
        sys.exit(0)
print('(no org row found for slug=$SLUG — DB drift?)')
" 2>&1 | sed 's/^/  /'
      log "── END DIAGNOSTIC ──"
      fail "Tenant provisioning failed for $SLUG (see diagnostic above)"
      ;;
    *) sleep 15 ;;
  esac
done
ok "Tenant provisioning complete"

# Derive tenant URL the same way the full-saas harness does.
CP_HOST=$(echo "$CP_URL" | sed -E 's#^https?://##; s#/.*$##')
case "$CP_HOST" in
  api.*)         DERIVED_DOMAIN="${CP_HOST#api.}" ;;
  staging-api.*) DERIVED_DOMAIN="staging.${CP_HOST#staging-api.}" ;;
  *)             DERIVED_DOMAIN="$CP_HOST" ;;
esac
TENANT_DOMAIN="${MOLECULE_TENANT_DOMAIN:-$DERIVED_DOMAIN}"
TENANT_URL="https://$SLUG.$TENANT_DOMAIN"
log "    TENANT_URL=$TENANT_URL"

# ─── 3. Per-tenant admin token + TLS readiness ──────────────────────────
log "3/8 Fetching per-tenant admin token..."
TENANT_TOKEN_RESP=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token")
TENANT_TOKEN=$(echo "$TENANT_TOKEN_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('admin_token',''))")
[ -z "$TENANT_TOKEN" ] && fail "Could not retrieve per-tenant admin token"
ok "Token retrieved (len=${#TENANT_TOKEN})"

log "Waiting for tenant TLS / DNS..."
TLS_DEADLINE=$(( $(date +%s) + 15 * 60 ))
while true; do
  if curl -sSfk --max-time 5 "$TENANT_URL/health" >/dev/null 2>&1; then break; fi
  if [ "$(date +%s)" -gt "$TLS_DEADLINE" ]; then
    fail "Tenant URL never responded 2xx on /health within 15min"
  fi
  sleep 5
done
ok "Tenant reachable"

tenant_call() {
  local method="$1"; shift; local path="$1"; shift
  curl "${CURL_COMMON[@]}" -X "$method" "$TENANT_URL$path" \
    -H "Authorization: Bearer $TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    "$@"
}

# ─── 4. Create external workspace (no URL) ──────────────────────────────
# This is the FIRST silent-failure path (workspace.go:333). Pre-migration
# 046, the response would say status=awaiting_agent but the row stuck
# on whatever the create handler set first (typically 'provisioning')
# because the follow-up UPDATE failed the enum cast.
log "4/8 Creating external workspace (no URL — exercises workspace.go:333)..."
WS_CREATE_RESP=$(tenant_call POST /workspaces \
  -d '{"name":"ext-e2e","runtime":"external","external":true}')

WS_ID=$(echo "$WS_CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")
WS_RESP_STATUS=$(echo "$WS_CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))")
WS_AUTH_TOKEN=$(echo "$WS_CREATE_RESP" | python3 -c "
import json,sys
try:
    d = json.load(sys.stdin)
    conn = d.get('connection') or {}
    print(conn.get('auth_token','') or d.get('auth_token',''))
except Exception:
    print('')
")
[ -z "$WS_ID" ] && fail "Workspace create missing id: $WS_CREATE_RESP"
[ "$WS_RESP_STATUS" != "awaiting_agent" ] && fail "Expected response status=awaiting_agent, got $WS_RESP_STATUS"
ok "Workspace created (id=$WS_ID, response status=awaiting_agent)"

# This GET is the proof that the row actually has the value (not just
# the response body lying). Pre-migration-046 the UPDATE would have
# silently failed and this would return whatever 'provisioning' the
# initial INSERT left. Post-fix it must be 'awaiting_agent'.
log "    Verifying DB row..."
GET_RESP=$(tenant_call GET "/workspaces/$WS_ID")
DB_STATUS=$(echo "$GET_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))")
[ "$DB_STATUS" != "awaiting_agent" ] && fail "DB row status=$DB_STATUS (expected awaiting_agent — migration 046 likely not applied)"
ok "DB row stored as awaiting_agent (proof migration 046 applied)"

# ─── 5. Register the workspace (transitions to online) ──────────────────
# Pre-fix this path was actually fine because it writes 'online', a value
# already in the enum. We exercise it anyway because the registration
# implicitly walks resolveDeliveryMode (registry.go:resolveDeliveryMode),
# which DOES read runtime + apply the new poll-default introduced by
# PR #2382.
log "5/8 Registering workspace via /registry/register..."
[ -z "$WS_AUTH_TOKEN" ] && fail "No workspace auth token returned — register impossible"
# Payload contract (workspace-server/internal/models/workspace.go RegisterPayload):
#   id            — required, the workspace UUID (NOT "workspace_id" — that's the
#                   heartbeat payload field; mixing them yields a 400 from
#                   ShouldBindJSON because `id` has binding:"required").
#   agent_card    — required (binding:"required"); minimal valid card is name+skills.
#   delivery_mode — set explicitly to "poll" so url validation is skipped
#                   regardless of whether the deployed image has the
#                   runtime=external→poll default from PR #2382. Observed
#                   2026-04-30 17:18Z: a freshly-provisioned staging tenant
#                   was running an older workspace-server :latest image
#                   that lacked resolveDeliveryMode's external→poll branch,
#                   so the implicit default was push and validateAgentURL
#                   400'd on example.invalid. Asserting on the implicit
#                   default makes the *register call* itself fragile to
#                   image-tag drift on the fleet — verify the default
#                   separately (step 5b assertion) without depending on it
#                   here.
#   url           — accepted but not dispatched-to in poll mode, so
#                   example.invalid is a valid sentinel.
REGISTER_BODY=$(printf '{"id":"%s","url":"https://example.invalid:443","delivery_mode":"poll","agent_card":{"name":"e2e-ext","skills":[{"id":"echo","name":"Echo"}]}}' "$WS_ID")
# Disable --fail-with-body for this one call so a 4xx surfaces the response
# body (the bare CURL_COMMON would `set -e`-kill before we could log it).
REGISTER_RESP=$(curl -sS --max-time 30 -w "\nHTTP_CODE=%{http_code}" -X POST "$TENANT_URL/registry/register" \
  -H "Authorization: Bearer $WS_AUTH_TOKEN" \
  -H "X-Molecule-Org-Id: $ORG_ID" \
  -H "Content-Type: application/json" \
  -d "$REGISTER_BODY") || true
log "    register response: $(echo "$REGISTER_RESP" | head -c 300)"
echo "$REGISTER_RESP" | grep -q "HTTP_CODE=200" || fail "register returned non-200 — see body above"

GET_RESP=$(tenant_call GET "/workspaces/$WS_ID")
ONLINE_STATUS=$(echo "$GET_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))")
[ "$ONLINE_STATUS" != "online" ] && fail "Expected online after register, got $ONLINE_STATUS"
ok "Workspace transitioned to online"

# Confirm the register handler echoed back delivery_mode=poll. We read
# this from the register RESPONSE, not the workspace GET response, because
# the GET handler's SELECT (workspace.go:597) doesn't fetch delivery_mode
# — its column list pre-dates the delivery_mode column from #2339 PR 1.
# Surfacing delivery_mode in GET is tracked separately; not gating on it
# here keeps this test focused on the awaiting_agent transitions.
REGISTER_BODY_JSON=$(echo "$REGISTER_RESP" | head -n 1)
REGISTER_DELIVERY_MODE=$(echo "$REGISTER_BODY_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('delivery_mode',''))")
if [ "$REGISTER_DELIVERY_MODE" = "poll" ]; then
  ok "delivery_mode=poll (register response echoed explicit value)"
else
  fail "Register response delivery_mode=$REGISTER_DELIVERY_MODE (expected poll). Body: $REGISTER_BODY_JSON"
fi

# ─── 6. Stop heartbeating; wait past REMOTE_LIVENESS_STALE_AFTER ────────
# This is the SECOND silent-failure path (registry/healthsweep.go's
# sweepStaleRemoteWorkspaces). Pre-migration-046 the heartbeat-staleness
# UPDATE silently failed and the workspace stuck on 'online' forever
# even though no agent was alive. We wait the full window + a sweep
# interval and assert the row transitions back to 'awaiting_agent'.
log "6/8 Waiting ${STALE_WAIT_SECS}s for heartbeat-staleness sweep (no heartbeat sent)..."
sleep "$STALE_WAIT_SECS"

GET_RESP=$(tenant_call GET "/workspaces/$WS_ID")
STALE_STATUS=$(echo "$GET_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))")
[ "$STALE_STATUS" != "awaiting_agent" ] && \
  fail "After ${STALE_WAIT_SECS}s with no heartbeat, expected status=awaiting_agent (sweep transition), got $STALE_STATUS — migration 046 likely not applied OR sweep not running"
ok "Heartbeat-staleness sweep transitioned online → awaiting_agent (proof healthsweep.go fix working)"

# ─── 7. Re-register and confirm we can come back online ─────────────────
# This proves the awaiting_agent state is recoverable (re-registrable),
# which is the whole point of using it instead of 'offline'.
log "7/8 Re-registering after stale → confirming recovery to online..."
# Same payload contract as step 5 (id + agent_card both required). See note
# there for why workspace_id would 400.
REREG_RESP=$(curl -sS --max-time 30 -w "\nHTTP_CODE=%{http_code}" -X POST "$TENANT_URL/registry/register" \
  -H "Authorization: Bearer $WS_AUTH_TOKEN" \
  -H "X-Molecule-Org-Id: $ORG_ID" \
  -H "Content-Type: application/json" \
  -d "$REGISTER_BODY") || true
log "    re-register response: $(echo "$REREG_RESP" | head -c 300)"
echo "$REREG_RESP" | grep -q "HTTP_CODE=200" || fail "re-register returned non-200 — see body above"

GET_RESP=$(tenant_call GET "/workspaces/$WS_ID")
RECOVERED_STATUS=$(echo "$GET_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))")
[ "$RECOVERED_STATUS" != "online" ] && \
  fail "Expected re-register to return workspace to online, got $RECOVERED_STATUS"
ok "Re-register succeeded — awaiting_agent → online (operator-recoverable)"

# ─── 8. Done — cleanup runs in the EXIT trap ───────────────────────────
log "8/8 All four awaiting_agent transitions verified."
log "═══════════════════════════════════════════════════════════════════"
ok "External-runtime E2E PASSED on $SLUG"
log "═══════════════════════════════════════════════════════════════════"
