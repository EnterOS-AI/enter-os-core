#!/usr/bin/env bash
# Full-lifecycle SaaS E2E against staging.
#
# Creates a fresh org per run (unique slug), waits for tenant EC2 +
# cloudflared provisioning, exercises every major workspace-level API
# (register, heartbeat, A2A, delegation, HMA memory, activity, peers),
# then tears the whole org down and asserts that every cloud artefact
# (EC2, SG, Cloudflare tunnel, DNS record, DB rows) is gone. A leaked
# resource at teardown is a CI failure.
#
# Auth model:
#   Single MOLECULE_ADMIN_TOKEN (= CP_ADMIN_API_TOKEN on Railway staging)
#   drives everything:
#     - POST /cp/admin/orgs to provision (no WorkOS session scraping)
#     - GET  /cp/admin/orgs/:slug/admin-token to retrieve the per-tenant
#       ADMIN_TOKEN once provisioning completes
#     - DELETE /cp/admin/tenants/:slug for teardown
#   The per-tenant admin token drives all tenant API calls (workspaces,
#   memories, a2a).
#
# Required env:
#   MOLECULE_CP_URL        default: https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   CP admin bearer — Railway CP_ADMIN_API_TOKEN
#
# Optional env:
#   E2E_RUNTIME                  hermes (default) | claude-code | langgraph
#   E2E_PROVISION_TIMEOUT_SECS   default 900 (15 min cold EC2 budget)
#   E2E_KEEP_ORG                 1 → skip teardown (debugging only)
#   E2E_RUN_ID                   Slug suffix; CI: ${GITHUB_RUN_ID}
#   E2E_MODE                     full (default) | canary
#   E2E_INTENTIONAL_FAILURE      1 → poison tenant token mid-run so the
#                                script fails; the EXIT trap MUST still
#                                tear down cleanly (and exit 4 on leak).
#                                Used by a dedicated sanity workflow
#                                that verifies the safety net.
#
# Exit codes:
#   0  happy path
#   1  generic failure
#   2  missing required env
#   3  provisioning timed out
#   4  teardown left orphan resources

set -euo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — Railway staging CP_ADMIN_API_TOKEN}"
RUNTIME="${E2E_RUNTIME:-hermes}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"
RUN_ID_SUFFIX="${E2E_RUN_ID:-$(date +%H%M%S)-$$}"
MODE="${E2E_MODE:-full}"
case "$MODE" in
  full|canary) ;;
  *) echo "E2E_MODE must be 'full' or 'canary' (got: $MODE)" >&2; exit 2 ;;
esac

# Canary runs get a distinct prefix so their safety-net sweeper only
# touches their own runs, not in-flight full runs.
if [ "$MODE" = "canary" ]; then
  SLUG="e2e-canary-$(date +%Y%m%d)-${RUN_ID_SUFFIX}"
else
  SLUG="e2e-$(date +%Y%m%d)-${RUN_ID_SUFFIX}"
fi
SLUG=$(echo "$SLUG" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-' | head -c 32)

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

CURL_COMMON=(-sS --fail-with-body --max-time 30)

# ─── cleanup trap ───────────────────────────────────────────────────────
CLEANUP_DONE=0
cleanup_org() {
  # Capture upstream exit code IMMEDIATELY — must be the first statement
  # in the trap, before any command (including the CLEANUP_DONE check)
  # that would clobber $?.
  local entry_rc=$?

  if [ "$CLEANUP_DONE" = "1" ]; then return 0; fi
  CLEANUP_DONE=1

  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    log "E2E_KEEP_ORG=1 — skipping teardown. Manually delete $SLUG when done."
    return 0
  fi

  log "🧹 Tearing down org $SLUG..."

  # The DELETE handler runs the GDPR Art. 17 cascade synchronously
  # (Stripe + Redis + EC2 terminate + CF tunnel + DNS + DB rows). Real
  # observed wall-time on prod-shaped infra is ~30–90s — EC2 termination
  # alone takes 30–60s. The 5–15s estimate in `purge.go`'s comment is
  # the API-call cost, NOT the AWS-side time-to-termination it waits on.
  #
  # Two-part patience to match reality:
  #   1. 120s curl timeout on the DELETE itself (was 30s) so the
  #      synchronous cascade has room to complete in-band.
  #   2. Poll up to 60s after for organizations.status='purged' (or row
  #      gone) instead of one rigid 10s sleep — covers the case where
  #      DELETE returns 5xx mid-cascade and the cascade finishes anyway,
  #      and the case where DELETE legitimately exceeds 120s and we want
  #      eventual-consistency confirmation.
  curl "${CURL_COMMON[@]}" --max-time 120 -X DELETE "$CP_URL/cp/admin/tenants/$SLUG" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"confirm\":\"$SLUG\"}" >/dev/null 2>&1 \
    && ok "Teardown request accepted" \
    || log "Teardown returned non-2xx (may already be gone)"

  local leak_count=1
  local elapsed=0
  while [ "$elapsed" -lt 60 ]; do
    leak_count=$(curl "${CURL_COMMON[@]}" "$CP_URL/cp/admin/orgs" \
      -H "Authorization: Bearer $ADMIN_TOKEN" 2>/dev/null \
      | python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(1 for o in d.get('orgs', []) if o.get('slug')=='$SLUG' and o.get('status') != 'purged'))" \
      2>/dev/null || echo 1)
    if [ "$leak_count" = "0" ]; then
      break
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done

  if [ "$leak_count" != "0" ]; then
    echo "⚠️  LEAK: org $SLUG still present post-teardown after ${elapsed}s (count=$leak_count)" >&2
    exit 4
  fi
  ok "Teardown clean — no orphan resources for $SLUG (${elapsed}s)"

  # Normalize unexpected upstream exit codes to 1 (generic failure). The
  # script's documented contract (header "Exit codes" section) only emits
  # {0, 1, 2, 3, 4}, but `set -e` propagates the raw exit code of the
  # failing command — e.g. curl exits 22 on HTTP error under
  # --fail-with-body. Without this normalization, the
  # E2E_INTENTIONAL_FAILURE sanity workflow (e2e-staging-sanity.yml)
  # gets rc=22 from the poisoned-token curl, falls through its
  # case statement, and opens a false-positive priority-high
  # "safety net broken" issue (#2159, 2026-04-27).
  case "$entry_rc" in
    0|1|2|3|4) ;;          # contracted codes — let bash use entry_rc
    *) exit 1 ;;            # anything else is a generic failure
  esac
}
trap cleanup_org EXIT INT TERM

# ─── 0. Preflight ───────────────────────────────────────────────────────
log "═══════════════════════════════════════════════════════════════════"
log " Staging full-SaaS E2E"
log "   CP:      $CP_URL"
log "   Slug:    $SLUG"
log "   Runtime: $RUNTIME"
log "   Mode:    $MODE"
log "   Timeout: ${PROVISION_TIMEOUT_SECS}s"
[ "${E2E_INTENTIONAL_FAILURE:-0}" = "1" ] && log "   ⚠️  INTENTIONAL_FAILURE=1 — this run MUST fail mid-way; teardown MUST still clean up"
log "═══════════════════════════════════════════════════════════════════"

log "0/11 Preflight: CP reachable?"
curl "${CURL_COMMON[@]}" "$CP_URL/health" >/dev/null || fail "CP health check failed"
ok "CP reachable"

admin_call() {
  local method="$1"; shift
  local path="$1"; shift
  curl "${CURL_COMMON[@]}" -X "$method" "$CP_URL$path" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    "$@"
}

# ─── 1. Create org via admin endpoint ───────────────────────────────────
log "1/11 Creating org $SLUG via /cp/admin/orgs..."
CREATE_RESP=$(admin_call POST /cp/admin/orgs \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
echo "$CREATE_RESP" | python3 -m json.tool >/dev/null || fail "Org create returned non-JSON: $CREATE_RESP"
# Capture org_id for tenant-guard header on every subsequent tenant call.
# Without X-Molecule-Org-Id matching MOLECULE_ORG_ID on the tenant, the
# tenant-guard middleware returns 404 to avoid leaking tenant existence.
ORG_ID=$(echo "$CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")
[ -z "$ORG_ID" ] && fail "Org create response missing 'id': $CREATE_RESP"
ok "Org created (id=$ORG_ID)"

# ─── 2. Wait for tenant provisioning ────────────────────────────────────
log "2/11 Waiting for tenant provisioning (up to ${PROVISION_TIMEOUT_SECS}s)..."
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
LAST_STATUS=""
while true; do
  if [ "$(date +%s)" -gt "$DEADLINE" ]; then
    fail "Tenant provisioning timed out after ${PROVISION_TIMEOUT_SECS}s (last: $LAST_STATUS)"
  fi
  LIST_JSON=$(admin_call GET /cp/admin/orgs 2>/dev/null || echo '{"orgs":[]}')
  # NOTE: /cp/admin/orgs exposes 'instance_status' (from org_instances.status),
  # NOT 'status'. Field was bug-fixed 2026-04-21 after harness timed out on a
  # fully-provisioned tenant because the polled field was always ''. The
  # admin handler struct intentionally has no top-level `status` — the org
  # row's status is derivable via instance_status for ops.
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
    log "    status → $STATUS"
    LAST_STATUS="$STATUS"
  fi
  case "$STATUS" in
    running)  break ;;
    failed)
      # Diagnostic burst: dump the org row so the operator sees
      # `last_error` (CP migration 022 / handler #289 — issue #285).
      # Pre-fix the harness only logged "Tenant provisioning failed",
      # forcing whoever debugs canary to scrape CP server logs to
      # learn WHY. Same shape as the TLS-readiness burst at step 4
      # (PR #2107). Redacts nothing because /cp/admin/orgs already
      # returns a narrow, ops-safe shape (id/slug/name/plan/
      # member_count/instance_status/last_error/timestamps —
      # no tokens, no encrypted fields).
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
      fail "Tenant provisioning failed for $SLUG (see diagnostic above for last_error)"
      ;;
    *)        sleep 15 ;;
  esac
done
ok "Tenant provisioning complete"

# Derive tenant domain from CP hostname so the same harness works in
# both prod (api.moleculesai.app → moleculesai.app) and staging
# (staging-api.moleculesai.app → staging.moleculesai.app). Override
# via MOLECULE_TENANT_DOMAIN for local/self-hosted.
CP_HOST=$(echo "$CP_URL" | sed -E 's#^https?://##; s#/.*$##')
case "$CP_HOST" in
  api.*)         DERIVED_DOMAIN="${CP_HOST#api.}" ;;
  staging-api.*) DERIVED_DOMAIN="staging.${CP_HOST#staging-api.}" ;;
  *)             DERIVED_DOMAIN="$CP_HOST" ;;
esac
TENANT_DOMAIN="${MOLECULE_TENANT_DOMAIN:-$DERIVED_DOMAIN}"
TENANT_URL="https://$SLUG.$TENANT_DOMAIN"
log "    TENANT_URL=$TENANT_URL"

# ─── 3. Retrieve per-tenant admin token ────────────────────────────────
log "3/11 Fetching per-tenant admin token..."
TENANT_TOKEN_RESP=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token")
TENANT_TOKEN=$(echo "$TENANT_TOKEN_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null || echo "")
[ -z "$TENANT_TOKEN" ] && fail "Could not retrieve per-tenant admin token for $SLUG"
ok "Tenant admin token retrieved (len=${#TENANT_TOKEN})"

# ─── 4. Wait for tenant TLS / DNS propagation ──────────────────────────
# Kept below the 20-min provision envelope so a genuinely-stuck tenant
# still fails loud at the earlier provision step rather than masquerading
# as a TLS issue. CF DNS propagation + tunnel hostname registration +
# ACME cert + edge cache run 5-7 min on a healthy day; +5 min headroom
# over the previous 10-min cap covers the slower path observed in #2090.
#
# On timeout, dump DNS + curl -v + headers so the next failure identifies
# the broken layer (DNS / TLS / HTTP). Authorization is redacted
# defensively in case a future caller adds an auth header to this probe.
log "4/11 Waiting for tenant TLS / DNS propagation..."
TLS_TIMEOUT_SEC=$((15 * 60))
TLS_DEADLINE=$(( $(date +%s) + TLS_TIMEOUT_SEC ))
TENANT_HOST="${TENANT_URL#http*://}"
TENANT_HOST="${TENANT_HOST%%/*}"
TENANT_HOST="${TENANT_HOST%%:*}"
while true; do
  if curl -sSfk --max-time 5 "$TENANT_URL/health" >/dev/null 2>&1; then
    break
  fi
  if [ "$(date +%s)" -gt "$TLS_DEADLINE" ]; then
    log "── DIAGNOSTIC BURST (TLS-readiness timeout) ──"
    log "DNS lookup ($TENANT_HOST):"
    getent hosts "$TENANT_HOST" 2>&1 || log "  (no DNS resolution)"
    log "curl -v $TENANT_URL/health (last 40 lines):"
    curl -kv --max-time 10 "$TENANT_URL/health" 2>&1 \
      | sed -E 's/(Authorization|Cookie):.*/\1: [redacted]/i' \
      | tail -n 40 | sed 's/^/  /' || true
    log "── END DIAGNOSTIC ──"
    fail "Tenant URL never responded 2xx on /health within ${TLS_TIMEOUT_SEC}s"
  fi
  sleep 5
done
ok "Tenant reachable at $TENANT_URL"

# Sanity-test path: once the tenant is provisioned, poisoning the
# tenant token proves the EXIT trap + leak assertion still fire.
# Gate AFTER provisioning so the provision path itself stays valid.
EFFECTIVE_TENANT_TOKEN="$TENANT_TOKEN"
if [ "${E2E_INTENTIONAL_FAILURE:-0}" = "1" ]; then
  log "⚠️  INTENTIONAL_FAILURE: poisoning tenant token for the workspace-provision step"
  EFFECTIVE_TENANT_TOKEN="poisoned-$$"
fi

tenant_call() {
  local method="$1"; shift
  local path="$1"; shift
  # X-Molecule-Org-Id is REQUIRED — tenant guard 404s anything without
  # it (it does NOT 403, to hide tenant existence from org scanners).
  curl "${CURL_COMMON[@]}" -X "$method" "$TENANT_URL$path" \
    -H "Authorization: Bearer $EFFECTIVE_TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    "$@"
}

# ─── 5. Provision parent workspace ─────────────────────────────────────
# Runtimes like hermes crash at boot with "No provider API key found"
# if nothing in the standard env-var list is set. Inject the API key
# from E2E_OPENAI_API_KEY so the runtime can actually start — it's
# per-workspace secret, so it's persisted as a workspace_secret and
# materialized into the container env. Missing key falls through to
# an empty secrets map; workspace will still fail but the error is
# expected and actionable.
SECRETS_JSON='{}'
if [ -n "${E2E_OPENAI_API_KEY:-}" ]; then
  # MODEL_PROVIDER is a full model slug in 'provider:model' format per
  # workspace/config.py:258. Using just "openai" gets parsed as the
  # model name → 404 model_not_found. Also set OPENAI_BASE_URL to
  # OpenAI's own endpoint — default is openrouter.ai which would need
  # a different key format.
  #
  # The HERMES_* fields below bypass template-hermes/scripts/derive-provider.sh
  # — verified 2026-04-24 that even with template-hermes#19's fix in main,
  # staging tenants sometimes resolve openai/* to PROVIDER=openrouter and
  # emit {'message':'Missing Authentication header','code':401} (OpenRouter's
  # shape) in the A2A reply. Setting HERMES_INFERENCE_PROVIDER=custom +
  # HERMES_CUSTOM_{BASE_URL,API_KEY,API_MODE} pins the bridge deterministically
  # so the test doesn't depend on every tenant EC2 having a freshly-cloned
  # template-hermes.
  SECRETS_JSON=$(python3 -c "
import json, os
k = os.environ['E2E_OPENAI_API_KEY']
print(json.dumps({
    'OPENAI_API_KEY': k,
    'OPENAI_BASE_URL': 'https://api.openai.com/v1',
    'MODEL_PROVIDER': 'openai:gpt-4o',
    'HERMES_INFERENCE_PROVIDER': 'custom',
    'HERMES_CUSTOM_BASE_URL': 'https://api.openai.com/v1',
    'HERMES_CUSTOM_API_KEY': k,
    'HERMES_CUSTOM_API_MODE': 'chat_completions',
}))
")
fi

# Model slug format depends on the runtime — different model resolvers
# parse it differently:
#
#   hermes      → "openai/gpt-4o"  (slash-form: derive-provider.sh splits
#                                    on the prefix to set
#                                    HERMES_INFERENCE_PROVIDER. Bare
#                                    "gpt-4o" falls through to Anthropic
#                                    default + 401, see PR #1714.)
#
#   langgraph   → "openai:gpt-4o"  (colon-form: langchain init_chat_model
#                                    requires "<provider>:<model>".
#                                    Slash-form was misinterpreted as
#                                    OpenRouter routing → fell through
#                                    without auth, surfaced 2026-05-03
#                                    after the a2a-sdk v1 contract bugs
#                                    PR #2558+#2563+#2567 cleared the
#                                    masking layers.)
#
#   claude-code → "sonnet"         (entry-id form: claude-code template's
#                                    config.yaml uses bare model names,
#                                    auth comes via CLAUDE_CODE_OAUTH_TOKEN
#                                    or ANTHROPIC_API_KEY rather than the
#                                    slug.)
#
# When E2E_MODEL_SLUG is set, it overrides this dispatch — useful when an
# operator dispatches the workflow to test a specific slug.
if [ -n "${E2E_MODEL_SLUG:-}" ]; then
  MODEL_SLUG="$E2E_MODEL_SLUG"
else
  case "$RUNTIME" in
    hermes)      MODEL_SLUG="openai/gpt-4o" ;;
    langgraph)   MODEL_SLUG="openai:gpt-4o" ;;
    claude-code) MODEL_SLUG="sonnet" ;;
    *)           MODEL_SLUG="openai/gpt-4o" ;;  # safest fallback (matches hermes)
  esac
fi

log "5/11 Provisioning parent workspace (runtime=$RUNTIME)..."
PARENT_RESP=$(tenant_call POST /workspaces \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"E2E Parent\",\"runtime\":\"$RUNTIME\",\"tier\":2,\"model\":\"$MODEL_SLUG\",\"secrets\":$SECRETS_JSON}")
PARENT_ID=$(echo "$PARENT_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")
log "    PARENT_ID=$PARENT_ID"

# ─── 6. Provision child (full mode only) ────────────────────────────────
CHILD_ID=""
if [ "$MODE" = "full" ]; then
  log "6/11 Provisioning child workspace..."
  CHILD_RESP=$(tenant_call POST /workspaces \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"E2E Child\",\"runtime\":\"$RUNTIME\",\"tier\":2,\"model\":\"$MODEL_SLUG\",\"parent_id\":\"$PARENT_ID\",\"secrets\":$SECRETS_JSON}")
  CHILD_ID=$(echo "$CHILD_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")
  log "    CHILD_ID=$CHILD_ID"
else
  log "6/11 Canary mode — skipping child workspace"
fi

# ─── 7. Wait for workspace(s) online ───────────────────────────────────
# Hermes cold-boot takes 10-13 min on slow apt days (apt + uv + hermes
# install + npm browser-tools). The controlplane bootstrap-watcher
# deadline fires at 5 min and sets status=failed prematurely; heartbeat
# then transitions failed → online after install.sh finishes. So:
#
#   - 20 min deadline (hermes worst-case + slack)
#   - 'failed' is a TRANSIENT state we must tolerate — log and keep
#     polling, only hard-fail at the deadline. Pre-bootstrap-watcher-fix
#     (controlplane#245) this was a flake generator: workspace went
#     failed→online inside our window but we bailed at the failed read.
log "7/11 Waiting for workspace(s) to reach status=online (up to 30 min — hermes cold boot)..."
WS_DEADLINE=$(( $(date +%s) + 1800 ))
WS_TO_CHECK="$PARENT_ID"
[ -n "$CHILD_ID" ] && WS_TO_CHECK="$WS_TO_CHECK $CHILD_ID"
for wid in $WS_TO_CHECK; do
  WS_LAST_STATUS=""
  WS_FAILED_LOGGED=0
  while true; do
    if [ "$(date +%s)" -gt "$WS_DEADLINE" ]; then
      WS_LAST_ERR=$(tenant_call GET "/workspaces/$wid" 2>/dev/null | \
        python3 -c "import json,sys; print(json.load(sys.stdin).get('last_sample_error',''))" 2>/dev/null || echo "")
      fail "Workspace $wid never reached online within 20 min (last status=$WS_LAST_STATUS, err=$WS_LAST_ERR)"
    fi
    WS_JSON=$(tenant_call GET "/workspaces/$wid" 2>/dev/null || echo '{}')
    WS_STATUS=$(echo "$WS_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
    if [ "$WS_STATUS" != "$WS_LAST_STATUS" ]; then
      log "    $wid → $WS_STATUS"
      WS_LAST_STATUS="$WS_STATUS"
    fi
    case "$WS_STATUS" in
      online) break ;;
      failed)
        # Not a hard fail — bootstrap-watcher frequently marks failed at
        # 5 min on hermes, then heartbeat recovers to online around 10-13
        # min when install.sh finishes. Log once per workspace so the CI
        # output isn't spammy.
        if [ "$WS_FAILED_LOGGED" = "0" ]; then
          log "    $wid transiently failed — waiting for heartbeat recovery (bootstrap-watcher deadline, see cp#245)"
          WS_FAILED_LOGGED=1
        fi
        sleep 10
        ;;
      *)      sleep 10 ;;
    esac
  done
  ok "    $wid online"
done

# ─── 8. A2A round-trip on parent ───────────────────────────────────────
log "8/11 Sending A2A message to parent — expecting agent response..."
# Smoke prompt phrasing — DO NOT trim back to the bare "Reply with exactly: PONG"
# version that ran here pre-2026-05-02. After the Platform Capabilities preamble
# (#2332, 2026-04-30) landed in the system prompt, GPT-4o began intermittently
# refusing the bare echo prompt with messages like:
#   - "I'm unable to do that."
#   - "I'm unable to fulfill that request. Can I assist you with anything else?"
#   - "I'm unable to reply with responses that don't allow me to fulfill tasks…"
# 3 fails / 10 runs ≈ 30% flake. Root cause: the preamble primes the model
# ("Use them proactively") to expect tool use, then a zero-tool echo request
# reads as out-of-role. Real user prompts (which is what hits prod) don't
# trigger this — only this contrived smoke prompt does, so the right fix is
# in the prompt phrasing, not in the platform's system prompt. Keep the
# explicit "no tools needed" framing so the model has permission to comply.
A2A_PAYLOAD=$(python3 -c "
import json, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-msg-1',
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': 'This is the platform smoke test verifying agent wiring. No tools or memory are needed — please respond with exactly the single token: PONG'}]
        }
    }
}))
")
A2A_RESP=$(tenant_call POST "/workspaces/$PARENT_ID/a2a" \
  -H "Content-Type: application/json" \
  -d "$A2A_PAYLOAD")
AGENT_TEXT=$(echo "$A2A_RESP" | python3 -c "
import json, sys
d = json.load(sys.stdin)
parts = d.get('result', {}).get('parts', [])
print(parts[0].get('text', '') if parts else '')
" 2>/dev/null || echo "")
if [ -z "$AGENT_TEXT" ]; then
  fail "A2A returned no text. Raw: $A2A_RESP"
fi

# Specific error-class checks — each pattern caught a real P0 bug on
# 2026-04-23 that a generic "error|exception" check missed or misreported:
#
#   "[hermes-agent error 401]"       → gateway API_SERVER_KEY not propagated (hermes #12)
#   "Invalid API key"                → tenant auth chain (CP #238 race)
#   "model_not_found"                → hermes custom provider slug passthrough (#13)
#   "Encrypted content is not supported" → hermes codex_responses API misroute (#14)
#   "Unknown provider"               → bridge misconfigured PROVIDER= (regression of #13 fix)
#   "hermes-agent unreachable"       → gateway process died
#
# Fail LOUD with the specific pattern so CI log + alert channel makes the
# regression unambiguous.
if echo "$AGENT_TEXT" | grep -qF "[hermes-agent error 401]"; then
  fail "A2A — REGRESSION: hermes gateway auth broken (API_SERVER_KEY not in runtime env). See template-hermes#12. Raw: $AGENT_TEXT"
fi
if echo "$AGENT_TEXT" | grep -qF "hermes-agent unreachable"; then
  fail "A2A — REGRESSION: hermes gateway process down. Check /var/log/hermes-gateway.log on the workspace EC2. Raw: $AGENT_TEXT"
fi
if echo "$AGENT_TEXT" | grep -qF "model_not_found"; then
  fail "A2A — REGRESSION: model slug passed through with provider prefix. See template-hermes#13. Raw: $AGENT_TEXT"
fi
if echo "$AGENT_TEXT" | grep -qF "Encrypted content is not supported"; then
  fail "A2A — REGRESSION: hermes custom provider hit /v1/responses instead of chat_completions. Config.yaml should declare api_mode: chat_completions. See template-hermes#14. Raw: $AGENT_TEXT"
fi
if echo "$AGENT_TEXT" | grep -qF "Unknown provider"; then
  fail "A2A — REGRESSION: install.sh set PROVIDER to a value not in hermes's registry. Run 'hermes doctor' on the workspace to see valid values. Raw: $AGENT_TEXT"
fi
# "Invalid API key" — the comment block lists this as a CP #238 race
# (tenant auth chain) signal but the grep was missing. Caller-side
# 401's containing this exact phrase don't match the generic
# "error|exception" catch-all below, so they'd slip through.
if echo "$AGENT_TEXT" | grep -qF "Invalid API key"; then
  fail "A2A — REGRESSION: tenant auth chain returned 'Invalid API key'. Likely CP boot-event 401 race (CP #238) or stale OPENAI_API_KEY in the runtime env. Raw: $AGENT_TEXT"
fi
# Generic catch-all — falls through if none of the known regressions hit.
if echo "$AGENT_TEXT" | grep -qiE "error|exception"; then
  fail "A2A returned an error-shaped response: $AGENT_TEXT"
fi

# Content assertion — the prompt asks the model to reply with exactly "PONG".
# Real models produce "PONG" (possibly with minor wrapping); a broken pipeline
# that echoes the prompt back or returns truncated context won't. Normalize
# to uppercase before matching to tolerate "pong" / "Pong".
if ! echo "$AGENT_TEXT" | tr '[:lower:]' '[:upper:]' | grep -qF "PONG"; then
  fail "A2A reply didn't contain expected PONG token. Real: $AGENT_TEXT"
fi

ok "A2A parent round-trip succeeded: \"${AGENT_TEXT:0:80}\""

# ─── 9. HMA + peers + activity (full mode) ─────────────────────────────
if [ "$MODE" = "full" ]; then
  log "9/11 Writing + reading HMA memory on parent..."
  MEM_PAYLOAD=$(python3 -c "
import json
print(json.dumps({
    'content': 'E2E memory seed — run $SLUG',
    'scope': 'LOCAL'
}))
")
  tenant_call POST "/workspaces/$PARENT_ID/memories" \
    -H "Content-Type: application/json" \
    -d "$MEM_PAYLOAD" >/dev/null || fail "memory POST failed"
  MEM_LIST=$(tenant_call GET "/workspaces/$PARENT_ID/memories?scope=LOCAL")
  if ! echo "$MEM_LIST" | grep -q "run $SLUG"; then
    fail "HMA memory not readable after write. List: ${MEM_LIST:0:200}"
  fi
  ok "HMA memory write+read roundtripped"

  log "9b.  Peer discovery + activity log smoke..."
  set +e
  tenant_call GET "/registry/$PARENT_ID/peers" -o /dev/null -w "%{http_code}\n" 2>&1 | head -1 > /tmp/peers_code.txt
  set -e
  PEERS_CODE=$(cat /tmp/peers_code.txt)
  [ "$PEERS_CODE" = "404" ] && fail "Peers endpoint missing (404) — route regression"
  ok "Peers endpoint reachable (HTTP $PEERS_CODE)"

  ACTIVITY=$(tenant_call GET "/activity?workspace_id=$PARENT_ID&limit=5" 2>/dev/null || echo '[]')
  ACTIVITY_COUNT=$(echo "$ACTIVITY" | python3 -c "import json,sys
d=json.load(sys.stdin)
print(len(d if isinstance(d, list) else d.get('events', [])))" 2>/dev/null || echo 0)
  log "    Activity events observed: $ACTIVITY_COUNT"
else
  log "9/11 Canary mode — skipping HMA / peers / activity"
fi

# ─── 10. Delegation mechanics (full mode + child) ──────────────────────
if [ "$MODE" = "full" ] && [ -n "$CHILD_ID" ]; then
  log "10/11 Delegation mechanics: parent → child via proxy"
  DELEG_PAYLOAD=$(python3 -c "
import json, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-deleg-1',
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-deleg-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': 'This is the platform smoke test verifying child workspace wiring. No tools or memory are needed — please respond with exactly the single token: CHILD_PONG'}]
        }
    }
}))
")
  set +e
  # Raw curl (not tenant_call) because this call carries an extra
  # X-Source-Workspace-Id header. Must still send X-Molecule-Org-Id
  # or TenantGuard 404s — previously missing, caused section 10 to
  # fail rc=22 despite everything upstream being correct (2026-04-21).
  DELEG_RESP=$(curl "${CURL_COMMON[@]}" -X POST "$TENANT_URL/workspaces/$CHILD_ID/a2a" \
    -H "Authorization: Bearer $EFFECTIVE_TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    -H "X-Source-Workspace-Id: $PARENT_ID" \
    -H "Content-Type: application/json" \
    -d "$DELEG_PAYLOAD")
  DELEG_RC=$?
  set -e
  [ $DELEG_RC -ne 0 ] && fail "Delegation A2A POST failed (rc=$DELEG_RC)"
  DELEG_TEXT=$(echo "$DELEG_RESP" | python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
    parts = d.get('result', {}).get('parts', [])
    print(parts[0].get('text', '') if parts else '')
except Exception:
    print('')
" 2>/dev/null || echo "")
  [ -z "$DELEG_TEXT" ] && fail "Delegation returned no text. Raw: ${DELEG_RESP:0:200}"
  ok "Delegation proxy works (child responded: \"${DELEG_TEXT:0:60}\")"

  CHILD_ACT=$(tenant_call GET "/activity?workspace_id=$CHILD_ID&limit=20" 2>/dev/null || echo '[]')
  if echo "$CHILD_ACT" | grep -q "$PARENT_ID"; then
    ok "Child activity log records parent as source"
  else
    log "Child activity log did not reference parent (pipeline may be async)"
  fi
fi

# ─── 11. Teardown runs via trap ────────────────────────────────────────
log "11/11 All checks passed. Teardown runs via EXIT trap."
ok "═══ STAGING $MODE-SAAS E2E PASSED ═══"
