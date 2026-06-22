#!/usr/bin/env bash
set -uo pipefail
#
# test_keyless_feature_contracts_e2e.sh — REQUIRED-lane (E2E API Smoke Test)
# keyless HTTP-contract coverage for feature endpoints that ship WITHOUT an
# LLM key and had NO e2e assertion before (coverage-audit gap list).
#
# Why a NEW script (not added to test_api.sh): PR #2286 is concurrently
# rewriting test_api.sh's auth helpers + _lib.sh (e2e_admin_auth_args) and the
# test_priority_runtimes mock arm. Keeping these assertions in a standalone
# file avoids a merge conflict with that in-flight PR and keeps the new feature
# coverage independently reviewable. The mock-runtime A2A canned round-trip is
# OWNED by #2286's `mock` arm (run_mock) — intentionally NOT duplicated here.
#
# Every endpoint below is exercised against a runtime=external workspace so NO
# LLM key is needed. For each we assert the real HTTP contract: the happy path
# AND a meaningful failure mode (401 without auth, 400 on bad input, or the
# documented fail-closed status) so the test catches REAL regressions, not
# just 200s.
#
# Auth model (matches workspace-server/internal/middleware/wsauth_middleware.go):
#   * WorkspaceAuth (/workspaces/:id/*) is STRICT once a token exists — a
#     bearer-less request 401s (devmode fail-open needs MOLECULE_ENV=dev AND
#     ADMIN_TOKEN unset, neither of which the e2e-api job sets).
#   * AdminAuth routes accept the platform ADMIN_TOKEN (post-#2286) OR, when no
#     ADMIN_TOKEN is configured, any valid workspace bearer (Tier-3 fallback) —
#     so the workspace token we mint authenticates admin routes in BOTH the
#     pre-#2286 (no ADMIN_TOKEN) and post-#2286 (ADMIN_TOKEN set) CI shapes.
#
# Local-run shape (mirrors the e2e-api job — real PG+Redis+platform):
#   DATABASE_URL=... REDIS_URL=... ADMIN_TOKEN=... ./platform-server &
#   BASE=http://127.0.0.1:$PORT bash tests/e2e/test_keyless_feature_contracts_e2e.sh

source "$(dirname "$0")/_lib.sh"  # sets BASE default

PASS=0
FAIL=0

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1"; echo "  $2"; FAIL=$((FAIL + 1)); }

# assert_contains DESC EXPECTED_SUBSTRING ACTUAL
assert_contains() {
  if printf '%s' "$3" | grep -qF "$2"; then
    pass "$1"
  else
    fail "$1" "expected to contain [$2] — got: $3"
  fi
}

# http_code METHOD URL [curl-args...] → prints the HTTP status code only.
http_code() {
  local method="$1" url="$2"; shift 2
  curl -s -o /dev/null -w "%{http_code}" -X "$method" "$url" "$@"
}

# body_and_code METHOD URL [curl-args...] → prints "<body>\n<code>".
body_and_code() {
  local method="$1" url="$2"; shift 2
  curl -s -w $'\n%{http_code}' -X "$method" "$url" "$@"
}

echo "=== Keyless feature HTTP-contract E2E (required lane) ==="
echo ""

# Platform admin bearer when the job set one (#2286 shape). When ADMIN_TOKEN is
# configured, AdminAuth's Tier-1 fail-open is OFF even before the first token
# exists, so admin-gated create / list / delete must carry it from the start.
# Pre-#2286 (no ADMIN_TOKEN) this is empty → fail-open create works bare.
ENV_ADMIN="${MOLECULE_ADMIN_TOKEN:-${ADMIN_TOKEN:-}}"
ENV_ADMIN_AUTH=()
[ -n "$ENV_ADMIN" ] && ENV_ADMIN_AUTH=(-H "Authorization: Bearer $ENV_ADMIN")

# Reproducible counts across reruns. e2e_cleanup_all_workspaces hits the
# admin-gated list/delete; the platform admin bearer (if set) goes via the
# MOLECULE_ADMIN_TOKEN/ADMIN_TOKEN env the helper already reads.
e2e_cleanup_all_workspaces

# ---------------------------------------------------------------------------
# Fixture: one external workspace, registered → online. Keyless (external=true
# means no container is provisioned and no LLM key is consulted).
# ---------------------------------------------------------------------------
R=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
  ${ENV_ADMIN_AUTH[@]+"${ENV_ADMIN_AUTH[@]}"} \
  -d '{"name":"Keyless Fixture","tier":1,"runtime":"external","external":true}')
WS_ID=$(printf '%s' "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
if [ -z "$WS_ID" ]; then
  echo "FATAL: could not create fixture workspace — got: $R" >&2
  exit 2
fi
assert_contains "POST /workspaces (external fixture created)" '"status":"awaiting_agent"' "$R"

# Workspace token: register returns one; else mint via the admin endpoint.
WS_TOKEN=$(printf '%s' "$R" | e2e_extract_token)
if [ -z "$WS_TOKEN" ]; then
  WS_TOKEN=$(e2e_mint_workspace_token "$WS_ID" 2>/dev/null || echo "")
fi
if [ -z "$WS_TOKEN" ]; then
  echo "FATAL: could not obtain workspace token for $WS_ID" >&2
  exit 2
fi
AUTH=(-H "Authorization: Bearer $WS_TOKEN")

# Admin bearer: explicit platform ADMIN_TOKEN if the job set one (#2286 shape),
# else the workspace token (AdminAuth Tier-3 accepts it pre-#2286).
ADMIN_BEARER="${ENV_ADMIN:-$WS_TOKEN}"
ADMIN_AUTH=(-H "Authorization: Bearer $ADMIN_BEARER")

# Bring the fixture online so lifecycle (hibernate) has a hibernatable state.
curl -s -X POST "$BASE/registry/register" -H "Content-Type: application/json" "${AUTH[@]}" \
  -d "{\"id\":\"$WS_ID\",\"url\":\"https://example.com/keyless\",\"agent_card\":{\"name\":\"Keyless Fixture\",\"skills\":[{\"id\":\"noop\",\"name\":\"Noop\"}]}}" >/dev/null

# ===========================================================================
# 1. Terminal diagnose — GET /workspaces/:id/terminal/diagnose (wsAuth)
#    External workspace has no instance_id → diagnoseLocal path → 200 with a
#    deterministic report (ok=false, first_failure on docker/container). The
#    /terminal endpoint itself is a WebSocket upgrade (not HTTP-assertable
#    keyless); diagnose is its pure-HTTP sibling and the real contract surface.
# ===========================================================================
echo "--- /terminal/diagnose ---"
BC=$(body_and_code GET "$BASE/workspaces/$WS_ID/terminal/diagnose" "${AUTH[@]}")
DIAG_CODE=$(printf '%s' "$BC" | tail -n1)
DIAG_BODY=$(printf '%s' "$BC" | sed '$d')
assert_contains "GET /terminal/diagnose (200 report)" "200" "$DIAG_CODE"
assert_contains "GET /terminal/diagnose (carries workspace_id)" "\"workspace_id\":\"$WS_ID\"" "$DIAG_BODY"
assert_contains "GET /terminal/diagnose (has steps[])" '"steps"' "$DIAG_BODY"
# Failure mode: no bearer → 401 (WorkspaceAuth strict once a token exists).
assert_contains "GET /terminal/diagnose (no auth → 401)" "401" \
  "$(http_code GET "$BASE/workspaces/$WS_ID/terminal/diagnose")"

# ===========================================================================
# 2. Webhooks (public) — POST /webhooks/:type
#    Public, no auth. telegram adapter: empty update body → (nil,nil) → 200
#    ignored; non-JSON → parse error → 400; unknown type → 404.
# ===========================================================================
echo "--- /webhooks/:type ---"
BC=$(body_and_code POST "$BASE/webhooks/telegram" -H "Content-Type: application/json" -d '{}')
WH_CODE=$(printf '%s' "$BC" | tail -n1)
WH_BODY=$(printf '%s' "$BC" | sed '$d')
assert_contains "POST /webhooks/telegram (non-message update → 200)" "200" "$WH_CODE"
assert_contains "POST /webhooks/telegram (status ignored)" '"status":"ignored"' "$WH_BODY"
assert_contains "POST /webhooks/telegram (bad JSON → 400)" "400" \
  "$(http_code POST "$BASE/webhooks/telegram" -H 'Content-Type: application/json' -d 'not-json')"
assert_contains "POST /webhooks/<unknown> (→ 404)" "404" \
  "$(http_code POST "$BASE/webhooks/nope-not-a-channel" -H 'Content-Type: application/json' -d '{}')"

# ===========================================================================
# 3. Budget — GET /workspaces/:id/budget (wsAuth) + PATCH (admin)
#    GET: fresh workspace → multi-period view, no limits, zero spend.
#    PATCH: set monthly limit (admin) → reflected; bad input → 400.
# ===========================================================================
echo "--- /budget ---"
BUD=$(curl -s "$BASE/workspaces/$WS_ID/budget" "${AUTH[@]}")
assert_contains "GET /budget (has periods map)" '"periods"' "$BUD"
assert_contains "GET /budget (monthly_spend 0 on fresh ws)" '"monthly_spend":0' "$BUD"
# PATCH is admin-gated (router.go:419). Set a monthly limit and verify echo.
PB=$(curl -s -X PATCH "$BASE/workspaces/$WS_ID/budget" -H "Content-Type: application/json" "${ADMIN_AUTH[@]}" \
  -d '{"budget_limits":{"monthly":2000}}')
assert_contains "PATCH /budget (monthly limit set → echoed)" '"budget_limit":2000' "$PB"
# Re-read confirms persistence.
assert_contains "GET /budget (limit persisted)" '"budget_limit":2000' \
  "$(curl -s "$BASE/workspaces/$WS_ID/budget" "${AUTH[@]}")"
# Failure: empty body → 400 "budget_limits or budget_limit field is required".
assert_contains "PATCH /budget (empty body → 400)" "400" \
  "$(http_code PATCH "$BASE/workspaces/$WS_ID/budget" -H 'Content-Type: application/json' "${ADMIN_AUTH[@]}" -d '{}')"
# Failure: unknown period → 400.
assert_contains "PATCH /budget (unknown period → 400)" "400" \
  "$(http_code PATCH "$BASE/workspaces/$WS_ID/budget" -H 'Content-Type: application/json' "${ADMIN_AUTH[@]}" -d '{"budget_limits":{"yearly":1}}')"
# Failure: GET without bearer → 401.
assert_contains "GET /budget (no auth → 401)" "401" "$(http_code GET "$BASE/workspaces/$WS_ID/budget")"

# ===========================================================================
# 4. Checkpoints — POST/GET/DELETE /workspaces/:id/checkpoints* (wsAuth)
#    Fully self-contained CRUD over workflow_checkpoints (#788). Upsert → latest
#    → list-by-wfid → delete → 404. Failure modes: missing workflow_id → 400,
#    empty latest → 404.
# ===========================================================================
echo "--- /checkpoints ---"
WFID="kl-wf-$$"
CP=$(curl -s -X POST "$BASE/workspaces/$WS_ID/checkpoints" -H "Content-Type: application/json" "${AUTH[@]}" \
  -d "{\"workflow_id\":\"$WFID\",\"step_name\":\"step-a\",\"step_index\":1,\"payload\":{\"k\":\"v\"}}")
assert_contains "POST /checkpoints (upsert → id + workflow_id)" "\"workflow_id\":\"$WFID\"" "$CP"
assert_contains "GET /checkpoints/latest (200 newest)" "\"workflow_id\":\"$WFID\"" \
  "$(curl -s "$BASE/workspaces/$WS_ID/checkpoints/latest" "${AUTH[@]}")"
assert_contains "GET /checkpoints/:wfid (lists the step)" '"step_name":"step-a"' \
  "$(curl -s "$BASE/workspaces/$WS_ID/checkpoints/$WFID" "${AUTH[@]}")"
DEL=$(curl -s -X DELETE "$BASE/workspaces/$WS_ID/checkpoints/$WFID" "${AUTH[@]}")
assert_contains "DELETE /checkpoints/:wfid (deleted count)" '"deleted":1' "$DEL"
assert_contains "GET /checkpoints/:wfid (after delete → 404)" "404" \
  "$(http_code GET "$BASE/workspaces/$WS_ID/checkpoints/$WFID" "${AUTH[@]}")"
# Failure: missing workflow_id → 400 (binding:required).
assert_contains "POST /checkpoints (missing workflow_id → 400)" "400" \
  "$(http_code POST "$BASE/workspaces/$WS_ID/checkpoints" -H 'Content-Type: application/json' "${AUTH[@]}" -d '{"step_name":"x"}')"
# Failure: no bearer → 401.
assert_contains "POST /checkpoints (no auth → 401)" "401" \
  "$(http_code POST "$BASE/workspaces/$WS_ID/checkpoints" -H 'Content-Type: application/json' -d '{"workflow_id":"x","step_name":"y"}')"

# ===========================================================================
# 5. Audit — GET /workspaces/:id/audit (wsAuth)
#    EU AI Act ledger query (#594). Fresh ws → empty events, total 0,
#    chain_valid null (AUDIT_LEDGER_SALT unset). Failure: bad RFC3339 from → 400.
# ===========================================================================
echo "--- /audit ---"
AUD=$(curl -s "$BASE/workspaces/$WS_ID/audit" "${AUTH[@]}")
assert_contains "GET /audit (total 0 on fresh ws)" '"total":0' "$AUD"
assert_contains "GET /audit (chain_valid null without salt)" '"chain_valid":null' "$AUD"
assert_contains "GET /audit (bad 'from' → 400)" "400" \
  "$(http_code GET "$BASE/workspaces/$WS_ID/audit?from=not-a-date" "${AUTH[@]}")"
assert_contains "GET /audit (no auth → 401)" "401" "$(http_code GET "$BASE/workspaces/$WS_ID/audit")"

# ===========================================================================
# 6. Traces — GET /workspaces/:id/traces (wsAuth)
#    Langfuse proxy (#590). No LANGFUSE_* configured → 200 [] (graceful empty),
#    never a 5xx. Failure: no auth → 401.
# ===========================================================================
echo "--- /traces ---"
BC=$(body_and_code GET "$BASE/workspaces/$WS_ID/traces" "${AUTH[@]}")
TR_CODE=$(printf '%s' "$BC" | tail -n1)
TR_BODY=$(printf '%s' "$BC" | sed '$d')
assert_contains "GET /traces (200 without Langfuse)" "200" "$TR_CODE"
assert_contains "GET /traces (empty list)" '[]' "$TR_BODY"
assert_contains "GET /traces (no auth → 401)" "401" "$(http_code GET "$BASE/workspaces/$WS_ID/traces")"

# ===========================================================================
# 7. Session search — GET /workspaces/:id/session-search (wsAuth)
#    Searches activity_logs. Seed one activity row, then assert q-filter finds
#    it and a non-matching q returns []. Failure: no auth → 401.
# ===========================================================================
echo "--- /session-search ---"
curl -s -X POST "$BASE/workspaces/$WS_ID/activity" -H "Content-Type: application/json" "${AUTH[@]}" \
  -d '{"activity_type":"agent_log","method":"inference","summary":"keyless-needle marker"}' >/dev/null
assert_contains "GET /session-search?q=keyless-needle (finds row)" 'keyless-needle' \
  "$(curl -s "$BASE/workspaces/$WS_ID/session-search?q=keyless-needle" "${AUTH[@]}")"
assert_contains "GET /session-search?q=<no-match> (empty)" '[]' \
  "$(curl -s "$BASE/workspaces/$WS_ID/session-search?q=zzz-no-such-token-zzz" "${AUTH[@]}")"
assert_contains "GET /session-search (no auth → 401)" "401" \
  "$(http_code GET "$BASE/workspaces/$WS_ID/session-search?q=x")"

# ===========================================================================
# 8. Rescue — GET /workspaces/:id/rescue (wsAuth)
#    RFC internal#742. Fail-CLOSED contract: the e2e-api job has no
#    MOLECULE_ORG_ID, so the handler returns 503 platform_misconfigured rather
#    than leaking cross-org. That fail-closed behaviour IS the keyless contract
#    we gate here (a regression that drops the org guard would flip this to a
#    200/404 and turn this assertion RED). Failure mode: no auth → 401.
# ===========================================================================
echo "--- /rescue ---"
BC=$(body_and_code GET "$BASE/workspaces/$WS_ID/rescue" "${AUTH[@]}")
RES_CODE=$(printf '%s' "$BC" | tail -n1)
RES_BODY=$(printf '%s' "$BC" | sed '$d')
if [ "$RES_CODE" = "404" ]; then
  # MOLECULE_ORG_ID was set in this environment → no-bundle path.
  assert_contains "GET /rescue (no bundle → 404, org configured)" 'no rescue bundle' "$RES_BODY"
else
  # No MOLECULE_ORG_ID (the e2e-api default) → fail-closed 503.
  assert_contains "GET /rescue (fail-closed 503 without MOLECULE_ORG_ID)" "503" "$RES_CODE"
  assert_contains "GET /rescue (platform_misconfigured code)" 'platform_misconfigured' "$RES_BODY"
fi
assert_contains "GET /rescue (no auth → 401)" "401" "$(http_code GET "$BASE/workspaces/$WS_ID/rescue")"

# ===========================================================================
# 9. LLM billing-mode admin toggle — GET/PUT /admin/workspaces/:id/llm-billing-mode
#    (AdminAuth). Flip to byok → read back override; bad UUID → 400; missing
#    'mode' key → 400; unknown mode → 400.
# ===========================================================================
echo "--- /admin/workspaces/:id/llm-billing-mode ---"
assert_contains "GET llm-billing-mode (resolves a mode)" '"resolved_mode"' \
  "$(curl -s "$BASE/admin/workspaces/$WS_ID/llm-billing-mode" "${ADMIN_AUTH[@]}")"
PUTBM=$(curl -s -X PUT "$BASE/admin/workspaces/$WS_ID/llm-billing-mode" -H "Content-Type: application/json" "${ADMIN_AUTH[@]}" \
  -d '{"mode":"byok"}')
assert_contains "PUT llm-billing-mode byok (override set)" '"workspace_override":"byok"' "$PUTBM"
assert_contains "GET llm-billing-mode (byok persisted)" '"workspace_override":"byok"' \
  "$(curl -s "$BASE/admin/workspaces/$WS_ID/llm-billing-mode" "${ADMIN_AUTH[@]}")"
# Clear the override (null) so we don't leave fixture state skewed.
curl -s -X PUT "$BASE/admin/workspaces/$WS_ID/llm-billing-mode" -H "Content-Type: application/json" "${ADMIN_AUTH[@]}" \
  -d '{"mode":null}' >/dev/null
# Failure: malformed UUID → 400.
assert_contains "PUT llm-billing-mode (bad UUID → 400)" "400" \
  "$(http_code PUT "$BASE/admin/workspaces/not-a-uuid/llm-billing-mode" -H 'Content-Type: application/json' "${ADMIN_AUTH[@]}" -d '{"mode":"byok"}')"
# Failure: missing 'mode' key → 400.
assert_contains "PUT llm-billing-mode (missing mode → 400)" "400" \
  "$(http_code PUT "$BASE/admin/workspaces/$WS_ID/llm-billing-mode" -H 'Content-Type: application/json' "${ADMIN_AUTH[@]}" -d '{}')"
# Failure: unknown mode string → 400.
assert_contains "PUT llm-billing-mode (unknown mode → 400)" "400" \
  "$(http_code PUT "$BASE/admin/workspaces/$WS_ID/llm-billing-mode" -H 'Content-Type: application/json' "${ADMIN_AUTH[@]}" -d '{"mode":"bogus-mode"}')"

# ===========================================================================
# 10. Lifecycle — Pause → Resume + Hibernate (wsAuth)
#     Pause works backend-agnostically (StopWorkspaceAuto no-ops on no backend)
#     → status=paused. Resume re-provisions: 200 provisioning when a provisioner
#     is wired (the e2e-api host has Docker), or 503 provisioner-not-available
#     otherwise — both are valid contracts, so accept either. Failure modes:
#     resume a non-paused ws → 404; hibernate a non-online ws → 404.
# ===========================================================================
echo "--- lifecycle (resume / hibernate) ---"
# Pause the (online) fixture → status paused.
PA=$(curl -s -X POST "$BASE/workspaces/$WS_ID/pause" "${AUTH[@]}")
assert_contains "POST /pause (online → paused)" '"status":"paused"' "$PA"
# Resume the paused fixture — accept 200 provisioning OR 503 (no provisioner).
BC=$(body_and_code POST "$BASE/workspaces/$WS_ID/resume" "${AUTH[@]}")
RSM_CODE=$(printf '%s' "$BC" | tail -n1)
RSM_BODY=$(printf '%s' "$BC" | sed '$d')
if [ "$RSM_CODE" = "200" ]; then
  assert_contains "POST /resume (paused → provisioning)" '"status":"provisioning"' "$RSM_BODY"
elif [ "$RSM_CODE" = "503" ]; then
  assert_contains "POST /resume (no provisioner → 503 contract)" 'provisioner not available' "$RSM_BODY"
else
  fail "POST /resume (expected 200 or 503)" "got HTTP $RSM_CODE — $RSM_BODY"
fi
# Failure: resume a workspace that is NOT paused → 404.
# (After the resume above it is provisioning/online, not paused.)
assert_contains "POST /resume (not-paused → 404)" "404" \
  "$(http_code POST "$BASE/workspaces/$WS_ID/resume" "${AUTH[@]}")"
# Hibernate: bring the fixture back online first, then hibernate it.
curl -s -X POST "$BASE/registry/register" -H "Content-Type: application/json" "${AUTH[@]}" \
  -d "{\"id\":\"$WS_ID\",\"url\":\"https://example.com/keyless\",\"agent_card\":{\"name\":\"Keyless Fixture\",\"skills\":[{\"id\":\"noop\",\"name\":\"Noop\"}]}}" >/dev/null
HB=$(curl -s -X POST "$BASE/workspaces/$WS_ID/hibernate" "${AUTH[@]}")
assert_contains "POST /hibernate (online → hibernated)" '"status":"hibernated"' "$HB"
# Failure: hibernate again (now hibernated, not online/degraded) → 404.
assert_contains "POST /hibernate (not-hibernatable → 404)" "404" \
  "$(http_code POST "$BASE/workspaces/$WS_ID/hibernate" "${AUTH[@]}")"
# Failure: no bearer → 401.
assert_contains "POST /resume (no auth → 401)" "401" "$(http_code POST "$BASE/workspaces/$WS_ID/resume")"

# ---------------------------------------------------------------------------
# Cleanup — delete the fixture (admin-gated DELETE + per-workspace bearer).
# ---------------------------------------------------------------------------
e2e_delete_workspace "$WS_ID" "Keyless Fixture" "${ADMIN_AUTH[@]}"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
