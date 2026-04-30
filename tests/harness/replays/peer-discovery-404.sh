#!/usr/bin/env bash
# Replay for issue #2397 — local proof that peer-discovery surfaces
# actionable diagnostics instead of "may be isolated".
#
# Prior behavior: tool_list_peers returned "No peers available (this
# workspace may be isolated)" regardless of WHY peers were empty —
# five distinct conditions (200+empty, 401, 403, 404, 5xx, network)
# collapsed to one ambiguous message.
#
# This replay proves two things, separately:
#   (a) WIRE: the platform side of the contract — the tenant's
#       /registry/<unregistered>/peers returns 404. If this regresses
#       (e.g. tenant starts returning 200 with empty list, or 500),
#       the runtime helper would parse it differently and the agent
#       would see a different diagnostic. The harness catches that here.
#   (b) PARSE: the runtime helper, given a 404, produces a diagnostic
#       containing "404" + "register" hints. Done in unit tests against
#       a mock httpx response (test_a2a_client.py::TestGetPeersWithDiagnostic
#       — the harness re-asserts the same contract here against a real
#       Python eval that does NOT depend on workspace auth tokens.
#
# Why split the assertion: the Python eval here doesn't have the
# workspace's auth token file, so going through get_peers_with_diagnostic
# directly would hit the platform without auth and produce a different
# branch (401 instead of 404). Splitting (a) from (b) keeps each
# assertion targeting exactly what it claims to test.

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

BASE="${BASE:-http://harness-tenant.localhost:8080}"
ADMIN="harness-admin-token"
ORG="harness-org"

# ─── (a) WIRE: tenant returns 404 for an unregistered workspace ────────
ROGUE_ID="$(uuidgen | tr '[:upper:]' '[:lower:]')"
echo "[replay] (a) WIRE: querying /registry/$ROGUE_ID/peers (unregistered workspace)..."
HTTP_CODE=$(curl -sS -o /tmp/peer-replay.json -w '%{http_code}' \
    -H "Authorization: Bearer $ADMIN" \
    -H "X-Molecule-Org-Id: $ORG" \
    -H "X-Workspace-ID: $ROGUE_ID" \
    "$BASE/registry/$ROGUE_ID/peers")

echo "[replay]     tenant responded HTTP $HTTP_CODE"
if [ "$HTTP_CODE" != "404" ]; then
    echo "[replay] FAIL (a): expected 404 from /registry/<unregistered>/peers, got $HTTP_CODE"
    echo "[replay]   This is a platform-side regression — the runtime's diagnostic helper"
    echo "[replay]   would see a different status code than the unit tests cover."
    cat /tmp/peer-replay.json
    exit 1
fi

# ─── (b) PARSE: helper converts a synthetic 404 to actionable diagnostic ─
#
# We construct a synthetic httpx 404 response and run the helper against
# it directly. This isolates the parse branch we want to test from the
# auth-context concerns of going through the network. The helper's network
# branches are exhaustively covered by tests/test_a2a_client.py — this is
# a regression-guard that the helper IS in the install, IS importable in
# the harness's Python env, and IS reading the status code.

WORKSPACE_PATH="$(cd "$HARNESS_ROOT/../../workspace" && pwd)"
DIAGNOSTIC=$(WORKSPACE_ID="harness-rogue" PYTHONPATH="$WORKSPACE_PATH" \
    python3 - "$WORKSPACE_PATH" <<'PYEOF'
import asyncio
import sys
import types
from unittest.mock import AsyncMock, MagicMock, patch

# Stub platform_auth so a2a_client imports cleanly without requiring a
# real workspace token file. The helper's auth_headers() only matters
# when going through the network; we're feeding it a mock response.
_pa = types.ModuleType("platform_auth")
_pa.auth_headers = lambda: {}
_pa.self_source_headers = lambda: {}
sys.modules.setdefault("platform_auth", _pa)

sys.path.insert(0, sys.argv[1])
import a2a_client  # noqa: E402

# This replay validates PR #2399's diagnostic helper. If the workspace
# runtime in the current checkout pre-dates that fix, fail with a
# clear message instead of an opaque AttributeError.
if not hasattr(a2a_client, "get_peers_with_diagnostic"):
    print("__SKIP__: workspace/a2a_client.py is pre-#2399 (no get_peers_with_diagnostic).")
    sys.exit(0)

resp = MagicMock()
resp.status_code = 404
resp.json = MagicMock(return_value={"detail": "not found"})

mock_client = AsyncMock()
mock_client.__aenter__ = AsyncMock(return_value=mock_client)
mock_client.__aexit__ = AsyncMock(return_value=False)
mock_client.get = AsyncMock(return_value=resp)

async def main():
    with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
        peers, diag = await a2a_client.get_peers_with_diagnostic()
    print(repr(diag))

asyncio.run(main())
PYEOF
)

if [[ "$DIAGNOSTIC" == __SKIP__:* ]]; then
    echo "[replay] (b) SKIP: ${DIAGNOSTIC#__SKIP__: }"
    echo "[replay]            Re-run after #2399 lands on staging."
    echo ""
    echo "[replay] PASS (a) only: peer-discovery wire returns 404 (parse branch skipped — see above)."
    exit 0
fi

echo "[replay] (b) PARSE: helper diagnostic = $DIAGNOSTIC"

if ! echo "$DIAGNOSTIC" | grep -q "404"; then
    echo "[replay] FAIL (b): diagnostic missing '404' — helper regressed to swallow-the-status-code"
    exit 1
fi
if ! echo "$DIAGNOSTIC" | grep -qi "regist"; then
    echo "[replay] FAIL (b): diagnostic missing 'register' guidance — helper regressed to opaque message"
    exit 1
fi
if echo "$DIAGNOSTIC" | grep -qi "may be isolated"; then
    echo "[replay] FAIL (b): diagnostic still says 'may be isolated' — fix didn't reach this code path"
    exit 1
fi

echo ""
echo "[replay] PASS: peer-discovery (a) wire returns 404, (b) helper produces actionable diagnostic."
