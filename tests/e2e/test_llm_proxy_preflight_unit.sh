#!/usr/bin/env bash
# Unit tests for tests/e2e/lib/llm_proxy_preflight.sh (core#2675).
#
# Verifies:
#   1. Config-missing path (exit 71) when E2E_LLM_PROXY_URL is unset AND
#      MOLECULE_CP_URL is unset.
#   2. DEP-DOWN path (exit 70) when the proxy URL is unreachable.
#   3. DEP-DOWN path (exit 70) when the proxy returns 5xx.
#   4. Happy path (exit 0) when the proxy returns any HTTP response,
#      including 401 (the #76 semantics fix: an unauthenticated probe
#      against an auth-required proxy must NOT be classified as
#      dependency-down).
#   5. The error message starts with the `DEP-DOWN:staging-llm` prefix
#      that the redgate-reporter parses for dedup.
#
# These tests use a small Python helper as a stand-in for the actual LLM
# proxy (avoids needing a real proxy in the test environment). The Python
# helper listens on a localhost port and serves a configurable response.

set -uo pipefail

# Find the lib under test. Allow override for CI flexibility.
LIB_PATH="${LIB_PATH:-$(cd "$(dirname "$0")" && pwd)/lib/llm_proxy_preflight.sh}"

# shellcheck source=lib/llm_proxy_preflight.sh
# shellcheck disable=SC1091
source "$LIB_PATH"

# Start a tiny Python HTTP server to stand in for the LLM proxy. We use
# Python's http.server because it ships in the base image and doesn't
# require extra dependencies. Each test picks a free port via Python's
# socket binding (avoids race conditions in test parallelism).
PY_SERVER_PORT=""
PY_SERVER_LOG=$(mktemp)
PY_SERVER_PID=

start_test_server() {
  local mode="$1"  # "ok" | "down" | "unauth"
  # Pick a free port via socket binding; pass it explicitly to the server.
  local port
  port=$(python3 -c "
import socket
s = socket.socket()
s.bind(('127.0.0.1', 0))
print(s.getsockname()[1])
s.close()
")
  cat > /tmp/_llm_preflight_test_server.py <<PYEOF
import http.server, json, sys
mode = "$mode"
port = $port
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        if mode == "down":
            self.send_error(503, "simulated outage")
            return
        if mode == "unauth":
            self.send_response(401)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"error":"unauthorized"}')
            return
        # ok
        body = {"choices":[{"message":{"role":"assistant","content":"pong"}}]}
        payload = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)
    def log_message(self, *args, **kwargs):
        pass
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
PYEOF
  python3 /tmp/_llm_preflight_test_server.py >"$PY_SERVER_LOG" 2>&1 &
  PY_SERVER_PID=$!
  # Give the server a moment to bind.
  sleep 0.3
  PY_SERVER_PORT="$port"
}

stop_test_server() {
  if [ -n "$PY_SERVER_PID" ]; then
    kill "$PY_SERVER_PID" 2>/dev/null || true
    wait "$PY_SERVER_PID" 2>/dev/null || true
  fi
  rm -f /tmp/_llm_preflight_test_server.py "$PY_SERVER_LOG"
}
trap stop_test_server EXIT

# Test 1: config-missing path.
test_config_missing() {
  unset E2E_LLM_PROXY_URL
  unset MOLECULE_CP_URL
  local out rc
  out=$(llm_proxy_preflight 2>&1)
  rc=$?
  if [ "$rc" -ne 71 ]; then
    echo "FAIL: test_config_missing expected exit 71, got $rc"
    echo "  output: $out"
    return 1
  fi
  # Config-missing emits CONFIG-MISSING, NOT DEP-DOWN — see the lib's
  # comment on the status description prefixes. The two dedup buckets
  # are distinct in the redgate-reporter.
  if ! echo "$out" | grep -q "CONFIG-MISSING:staging-llm-proxy-url"; then
    echo "FAIL: test_config_missing output missing CONFIG-MISSING:staging-llm-proxy-url prefix"
    echo "  output: $out"
    return 1
  fi
  if echo "$out" | grep -q "DEP-DOWN:staging-llm"; then
    echo "FAIL: test_config_missing output should NOT contain DEP-DOWN:staging-llm (config-missing is a separate dedup bucket)"
    echo "  output: $out"
    return 1
  fi
  echo "PASS: test_config_missing"
  return 0
}

# Test 2: proxy unreachable (TCP connection refused) → exit 70.
test_proxy_unreachable() {
  PY_SERVER_PORT=1  # port 1 is privileged, will refuse
  start_test_server "ok"  # we ignore the server, just want the lib to hit a dead port
  sleep 0.3
  E2E_LLM_PROXY_URL="http://127.0.0.1:1/v1/chat/completions"
  local out rc
  out=$(llm_proxy_preflight 2>&1)
  rc=$?
  if [ "$rc" -ne 70 ]; then
    echo "FAIL: test_proxy_unreachable expected exit 70, got $rc"
    echo "  output: $out"
    return 1
  fi
  if ! echo "$out" | grep -q "DEP-DOWN:staging-llm"; then
    echo "FAIL: test_proxy_unreachable output missing DEP-DOWN:staging-llm prefix"
    echo "  output: $out"
    return 1
  fi
  echo "PASS: test_proxy_unreachable"
  return 0
}

# Test 3: proxy returns 401 (auth required) → exit 0 (#76 semantics fix).
test_401_reachable() {
  PY_SERVER_PORT=0
  start_test_server "unauth"
  # E2E_LLM_PROXY_URL is read by the sourced llm_proxy_preflight helper
  # (lib/llm_proxy_preflight.sh) via ${E2E_LLM_PROXY_URL:-}. Export it
  # here so shellcheck doesn't false-positive SC2034 (appears unused) when
  # the test file is checked in isolation.
  export E2E_LLM_PROXY_URL="http://127.0.0.1:${PY_SERVER_PORT}/v1/chat/completions"
  local out rc
  out=$(llm_proxy_preflight 2>&1)
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "FAIL: test_401_reachable expected exit 0, got $rc"
    echo "  output: $out"
    return 1
  fi
  stop_test_server
  PY_SERVER_PID=
  echo "PASS: test_401_reachable"
  return 0
}

# Test 4: happy path → exit 0.
test_ok() {
  PY_SERVER_PORT=0
  start_test_server "ok"
  # E2E_LLM_PROXY_URL is read by the sourced llm_proxy_preflight helper
  # (lib/llm_proxy_preflight.sh) via ${E2E_LLM_PROXY_URL:-}. Export it
  # here so shellcheck doesn't false-positive SC2034 (appears unused) when
  # the test file is checked in isolation.
  export E2E_LLM_PROXY_URL="http://127.0.0.1:${PY_SERVER_PORT}/v1/chat/completions"
  local out rc
  out=$(llm_proxy_preflight 2>&1)
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "FAIL: test_ok expected exit 0, got $rc"
    echo "  output: $out"
    return 1
  fi
  stop_test_server
  PY_SERVER_PID=
  echo "PASS: test_ok"
  return 0
}

# Test 5: proxy returns 503 (simulated outage) → exit 70.
test_503() {
  PY_SERVER_PORT=0
  start_test_server "down"
  # E2E_LLM_PROXY_URL is read by the sourced llm_proxy_preflight helper
  # (lib/llm_proxy_preflight.sh) via ${E2E_LLM_PROXY_URL:-}. Export it
  # here so shellcheck doesn't false-positive SC2034 (appears unused) when
  # the test file is checked in isolation.
  export E2E_LLM_PROXY_URL="http://127.0.0.1:${PY_SERVER_PORT}/v1/chat/completions"
  local out rc
  out=$(llm_proxy_preflight 2>&1)
  rc=$?
  if [ "$rc" -ne 70 ]; then
    echo "FAIL: test_503 expected exit 70, got $rc"
    echo "  output: $out"
    return 1
  fi
  stop_test_server
  PY_SERVER_PID=
  echo "PASS: test_503"
  return 0
}

failed=0
test_config_missing || failed=$((failed+1))
test_proxy_unreachable || failed=$((failed+1))
test_401_reachable || failed=$((failed+1))
test_ok || failed=$((failed+1))
test_503 || failed=$((failed+1))

if [ "$failed" -gt 0 ]; then
  echo "FAILED: $failed test(s)"
  exit 1
fi
echo "All llm_proxy_preflight unit tests passed"
