#!/usr/bin/env bash
# Seed the harness with two registered workspaces so peer-discovery
# replay scripts have something to discover.
#
# - "alpha"  parent (tier 0)
# - "beta"   child of alpha (tier 1)
#
# Both register via the platform's /workspaces endpoint, which is what
# CP does at provision time. The platform then has them in its DB;
# tool_list_peers from inside alpha can resolve beta as a peer.

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

# shellcheck source=_curl.sh
source "$HERE/_curl.sh"

echo "[seed] confirming tenant is reachable via cf-proxy..."
HEALTH=$(curl_anon "$BASE/health" || echo "")
if [ -z "$HEALTH" ]; then
    echo "[seed] FAILED: $BASE/health unreachable. Did ./up.sh complete?"
    exit 1
fi
echo "[seed]   $HEALTH"

echo "[seed] confirming /buildinfo returns the harness GIT_SHA..."
BUILD=$(curl_anon "$BASE/buildinfo" || echo "")
echo "[seed]   $BUILD"

# Create alpha (parent) and beta (child of alpha). The handler always
# generates the workspace id server-side and ignores any id in the
# request body, so we capture the returned id rather than minting one
# locally — older versions of this script minted client-side and would
# silently desync from the workspaces table, breaking FK-dependent
# replays (chat-history seeds activity_logs which has a FK to workspaces).
echo "[seed] creating workspace 'alpha' (parent)..."
ALPHA_ID=$(curl_admin -X POST "$BASE/workspaces" \
    -d '{"name":"alpha","tier":0,"runtime":"langgraph"}' \
    | jq -r '.id')
if [ -z "$ALPHA_ID" ] || [ "$ALPHA_ID" = "null" ]; then
    echo "[seed] FAIL: alpha workspace creation returned no id"
    exit 1
fi
echo "[seed]   alpha id=$ALPHA_ID"

echo "[seed] creating workspace 'beta' (child of alpha)..."
BETA_ID=$(curl_admin -X POST "$BASE/workspaces" \
    -d "{\"name\":\"beta\",\"tier\":1,\"parent_id\":\"$ALPHA_ID\",\"runtime\":\"langgraph\"}" \
    | jq -r '.id')
if [ -z "$BETA_ID" ] || [ "$BETA_ID" = "null" ]; then
    echo "[seed] FAIL: beta workspace creation returned no id"
    exit 1
fi
echo "[seed]   beta id=$BETA_ID"

# Stash IDs so replay scripts pick them up.
{
    echo "ALPHA_ID=$ALPHA_ID"
    echo "BETA_ID=$BETA_ID"
} > "$HERE/.seed.env"

echo ""
echo "[seed] done. IDs persisted to tests/harness/.seed.env"
echo "[seed]   ALPHA_ID=$ALPHA_ID"
echo "[seed]   BETA_ID=$BETA_ID"
