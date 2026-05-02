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
    local body
    if [ -n "$parent" ]; then
        body="{\"name\":\"$name\",\"tier\":$tier,\"parent_id\":\"$parent\",\"runtime\":\"langgraph\"}"
    else
        body="{\"name\":\"$name\",\"tier\":$tier,\"runtime\":\"langgraph\"}"
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
{
    echo "ALPHA_PARENT_ID=$ALPHA_PARENT_ID"
    echo "ALPHA_CHILD_ID=$ALPHA_CHILD_ID"
    echo "BETA_PARENT_ID=$BETA_PARENT_ID"
    echo "BETA_CHILD_ID=$BETA_CHILD_ID"
    echo "# legacy aliases — pre-Phase-2 replays expect these names"
    echo "ALPHA_ID=$ALPHA_PARENT_ID"
    echo "BETA_ID=$ALPHA_CHILD_ID"
} > "$HERE/.seed.env"

echo ""
echo "[seed] done. IDs persisted to tests/harness/.seed.env"
echo "[seed]   alpha: parent=$ALPHA_PARENT_ID child=$ALPHA_CHILD_ID"
echo "[seed]   beta : parent=$BETA_PARENT_ID child=$BETA_CHILD_ID"
