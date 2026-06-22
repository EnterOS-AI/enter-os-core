#!/usr/bin/env bash
# cp-stub-provision-config — #2863 burn-down: prove the harness's CP-stub
# handles /cp/workspaces/provision + /cp/tenants/config so the harness
# tenant's provision + config-fetch calls land on the stub (not real
# prod CP). Phase 1 of the #2863 plan (see .claude/plans/2863-harness-fix-plan.md).
#
# This replay is INTENTIONALLY DISTINCT from canary-smoke-a2a-pong.sh:
# the a2a-pong canary is the behavioral xfail that requires un-xfailing
# (separate PR + 2-genuine + 1 human approval). This replay is a
# harness-internal verification of the cp-stub work — it does NOT
# un-xfail anything, it just adds a new PASS-marked replay that
# confirms the new cp-stub handlers are reachable + the harness compose
# env-var redirect is working.
#
# Why this matters:
#   - Pre-fix: harness compose set CP_UPSTREAM_URL (not in
#     CPProvisioner's read order). Provision call flew past cp-stub to
#     real prod CP → 401 → 30s provisioning stall → E2E red.
#   - Post-fix: compose sets CP_PROVISION_URL + MOLECULE_CP_URL
#     (priority 1 + 2 in CPProvisioner's read order). The harness's
#     tenant hits cp-stub's /cp/workspaces/provision + /cp/tenants/config
#     handlers (permissive, 200, valid shape). Provision succeeds;
#     staging E2E goes green on the next main run.
#
# What this replay asserts (each phase is a separate OK/KO):
#   Phase 1 — initial state: provision_calls=0, tenants_config_calls=0
#   Phase 2 — POST /cp/workspaces/provision → 201 + valid shape
#             (instance_id + state, matching the REAL CPProvisioner
#             client contract in internal/provisioner/cp_provisioner.go)
#             AND __/stub/state.provision_calls == 1
#   Phase 3 — GET /cp/tenants/config → 200 + valid shape
#             AND __/stub/state.tenants_config_calls == 1
#   Phase 4 — method-not-allowed cases: POST /cp/tenants/config → 405,
#             GET /cp/workspaces/provision → 405 (regression check:
#             if the cp-stub ever stops enforcing the verb, this fires)

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARNESS_ROOT="$(dirname "$HERE")"
cd "$HARNESS_ROOT"

if [ ! -f .seed.env ]; then
    echo "[replay] no .seed.env — running ./seed.sh first..."
    ./seed.sh
fi
# shellcheck source=/dev/null
source .seed.env
# shellcheck source=../_curl.sh
source "$HARNESS_ROOT/_curl.sh"

PASS=0
FAIL=0

ok() { PASS=$((PASS+1)); printf "  \033[32m✓\033[0m %s\n" "$*"; }
ko() { FAIL=$((FAIL+1)); printf "  \033[31m✗\033[0m %s\n" "$*"; }

# CP_STUB_BASE is set in _curl.sh from .seed.env (or by ./up.sh).
: "${CP_STUB_BASE:?CP_STUB_BASE must be set in .seed.env — run ./seed.sh first}"

echo "[replay] cp-stub-provision-config — #2863 burn-down: cp-stub provision + config reachability"
echo "[replay] CP_STUB_BASE=$CP_STUB_BASE"

# ---------------------------------------------------------------- Phase 1
# Initial state — both counters should be 0 (or at any rate, we record
# the start values so we can assert delta). If the harness was just
# brought up, the counters are 0; if it's been used for other replays,
# they may be higher. We capture the start values for the delta check.
echo "[replay] phase 1: capture initial __/stub/state ..."
INITIAL_STATE=$(curl -sS --max-time 10 "$CP_STUB_BASE/__stub/state")
INITIAL_PROVISION=$(echo "$INITIAL_STATE" | python3 -c "import json,sys; print(json.load(sys.stdin).get('provision_calls', 0))")
INITIAL_TENANTS_CONFIG=$(echo "$INITIAL_STATE" | python3 -c "import json,sys; print(json.load(sys.stdin).get('tenants_config_calls', 0))")
echo "[replay]   initial provision_calls=$INITIAL_PROVISION tenants_config_calls=$INITIAL_TENANTS_CONFIG"
ok "captured initial __/stub/state"

# ---------------------------------------------------------------- Phase 2
# POST /cp/workspaces/provision. The cp-stub should return 201 + a
# provision-response shape that matches the REAL CPProvisioner client's
# contract (internal/provisioner/cp_provisioner.go:339-363 + the
# cpProvisionResponse struct at :210-215). The client treats 201 as
# success and reads instance_id + state. The prior cp-stub contract
# (200 + workspace_id/status/phase/url) was incorrect — it sent the
# client into its failure branch with `provision failed (200):
# <unstructured body>`, which the CR2 review_id 11928 flagged on the
# prior head 30a6bea. After
# the call, the __/stub/state.provision_calls counter should have
# incremented by exactly 1.
echo "[replay] phase 2: POST /cp/workspaces/provision ..."

# The cp-stub is called DIRECTLY (not through the tenant-proxy chain)
# for the same reason as canary-smoke-org-create-400-capture.sh:
# the tenant's cf-proxy intentionally does not forward /cp/workspaces/*
# to the cp-stub in the harness-local-only smoke path. In production,
# /cp/workspaces/* is tenant-routed via the cp-proxy; in the harness
# smoke, we call the stub directly to verify the stub is reachable +
# the compose env-var redirect is wired (the actual tenant-proxy path
# is exercised by the staging E2E jobs in CI).
RESP=$(curl -sS --max-time 30 \
    -H "Content-Type: application/json" \
    -X POST "$CP_STUB_BASE/cp/workspaces/provision" \
    -d '{"workspace_id":"harness-replay-$$"}' \
    -w "\n%{http_code}" 2>/dev/null) || RESP="000
"

# Split body + status (last line is the status code)
HTTP_CODE=$(echo "$RESP" | tail -n 1)
BODY=$(echo "$RESP" | sed '$d')

echo "[replay]   HTTP $HTTP_CODE"
echo "[replay]   body: $BODY"

if [ "$HTTP_CODE" = "201" ]; then
    ok "POST /cp/workspaces/provision returned 201 (cp-stub handler reachable, matches CPProvisioner success contract)"
else
    ko "POST /cp/workspaces/provision returned $HTTP_CODE (expected 201 — cp-stub handler not wired, or env-var redirect failed, or the response shape regressed to non-201)"
fi

# Assert the response shape — must include instance_id + state
# (the two fields the real CPProvisioner.client reads on success).
# workspace_id + url are also returned for observability (mirrors the
# real CP's wire log) but are NOT consumed by the client; we assert
# them too as a wire-shape drift-gate (any future change to the
# real CP's response should be reflected in the stub, and vice versa).
for field in instance_id state workspace_id url; do
    if echo "$BODY" | python3 -c "
import json,sys
d = json.loads(sys.stdin.read())
sys.exit(0 if '$field' in d else 1)
" 2>/dev/null; then
        ok "response body has required field '$field'"
    else
        ko "response body missing required field '$field': $BODY"
    fi
done

# Assert the counter incremented
STATE_AFTER_PROVISION=$(curl -sS --max-time 10 "$CP_STUB_BASE/__stub/state")
PROVISION_AFTER=$(echo "$STATE_AFTER_PROVISION" | python3 -c "import json,sys; print(json.load(sys.stdin).get('provision_calls', 0))")
EXPECTED_PROVISION=$((INITIAL_PROVISION + 1))
if [ "$PROVISION_AFTER" = "$EXPECTED_PROVISION" ]; then
    ok "provision_calls incremented $INITIAL_PROVISION → $PROVISION_AFTER (==SSOT: request reached the stub)"
else
    ko "provision_calls expected $EXPECTED_PROVISION, got $PROVISION_AFTER — request did NOT reach the stub (env-var redirect broken, or counter not wired)"
fi

# ---------------------------------------------------------------- Phase 3
# GET /cp/tenants/config. Mirror of Phase 2 but for the config-fetch
# call. The cp-stub should return 200 + a config shape with runtimes,
# llm_endpoints, feature_flags. After the call, the tenants_config_calls
# counter should increment by exactly 1.
echo "[replay] phase 3: GET /cp/tenants/config ..."

RESP=$(curl -sS --max-time 30 \
    -X GET "$CP_STUB_BASE/cp/tenants/config" \
    -w "\n%{http_code}" 2>/dev/null) || RESP="000
"

HTTP_CODE=$(echo "$RESP" | tail -n 1)
BODY=$(echo "$RESP" | sed '$d')

echo "[replay]   HTTP $HTTP_CODE"
echo "[replay]   body: $BODY"

if [ "$HTTP_CODE" = "200" ]; then
    ok "GET /cp/tenants/config returned 200 (cp-stub handler reachable)"
else
    ko "GET /cp/tenants/config returned $HTTP_CODE (expected 200 — cp-stub handler not wired)"
fi

# Assert the response shape matches the real CP's tenant-config shape
for field in runtimes llm_endpoints feature_flags; do
    if echo "$BODY" | python3 -c "
import json,sys
d = json.loads(sys.stdin.read())
sys.exit(0 if '$field' in d else 1)
" 2>/dev/null; then
        ok "config body has required field '$field'"
    else
        ko "config body missing required field '$field': $BODY"
    fi
done

# Assert the counter incremented
STATE_AFTER_CONFIG=$(curl -sS --max-time 10 "$CP_STUB_BASE/__stub/state")
CONFIG_AFTER=$(echo "$STATE_AFTER_CONFIG" | python3 -c "import json,sys; print(json.load(sys.stdin).get('tenants_config_calls', 0))")
EXPECTED_CONFIG=$((INITIAL_TENANTS_CONFIG + 1))
if [ "$CONFIG_AFTER" = "$EXPECTED_CONFIG" ]; then
    ok "tenants_config_calls incremented $INITIAL_TENANTS_CONFIG → $CONFIG_AFTER (==SSOT: request reached the stub)"
else
    ko "tenants_config_calls expected $EXPECTED_CONFIG, got $CONFIG_AFTER — request did NOT reach the stub"
fi

# ---------------------------------------------------------------- Phase 4
# Method-not-allowed regression checks. If the cp-stub ever stops
# enforcing the verb (e.g. someone refactors and removes the 405
# branches), these assertions fire. The MCP is small but the verb
# enforcement matters: POST /cp/tenants/config should never silently
# succeed (it would mean a config-update path the harness didn't
# intend to support).
echo "[replay] phase 4: verb enforcement regression checks ..."

# POST /cp/tenants/config should be 405 (only GET is allowed)
HTTP_CODE=$(curl -sS --max-time 10 -o /dev/null -w "%{http_code}" \
    -X POST "$CP_STUB_BASE/cp/tenants/config" 2>/dev/null || echo "000")
if [ "$HTTP_CODE" = "405" ]; then
    ok "POST /cp/tenants/config returned 405 (verb enforcement intact)"
else
    ko "POST /cp/tenants/config returned $HTTP_CODE (expected 405 — verb enforcement regressed)"
fi

# GET /cp/workspaces/provision should be 405 (only POST is allowed)
HTTP_CODE=$(curl -sS --max-time 10 -o /dev/null -w "%{http_code}" \
    -X GET "$CP_STUB_BASE/cp/workspaces/provision" 2>/dev/null || echo "000")
if [ "$HTTP_CODE" = "405" ]; then
    ok "GET /cp/workspaces/provision returned 405 (verb enforcement intact)"
else
    ko "GET /cp/workspaces/provision returned $HTTP_CODE (expected 405 — verb enforcement regressed)"
fi

echo ""
echo "[replay] PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
