# shellcheck shell=bash
# Shared peer-visibility assertion core — runtime/backend-AGNOSTIC.
#
# WHY THIS FILE EXISTS
# --------------------
# The peer-visibility gate (PR #1298) was staging-only. Per the standing
# rule that the local prod-mimic stack must run a MANDATORY local-Postgres
# E2E BEFORE staging E2E (memory: feedback_local_must_mimic_production,
# feedback_mandatory_local_e2e_before_ship, feedback_local_test_before_
# staging_e2e), peer-visibility must also run against the local stack.
#
# The ASSERTION must be byte-identical between local and staging — only
# provisioning differs. So the literal MCP `list_peers` call + every
# anti-proxy / anti-native-fallback guarantee lives HERE, sourced by both
# tests/e2e/test_peer_visibility_mcp_staging.sh (staging/CP backend) and
# tests/e2e/test_peer_visibility_mcp_local.sh (local docker-compose
# backend). If this assertion ever diverges between the two, that is the
# bug — keep it in one place.
#
# THIS IS NOT A PROXY. pv_assert_runtime issues the byte-for-byte
# JSON-RPC `tools/call name=list_peers` envelope to `POST
# /workspaces/:id/mcp` using the workspace's OWN bearer token, through
# the real WorkspaceAuth + MCPRateLimiter middleware chain — the exact
# call mcp_molecule_list_peers makes from a canvas agent. It does NOT
# read a registry row, /health, the heartbeat table, or
# GET /registry/:id/peers.
#
# Contract:
#   pv_assert_runtime <runtime> <ws_id> <ws_bearer> <base_url> \
#                     <org_id_or_empty> <all_ws_ids_space_separated>
#
#     <org_id_or_empty>  staging: the X-Molecule-Org-Id header value.
#                        local:   "" (the local single-tenant stack does
#                                 not gate on the org header; the header
#                                 is simply omitted when empty).
#     <all_ws_ids>       every provisioned workspace id (parent + every
#                        runtime sibling). The expected peer set for this
#                        runtime is every id in here EXCEPT <ws_id>.
#
#   Sets the global PV_VERDICT to one of:
#     OK
#     FAIL(http=<code>)
#     FAIL(native-fallback)
#     FAIL(rpc=<detail>)
#     FAIL(peers=<detail>)
#     FAIL(unknown)
#   Returns 0 when PV_VERDICT=OK, 1 otherwise. Never exits — the caller
#   owns aggregation + the gate exit code (10 = regression reproduced).
#
# The literal JSON-RPC envelope. Identical to what
# workspace/platform_tools/registry.py's mcp_molecule_list_peers emits.
PV_RPC_BODY='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_peers","arguments":{}}}'

pv_assert_runtime() {
  local rt="$1" wid="$2" wtok="$3" base_url="$4" org_id="$5" all_ws_ids="$6"

  # Expected peer set = every OTHER provisioned workspace, excluding the
  # caller itself. Byte-identical selection to the original staging script.
  local expect_ids
  expect_ids=$(echo "$all_ws_ids" | tr ' ' '\n' | grep -v "^${wid}$" | grep -v '^$')

  # X-Molecule-Org-Id only when the backend supplies one (staging multi-
  # tenant). Local single-tenant omits it — the same WorkspaceAuth +
  # MCPRateLimiter chain still runs; only the tenant-routing header differs.
  local org_header=()
  if [ -n "$org_id" ]; then
    org_header=(-H "X-Molecule-Org-Id: $org_id")
  fi

  local resp http_code body
  set +e
  resp=$(curl -sS -X POST "$base_url/workspaces/$wid/mcp" \
    -H "Authorization: Bearer $wtok" \
    ${org_header[@]+"${org_header[@]}"} \
    -H "Content-Type: application/json" \
    -d "$PV_RPC_BODY" \
    -o /tmp/pv_mcp_body.json -w "%{http_code}" 2>/dev/null)
  set -e
  http_code="$resp"
  body=$(cat /tmp/pv_mcp_body.json 2>/dev/null || echo '')

  echo "--- $rt (ws=$wid) ---"
  echo "    HTTP $http_code"
  echo "    body: $(echo "$body" | head -c 600)"

  # (1) HTTP 200 — a 401 (WorkspaceAuth reject, the Hermes symptom) fails here.
  if [ "$http_code" != "200" ]; then
    echo "  ✗ $rt: list_peers MCP call returned HTTP $http_code (expected 200)"
    PV_VERDICT="FAIL(http=$http_code)"
    return 1
  fi

  # (2) JSON-RPC result present, not an error object; expected sibling IDs
  #     present; not a native-sessions fallback. Byte-identical to the
  #     original staging script's inline python.
  local parse
  parse=$(echo "$body" | python3 -c "
import sys, json
expect = set(filter(None, '''$expect_ids'''.split()))
try:
    d = json.load(sys.stdin)
except Exception as e:
    print('PARSE_ERROR:' + str(e)); sys.exit(0)
if isinstance(d, dict) and d.get('error') is not None:
    print('RPC_ERROR:' + json.dumps(d['error'])[:200]); sys.exit(0)
res = d.get('result') if isinstance(d, dict) else None
if res is None:
    print('NO_RESULT'); sys.exit(0)
# MCP tools/call result shape: {content:[{type:text,text:'<json or prose>'}]}
text = ''
if isinstance(res, dict):
    for c in res.get('content', []):
        if c.get('type') == 'text':
            text += c.get('text', '')
text_l = text.lower()
# Native-sessions fallback signature (the OpenClaw symptom): the agent
# answered from its own runtime session list, not the platform peer set.
if 'sessions_list' in text_l or 'no platform peers' in text_l or 'native session' in text_l:
    print('NATIVE_FALLBACK:' + text[:200]); sys.exit(0)
# The expected sibling IDs must literally appear in the returned peer text.
found = sorted(i for i in expect if i in text)
missing = sorted(expect - set(found))
if not expect:
    print('NO_EXPECTED_PEERS_CONFIGURED'); sys.exit(0)
if missing:
    print('MISSING_PEERS:found=%d/%d missing=%s' % (len(found), len(expect), ','.join(m[:8] for m in missing)))
    sys.exit(0)
print('OK:found=%d/%d' % (len(found), len(expect)))
" 2>/dev/null)

  case "$parse" in
    OK:*)
      echo "  ✓ $rt: list_peers returned 200 and contains all expected peers ($parse)"
      PV_VERDICT="OK"
      return 0
      ;;
    NATIVE_FALLBACK:*)
      echo "  ✗ $rt: list_peers fell back to NATIVE sessions — sees no platform peers ($parse)"
      PV_VERDICT="FAIL(native-fallback)"
      return 1
      ;;
    RPC_ERROR:*|NO_RESULT|PARSE_ERROR:*)
      echo "  ✗ $rt: list_peers MCP call did not return a usable result ($parse)"
      PV_VERDICT="FAIL(rpc=$parse)"
      return 1
      ;;
    MISSING_PEERS:*)
      echo "  ✗ $rt: list_peers returned 200 but peer set is wrong/empty ($parse)"
      PV_VERDICT="FAIL(peers=$parse)"
      return 1
      ;;
    NO_EXPECTED_PEERS_CONFIGURED)
      # Caller bug, not a runtime regression — surface loudly so a
      # mis-wired backend can't mint a false green.
      echo "  ✗ $rt: no expected peers were configured for this caller"
      # shellcheck disable=SC2034 # exported verdict is read by the caller's map plumbing.
      PV_VERDICT="FAIL(rpc=NO_EXPECTED_PEERS_CONFIGURED)"
      return 1
      ;;
    *)
      echo "  ✗ $rt: unexpected verdict '$parse'"
      # shellcheck disable=SC2034 # exported verdict is read by the caller's map plumbing.
      PV_VERDICT="FAIL(unknown)"
      return 1
      ;;
  esac
}
