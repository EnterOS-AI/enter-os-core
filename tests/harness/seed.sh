#!/usr/bin/env bash
# Seed the harness with two registered workspaces so peer-discovery
# replay scripts have something to discover.
#
# - "alpha"  parent (tier 0)
# - "beta"   child of alpha (tier 1)
#
# Both register via the platform's /registry/register endpoint, which
# is what real workspaces do at boot. The platform then has them in its
# DB; tool_list_peers from inside alpha can resolve beta as a peer.

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

BASE="${BASE:-http://harness-tenant.localhost:8080}"
ADMIN="harness-admin-token"
ORG="harness-org"

curl_admin() {
    curl -sS -H "Authorization: Bearer $ADMIN" \
            -H "X-Molecule-Org-Id: $ORG" \
            -H "Content-Type: application/json" "$@"
}

echo "[seed] confirming tenant is reachable via cf-proxy..."
HEALTH=$(curl -sS "$BASE/health" || echo "")
if [ -z "$HEALTH" ]; then
    echo "[seed] FAILED: $BASE/health unreachable. Did ./up.sh complete? Did you add"
    echo "       127.0.0.1 harness-tenant.localhost to /etc/hosts?"
    exit 1
fi
echo "[seed]   $HEALTH"

echo "[seed] confirming /buildinfo returns the harness GIT_SHA..."
BUILD=$(curl -sS "$BASE/buildinfo" || echo "")
echo "[seed]   $BUILD"

# Mint a fresh admin-call workspace ID for the parent. Platform's
# /admin/workspaces/:id/test-token mints a per-workspace bearer; the
# replay scripts use it to call the workspace-scoped routes.
echo "[seed] creating workspace 'alpha' (parent)..."
ALPHA_ID=$(uuidgen | tr '[:upper:]' '[:lower:]')
curl_admin -X POST "$BASE/workspaces" \
    -d "{\"id\":\"$ALPHA_ID\",\"name\":\"alpha\",\"tier\":0,\"runtime\":\"langgraph\"}" \
    >/dev/null
echo "[seed]   alpha id=$ALPHA_ID"

echo "[seed] creating workspace 'beta' (child of alpha)..."
BETA_ID=$(uuidgen | tr '[:upper:]' '[:lower:]')
curl_admin -X POST "$BASE/workspaces" \
    -d "{\"id\":\"$BETA_ID\",\"name\":\"beta\",\"tier\":1,\"parent_id\":\"$ALPHA_ID\",\"runtime\":\"langgraph\"}" \
    >/dev/null
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
