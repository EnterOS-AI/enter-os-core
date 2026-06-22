#!/usr/bin/env bash
# Seed BOTH tenants with parent + child workspaces so peer-discovery
# and cross-tenant replays have something to discover.
#
# Tenant alpha:
#   - alpha-parent (tier 0)
#   - alpha-child  (tier 1, child of alpha-parent)
# Tenant beta:
#   - beta-parent  (tier 0)
#   - beta-child   (tier 1, child of beta-parent)
#
# IDs are server-generated (POST /workspaces ignores body.id) — we
# capture the returned id rather than minting client-side. Older
# versions silently desynced from the workspaces table, breaking
# FK-dependent replays.
#
# All four IDs persist to .seed.env so replays can target any of them.

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

# shellcheck source=_curl.sh
source "$HERE/_curl.sh"

create_workspace() {
    local tenant="$1" name="$2" tier="$3" parent="${4:-}"
    # Use the harness's default runtime (hermes echo — what the
    # replays actually exercise; in the runtime registry allowlist)
    # with a platform-billed model (vendor/model slash form
    # `moonshot/kimi-k2.6` — no BYOK credential needed per
    # workspace-server/cmd/server/cp_config.go + model_registry_validation.go).
    # Earlier attempts that broke:
    #   runtime=claude-code, model=sonnet  → 422 MISSING_BYOK_CREDENTIAL
    #     (core#2608 create-boundary; harness provisions no OAuth token)
    #   runtime=moonshot, model=moonshot/kimi-k2.6
    #     → 422 FAIL-CLOSED "unsupported runtime moonshot" (moonshot is
    #       not in the runtime registry; only the model field accepts
    #       the vendor slash form)
    #   runtime=hermes (no model)  → 422 FAIL-CLOSED "model is required"
    #     (CTO 2026-05-22 SSOT directive forbids silent DefaultModel fallback)
    local body
    if [ -n "$parent" ]; then
        body="{\"name\":\"$name\",\"tier\":$tier,\"parent_id\":\"$parent\",\"runtime\":\"hermes\",\"model\":\"moonshot/kimi-k2.6\"}"
    else
        body="{\"name\":\"$name\",\"tier\":$tier,\"runtime\":\"hermes\",\"model\":\"moonshot/kimi-k2.6\"}"
    fi
    local id
    if [ "$tenant" = "alpha" ]; then
        id=$(curl_alpha_admin -X POST "$BASE/workspaces" -d "$body" | jq -r '.id')
    else
        id=$(curl_beta_admin -X POST "$BASE/workspaces" -d "$body" | jq -r '.id')
    fi
    if [ -z "$id" ] || [ "$id" = "null" ]; then
        echo "[seed] FAIL: $tenant/$name workspace creation returned no id" >&2
        return 1
    fi
    echo "$id"
}

echo "[seed] confirming both tenants reachable..."
ALPHA_HEALTH=$(curl_alpha_anon "$BASE/health" || echo "")
BETA_HEALTH=$(curl_beta_anon "$BASE/health" || echo "")
if [ -z "$ALPHA_HEALTH" ] || [ -z "$BETA_HEALTH" ]; then
    echo "[seed] FAIL: tenant unreachable. alpha='$ALPHA_HEALTH' beta='$BETA_HEALTH'"
    echo "       Did ./up.sh complete cleanly?"
    exit 1
fi
echo "[seed]   alpha: $ALPHA_HEALTH"
echo "[seed]   beta : $BETA_HEALTH"

echo ""
echo "[seed] tenant alpha — creating alpha-parent + alpha-child ..."
ALPHA_PARENT_ID=$(create_workspace alpha alpha-parent 0)
echo "[seed]   alpha-parent id=$ALPHA_PARENT_ID"
ALPHA_CHILD_ID=$(create_workspace alpha alpha-child 1 "$ALPHA_PARENT_ID")
echo "[seed]   alpha-child  id=$ALPHA_CHILD_ID"

echo ""
echo "[seed] tenant beta — creating beta-parent + beta-child ..."
BETA_PARENT_ID=$(create_workspace beta beta-parent 0)
echo "[seed]   beta-parent  id=$BETA_PARENT_ID"
BETA_CHILD_ID=$(create_workspace beta beta-child 1 "$BETA_PARENT_ID")
echo "[seed]   beta-child   id=$BETA_CHILD_ID"

# Stash IDs for replay scripts.
#
# Backwards-compat: ALPHA_ID + BETA_ID aliases keep pre-Phase-2 replays
# working (they used these names for the alpha tenant's parent + child).
# Also: ALPHA_WORKSPACE_ID + BETA_WORKSPACE_ID aliases for the canary-
# smoke a2a-pong + org-create-400 replays (they expect a single
# "workspace" name per tenant; defaulting to the parent).
{
    echo "ALPHA_PARENT_ID=$ALPHA_PARENT_ID"
    echo "ALPHA_CHILD_ID=$ALPHA_CHILD_ID"
    echo "BETA_PARENT_ID=$BETA_PARENT_ID"
    echo "BETA_CHILD_ID=$BETA_CHILD_ID"
    echo "# legacy aliases — pre-Phase-2 replays expect these names"
    echo "ALPHA_ID=$ALPHA_PARENT_ID"
    echo "BETA_ID=$ALPHA_CHILD_ID"
    echo "# canary-smoke replays (a2a-pong, org-create-400) expect a single
# workspace name per tenant; default to the parent workspace.
# (The replays don't use child workspaces, so parent == "the
# workspace" for their purposes.)"
    echo "ALPHA_WORKSPACE_ID=$ALPHA_PARENT_ID"
    echo "BETA_WORKSPACE_ID=$BETA_PARENT_ID"
    # CP_STUB_BASE — the URL the host uses to reach the cp-stub service.
    # Replays run on the host (./run-all-replays.sh — see compose.yml's
    # #2867 address-fix), and compose publishes cp-stub's port 9090 to
    # the host loopback (cp-stub.ports: "9090:9090"). Default to
    # http://localhost:9090; allow override via env for staging mirrors
    # where the cp-stub is reachable at a different host/port.
    echo "CP_STUB_BASE=${CP_STUB_BASE:-http://localhost:9090}"
} > "$HERE/.seed.env"

echo ""
echo "[seed] done. IDs persisted to tests/harness/.seed.env"
echo "[seed]   alpha: parent=$ALPHA_PARENT_ID child=$ALPHA_CHILD_ID"
echo "[seed]   beta : parent=$BETA_PARENT_ID child=$BETA_CHILD_ID"
