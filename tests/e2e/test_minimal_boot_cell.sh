#!/usr/bin/env bash
# cp#455 — Minimal-cell boot-to-registration harness.
# CTO directive 14eb4f07: "build the minimal claude-code+kimi cell,
# it should now go GREEN since the fix is live."
#
# Stage 1 of 5-stage rollout. Reduced to the minimum boot-to-
# registration surface so each cell run is ~3-5 min wall-clock.
#
# Four assertions (per Researcher Task #79 spec):
#   1. Provision request accepted; workspace transitions to booting/running
#   2. Controlplane receives /registry/register for that workspace_id
#   3. JSON-RPC/completion route returns successful minimal response
#   4. Teardown terminates workspace even on failure (trap)
#
# Cost controls (mandatory):
#   - SPOT instances (via the dispatch-only EC2 provisioning path;
#     we don't set instance type — that's the provisioner's call)
#   - Fast teardown ~3-5 min wall-clock
#   - Structured per-cell results JSON output
#
# Auth model (mirrors test_staging_full_saas.sh):
#   Single MOLECULE_ADMIN_TOKEN drives everything.
#     - POST /cp/admin/orgs to provision
#     - GET  /cp/admin/orgs/:slug/admin-token for per-tenant token
#     - DELETE /cp/admin/tenants/:slug for teardown
#   Per-tenant admin token drives tenant API calls (workspaces,
#   /registry/register, JSON-RPC completion).
#
# Required env:
#   MOLECULE_CP_URL        default: https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   CP admin bearer
#
# Optional env (passed from workflow_dispatch inputs):
#   E2E_RUNTIME            default claude-code
#   E2E_BILLING_MODE       default platform_managed
#   E2E_PROVIDER           default platform
#   E2E_MODEL              default moonshot/kimi-k2.6
#   E2E_RUN_ID             Slug suffix; CI: cp455-${GITHUB_RUN_ID}
#   E2E_PROVISION_TIMEOUT_SECS  default 300 (5 min — fast teardown budget)
#   E2E_KEEP_ORG           1 → skip teardown (debugging only)
#
# Exit codes:
#   0  happy path
#   1  generic failure
#   2  missing required env
#   3  provisioning timed out (assertion 1)
#   4  register timeout (assertion 2)
#   5  completion failure (assertion 3)
#   6  teardown left orphan (assertion 4)

set -uo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — Railway staging CP_ADMIN_API_TOKEN}"
RUNTIME="${E2E_RUNTIME:-claude-code}"
BILLING_MODE="${E2E_BILLING_MODE:-platform_managed}"
PROVIDER="${E2E_PROVIDER:-platform}"
MODEL="${E2E_MODEL:-moonshot/kimi-k2.6}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-300}"
KEEP_ORG="${E2E_KEEP_ORG:-}"
RUN_ID_SUFFIX="${E2E_RUN_ID:-$(date +%H%M%S)-$$}"

# log/fail/ok MUST be defined BEFORE the assert_collision_proof_slug call
# below (which uses `|| fail "..."`). Defining them after the call would
# error on a bad slug with `fail: command not found` instead of the
# intended diagnostic. Mirrors the order in test_staging_full_saas.sh.
log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

# Collision-proof slug (core#2782). The prior `cp455-${RUNTIME}-$RUN_ID_SUFFIX`
# shape used a raw timestamp tail and could collide between two CI
# runs (e.g. retry of run 3606 + fresh run 3607) on POST
# /cp/admin/orgs 409. Migrating to the shared helper appends an 8-char
# uuid so every run gets a unique slug regardless of how the workflow
# composes E2E_RUN_ID. The literal `cp455-` prefix is preserved
# (semantic — cp issue #455) — the sweeper doesn't cover this prefix
# but the EXIT trap at `on_exit` handles teardown, so no orphan risk.
# Note: this file is NOT covered by lint_cleanup_traps.sh's
# `test_*staging*` glob, so the e2e-/rt-e2e- prefix rule doesn't
# apply here. The sweeper only reaps e2e-*/rt-e2e-* anyway.
# shellcheck source=lib/collision-proof-slug.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/collision-proof-slug.sh"
# Compute the prefix length dynamically: "cp455-" (6 chars) +
# RUNTIME length. RUNTIME is set by the harness to one of the
# known runtime names (claude-code, codex, hermes, openclaw),
# so the prefix is bounded.
SLUG_PREFIX="cp455-${RUNTIME}-"
SLUG="${SLUG_PREFIX}$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" ${#SLUG_PREFIX})"
assert_collision_proof_slug "$SLUG" || fail "Bug in make_collision_proof_slug: produced non-collision-proof slug '$SLUG'"

WORKSPACE_ID=""
TENANT_TOKEN=""
RESULT_JSON="/tmp/cell-result.json"
PROVISION_START_EPOCH=""
PROVISION_END_EPOCH=""
REGISTER_STATUS="not_attempted"
COMPLETION_STATUS="not_attempted"
TEARDOWN_STATUS="not_attempted"
EXIT_CODE=0

# Structured per-cell results writer. Emits JSON with all 4
# assertion statuses + elapsed timing. Called from EXIT trap so
# results are captured even on early failure.
write_result() {
  local elapsed="${1:-0}"
  cat > "${RESULT_JSON}" <<JSON
{
  "runtime": "${RUNTIME}",
  "billing_mode": "${BILLING_MODE}",
  "provider": "${PROVIDER}",
  "model": "${MODEL}",
  "workspace_id": "${WORKSPACE_ID}",
  "register_status": "${REGISTER_STATUS}",
  "completion_status": "${COMPLETION_STATUS}",
  "teardown_status": "${TEARDOWN_STATUS}",
  "elapsed_seconds": ${elapsed},
  "exit_code": ${EXIT_CODE},
  "ts": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
JSON
}

# EXIT trap — ALWAYS run. Writes structured results, tears down
# workspace if we have one, never lets the script exit without
# emitting /tmp/cell-result.json.
on_exit() {
  local exit_code=$?
  EXIT_CODE=${exit_code}
  local now
  now=$(date +%s)
  local elapsed=0
  if [ -n "${PROVISION_START_EPOCH:-}" ] && [ "${PROVISION_START_EPOCH}" -gt 0 ] 2>/dev/null; then
    elapsed=$(( now - PROVISION_START_EPOCH ))
  fi

  # Assertion 4: teardown terminates workspace even on failure.
  if [ -z "${KEEP_ORG}" ] && [ -n "${SLUG:-}" ]; then
    if [ -n "${WORKSPACE_ID:-}" ] || [ -n "${SLUG:-}" ]; then
      echo "::group::Teardown (trap)"
      echo "DELETE ${CP_URL}/cp/admin/tenants/${SLUG}"
      local teardown_http_code
      teardown_http_code=$(curl -sS -o /dev/null -w '%{http_code}' \
        -X DELETE \
        -H "Authorization: Bearer ${ADMIN_TOKEN}" \
        --max-time 60 \
        "${CP_URL}/cp/admin/tenants/${SLUG}" || echo "000")
      if [ "${teardown_http_code}" = "200" ] || [ "${teardown_http_code}" = "204" ] || [ "${teardown_http_code}" = "404" ]; then
        TEARDOWN_STATUS="ok"
        echo "Teardown OK (HTTP ${teardown_http_code})"
      else
        TEARDOWN_STATUS="leak_risk_http_${teardown_http_code}"
        echo "::error::Teardown returned HTTP ${teardown_http_code} — orphan risk"
        # Bump exit code to 6 if teardown is the failure source.
        if [ "${EXIT_CODE}" -eq 0 ]; then
          EXIT_CODE=6
        fi
      fi
      echo "::endgroup::"
    fi
  else
    TEARDOWN_STATUS="skipped_keep_org"
  fi

  write_result "${elapsed}"
  echo "Structured results written to ${RESULT_JSON}"
  cat "${RESULT_JSON}"
  exit "${EXIT_CODE}"
}
trap on_exit EXIT
trap 'echo "::error::Script aborted on signal"; exit 130' INT TERM

PROVISION_START_EPOCH=$(date +%s)

# Assertion 1: Provision request accepted; workspace transitions to
# booting/running.
echo "::group::Assertion 1: Provision"
echo "POST ${CP_URL}/cp/admin/orgs  slug=${SLUG}  runtime=${RUNTIME}  billing_mode=${BILLING_MODE}  provider=${PROVIDER}  model=${MODEL}"
PROVISION_HTTP_CODE=$(curl -sS -o /tmp/provision-resp.json -w '%{http_code}' \
  -X POST \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  --max-time 30 \
  -d "$(cat <<JSON
{
  "slug": "${SLUG}",
  "runtime": "${RUNTIME}",
  "billing_mode": "${BILLING_MODE}",
  "provider": "${PROVIDER}",
  "model": "${MODEL}",
  "tier": "spot",
  "tags": {
    "cp455_minimal_cell": "1",
    "run_id": "${RUN_ID_SUFFIX}"
  }
}
JSON
)" \
  "${CP_URL}/cp/admin/orgs" || echo "000")
echo "HTTP ${PROVISION_HTTP_CODE}"
if [ "${PROVISION_HTTP_CODE}" != "202" ] && [ "${PROVISION_HTTP_CODE}" != "200" ]; then
  echo "::error::Provision failed (HTTP ${PROVISION_HTTP_CODE})"
  cat /tmp/provision-resp.json 2>/dev/null || true
  EXIT_CODE=1
  exit "${EXIT_CODE}"
fi
echo "::endgroup::"

# Wait for org to reach running + retrieve per-tenant token. Bounded
# at PROVISION_TIMEOUT_SECS. We poll the admin token endpoint; once
# the org is up, the endpoint returns 200 with the token, and the
# workspace_id is in the same response or in a follow-up /orgs/:slug
# call.
echo "::group::Wait for org to be ready (max ${PROVISION_TIMEOUT_SECS}s)"
WAIT_START=$(date +%s)
WAIT_DEADLINE=$(( WAIT_START + PROVISION_TIMEOUT_SECS ))
TENANT_TOKEN=""
while [ "$(date +%s)" -lt "${WAIT_DEADLINE}" ]; do
  TOKEN_HTTP_CODE=$(curl -sS -o /tmp/token-resp.json -w '%{http_code}' \
    -H "Authorization: Bearer ${ADMIN_TOKEN}" \
    --max-time 10 \
    "${CP_URL}/cp/admin/orgs/${SLUG}/admin-token" || echo "000")
  if [ "${TOKEN_HTTP_CODE}" = "200" ]; then
    TENANT_TOKEN=$(jq -r '.admin_token // .token // empty' /tmp/token-resp.json 2>/dev/null || echo "")
    if [ -n "${TENANT_TOKEN}" ]; then
      WORKSPACE_ID=$(jq -r '.workspace_id // .default_workspace_id // empty' /tmp/token-resp.json 2>/dev/null || echo "")
      if [ -z "${WORKSPACE_ID}" ]; then
        # Fallback: list orgs and find by slug
        WORKSPACE_ID=$(curl -sS -H "Authorization: Bearer ${ADMIN_TOKEN}" \
          "${CP_URL}/cp/admin/orgs/${SLUG}" | jq -r '.workspace_id // .default_workspace_id // empty' 2>/dev/null || echo "")
      fi
      if [ -n "${WORKSPACE_ID}" ]; then
        PROVISION_END_EPOCH=$(date +%s)
        echo "Org ready in $(( PROVISION_END_EPOCH - WAIT_START ))s — workspace_id=${WORKSPACE_ID}"
        break
      fi
    fi
  fi
  sleep 5
done
if [ -z "${TENANT_TOKEN}" ] || [ -z "${WORKSPACE_ID}" ]; then
  echo "::error::Provision timed out (org never reached running within ${PROVISION_TIMEOUT_SECS}s)"
  EXIT_CODE=3
  exit "${EXIT_CODE}"
fi
echo "::endgroup::"

# Assertion 2: Controlplane receives /registry/register for that
# workspace_id. The harness doesn't POST to /registry/register
# directly — that's the workspace-server's own job on boot. We
# verify the registration was received by polling the registry
# endpoint (or by checking that a /workspaces/:id call returns
# the expected fields).
echo "::group::Assertion 2: /registry/register for workspace_id=${WORKSPACE_ID}"
REGISTER_DEADLINE=$(( $(date +%s) + 60 ))
while [ "$(date +%s)" -lt "${REGISTER_DEADLINE}" ]; do
  REG_HTTP_CODE=$(curl -sS -o /tmp/reg-resp.json -w '%{http_code}' \
    -H "Authorization: Bearer ${TENANT_TOKEN}" \
    --max-time 10 \
    "${CP_URL}/cp/registry/workspaces/${WORKSPACE_ID}" || echo "000")
  if [ "${REG_HTTP_CODE}" = "200" ]; then
    REGISTERED=$(jq -r '.registered // .workspace_id // empty' /tmp/reg-resp.json 2>/dev/null || echo "")
    if [ -n "${REGISTERED}" ]; then
      REGISTER_STATUS="ok"
      echo "Registry confirms workspace_id=${WORKSPACE_ID} registered"
      break
    fi
  fi
  sleep 3
done
if [ "${REGISTER_STATUS}" != "ok" ]; then
  echo "::error::Registry did not confirm registration within 60s"
  cat /tmp/reg-resp.json 2>/dev/null || true
  EXIT_CODE=4
  exit "${EXIT_CODE}"
fi
echo "::endgroup::"

# Assertion 3: JSON-RPC/completion route returns successful minimal
# response. One minimal completion call — keep payload small.
echo "::group::Assertion 3: JSON-RPC completion"
COMPLETION_HTTP_CODE=$(curl -sS -o /tmp/completion-resp.json -w '%{http_code}' \
  -X POST \
  -H "Authorization: Bearer ${TENANT_TOKEN}" \
  -H "Content-Type: application/json" \
  --max-time 30 \
  -d "$(cat <<JSON
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "completion",
  "params": {
    "workspace_id": "${WORKSPACE_ID}",
    "model": "${MODEL}",
    "messages": [{"role": "user", "content": "ping"}],
    "max_tokens": 1
  }
}
JSON
)" \
  "${CP_URL}/cp/rpc" || echo "000")
echo "HTTP ${COMPLETION_HTTP_CODE}"
if [ "${COMPLETION_HTTP_CODE}" != "200" ]; then
  echo "::error::Completion failed (HTTP ${COMPLETION_HTTP_CODE})"
  cat /tmp/completion-resp.json 2>/dev/null || true
  EXIT_CODE=5
  exit "${EXIT_CODE}"
fi
# Verify JSON-RPC 2.0 success envelope
RPC_ERROR=$(jq -r '.error // empty' /tmp/completion-resp.json 2>/dev/null || echo "")
if [ -n "${RPC_ERROR}" ]; then
  echo "::error::Completion returned JSON-RPC error: ${RPC_ERROR}"
  cat /tmp/completion-resp.json 2>/dev/null || true
  EXIT_CODE=5
  exit "${EXIT_CODE}"
fi
RPC_RESULT=$(jq -r '.result // empty' /tmp/completion-resp.json 2>/dev/null || echo "")
if [ -z "${RPC_RESULT}" ] || [ "${RPC_RESULT}" = "null" ]; then
  echo "::error::Completion response missing result field"
  cat /tmp/completion-resp.json 2>/dev/null || true
  EXIT_CODE=5
  exit "${EXIT_CODE}"
fi
COMPLETION_STATUS="ok"
echo "Completion OK"
echo "::endgroup::"

echo "All 4 assertions passed for ${SLUG} (workspace_id=${WORKSPACE_ID})"