#!/usr/bin/env bash
# Real-staging E2E for the concierge user_tasks primitive (Feature 3 of the
# concierge / platform-agent set). Exercises the FULL agent→user "ask" contract
# both surfaces expose, END-TO-END against a real EC2-backed staging tenant:
#
#   REST (per-workspace, tenant-admin-token authenticated):
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
#   Cross-workspace authz: workspace B cannot PATCH/DELETE workspace A's task
#   (the user_tasks handler scopes every mutation by the URL :id, so a B-path
#   call against an A-owned task 404s — the same scoping the local
#   test_user_tasks_e2e.sh pins, here proven over the real tenant ws-server).
#
# Why a real-staging sibling to the LOCAL test_user_tasks_e2e.sh: the local one
# runs against a dev workspace-server with external/in-memory workspaces. This
# one provisions a REAL throwaway org + tenant (same CP-admin scaffolding as
# test_staging_full_saas.sh) and drives the user_tasks surfaces through the live
# tenant auth chain (TenantGuard + WorkspaceAuth + Cloudflare edge) — the exact
# path a canvas concierge agent hits in production. It REUSES the staging
# harness's env contract, org-provision/teardown shape, _lib.sh helpers, and the
# AWS-leak-check lib, so the org lifecycle scaffolding is shared, not duplicated.
#
# NOTE: user_tasks is a pure DB/handler primitive — no LLM container is needed.
# We DO NOT wait for any workspace to boot online (no MINIMAX/ANTHROPIC key
# required), which keeps this test fast and decoupled from EC2 cold-boot flake.
# Workspaces are created in 'external' mode so the tenant ws-server registers
# the row without provisioning an EC2 (no leak beyond the org teardown).
#
# Required env (same contract as test_staging_full_saas.sh):
#   MOLECULE_CP_URL        default: https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   CP admin bearer — Railway staging CP_ADMIN_API_TOKEN
#
# Optional env:
#   E2E_PROVISION_TIMEOUT_SECS   default 900 (15 min cold tenant EC2 budget)
#   E2E_KEEP_ORG                 1 → skip teardown (debugging only)
#   E2E_RUN_ID                   slug suffix; CI: ${GITHUB_RUN_ID}-${RUN_ATTEMPT}
#   E2E_AWS_LEAK_CHECK           auto (default) | required | off
#   E2E_AWS_TERMINATE_LEAKS      1 → terminate slug-tagged leaked EC2 on exit
#
# Exit codes:
#   0  happy path
#   1  generic / assertion failure
#   2  missing required env
#   3  provisioning timed out
#   4  teardown left orphan resources
set -euo pipefail

# _lib.sh gives us sanitize/admin-auth conventions shared across the suite.
# shellcheck disable=SC1091
# shellcheck source=_lib.sh
source "$(dirname "$0")/_lib.sh"
# AWS-leak-check lib — same teardown leak assertion the full-SaaS harness uses.
# shellcheck disable=SC1091
# shellcheck source=lib/aws_leak_check.sh
source "$(dirname "$0")/lib/aws_leak_check.sh"

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — Railway staging CP_ADMIN_API_TOKEN}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"
# RUN_ID_SUFFIX removed (core#2782 follow-up shellcheck): the slug now
# comes from make_collision_proof_slug below; the old suffix var is dead.

# Collision-proof slug (core#2782). The prior `head -c 32` truncation
# dropped the run_attempt suffix and let two parallel/retry runs
# collide (POST /cp/admin/orgs 409). The helper appends a random
# 8-char uuid so every run gets a unique slug regardless of how
# the workflow composes E2E_RUN_ID. The `source` + `assert` run
# AFTER log/fail/ok are defined below so the assert can call `fail`
# on mismatch. Slug MUST start with 'e2e-' so sweep-stale-e2e-orgs.yml
# + lint_cleanup_traps.sh reap any orphan. (The lint requires a
# quoted SLUG=... with a literal e2e-/rt-e2e- head.)
# shellcheck source=lib/collision-proof-slug.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/collision-proof-slug.sh"

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

# SLUG construction runs after log/fail/ok so the assert can call `fail`.
SLUG="e2e-cncrg-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 10)"
assert_collision_proof_slug "$SLUG" || fail "Bug in make_collision_proof_slug: produced non-collision-proof slug '$SLUG'"

PASS=0
FAIL=0
check() {  # <desc> <expected-substr> <actual>
  if echo "$3" | grep -qF -- "$2"; then echo "  PASS: $1"; PASS=$((PASS + 1));
  else echo "  FAIL: $1"; echo "    expected to contain: $2"; echo "    got: $(echo "$3" | head -c 300)"; FAIL=$((FAIL + 1)); fi
}
check_not() {  # <desc> <unexpected-substr> <actual>
  if echo "$3" | grep -qF -- "$2"; then echo "  FAIL: $1 (should NOT contain: $2)"; FAIL=$((FAIL + 1));
  else echo "  PASS: $1"; PASS=$((PASS + 1)); fi
}
check_code() {  # <desc> <expected> <actual>
  if [ "$3" = "$2" ]; then echo "  PASS: $1 (HTTP $3)"; PASS=$((PASS + 1));
  else echo "  FAIL: $1 (expected HTTP $2, got HTTP $3)"; FAIL=$((FAIL + 1)); fi
}

CURL_COMMON=(-sS --max-time 30)
TMPDIR_E2E=$(mktemp -d -t cncrg-staging-XXXXXX)

# ─── teardown trap (org delete + leak check) ─────────────────────────────────
CLEANUP_DONE=0
cleanup_org() {
  local entry_rc=$?
  [ "$CLEANUP_DONE" = "1" ] && return 0
  CLEANUP_DONE=1
  rm -rf "$TMPDIR_E2E" 2>/dev/null || true

  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    log "E2E_KEEP_ORG=1 — skipping teardown. Manually delete $SLUG when done."
    return 0
  fi
  log "🧹 Tearing down org $SLUG..."
  if curl "${CURL_COMMON[@]}" --max-time 120 -X DELETE "$CP_URL/cp/admin/tenants/$SLUG" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
    -d "{\"confirm\":\"$SLUG\"}" >/dev/null 2>&1; then
    ok "Teardown request accepted"
  else
    log "Teardown returned non-2xx (may already be gone)"
  fi

  # Eventual-consistency wait: org row gone / purged.
  local leak_count=1 elapsed=0
  while [ "$elapsed" -lt 60 ]; do
    leak_count=$(curl "${CURL_COMMON[@]}" "$CP_URL/cp/admin/orgs" \
      -H "Authorization: Bearer $ADMIN_TOKEN" 2>/dev/null \
      | python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(1 for o in d.get('orgs', []) if o.get('slug')=='$SLUG' and o.get('status') != 'purged'))" \
      2>/dev/null || echo 1)
    [ "$leak_count" = "0" ] && break
    sleep 5; elapsed=$((elapsed + 5))
  done
  if [ "$leak_count" != "0" ]; then
    echo "⚠️  LEAK: org $SLUG still present post-teardown after ${elapsed}s (count=$leak_count)" >&2
    exit 4
  fi
  local aws_leak_rc=0
  e2e_verify_no_ec2_leaks_for_slug "$SLUG" || aws_leak_rc=$?
  if [ "$aws_leak_rc" != "0" ]; then
    case "$aws_leak_rc" in 2) exit 2 ;; *) exit 4 ;; esac
  fi
  ok "Teardown clean — no orphan org or EC2 resources for $SLUG (${elapsed}s)"
  case "$entry_rc" in 0|1|2|3|4) ;; *) exit 1 ;; esac
}
trap cleanup_org EXIT INT TERM

admin_call() {  # <method> <path> [curl args…]
  local method="$1" path="$2"; shift 2
  curl "${CURL_COMMON[@]}" -X "$method" "$CP_URL$path" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" "$@"
}

# ─── 0. Preflight ────────────────────────────────────────────────────────────
log "═══ Staging concierge user_tasks E2E ═══  CP=$CP_URL  Slug=$SLUG"
curl "${CURL_COMMON[@]}" "$CP_URL/health" >/dev/null || fail "CP health check failed"
ok "CP reachable"

# ─── 1. Create org ───────────────────────────────────────────────────────────
log "1/6 Creating org $SLUG..."
CREATE_RESP=$(admin_call POST /cp/admin/orgs \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
echo "$CREATE_RESP" | python3 -m json.tool >/dev/null || fail "Org create non-JSON: $CREATE_RESP"
ORG_ID=$(echo "$CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")
[ -z "$ORG_ID" ] && fail "Org create response missing 'id': $CREATE_RESP"
ok "Org created (id=$ORG_ID)"

# ─── 2. Wait for tenant provisioning ─────────────────────────────────────────
log "2/6 Waiting for tenant provisioning (up to ${PROVISION_TIMEOUT_SECS}s)..."
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
LAST_STATUS=""
while true; do
  [ "$(date +%s)" -gt "$DEADLINE" ] && exit 3
  LIST_JSON=$(admin_call GET /cp/admin/orgs 2>/dev/null || echo '{"orgs":[]}')
  STATUS=$(echo "$LIST_JSON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for o in d.get('orgs', []):
    if o.get('slug') == '$SLUG':
        print(o.get('instance_status', '')); sys.exit(0)
print('')" 2>/dev/null || echo "")
  if [ "$STATUS" != "$LAST_STATUS" ]; then log "    status → $STATUS"; LAST_STATUS="$STATUS"; fi
  case "$STATUS" in
    running) break ;;
    failed)  fail "Tenant provisioning failed for $SLUG" ;;
    *)       sleep 15 ;;
  esac
done
ok "Tenant provisioning complete"

# Derive tenant domain from CP hostname (prod vs staging).
CP_HOST=$(echo "$CP_URL" | sed -E 's#^https?://##; s#/.*$##')
case "$CP_HOST" in
  api.*)         DERIVED_DOMAIN="${CP_HOST#api.}" ;;
  staging-api.*) DERIVED_DOMAIN="staging.${CP_HOST#staging-api.}" ;;
  *)             DERIVED_DOMAIN="$CP_HOST" ;;
esac
TENANT_DOMAIN="${MOLECULE_TENANT_DOMAIN:-$DERIVED_DOMAIN}"
TENANT_URL="https://$SLUG.$TENANT_DOMAIN"
log "    TENANT_URL=$TENANT_URL"

# ─── 3. Per-tenant admin token + TLS readiness ───────────────────────────────
log "3/6 Fetching per-tenant admin token..."
TENANT_TOKEN=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token" \
  | python3 -c "import json,sys; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null || echo "")
[ -z "$TENANT_TOKEN" ] && fail "Could not retrieve per-tenant admin token for $SLUG"
ok "Tenant admin token retrieved (len=${#TENANT_TOKEN})"

log "    Waiting for tenant TLS / DNS propagation..."
TLS_DEADLINE=$(( $(date +%s) + 15 * 60 ))
while true; do
  curl -sSfk --max-time 5 "$TENANT_URL/health" >/dev/null 2>&1 && break
  [ "$(date +%s)" -gt "$TLS_DEADLINE" ] && fail "Tenant /health never 2xx within 15m"
  sleep 5
done
ok "Tenant reachable at $TENANT_URL"

# tenant_call: Authorization (tenant admin token, valid for every workspace) +
# X-Molecule-Org-Id (TenantGuard 404s without it) + Origin (Cloudflare edge).
tenant_call() {  # <method> <path> [curl args…]
  local method="$1" path="$2"; shift 2
  curl "${CURL_COMMON[@]}" -X "$method" "$TENANT_URL$path" \
    -H "Authorization: Bearer $TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    -H "Origin: $TENANT_URL" "$@"
}

# Create an external workspace (row only — no EC2). Echoes its id.
#
# Bounded retry around the external-row create only. The external create still
# runs a DB transaction + post-commit token/status work before returning 201,
# so under staging control-plane latency the one-shot curl could exit rc=28
# (CURL_COMMON --max-time 30 -> "curl: (28) Operation timed out") and the helper
# parsed no id, hard-failing user_tasks before any assertion (issue #2743).
# This is provisioning-latency flake, not a user_tasks contract failure -- so we
# retry transient cases (rc=28 / connection error -> http 000, or 2xx-but-no-id)
# with a longer per-call timeout (mirroring the teardown DELETE --max-time 120)
# and short backoff. Semantic 4xx/5xx stay hard-red with the response body.
CREATE_WS_ATTEMPTS=${CREATE_WS_ATTEMPTS:-5}
CREATE_WS_MAX_TIME=${CREATE_WS_MAX_TIME:-90}
create_external_ws() {  # <name>
  local name="$1" attempt body code id rc
  for attempt in $(seq 1 "$CREATE_WS_ATTEMPTS"); do
    body=$(mktemp "$TMPDIR_E2E/ws_create.XXXXXX")
    # Longer --max-time wins over CURL_COMMON's 30s (later flag); capture rc so
    # rc=28 is classified as transient latency rather than a no-id hard fail.
    set +e
    code=$(tenant_call POST /workspaces --max-time "$CREATE_WS_MAX_TIME" \
      -H "Content-Type: application/json" \
      -d "{\"name\":\"$name\",\"tier\":1,\"runtime\":\"external\",\"external\":true}" \
      -o "$body" -w "%{http_code}" 2>/dev/null)
    rc=$?
    set -e
    id=$(python3 -c "import sys,re
b=open('$body',encoding='utf-8').read()
m=re.search(r'\"id\"\s*:\s*\"([^\"]+)\"', b)
print(m.group(1) if m else '')" 2>/dev/null || echo '')
    if [ -n "$id" ]; then echo "$id"; rm -f "$body"; return 0; fi
    # Semantic failure (got an HTTP response in 4xx/5xx) -> hard-red immediately.
    case "$code" in
      4??|5??)
        fail "external ws create '$name' failed HTTP $code: $(head -c 500 "$body")" ;;
    esac
    # Transient: rc=28 (timeout), connection error (code 000), or 2xx-with-no-id.
    log "    ws create '$name' transient (attempt $attempt/$CREATE_WS_ATTEMPTS: curl rc=$rc http=$code) -- retrying"
    rm -f "$body"
    sleep $(( attempt * 3 ))
  done
  fail "external ws create '$name' returned no id after $CREATE_WS_ATTEMPTS attempts (last curl rc=$rc http=$code; staging control-plane latency, rc=28 class)"
}

# MCP JSON-RPC tools/call against /workspaces/:id/mcp. Echoes the result text
# (result.content[].text). Persists HTTP code to a file (runs in $()).
MCP_CODE_FILE="$TMPDIR_E2E/mcp_code"
mcp_call() {  # <wsid> <tool> <args-json>
  local wsid="$1" tool="$2" args="$3" out code
  out="$TMPDIR_E2E/mcp_out"
  set +e
  code=$(tenant_call POST "/workspaces/$wsid/mcp" -H "Content-Type: application/json" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$tool\",\"arguments\":$args}}" \
    -o "$out" -w "%{http_code}" 2>/dev/null)
  set -e
  printf '%s' "$code" > "$MCP_CODE_FILE"
  python3 -c "
import sys, json
try: d = json.load(open('$out'))
except Exception: print(''); sys.exit(0)
res = d.get('result') if isinstance(d, dict) else None
print(''.join(c.get('text','') for c in res.get('content', [])) if isinstance(res, dict) else '')"
}
mcp_http_code() { cat "$MCP_CODE_FILE" 2>/dev/null || echo ''; }

# ─── 4. Provision two workspaces (A raises asks, B probes cross-ws authz) ─────
log "4/6 Creating two tenant workspaces (external rows — no EC2)..."
WS_A=$(create_external_ws "Concierge-UT-A-$$")
[ -n "$WS_A" ] || fail "ws-A create returned no id"
WS_B=$(create_external_ws "Concierge-UT-B-$$")
[ -n "$WS_B" ] || fail "ws-B create returned no id"
ok "ws-A=$WS_A  ws-B=$WS_B"

# ─── 5. user_tasks REST + MCP + authz ────────────────────────────────────────
log "5/6 user_tasks contract (REST + MCP + cross-ws authz)..."

# 5.1 REST create → 201, status pending
R=$(tenant_call POST "/workspaces/$WS_A/user-tasks" -H "Content-Type: application/json" \
  -d '{"title":"Review the Q3 draft","detail":"Need your sign-off before send"}' \
  -o "$TMPDIR_E2E/c.json" -w "%{http_code}" 2>/dev/null || echo "000")
BODY=$(cat "$TMPDIR_E2E/c.json" 2>/dev/null || echo "")
check_code "REST create user-task" "201" "$R"
check "create returns status pending" '"status":"pending"' "$BODY"
TASK_ID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('user_task_id',''))" 2>/dev/null || echo "")
[ -n "$TASK_ID" ] || fail "no user_task_id returned: $BODY"
log "    TASK_ID=$TASK_ID"

# 5.2 REST read (this workspace + admin org-wide pending)
R=$(tenant_call GET "/workspaces/$WS_A/user-tasks")
check "GET ws-A user-tasks contains the task" "$TASK_ID" "$R"
check "GET ws-A user-tasks shows title" 'Review the Q3 draft' "$R"
R=$(tenant_call GET "/user-tasks/pending")
check "GET /user-tasks/pending (admin) contains the task" "$TASK_ID" "$R"
check "pending entry carries workspace_name" "Concierge-UT-A-$$" "$R"

# 5.3 REST PATCH title/detail → 200, applied
R=$(tenant_call PATCH "/workspaces/$WS_A/user-tasks/$TASK_ID" -H "Content-Type: application/json" \
  -d '{"title":"Review the Q3 draft (URGENT)","detail":"Sign-off needed by EOD"}' \
  -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
check_code "REST PATCH user-task" "200" "$R"
R=$(tenant_call GET "/workspaces/$WS_A/user-tasks")
check "PATCH applied new title" '(URGENT)' "$R"
check "PATCH applied new detail" 'Sign-off needed by EOD' "$R"

# 5.4 REST resolve done → 200, gone from pending
R=$(tenant_call POST "/workspaces/$WS_A/user-tasks/$TASK_ID/resolve" -H "Content-Type: application/json" \
  -d '{"status":"done","resolved_by":"cto"}' -o "$TMPDIR_E2E/r.json" -w "%{http_code}" 2>/dev/null || echo "000")
BODY=$(cat "$TMPDIR_E2E/r.json" 2>/dev/null || echo "")
check_code "REST resolve done" "200" "$R"
check "resolve echoes status done" '"status":"done"' "$BODY"
R=$(tenant_call GET "/user-tasks/pending")
check_not "resolved task no longer pending (admin feed)" "$TASK_ID" "$R"

# 5.5 MCP request_user_action → new pending task surfaces on the admin feed
TEXT=$(mcp_call "$WS_A" "request_user_action" '{"title":"Provide the staging API key","detail":"Blocked on it for the deploy"}')
check_code "MCP request_user_action HTTP" "200" "$(mcp_http_code)"
check "MCP request_user_action success text" 'Asked the user' "$TEXT"
R=$(tenant_call GET "/user-tasks/pending")
check "MCP-created ask appears in pending feed" 'Provide the staging API key' "$R"
MCP_TASK_ID=$(echo "$R" | python3 -c "
import sys, json
for t in json.load(sys.stdin):
    if t.get('title') == 'Provide the staging API key':
        print(t.get('id','')); break" 2>/dev/null || echo "")
log "    MCP_TASK_ID=$MCP_TASK_ID"

# 5.6 MCP list_user_tasks returns ws-A's task(s)
TEXT=$(mcp_call "$WS_A" "list_user_tasks" '{}')
check_code "MCP list_user_tasks HTTP" "200" "$(mcp_http_code)"
check "list_user_tasks contains the MCP task" 'Provide the staging API key' "$TEXT"
check "list_user_tasks shows it pending" '"status":"pending"' "$TEXT"

# 5.7 MCP update_user_task changes it
if [ -n "$MCP_TASK_ID" ]; then
  TEXT=$(mcp_call "$WS_A" "update_user_task" "{\"user_task_id\":\"$MCP_TASK_ID\",\"title\":\"Provide the PROD API key\"}")
  check_code "MCP update_user_task HTTP" "200" "$(mcp_http_code)"
  check "MCP update_user_task success text" 'User task updated' "$TEXT"
  TEXT=$(mcp_call "$WS_A" "list_user_tasks" '{}')
  check "update applied (new title)" 'Provide the PROD API key' "$TEXT"
  check_not "update applied (old title gone)" 'staging API key' "$TEXT"

  # 5.8 MCP delete_user_task → gone from list
  TEXT=$(mcp_call "$WS_A" "delete_user_task" "{\"user_task_id\":\"$MCP_TASK_ID\"}")
  check_code "MCP delete_user_task HTTP" "200" "$(mcp_http_code)"
  check "MCP delete_user_task success text" 'User task deleted' "$TEXT"
  TEXT=$(mcp_call "$WS_A" "list_user_tasks" '{}')
  check_not "deleted task gone from list" 'Provide the PROD API key' "$TEXT"
else
  echo "  FAIL: could not resolve MCP_TASK_ID — MCP update/delete steps skipped"
  FAIL=$((FAIL + 1))
fi

# 5.9 Cross-workspace authz: ws-B cannot mutate ws-A's task (scoped by URL :id)
SCOPE_ID=$(tenant_call POST "/workspaces/$WS_A/user-tasks" -H "Content-Type: application/json" \
  -d '{"title":"Scope probe task"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('user_task_id',''))" 2>/dev/null || echo "")
[ -n "$SCOPE_ID" ] || fail "scope-probe task create failed"
log "    SCOPE_ID=$SCOPE_ID (owned by ws-A)"
# ws-B PATCHes ws-A's task → 404 (workspace_id scope).
R=$(tenant_call PATCH "/workspaces/$WS_B/user-tasks/$SCOPE_ID" -H "Content-Type: application/json" \
  -d '{"title":"hijack"}' -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
check_code "ws-B PATCH of ws-A's task scoped out" "404" "$R"
# ws-B DELETEs ws-A's task → 404.
R=$(tenant_call DELETE "/workspaces/$WS_B/user-tasks/$SCOPE_ID" -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
check_code "ws-B DELETE of ws-A's task scoped out" "404" "$R"
# Task survived unchanged on ws-A.
R=$(tenant_call GET "/workspaces/$WS_A/user-tasks")
check "ws-A's task survived cross-ws attempts" "$SCOPE_ID" "$R"
check_not "ws-A's task title was NOT hijacked" 'hijack' "$R"
# ws-B's own list must NOT see ws-A's task at all.
R=$(tenant_call GET "/workspaces/$WS_B/user-tasks")
check_not "ws-B list excludes ws-A's task (read isolation)" "$SCOPE_ID" "$R"

# 5.10 Validation contracts
R=$(tenant_call POST "/workspaces/$WS_A/user-tasks" -H "Content-Type: application/json" \
  -d '{"detail":"no title here"}' -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
check_code "create without title → 400" "400" "$R"
R=$(tenant_call POST "/workspaces/$WS_A/user-tasks/$SCOPE_ID/resolve" -H "Content-Type: application/json" \
  -d '{"status":"banana"}' -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
check_code "resolve with invalid status → 400" "400" "$R"
R=$(tenant_call PATCH "/workspaces/$WS_A/user-tasks/$SCOPE_ID" -H "Content-Type: application/json" \
  -d '{"status":"banana"}' -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
check_code "PATCH with invalid status → 400" "400" "$R"

# ─── 6. Results ──────────────────────────────────────────────────────────────
log "6/6 Results: $PASS passed, $FAIL failed (teardown runs via EXIT trap)"
[ "$FAIL" -eq 0 ] || fail "$FAIL user_tasks assertion(s) failed"
ok "═══ STAGING CONCIERGE user_tasks E2E PASSED ($PASS checks) ═══"
