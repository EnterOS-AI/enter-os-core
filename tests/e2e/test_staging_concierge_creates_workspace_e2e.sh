#!/usr/bin/env bash
# FUNCTIONAL real-LLM E2E: prove the org concierge (the platform agent) can
# actually DO org-management work — send it a natural-language request and
# assert it REALLY CREATES a workspace via its platform MCP (87 org-admin tools,
# incl. create_workspace), NOT just that a REST API returned 200.
#
# This is the RFC docs/design/rfc-platform-agent.md §11.4 "Reach" check, made
# into a gating CI test:
#
#   "chat the platform agent → it list_workspaces then create_workspace via the
#    platform MCP and reports back via send_message_to_user."
#
# Unlike test_staging_concierge_e2e.sh (which drives the user_tasks REST+MCP
# primitive directly — a pure DB/handler contract with NO LLM), THIS test drives
# the AGENT: it sends an A2A message/send envelope (the user→concierge chat
# path) and asserts the DETERMINISTIC SIDE EFFECT — a workspace with the exact
# name we asked for now EXISTS in GET /workspaces — which can only happen if the
# concierge's LLM actually invoked the create_workspace platform-MCP tool.
#
# WHAT MUST BE LIVE for this to pass GREEN (else it SKIPs LOUD, never false-red):
#   • The org's concierge must be installed as the kind='platform' root AND
#     provisioned on the DEDICATED platform-agent image (Dockerfile.platform-agent),
#     which ships /opt/molecule-mcp-server — the ONLY image where the platform MCP
#     (create_workspace) lights up. On SaaS staging the CP installs + provisions it
#     at org-provision time. (See platform_agent.go's SELF-HOST CAVEAT: the ordinary
#     claude-code image does NOT ship the platform MCP, so create_workspace is a
#     no-op there.) A parallel agent is wiring the platform-agent image into the
#     staging provision path; until that lands, this test SKIPs LOUD with a clear
#     "concierge not on platform-agent image" message rather than failing red.
#   • A working model for the concierge. On SaaS the concierge is platform_managed
#     (the CP-exported LLM proxy supplies the model) so no BYOK key is needed for
#     the concierge itself.
#
# Env contract (same as test_staging_concierge_e2e.sh / test_staging_full_saas.sh):
#   MOLECULE_CP_URL        default: https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   CP admin bearer — Railway staging CP_ADMIN_API_TOKEN
#
# Optional env:
#   E2E_PROVISION_TIMEOUT_SECS    default 900 (15 min cold tenant EC2 budget)
#   E2E_CONCIERGE_ONLINE_SECS     default 900 (concierge boot-to-online budget)
#   E2E_AGENT_ACT_SECS            default 420 (LLM think+tool-call budget after we
#                                 send the message — generous for nondeterminism)
#   E2E_MCP_READY_SECS            default 180 (post-online wait for platform MCP
#                                 tool registration to surface via A2A probe)
#   E2E_KEEP_ORG                  1 → skip teardown (debugging only)
#   E2E_RUN_ID                    slug suffix; CI: ${GITHUB_RUN_ID}-${RUN_ATTEMPT}
#   E2E_AWS_LEAK_CHECK            auto (default) | required | off
#   E2E_AWS_TERMINATE_LEAKS      1 → terminate slug-tagged leaked EC2 on exit
#   E2E_REQUIRE_LIVE             1 → a SKIP for "no concierge on platform image"
#                                 becomes a hard FAIL (CI sets this so a silently-
#                                 missing platform-agent image can't false-green
#                                 the gate). Default 0 (local: skip-loud).
#
# Exit codes:
#   0  happy path (concierge created the workspace) OR honest skip-loud
#   1  generic / assertion failure (agent didn't act, or tool failed)
#   2  missing required env
#   3  provisioning timed out
#   4  teardown left orphan resources
#   5  E2E_REQUIRE_LIVE=1 but the concierge could not be exercised (no
#      platform-agent image / never came online) — false-green guard
set -euo pipefail

# shellcheck disable=SC1091
# shellcheck source=_lib.sh
source "$(dirname "$0")/_lib.sh"
# AWS-leak-check lib — same teardown leak assertion the full-SaaS harness uses.
# shellcheck disable=SC1091
# shellcheck source=lib/aws_leak_check.sh
source "$(dirname "$0")/lib/aws_leak_check.sh"
# Real-completion error-as-text scanner — used to detect the concierge
# surfacing its tool/LLM error AS a reply ("Agent error …") so a broken agent
# can't read as "asked but politely declined".
# shellcheck disable=SC1091
# shellcheck source=lib/completion_assert.sh
source "$(dirname "$0")/lib/completion_assert.sh"

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:-}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"
CONCIERGE_ONLINE_SECS="${E2E_CONCIERGE_ONLINE_SECS:-900}"
AGENT_ACT_SECS="${E2E_AGENT_ACT_SECS:-420}"
MCP_READY_SECS="${E2E_MCP_READY_SECS:-180}"
REQUIRE_LIVE="${E2E_REQUIRE_LIVE:-0}"

# ─── PR-mode early-exit (core#3081 / CR2 #12653) ──────────────────────────────
# A required status context that never fires on pull_request degrades the
# merge gate to a silent indefinite pending (the failure mode
# lint-required-no-paths exists to prevent). The workflow sets
# E2E_REQUIRE_LIVE=0 on pull_request runs because PRs do not have staging
# creds wired; the real staging test would just exit 2 at the ADMIN_TOKEN
# check below. The PR-mode gate is a self-check:
#   - bash -n on the script's own syntax (catches PR-merge regressions
#     that break the script BEFORE it runs).
# On push / dispatch / cron, E2E_REQUIRE_LIVE=1, the real staging test
# runs against live staging, and skip_loud on missing infra exits 5
# (HARD FAIL — the false-green guard).
if [ "${REQUIRE_LIVE}" = "0" ] && [ -z "${ADMIN_TOKEN}" ]; then
  log "PR-mode: E2E_REQUIRE_LIVE=0 and no MOLECULE_ADMIN_TOKEN — skipping live staging test."
  log "(the real staging test runs on push-to-main / dispatch / cron with E2E_REQUIRE_LIVE=1)"
  # Self-check: bash -n on the script's own syntax. The script IS the
  # gate on push; on PR, the gate is 'script exists and is bash-clean'.
  if ! bash -n "$0"; then
    fail "PR-mode self-check FAILED: bash -n on $0 returned non-zero — script has a syntax error"
  fi
  ok "PR-mode self-check PASSED: $(basename "$0") is bash-clean (real staging test runs on push-to-main with E2E_REQUIRE_LIVE=1)"
  exit 0
fi
# Beyond here, we are running for real: REQUIRE_LIVE=1 OR ADMIN_TOKEN
# is set. If ADMIN_TOKEN is set but REQUIRE_LIVE=0, that's an operator-
# dispatched local run (the original PR test path) — keep the original
# strict check below.
if [ -z "${ADMIN_TOKEN}" ]; then
  fail "MOLECULE_ADMIN_TOKEN required (Railway staging CP_ADMIN_API_TOKEN) — E2E_REQUIRE_LIVE=1 needs staging creds"
fi
# Collision-proof slug (core#2782). The prior `head -c 32` truncation
# dropped the run_attempt suffix and let two parallel/retry runs
# collide (POST /cp/admin/orgs 409). The helper appends a random
# 8-char uuid so every run gets a unique slug regardless of how
# the workflow composes E2E_RUN_ID. The `source` + `assert` run
# AFTER log/fail/ok are defined below so the assert can call `fail`
# on mismatch. Slug MUST start with 'e2e-' so sweep-stale-e2e-orgs.yml
# + lint_cleanup_traps.sh reap any orphan org. (The lint requires
# a quoted SLUG=... with a literal e2e-/rt-e2e- head.)
# shellcheck source=lib/collision-proof-slug.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/collision-proof-slug.sh"

# The workspace name we will ask the concierge to create. The literal
# `e2e-cncrg-w-` prefix is visible to the lint (so the SLUG=
# has a covered e2e- prefix in the assignment); the uuid suffix
# makes the name unique per run so a poll for it can never collide
# with a sibling run's name.
WORKER_NAME="e2e-cncrg-w-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 12)"
WORKER_NAME=$(echo "$WORKER_NAME" | tr -cd 'a-zA-Z0-9-' | head -c 48)
# Exported so the find_worker_by_name python subshell (run in a pipe) reads it
# via os.environ — a bare shell var would not survive into the subprocess env.
export WORKER_NAME

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

# SLUG construction runs after log/fail/ok so the assert can call `fail`.
SLUG="e2e-cncrg-mk-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 13)"
assert_collision_proof_slug "$SLUG" || fail "Bug in make_collision_proof_slug: produced non-collision-proof slug '$SLUG'"
# skip_loud <reason>: honest skip when the concierge can't be exercised. In CI
# (E2E_REQUIRE_LIVE=1) this is a HARD FAIL (exit 5) so a missing platform-agent
# image can't false-green the gate; locally it skips 0.
skip_loud() {
  echo "[$(date +%H:%M:%S)] ⏭️  SKIP: $*" >&2
  if [ "$REQUIRE_LIVE" = "1" ]; then
    echo "[$(date +%H:%M:%S)] ❌ E2E_REQUIRE_LIVE=1 — a skip is a false-green guard breach here. Failing." >&2
    exit 5
  fi
  exit 0
}

CURL_COMMON=(-sS --max-time 30)
TMPDIR_E2E=$(mktemp -d -t cncrg-mk-XXXXXX)

# ─── teardown trap (worker delete + org delete + leak check) ─────────────────
CLEANUP_DONE=0
WORKER_ID=""        # set once the concierge creates it (for targeted delete)
TENANT_URL=""       # set after provisioning
TENANT_TOKEN=""
ORG_ID=""
cleanup() {
  local entry_rc=$?
  [ "$CLEANUP_DONE" = "1" ] && return 0
  CLEANUP_DONE=1
  rm -rf "$TMPDIR_E2E" 2>/dev/null || true

  # Best-effort targeted delete of the worker the concierge created, so the org
  # delete below isn't the only thing reaping it (defensive — org delete cascades
  # anyway). Only attempted if we resolved its id and have tenant creds.
  if [ -n "$WORKER_ID" ] && [ -n "$TENANT_URL" ] && [ -n "$TENANT_TOKEN" ]; then
    curl "${CURL_COMMON[@]}" -X DELETE "$TENANT_URL/workspaces/$WORKER_ID?confirm=true" \
      -H "Authorization: Bearer $TENANT_TOKEN" \
      -H "X-Molecule-Org-Id: $ORG_ID" \
      -H "Origin: $TENANT_URL" \
      -H "X-Confirm-Name: $WORKER_NAME" >/dev/null 2>&1 || true
  fi

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
  case "$entry_rc" in 0|1|2|3|4|5) ;; *) exit 1 ;; esac
}
trap cleanup EXIT INT TERM

admin_call() {  # <method> <path> [curl args…]
  local method="$1" path="$2"; shift 2
  curl "${CURL_COMMON[@]}" -X "$method" "$CP_URL$path" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" "$@"
}

# tenant_call: Authorization (tenant admin token — also authenticates the
# concierge, which holds no per-workspace token: validateDiscoveryCaller's admin
# fallback) + X-Molecule-Org-Id (TenantGuard 404s without it) + Origin (edge WAF).
tenant_call() {  # <method> <path> [curl args…]
  local method="$1" path="$2"; shift 2
  curl "${CURL_COMMON[@]}" -X "$method" "$TENANT_URL$path" \
    -H "Authorization: Bearer $TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    -H "Origin: $TENANT_URL" "$@"
}

# list_workspaces_json: echo the raw GET /workspaces JSON array (tenant-scoped).
list_workspaces_json() { tenant_call GET /workspaces; }

# find_platform_root: echo the id of the kind='platform' parent_id-null root, or
# "" if none. This IS the concierge — the org's front-door agent.
find_platform_root() {
  list_workspaces_json | python3 -c "
import sys, json
try: rows = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
for w in rows if isinstance(rows, list) else []:
    if w.get('kind') == 'platform' and not w.get('parent_id'):
        print(w.get('id','')); break
else:
    print('')"
}

# workspace_field <id> <field>: echo a single field off GET /workspaces/:id.
workspace_field() {  # <id> <field>
  tenant_call GET "/workspaces/$1" | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
print(d.get('$2','') if isinstance(d, dict) else '')"
}

# find_worker_by_name: echo the id of a workspace whose name == WORKER_NAME, or
# "" if not present. THIS is the deterministic side effect we assert on.
find_worker_by_name() {
  list_workspaces_json | python3 -c "
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

# ─── 0. Preflight ────────────────────────────────────────────────────────────
log "═══ Staging concierge CREATES-A-WORKSPACE (real-LLM) E2E ═══  CP=$CP_URL  Slug=$SLUG"
log "    worker the concierge will be asked to create: name=$WORKER_NAME"
curl "${CURL_COMMON[@]}" "$CP_URL/health" >/dev/null || fail "CP health check failed"
ok "CP reachable"

# ─── 1. Create org (CP installs + provisions the concierge as platform root) ──
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

# ─── 4. Discover the concierge (kind='platform' root) + ensure it can act ─────
log "4/6 Discovering the concierge (kind='platform' root)..."
# The CP installs the platform agent at org-provision; allow a short settle for
# the row + re-parent backfill to land.
CONCIERGE_ID=""
DISC_DEADLINE=$(( $(date +%s) + 180 ))
while true; do
  CONCIERGE_ID=$(find_platform_root)
  [ -n "$CONCIERGE_ID" ] && break
  [ "$(date +%s)" -gt "$DISC_DEADLINE" ] && break
  sleep 10
done
if [ -z "$CONCIERGE_ID" ]; then
  skip_loud "no kind='platform' concierge root in this org — the platform agent was not installed at provision. \
This needs the CP platform-agent install (RFC §3) live on staging. Until then there is no agent to drive."
fi
ok "Concierge (platform root) = $CONCIERGE_ID"

# The concierge must be ONLINE + routable for its LLM to receive the A2A message
# and reach the platform MCP. Bounded poll — generous because a cold concierge
# boots its container + loads the platform MCP server before it is reachable.
log "    Waiting for the concierge to be online (up to ${CONCIERGE_ONLINE_SECS}s)..."
ONLINE_DEADLINE=$(( $(date +%s) + CONCIERGE_ONLINE_SECS ))
C_STATUS=""; C_URL=""; LAST_C_STATUS=""
while true; do
  C_STATUS=$(workspace_field "$CONCIERGE_ID" status)
  C_URL=$(workspace_field "$CONCIERGE_ID" url)
  if [ "$C_STATUS" != "$LAST_C_STATUS" ]; then log "    concierge → ${C_STATUS:-<none>}"; LAST_C_STATUS="$C_STATUS"; fi
  if [ "$C_STATUS" = "online" ] && [ -n "$C_URL" ]; then break; fi
  if [ "$(date +%s)" -gt "$ONLINE_DEADLINE" ]; then
    LAST_ERR=$(workspace_field "$CONCIERGE_ID" last_sample_error)
    skip_loud "concierge $CONCIERGE_ID never reached online+routable within ${CONCIERGE_ONLINE_SECS}s \
(last status='${C_STATUS}', url='${C_URL}', err='${LAST_ERR}'). On a tenant where the concierge is NOT \
provisioned on the platform-agent image (no /opt/molecule-mcp-server, no model), it cannot run the \
create_workspace tool — that is the parallel-agent image work this gate depends on."
  fi
  sleep 10
done
ok "Concierge online + routable (url assigned)"

# ─── 4.5. A2A-probe: assert the concierge's RUNTIME tool list includes ─────────
# mcp__molecule-platform__create_workspace (not just that the config declared it).
#
# core#3081 / Researcher #12646: the previous false-green slipped because the
# test asserted the mcp_servers.yaml TEXT, which only proves a config file
# exists on disk — it does NOT prove the concierge's LLM can actually call
# the tool. The whole point of the gate is to assert REAL capability: a
# runtime, live, actually-callable tool — not a proxy (file presence, plugin
# install, platform-agent image presence, mcp_servers.yaml text).
#
# Mechanism: send a structured A2A `message/send` envelope to the concierge
# asking it to enumerate its MCP tool names by their literal namespaced
# identifiers (the `mcp__<server>__<tool>` form that Claude Code's tool
# dispatcher uses), then parse the reply for the literal
# `mcp__molecule-platform__create_workspace` string. This is LLM-mediated
# (the concierge LLM must respond) but goes through the SAME A2A channel
# the real create_workspace call (5/6) will use, so a missing tool shows up
# as a missing-string-in-reply here, before the LLM-budget is burned on the
# 7-minute cold-concierge tool call that will never succeed.
#
# Defensive parsing: the concierge LLM may list tools in a few formats
# (`mcp__molecule-platform__create_workspace`, `create_workspace`, or as a
# JSON array). We accept any of the literal namespaced form OR a JSON array
# containing the namespaced form. A "yes" in any format is a PASS; an absent
# namespaced identifier is a HARD FAIL (skip_loud + E2E_REQUIRE_LIVE=1 →
# exit 5).
log "4.5/6 A2A-probe: asserting the concierge's RUNTIME tool list exposes mcp__molecule-platform__create_workspace (budget ${MCP_READY_SECS}s)..."
# Cold concierge: same wide per-call window + cold-start 5xx retry as the
# real create call (5/6). Each probe attempt retries transport 5xx up to 5
# times; we then POLL for the tool to surface, because the MCP server may
# register its tools a few seconds after the A2A endpoint returns 2xx
# (npx cold-start / RFC#3045).
PROBE_PROMPT='List every MCP tool you have access to, by its full namespaced identifier (e.g. mcp__server-name__tool-name). Output ONLY a JSON array of strings, no commentary, no markdown fence. Example: ["mcp__memory__commit_memory", "mcp__platform__create_workspace"]. Reply with [] if you have no MCP tools.'
A2A_PROBE_TMP="$TMPDIR_E2E/a2a_probe_out"
PROBE_POLL_INTERVAL_SECS=25
MCP_READY_START_TS=$(date +%s)
MCP_READY_DEADLINE=$(( MCP_READY_START_TS + MCP_READY_SECS ))
PROBE_HIT=0

while [ "$(date +%s)" -lt "$MCP_READY_DEADLINE" ]; do
  PROBE_TEXT=""
  PROBE_OK=0
  # Inner transport-level retry: only 502/503/504 are retried.
  for PROBE_ATTEMPT in $(seq 1 5); do
    : >"$A2A_PROBE_TMP"
    set +e
    PROBE_CODE=$(tenant_call POST "/workspaces/$CONCIERGE_ID/a2a" \
        --max-time "$AGENT_ACT_SECS" \
        -H "Content-Type: application/json" \
        -d "$(WORKER_NAME="$WORKER_NAME" PROBE_PROMPT="$PROBE_PROMPT" python3 -c "
import json, os
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-cncrg-mk-probe-' + os.urandom(4).hex(),
    'params': {
        'message': {
            'role': 'user',
            'messageId': 'e2e-probe-' + os.urandom(4).hex(),
            'parts': [{'kind': 'text', 'text': os.environ['PROBE_PROMPT']}],
        }
    }
}))")" \
      -o "$A2A_PROBE_TMP" -w '%{http_code}' 2>/dev/null)
    PROBE_RC=$?
    set -e
    PROBE_CODE=${PROBE_CODE:-000}
    PROBE_RESP=$(cat "$A2A_PROBE_TMP" 2>/dev/null || echo "")
    if [ "$PROBE_RC" = "0" ] && [ "$PROBE_CODE" -ge 200 ] && [ "$PROBE_CODE" -lt 300 ]; then
      PROBE_OK=1
      break
    fi
    if echo "$PROBE_CODE" | grep -Eq '^(502|503|504)$'; then
      log "    A2A-probe cold-start attempt $PROBE_ATTEMPT/5 returned $PROBE_CODE — retrying"
      [ "$PROBE_ATTEMPT" -lt 5 ] && { sleep 15; continue; }
    fi
    break
  done
  if [ "$PROBE_OK" != "1" ]; then
    fail "A2A-probe POST /workspaces/$CONCIERGE_ID/a2a failed (curl_rc=$PROBE_RC, http=$PROBE_CODE) after $PROBE_ATTEMPT attempt(s): $(echo "$PROBE_RESP" | head -c 400)"
  fi

  PROBE_TEXT=$(echo "$PROBE_RESP" | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
parts = (d.get('result') or {}).get('parts', []) if isinstance(d, dict) else []
print(parts[0].get('text','') if parts else '')" 2>/dev/null || echo "")
  log "    concierge probe reply (first 300 chars): $(echo "$PROBE_TEXT" | head -c 300)"

  # Decide: does the literal `mcp__molecule-platform__create_workspace` appear
  # anywhere in the reply text?  We strip a leading/trailing markdown fence if
  # present (some LLM outputs wrap the JSON array in ```json ... ```) and parse
  # for the namespaced identifier.
  PROBE_VERDICT=$(printf '%s' "$PROBE_TEXT" | python3 -c "
import sys, json, re
text = sys.stdin.read()
if not text:
    print('EMPTY'); sys.exit(0)
# Accept the namespaced identifier directly (covers the prose-format reply).
if 'mcp__molecule-platform__create_workspace' in text:
    print('HIT'); sys.exit(0)
# Tolerate the LLM wrapping the JSON array in a markdown fence (the
# literal triple-backtick form) or padding it with prose. Pull the first
# [...] match and parse as JSON; accept any list element containing the
# namespaced identifier.
m = re.search(r'\[[^\]]*\]', text, re.S)
if m:
    try:
        arr = json.loads(m.group(0))
        if isinstance(arr, list):
            for t in arr:
                if isinstance(t, str) and 'mcp__molecule-platform__create_workspace' in t:
                    print('HIT'); sys.exit(0)
    except Exception:
        pass
print('NO_HIT')
" 2>/dev/null || echo "PARSE_ERR")

  case "$PROBE_VERDICT" in
    HIT)
      ok "A2A-probe PASS: concierge's RUNTIME tool list contains mcp__molecule-platform__create_workspace — REAL capability confirmed (not just a config-text proxy)"
      PROBE_HIT=1
      break
      ;;
    *)
      ELAPSED=$(( $(date +%s) - MCP_READY_START_TS ))
      if [ "$ELAPSED" -ge "$MCP_READY_SECS" ]; then
        break
      fi
      log "    platform MCP tool not yet surfaced, re-probing (${ELAPSED}s elapsed, budget ${MCP_READY_SECS}s)"
      sleep "$PROBE_POLL_INTERVAL_SECS"
      ;;
  esac
done

if [ "$PROBE_HIT" != "1" ]; then
  case "$PROBE_VERDICT" in
    NO_HIT)
      skip_loud "A2A-probe FAIL: concierge's reply does NOT contain mcp__molecule-platform__create_workspace. The tool is NOT in the LLM's runtime tool list — even if /configs/mcp_servers.yaml declares it, the concierge's MCP layer is not surfacing it to the LLM (overlay applied to wrong path, server name mismatch, or molecule-mcp-server not actually running). Reply: $(echo "$PROBE_TEXT" | head -c 600)"
      ;;
    EMPTY)
      skip_loud "A2A-probe FAIL: concierge returned no text part to the tool-list probe. The A2A channel is up (HTTP 2xx) but the LLM did not reply — could be a cold-start model-load failure, a missing model, or a wired-but-not-running MCP server. Reply was empty."
      ;;
    PARSE_ERR)
      skip_loud "A2A-probe FAIL: probe response did not parse as JSON-RPC text. Transport was up (HTTP 2xx) but the envelope shape is wrong — possible concierge runtime regression. Reply: $(echo "$PROBE_TEXT" | head -c 600)"
      ;;
    *)
      skip_loud "A2A-probe FAIL: probe produced unknown verdict '$PROBE_VERDICT'. Reply: $(echo "$PROBE_TEXT" | head -c 400)"
      ;;
  esac
fi
unset PROBE_TEXT PROBE_RESP PROBE_CODE PROBE_RC PROBE_VERDICT A2A_PROBE_TMP PROBE_HIT PROBE_POLL_INTERVAL_SECS MCP_READY_START_TS MCP_READY_DEADLINE
# Pre-state: the worker MUST NOT exist yet (so its later appearance is causally
# the concierge's doing, not a pre-existing row).
PRE_EXISTING=$(find_worker_by_name)
[ -n "$PRE_EXISTING" ] && fail "worker '$WORKER_NAME' already exists pre-test ($PRE_EXISTING) — name collision, cannot prove causality"
ok "Pre-state confirmed: '$WORKER_NAME' does not exist yet"

# ─── 5. Drive the AGENT: A2A message/send → it must create the workspace ──────
log "5/6 Sending the concierge a natural-language create-workspace request..."
# Imperative + explicit to defuse LLM nondeterminism: name the tool, the exact
# workspace NAME and ROLE, and tell it not to ask a clarifying question. The
# message/send envelope is the canvas user→agent chat path (handlers/a2a_proxy.go),
# identical to the shape test_a2a_e2e.sh / test_staging_full_saas.sh use.
AGENT_PROMPT="Please create a new workspace in this org right now using your platform tools. \
Use the create_workspace tool with name exactly \"${WORKER_NAME}\" and role \"engineer\". \
Do not ask me any clarifying questions — the name and role are final. \
After the tool succeeds, reply with the new workspace id."
A2A_PAYLOAD=$(WORKER_NAME="$WORKER_NAME" AGENT_PROMPT="$AGENT_PROMPT" python3 -c "
import json, os, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-cncrg-mk-1',
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': os.environ['AGENT_PROMPT']}],
        }
    }
}))")

# Cold concierge: first turn opens TLS to the LLM, loads the platform MCP, runs
# a tool call. Give it a wide per-call window AND retry on edge cold-start 5xx.
A2A_TMP="$TMPDIR_E2E/a2a_out"
AGENT_TEXT=""
A2A_OK=0
for A2A_ATTEMPT in $(seq 1 8); do
  : >"$A2A_TMP"
  set +e
  A2A_CODE=$(tenant_call POST "/workspaces/$CONCIERGE_ID/a2a" \
    --max-time "$AGENT_ACT_SECS" \
    -H "Content-Type: application/json" \
    -d "$A2A_PAYLOAD" \
    -o "$A2A_TMP" -w '%{http_code}' 2>/dev/null)
  A2A_RC=$?
  set -e
  A2A_CODE=${A2A_CODE:-000}
  A2A_RESP=$(cat "$A2A_TMP" 2>/dev/null || echo "")
  if [ "$A2A_RC" = "0" ] && [ "$A2A_CODE" -ge 200 ] && [ "$A2A_CODE" -lt 300 ]; then
    A2A_OK=1
    break
  fi
  if echo "$A2A_CODE" | grep -Eq '^(502|503|504)$'; then
    log "    A2A cold-start attempt $A2A_ATTEMPT/8 returned $A2A_CODE — retrying"
    [ "$A2A_ATTEMPT" -lt 8 ] && { sleep 15; continue; }
  fi
  break
done
if [ "$A2A_OK" != "1" ]; then
  # A non-2xx A2A POST is an INFRA/transport failure (agent unreachable), not an
  # "agent declined" — distinct from the assertion below.
  fail "A2A POST /workspaces/$CONCIERGE_ID/a2a failed (curl_rc=$A2A_RC, http=$A2A_CODE) after $A2A_ATTEMPT attempt(s): $(echo "$A2A_RESP" | head -c 400)"
fi
AGENT_TEXT=$(echo "$A2A_RESP" | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
parts = (d.get('result') or {}).get('parts', []) if isinstance(d, dict) else []
print(parts[0].get('text','') if parts else '')" 2>/dev/null || echo "")
log "    concierge replied (first 300 chars): $(echo "$AGENT_TEXT" | head -c 300)"

# ─── 6. ASSERT the deterministic side effect: the worker now EXISTS ───────────
log "6/6 Polling GET /workspaces for the worker the concierge was asked to create..."
# The create is the side effect; the LLM may take a few turns / a moment to flush
# the tool call. Poll the NAME (deterministic) — tolerant of when exactly the row
# lands, intolerant of it never landing.
ACT_DEADLINE=$(( $(date +%s) + AGENT_ACT_SECS ))
while true; do
  WORKER_ID=$(find_worker_by_name)
  [ -n "$WORKER_ID" ] && break
  if [ "$(date +%s)" -gt "$ACT_DEADLINE" ]; then
    # The agent answered but the workspace never appeared → the LLM did NOT call
    # create_workspace (or the tool failed). Distinguish the two for the operator.
    if hit=$(a2a_completion_error_marker "$AGENT_TEXT"); then
      fail "TOOL FAILED: concierge surfaced an error-as-text reply (matched '$hit') and no workspace '$WORKER_NAME' was created. \
The platform MCP create_workspace tool errored. Reply: $(echo "$AGENT_TEXT" | head -c 400)"
    fi
    fail "AGENT DID NOT ACT: concierge replied but no workspace named '$WORKER_NAME' exists in GET /workspaces after ${AGENT_ACT_SECS}s. \
The concierge's LLM did not invoke the create_workspace platform-MCP tool. \
Reply: $(echo "$AGENT_TEXT" | head -c 400)"
  fi
  sleep 8
done
ok "DETERMINISTIC SIDE EFFECT CONFIRMED: workspace '$WORKER_NAME' now EXISTS (id=$WORKER_ID)"

# Confirm it is a real workspace row (kind='workspace') parented under the org —
# i.e. a genuine create, not a no-op echo. parent_id may be the concierge (the
# concierge creates children under itself by convention) or another node; we
# assert only that it's a non-platform workspace, which is what create_workspace
# yields.
WORKER_KIND=$(workspace_field "$WORKER_ID" kind)
if [ -n "$WORKER_KIND" ] && [ "$WORKER_KIND" != "workspace" ]; then
  fail "created node '$WORKER_NAME' has kind='$WORKER_KIND' (want 'workspace') — not a real worker create"
fi
ok "Created node is a real kind='workspace' row"

# Soft confirmation: the concierge SHOULD report back. Non-fatal (the side
# effect above is the hard proof) — but a reply that is itself an error is a
# yellow flag worth logging even though the row landed.
if [ -n "$AGENT_TEXT" ]; then
  if a2a_completion_error_marker "$AGENT_TEXT" >/dev/null; then
    log "    ⚠️  concierge reply looks like an error-as-text even though the workspace was created — investigate the tool result surfacing."
  else
    ok "Concierge replied confirming the action (non-error)"
  fi
else
  log "    (concierge returned no text part — the row landing is the proof; reply is optional)"
fi

ok "═══ STAGING CONCIERGE CREATES-A-WORKSPACE E2E PASSED ═══"
log "Proven: a natural-language A2A request → the concierge's LLM invoked create_workspace via the platform MCP → real org mutation (workspace '$WORKER_NAME' id=$WORKER_ID). Teardown runs via EXIT trap."
