#!/usr/bin/env bash
# test_saas_tenant.sh — smoke test a live SaaS tenant through the Cloudflare Worker
#
# Usage: TENANT_SLUG=example-org bash tests/e2e/test_saas_tenant.sh
#        TENANT_SLUG=example-org DIRECT_IP=<EC2_IP> bash tests/e2e/test_saas_tenant.sh
#
# Tests both Worker-proxied routes and (optionally) direct EC2 access.
# Exits 0 if all critical tests pass, 1 otherwise.

set -euo pipefail

SLUG="${TENANT_SLUG:?Set TENANT_SLUG=<org-slug>}"
BASE="https://${SLUG}.moleculesai.app"
DIRECT="${DIRECT_IP:-}"
PASS=0
FAIL=0
SKIP=0

check() {
  local label="$1" url="$2" expect="$3"
  local code
  code=$(curl -sk -o /dev/null -w "%{http_code}" --connect-timeout 5 "$url" 2>/dev/null || echo "000")
  if [ "$code" = "$expect" ]; then
    printf "  PASS  %-40s %s → %s\n" "$label" "$url" "$code"
    PASS=$((PASS + 1))
  else
    printf "  FAIL  %-40s %s → %s (expected %s)\n" "$label" "$url" "$code" "$expect"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== SaaS Tenant Smoke Test: ${SLUG} ==="
echo ""

echo "--- Worker routing ---"
check "health"           "$BASE/health"           "200"
check "canvas root"      "$BASE/"                 "200"
check "plugins"          "$BASE/plugins"          "200"
check "templates"        "$BASE/templates"        "200"
check "workspaces"       "$BASE/workspaces"       "200"
check "org/templates"    "$BASE/org/templates"    "200"
check "approvals/pending" "$BASE/approvals/pending" "200"
check "canvas/viewport"  "$BASE/canvas/viewport"  "200"
check "metrics"          "$BASE/metrics"          "200"

echo ""
echo "--- Error handling ---"
check "nonexistent workspace" "$BASE/workspaces/00000000-0000-0000-0000-000000000000" "401"
check "bad path"              "$BASE/does-not-exist" "200"  # canvas catch-all

echo ""
echo "--- WebSocket (upgrade header) ---"
ws_code=$(curl -sk -o /dev/null -w "%{http_code}" \
  -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
  "$BASE/ws" 2>/dev/null || echo "000")
if [ "$ws_code" = "101" ] || [ "$ws_code" = "400" ]; then
  printf "  PASS  %-40s %s → %s\n" "websocket upgrade" "$BASE/ws" "$ws_code"
  PASS=$((PASS + 1))
else
  printf "  FAIL  %-40s %s → %s (expected 101 or 400)\n" "websocket upgrade" "$BASE/ws" "$ws_code"
  FAIL=$((FAIL + 1))
fi

if [ -n "$DIRECT" ]; then
  echo ""
  echo "--- Direct EC2 (port 8080) ---"
  check "direct health"    "http://${DIRECT}:8080/health"   "200"
  check "direct metrics"   "http://${DIRECT}:8080/metrics"  "200"

  echo ""
  echo "--- Direct Canvas (port 3000) ---"
  check "direct canvas"    "http://${DIRECT}:3000/"         "200"
fi

echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped ==="
[ "$FAIL" -eq 0 ] && echo "ALL TESTS PASSED" || echo "SOME TESTS FAILED"
exit "$FAIL"
