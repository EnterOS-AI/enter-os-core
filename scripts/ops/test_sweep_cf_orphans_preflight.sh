#!/usr/bin/env bash
# test_sweep_cf_orphans_preflight.sh — hermetic regression test for the
# sweep-cf-orphans.sh preflight block (Researcher RCA 2026-06-12,
# runs 352709/job 476863 + 352596/job 476689 at SHA 15872306).
#
# The preflight was added so that an expired/revoked/wrong-scope CF
# token fails the sweep IMMEDIATELY (with a clear error message) and
# BEFORE any of the CP or AWS gather work happens. Without it, the
# script proceeded into the gather steps (~30s of wasted work) and
# then died on the CF DNS list call, leaving a half-completed audit
# log.
#
# This test stands up a tiny local HTTP server that mimics the
# Cloudflare API responses we need (token-verify + zone-lookup +
# dns-records), points the script at it via patch-and-redirect, and
# asserts the four critical behaviors:
#
#   (a) active token + reachable zone → preflight passes (script then
#       computes 0 decisions and exits 0; that's the expected
#       downstream behavior — the preflight itself passed)
#   (b) inactive token (success=false) → preflight fails fast with a
#       clear error; NO gather work ("Fetching CP..." or "Fetching
#       live EC2...") is printed
#   (c) bad zone id (mismatch between configured CF_ZONE_ID and
#       what the API returns) → preflight fails with the mismatch
#       message
#   (d) unreachable CF API (server returns 500 + non-JSON) →
#       preflight fails with a non-JSON error; no gather work happens
#
# Hermetic, no network, no jq needed (uses python3 for JSON checks).
set -euo pipefail

# Derive the source location: this test lives next to the script it
# exercises. If it's been moved (e.g. to /tmp for an isolated run),
# fall back to the repo's canonical scripts/ops path via git rev-parse.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "$SCRIPT_DIR/sweep-cf-orphans.sh" ]; then
  SCRIPT="$SCRIPT_DIR/sweep-cf-orphans.sh"
else
  REPO_ROOT="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel 2>/dev/null || true)"
  if [ -n "$REPO_ROOT" ] && [ -f "$REPO_ROOT/scripts/ops/sweep-cf-orphans.sh" ]; then
    SCRIPT="$REPO_ROOT/scripts/ops/sweep-cf-orphans.sh"
  else
    echo "FAIL: cannot locate sweep-cf-orphans.sh" >&2
    exit 1
  fi
fi

[ -f "$SCRIPT" ] || { echo "FAIL: script not found: $SCRIPT" >&2; exit 1; }
command -v python3 >/dev/null || { echo "FAIL: python3 not on PATH" >&2; exit 1; }

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

# Stand up a tiny HTTP server that emulates just enough of the
# Cloudflare API. The "scenario" is selected by the URL prefix:
#   /client/v4/...                   → default (active token, zone OK)
#   /scenario-inactive/client/v4/... → token verify returns success=false
#   /scenario-mismatch/client/v4/... → zone lookup returns different id
#   /scenario-down/client/v4/...     → 500 + non-JSON body
SRVDIR="$(mktemp -d)"
trap 'rm -rf "$SRVDIR"' EXIT
cat >"$SRVDIR/server.py" <<'PYEOF'
import http.server, json, socketserver, sys, urllib.parse
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        path = u.path
        # /scenario-inactive/... etc. are prefixes that drive the
        # failure mode. Strip the prefix and use it as the scenario.
        scenario = "active"
        if path.startswith("/scenario-inactive/"):
            scenario = "inactive"
            path = path[len("/scenario-inactive"):]
        elif path.startswith("/scenario-mismatch/"):
            scenario = "mismatch"
            path = path[len("/scenario-mismatch"):]
        elif path.startswith("/scenario-down/"):
            scenario = "down"
            path = path[len("/scenario-down"):]

        if "down" == scenario:
            # 500 + non-JSON body — exercises the non-JSON path
            body = b"not json"
            self.send_response(500)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(body)
            return

        rest = path.lstrip("/")
        if "tokens/verify" in rest:
            if scenario == "inactive":
                payload = {
                    "success": False,
                    "errors": [{"code": 9109, "message": "Invalid API token"}],
                    "messages": [],
                }
            else:
                payload = {
                    "success": True, "errors": [], "messages": [],
                    "result": {"id": "tok-1", "status": "active"},
                }
        elif "dns_records" in rest:
            payload = {"success": True, "errors": [], "messages": [], "result": []}
        elif "zones/" in rest:
            # URL is /client/v4/zones/{id}[/...]. rest is
            # "client/v4/zones/{id}[/...]" so the zone id is the
            # 4th segment (index 3). The previous seg[2] read
            # literally the literal "zones" token, which made
            # every active/down case return zone id "zones" and
            # trip the preflight's mismatch check.
            seg = rest.split("/")
            zone_id = seg[3] if len(seg) > 3 else (seg[-1] if seg else "test")
            if scenario == "mismatch":
                payload = {
                    "success": True, "errors": [], "messages": [],
                    "result": {"id": "DIFFERENT-ZONE-ID", "name": "moleculesai.app"},
                }
            else:
                payload = {
                    "success": True, "errors": [], "messages": [],
                    "result": {"id": zone_id, "name": "moleculesai.app"},
                }
        else:
            payload = {
                "success": False,
                "errors": [{"code": 10000, "message": "unknown endpoint"}],
                "messages": [],
            }
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a, **kw): pass

socketserver.TCPServer.allow_reuse_address = True
srv = socketserver.TCPServer(("127.0.0.1", int(sys.argv[1])), H)
srv.serve_forever()
PYEOF

# Find a free port. Use SO_REUSEADDR on the probe so we don't
# lose a port to TIME_WAIT after the probe (which races the server's
# bind in CI under load).
PORT=""
for tryport in $(seq 18080 18180); do
  if python3 -c "
import socket, sys
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
try:
    s.bind(('127.0.0.1', $tryport))
except OSError:
    sys.exit(1)
s.close()
sys.exit(0)
" 2>/dev/null; then
    PORT=$tryport
    break
  fi
done
[ -n "$PORT" ] || fail "could not find a free port in 18080-18180"

python3 "$SRVDIR/server.py" "$PORT" >"$SRVDIR/server.out" 2>&1 &
SRV_PID=$!
trap 'kill $SRV_PID 2>/dev/null || true; rm -rf "$SRVDIR"' EXIT

# Wait for server to bind (up to 15s — startup can be slow on busy
# CI runners, and a server that has bound-but-not-yet-accepting
# needs a moment to enter its accept loop). Use a Python-based
# readiness probe (TCP connect + HTTP GET + JSON parse) so we get
# a single source of truth on "the mock can serve the canonical
# request" rather than chaining curl + grep + shell, which has
# racey pipe-handle interactions under load. Also verify the
# server PID is still alive so a crash surfaces with its stderr
# instead of timing out silently.
ready=false
for _ in $(seq 1 75); do
  if ! kill -0 "$SRV_PID" 2>/dev/null; then
    cat "$SRVDIR/server.out" >&2 || true
    fail "mock server PID $SRV_PID died during startup; stderr above"
  fi
  if python3 -c "
import json, socket, sys, urllib.request, urllib.error
try:
    s = socket.create_connection(('127.0.0.1', $PORT), timeout=1)
except (OSError, socket.timeout):
    sys.exit(1)
s.close()
try:
    r = urllib.request.urlopen('http://127.0.0.1:$PORT/client/v4/user/tokens/verify', timeout=2)
    body = r.read().decode()
except (urllib.error.URLError, OSError, socket.timeout):
    sys.exit(1)
try:
    p = json.loads(body)
except Exception:
    sys.exit(1)
if p.get('result', {}).get('status') == 'active':
    sys.exit(0)
sys.exit(1)
" 2>/dev/null; then
    ready=true
    break
  fi
  sleep 0.2
done
if [[ "$ready" != "true" ]]; then
  echo "mock server stderr/stdout so far:" >&2
  cat "$SRVDIR/server.out" >&2 || true
  fail "mock server didn't come up on port $PORT within 15s"
fi
pass "mock CF server up on http://127.0.0.1:$PORT"

# Create a patched copy of the script with the CF base URL redirected
# to our mock. There are 3 hardcoded `https://api.cloudflare.com/client/v4`
# references; replace them all.
WORK="$SRVDIR/sweep-cf-orphans-patched.sh"
cp "$SCRIPT" "$WORK"
CF_BASE="https://api.cloudflare.com/client/v4"
MOCK_BASE="http://127.0.0.1:$PORT/client/v4"
MOCK_BASE_INACTIVE="http://127.0.0.1:$PORT/scenario-inactive/client/v4"
MOCK_BASE_MISMATCH="http://127.0.0.1:$PORT/scenario-mismatch/client/v4"
MOCK_BASE_DOWN="http://127.0.0.1:$PORT/scenario-down/client/v4"
sed -i "s|$CF_BASE|$MOCK_BASE|g" "$WORK"
EXPECTED_COUNT=4
ACTUAL_COUNT=$(grep -c "$MOCK_BASE" "$WORK" || true)
[ "$ACTUAL_COUNT" = "$EXPECTED_COUNT" ] \
  || fail "expected $EXPECTED_COUNT occurrences of mock base in patched script, got: $ACTUAL_COUNT"

# Make a per-test patched copy (for scenario-specific URL prefixes)
make_patched() {
  local url="$1"
  local out="$2"
  cp "$SCRIPT" "$out"
  sed -i "s|$CF_BASE|$url|g" "$out"
}

make_patched "$MOCK_BASE_INACTIVE" "$SRVDIR/patched-inactive.sh"
make_patched "$MOCK_BASE_MISMATCH"  "$SRVDIR/patched-mismatch.sh"
make_patched "$MOCK_BASE_DOWN"      "$SRVDIR/patched-down.sh"

# Common env
ENV_TOKENS=(
  CF_API_TOKEN=test-token-fake
  CF_ZONE_ID=test-zone-id
  CP_ADMIN_API_TOKEN=fake-cp-prod
  CP_STAGING_ADMIN_API_TOKEN=fake-cp-staging
  AWS_ACCESS_KEY_ID=fake
  AWS_SECRET_ACCESS_KEY=fake
)

# (a) Active token + reachable zone — preflight should pass. The
# script then computes 0 decisions on the empty mock DNS list and
# exits 0. The KEY assertion is the two preflight ✓ messages.
echo "=== (a) active token + reachable zone ==="
out_a=$(env "${ENV_TOKENS[@]}" bash "$WORK" 2>&1 || true)
echo "$out_a" | grep -q "CF token active" \
  || fail "(a) expected 'CF token active' in output, got: $(echo "$out_a" | head -10)"
echo "$out_a" | grep -q "zone test-zone-id reachable" \
  || fail "(a) expected 'zone test-zone-id reachable' in output, got: $(echo "$out_a" | head -10)"
pass "(a) preflight passes when token is active and zone is reachable"

# (b) Inactive token — preflight fails BEFORE any gather work.
# CRITICAL: the gather steps must NOT have happened. Use the same
# env as the success case (NOT just CF_API_TOKEN) so the script's
# `need CF_ZONE_ID` guard passes and we actually exercise the
# preflight's auth-failure path — otherwise a missing CF_ZONE_ID
# would short-circuit at the `need` check, masking the regression
# we want to catch.
echo "=== (b) inactive token ==="
out_b=$(env "${ENV_TOKENS[@]}" CF_API_TOKEN=inactive-token bash "$SRVDIR/patched-inactive.sh" 2>&1 || true)
echo "$out_b" | grep -q "CF preflight FAILED" \
  || fail "(b) expected 'CF preflight FAILED' in output, got: $(echo "$out_b" | head -10)"
if echo "$out_b" | grep -qE "Fetching CP prod org slugs|Fetching live EC2 Name tags|Fetching Cloudflare DNS records"; then
  fail "(b) preflight failed BUT gather steps ran — the fail-fast invariant is broken. Output: $(echo "$out_b" | head -20)"
fi
pass "(b) preflight fails fast on inactive token; NO gather steps ran"

# (c) Zone-id mismatch — preflight fails with the mismatch message.
echo "=== (c) zone id mismatch ==="
out_c=$(env "${ENV_TOKENS[@]}" bash "$SRVDIR/patched-mismatch.sh" 2>&1 || true)
echo "$out_c" | grep -q "zone id mismatch" \
  || fail "(c) expected 'zone id mismatch' in output, got: $(echo "$out_c" | head -10)"
if echo "$out_c" | grep -qE "Fetching CP prod org slugs|Fetching live EC2 Name tags|Fetching Cloudflare DNS records"; then
  fail "(c) preflight failed on zone mismatch BUT gather steps ran"
fi
pass "(c) preflight fails on zone-id mismatch; NO gather steps ran"

# (d) Unreachable CF API (500 + non-JSON).
echo "=== (d) CF API unreachable (500 + non-JSON) ==="
out_d=$(env "${ENV_TOKENS[@]}" bash "$SRVDIR/patched-down.sh" 2>&1 || true)
echo "$out_d" | grep -qE "non-JSON from /user/tokens/verify|CF preflight FAILED" \
  || fail "(d) expected preflight failure message; got: $(echo "$out_d" | head -10)"
if echo "$out_d" | grep -qE "Fetching CP prod org slugs|Fetching live EC2 Name tags|Fetching Cloudflare DNS records"; then
  fail "(d) preflight failed on 500 BUT gather steps ran"
fi
pass "(d) preflight fails on 500/non-JSON; NO gather steps ran"

# Stop the mock server
kill $SRV_PID 2>/dev/null || true

echo
echo "sweep-cf-orphans preflight regression test passed"
