#!/usr/bin/env bash
# Replay for the channel envelope peer_id trust-boundary fix
# (PR #2481, follow-up to PR #2471). Verifies that the PUBLISHED wheel
# installed on this machine — not local source — gates malformed peer_id
# at both the envelope builder and the agent_card_url builder.
#
# Why this matters:
#   - Unit tests in workspace/tests/ run against local source. They
#     prove the fix works in source. They DO NOT prove the published
#     wheel contains the fix.
#   - The wheel rewriter (scripts/build_runtime_package.py) renames
#     symbols + paths. Any rewrite drift could silently strip the
#     guard from the shipped artifact.
#   - This replay imports from `molecule_runtime.a2a_mcp_server` (the
#     wheel-rewritten path), exercises the actual published code, and
#     asserts the envelope shape. If the wheel build ever ships without
#     the guard, this fails — even if unit tests on local source pass.
#
# Phases:
#   A. Confirm an installed molecule-runtime version that contains the
#      #2481 fix (>= 0.1.78).
#   B. Call `_build_channel_notification` with peer_id="../../foo" and
#      assert (1) meta["peer_id"] == "", (2) no agent_card_url field,
#      (3) no peer_name/peer_role.
#   C. Symmetric case: peer_id with embedded XML-attribute injection
#      bytes — assert the same scrubbing.
#   D. Happy path: a valid UUID peer_id is preserved (proves we didn't
#      regress legitimate enrichment).
#   E. Direct check on the URL builder — `_agent_card_url_for("../../foo")`
#      must return "" and never an unsanitised URL.

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARNESS_ROOT="$(dirname "$HERE")"
cd "$HARNESS_ROOT"
# shellcheck source=../_curl.sh
source "$HARNESS_ROOT/_curl.sh"

PASS=0
FAIL=0

assert() {
    local desc="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
        printf "  PASS %s\n" "$desc"
        PASS=$((PASS + 1))
    else
        printf "  FAIL %s\n    expected: %s\n    got     : %s\n" "$desc" "$expected" "$actual" >&2
        FAIL=$((FAIL + 1))
    fi
}

# ─── Phase A: wheel version contains the fix ───────────────────────────
echo "[replay] A. confirming installed molecule-ai-workspace-runtime contains #2481..."
INSTALLED=$(pip3 show molecule-ai-workspace-runtime 2>/dev/null | awk -F': ' '/^Version:/ {print $2}')
if [ -z "$INSTALLED" ]; then
    echo "[replay] FAIL A: molecule-ai-workspace-runtime not installed."
    echo "         Install: pip3 install molecule-ai-workspace-runtime"
    exit 2
fi
echo "[replay]   installed version: $INSTALLED"

# 0.1.78 is the first published version after #2481 merged to staging.
# Compare via Python distutils-style version sort (works across patch
# bumps without sed-fragility).
HAS_FIX=$(python3 -c "
from packaging.version import parse
print('yes' if parse('$INSTALLED') >= parse('0.1.78') else 'no')
" 2>/dev/null || echo "unknown")
if [ "$HAS_FIX" != "yes" ]; then
    echo "[replay] FAIL A: installed $INSTALLED < 0.1.78 (the version that shipped the #2481 fix)."
    echo "         Upgrade: pip3 install --upgrade molecule-ai-workspace-runtime"
    exit 2
fi
echo "[replay]   ✓ contains #2481 trust-boundary fix"

# ─── Phase B-E: in-process assertions against the installed wheel ──────
# We don't need WORKSPACE_ID/PLATFORM_URL/MOLECULE_WORKSPACE_TOKEN to
# import the module — the env validation only fires at console-script
# entry. We use molecule_runtime.* (the wheel-rewritten import path)
# rather than workspace.a2a_mcp_server (local source) so this exercises
# the SHIPPED code.
echo ""
echo "[replay] B-E. exercising _build_channel_notification + _agent_card_url_for from the installed wheel..."

OUT=$(WORKSPACE_ID=00000000-0000-0000-0000-000000000000 \
      PLATFORM_URL=http://localhost:8080 \
      MOLECULE_WORKSPACE_TOKEN=stub \
      MOLECULE_MCP_DISABLE_HEARTBEAT=1 \
      python3 - <<'PYEOF'
import json
import sys

from molecule_runtime.a2a_mcp_server import _build_channel_notification
from molecule_runtime.a2a_client import _agent_card_url_for

results = []

def emit(name, value):
    results.append({"name": name, "value": value})

# ── B: path-traversal peer_id stripped from envelope ──
payload = _build_channel_notification({
    "peer_id": "../../foo",
    "kind": "peer_agent",
    "text": "redirect-attempt",
    "activity_id": "act-1",
    "method": "message/send",
    "created_at": "2026-05-01T00:00:00Z",
})
meta = payload["params"]["meta"]
emit("B1_peer_id_scrubbed", meta.get("peer_id", "<missing>"))
emit("B2_agent_card_url_absent", "absent" if "agent_card_url" not in meta else meta["agent_card_url"])
emit("B3_peer_name_absent", "absent" if "peer_name" not in meta else meta["peer_name"])
emit("B4_peer_role_absent", "absent" if "peer_role" not in meta else meta["peer_role"])

# ── C: XML-attribute-injection-shape peer_id ──
payload = _build_channel_notification({
    "peer_id": 'aaa" onclick="alert(1)',
    "kind": "peer_agent",
    "text": "xss",
})
meta = payload["params"]["meta"]
emit("C1_peer_id_scrubbed", meta.get("peer_id", "<missing>"))
emit("C2_agent_card_url_absent", "absent" if "agent_card_url" not in meta else "leaked")

# ── D: legitimate UUID is preserved ──
valid_uuid = "11111111-2222-3333-4444-555555555555"
payload = _build_channel_notification({
    "peer_id": valid_uuid,
    "kind": "peer_agent",
    "text": "legit",
})
meta = payload["params"]["meta"]
emit("D1_peer_id_preserved", meta.get("peer_id", "<missing>"))
# agent_card_url IS present (we don't gate the URL itself on whether the registry is reachable)
emit("D2_agent_card_url_present", "yes" if meta.get("agent_card_url", "").endswith(valid_uuid) else "no")

# ── E: direct URL builder gate ──
emit("E1_url_builder_strips_traversal", _agent_card_url_for("../../foo"))
emit("E2_url_builder_strips_xml", _agent_card_url_for('a" onclick="x'))
emit("E3_url_builder_accepts_uuid_endswith", "yes" if _agent_card_url_for(valid_uuid).endswith(valid_uuid) else "no")

print(json.dumps(results))
PYEOF
)

# Parse and assert each result.
echo "$OUT" | python3 -c "
import json, sys
results = json.loads(sys.stdin.read())
for r in results:
    print(f\"{r['name']}={r['value']}\")
" > /tmp/cha-envelope-results.txt

while IFS='=' read -r key value; do
    case "$key" in
        B1_peer_id_scrubbed)        assert "B1: malicious peer_id scrubbed to \"\"" "" "$value" ;;
        B2_agent_card_url_absent)   assert "B2: agent_card_url not emitted" "absent" "$value" ;;
        B3_peer_name_absent)        assert "B3: peer_name not enriched" "absent" "$value" ;;
        B4_peer_role_absent)        assert "B4: peer_role not enriched" "absent" "$value" ;;
        C1_peer_id_scrubbed)        assert "C1: XML-injection peer_id scrubbed" "" "$value" ;;
        C2_agent_card_url_absent)   assert "C2: XML-injection URL not emitted" "absent" "$value" ;;
        D1_peer_id_preserved)       assert "D1: valid UUID peer_id preserved" "11111111-2222-3333-4444-555555555555" "$value" ;;
        D2_agent_card_url_present)  assert "D2: agent_card_url present for valid id" "yes" "$value" ;;
        E1_url_builder_strips_traversal) assert "E1: _agent_card_url_for(\"../../foo\") returns \"\"" "" "$value" ;;
        E2_url_builder_strips_xml)       assert "E2: _agent_card_url_for(XML-injection) returns \"\"" "" "$value" ;;
        E3_url_builder_accepts_uuid_endswith) assert "E3: _agent_card_url_for(valid uuid) builds canonical URL" "yes" "$value" ;;
    esac
done < /tmp/cha-envelope-results.txt

echo ""
if [ "$FAIL" -gt 0 ]; then
    echo "[replay] FAIL: $PASS pass, $FAIL fail"
    echo ""
    echo "[replay] If B/C/E failed: the published wheel does NOT contain the #2481 fix."
    echo "[replay] Likely causes:"
    echo "         - Wheel rewriter dropped _validate_peer_id from molecule_runtime.a2a_client"
    echo "         - publish-runtime.yml regressed to a SHA before #2481 (check pip install version)"
    exit 1
fi
echo "[replay] PASS: $PASS/$PASS — channel envelope peer_id trust boundary holds in published wheel $INSTALLED"
