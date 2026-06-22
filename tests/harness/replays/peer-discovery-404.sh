#!/usr/bin/env bash
# Peer-discovery 404 replay (issue #2865).
#
# Validates that an unregistered workspace returns HTTP 404 from the platform
# /registry/{id}/peers endpoint. The parse/diagnostic-helper branch that used to
# live here has moved to the workspace-runtime test suite
# (molecule_runtime/a2a_client.py and its unit tests), where the helper actually
# lives; this replay now covers the wire behavior only.
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

# WIRE: tenant returns 404 for an unregistered workspace.
ROGUE_ID="$(uuidgen | tr '[:upper:]' '[:lower:]')"
echo "[replay] WIRE: querying /registry/$ROGUE_ID/peers (unregistered workspace)..."
HTTP_CODE=$(curl_admin -o /tmp/peer-replay.json -w '%{http_code}' \
    -H "X-Workspace-ID: $ROGUE_ID" \
    "$BASE/registry/$ROGUE_ID/peers")

echo "[replay]     tenant responded HTTP $HTTP_CODE"
if [ "$HTTP_CODE" != "404" ]; then
    echo "[replay] FAIL: expected 404 from /registry/<unregistered>/peers, got $HTTP_CODE"
    echo "[replay]   This is a platform-side regression — the runtime's diagnostic helper"
    echo "[replay]   would see a different status code than the unit tests cover."
    cat /tmp/peer-replay.json
    exit 1
fi

echo ""
echo "[replay] PASS: peer-discovery wire returns 404 for an unregistered workspace."
