#!/usr/bin/env bash
# E2E regression suite asserting that "dev mode" is fail-CLOSED.
#
# History: this file used to assert the local-dev fail-open escape hatches
# (GET /workspaces 200 with NO bearer, /workspaces/:id/activity 200 with no
# bearer) added in fix/quickstart-bugless. Under the CTO "nothing should be
# fail-open" directive (harden/no-fail-open-auth) those hatches were REMOVED:
# auth is fail-CLOSED in EVERY environment, local dev included. This suite now
# pins the inverse contract — bearer-less admin/workspace requests 401, and the
# SAME requests with the dev ADMIN_TOKEN bearer succeed.
#
# What it verifies:
#   1. GET /workspaces 401s with NO bearer once tokens exist (was: 200 via the
#      removed AdminAuth Tier-1b dev-mode hatch); 200 WITH the admin bearer.
#   2. GET /workspaces/:id/activity (and /delegations, /approvals/pending) 401
#      with no bearer (was: 200 via the WorkspaceAuth hatch); 200 WITH bearer.
#   3. GET /org/templates returns the curated set populated by clone-manifest.sh
#      (unauth-readable bootstrap surface — unchanged).
#
# Requires: platform running on :8080 with MOLECULE_ENV=development AND
#           ADMIN_TOKEN set (the dev value), with MOLECULE_ADMIN_TOKEN (or
#           ADMIN_TOKEN) exported here so the suite can present the bearer.
#           scripts/dev-start.sh provisions ADMIN_TOKEN locally; the e2e-api CI
#           job sets it on the platform and exports the matching bearer.
#
# Usage:
#   MOLECULE_ADMIN_TOKEN=dev-local-admin-token bash tests/e2e/test_dev_mode.sh
set -euo pipefail

# shellcheck source=_lib.sh
source "$(dirname "$0")/_lib.sh"

PASS=0
FAIL=0

fail() {
  echo "FAIL: $1"
  FAIL=$((FAIL + 1))
}

pass() {
  echo "PASS: $1"
  PASS=$((PASS + 1))
}

check_http() {
  local desc="$1" expected="$2" actual="$3"
  if [ "$actual" = "$expected" ]; then
    pass "$desc (HTTP $actual)"
  else
    fail "$desc — expected HTTP $expected, got $actual"
  fi
}

echo "=== Dev-mode fail-CLOSED regression tests ==="
echo ""

# The platform is fail-closed in every environment now, so the suite MUST have
# the admin bearer to drive the authenticated (200) assertions. Without it we
# cannot create / clean up workspaces — bail loudly rather than silently skip.
ADMIN_BEARER="${MOLECULE_ADMIN_TOKEN:-${ADMIN_TOKEN:-}}"
if [ -z "$ADMIN_BEARER" ]; then
  echo "FAIL: MOLECULE_ADMIN_TOKEN/ADMIN_TOKEN not set — auth is fail-closed in"
  echo "      every environment, so this suite needs the dev ADMIN_TOKEN bearer."
  echo "      e.g. MOLECULE_ADMIN_TOKEN=dev-local-admin-token bash $0"
  exit 1
fi
ADMIN_AUTH=(-H "Authorization: Bearer $ADMIN_BEARER")

e2e_cleanup_all_workspaces

# ----------------------------------------------------------------------
# Section 1 — AdminAuth is fail-CLOSED (dev-mode hatch removed)
# ----------------------------------------------------------------------
echo "--- Section 1: AdminAuth fail-closed ---"

# No bearer → 401 in dev mode (the removed hatch used to return 200).
R=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/workspaces")
check_http "GET /workspaces (no bearer) is fail-CLOSED" "401" "$R"

# With the dev admin bearer → 200.
R=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/workspaces" "${ADMIN_AUTH[@]}")
check_http "GET /workspaces (with admin bearer)" "200" "$R"

# Create a workspace (authenticated) so tokens land in the DB.
R=$(curl -s -w "\n%{http_code}" -X POST "$BASE/workspaces" \
  "${ADMIN_AUTH[@]}" \
  -H "Content-Type: application/json" \
  -d '{"name":"Dev-Mode-Test","tier":1,"runtime":"external","external":true}')
CODE=$(echo "$R" | tail -n1)
BODY=$(echo "$R" | sed '$d')
check_http "POST /workspaces (create, with admin bearer)" "201" "$CODE"

WS_ID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || true)
if [ -z "$WS_ID" ]; then
  fail "Could not extract workspace ID from create response"
  echo "=== Results: $PASS passed, $FAIL failed ==="
  exit 1
fi

# Ensure a real workspace token exists so AdminAuth sees a live token globally.
TOKEN=$(echo "$BODY" | e2e_extract_token)
if [ -z "$TOKEN" ]; then
  e2e_mint_workspace_token "$WS_ID" >/dev/null
fi

# With tokens now in the DB, the bearer-less call STILL 401s (no lazy-bootstrap
# / dev-mode fall-through), and the authenticated call still 200s.
R=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/workspaces")
check_http "GET /workspaces (after token minted, no bearer) is fail-CLOSED" "401" "$R"

R=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/workspaces" "${ADMIN_AUTH[@]}")
check_http "GET /workspaces (after token minted, with admin bearer)" "200" "$R"

# ----------------------------------------------------------------------
# Section 2 — WorkspaceAuth is fail-CLOSED (dev-mode hatch removed)
# ----------------------------------------------------------------------
echo ""
echo "--- Section 2: WorkspaceAuth fail-closed ---"

# No bearer → 401 (the removed hatch used to return 200).
R=$(curl -s -o /dev/null -w "%{http_code}" \
  "$BASE/workspaces/$WS_ID/activity?type=a2a_receive&limit=50")
check_http "GET /workspaces/:id/activity (no bearer) is fail-CLOSED" "401" "$R"

R=$(curl -s -o /dev/null -w "%{http_code}" \
  "$BASE/workspaces/$WS_ID/delegations")
check_http "GET /workspaces/:id/delegations (no bearer) is fail-CLOSED" "401" "$R"

R=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/approvals/pending")
check_http "GET /approvals/pending (no bearer) is fail-CLOSED" "401" "$R"

# Same requests WITH the admin bearer → 200.
R=$(curl -s -o /dev/null -w "%{http_code}" \
  "$BASE/workspaces/$WS_ID/activity?type=a2a_receive&limit=50" "${ADMIN_AUTH[@]}")
check_http "GET /workspaces/:id/activity (with admin bearer)" "200" "$R"

R=$(curl -s -o /dev/null -w "%{http_code}" \
  "$BASE/workspaces/$WS_ID/delegations" "${ADMIN_AUTH[@]}")
check_http "GET /workspaces/:id/delegations (with admin bearer)" "200" "$R"

R=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/approvals/pending" "${ADMIN_AUTH[@]}")
check_http "GET /approvals/pending (with admin bearer)" "200" "$R"

# ----------------------------------------------------------------------
# Section 3 — Template registry populated by setup.sh
# ----------------------------------------------------------------------
# GET /org/templates is an unauthenticated bootstrap surface (the template
# palette must render before the user has a credential) — unchanged.
echo ""
echo "--- Section 3: Template registry ---"

R=$(curl -s "$BASE/org/templates")
COUNT=$(echo "$R" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "0")
if [ "$COUNT" -gt 0 ]; then
  pass "GET /org/templates returns $COUNT template(s)"
else
  fail "GET /org/templates returned empty list — is clone-manifest.sh run? (bash scripts/clone-manifest.sh manifest.json workspace-configs-templates/ org-templates/ plugins/)"
fi

# ----------------------------------------------------------------------
# Cleanup
# ----------------------------------------------------------------------
e2e_delete_workspace "$WS_ID" "Dev-Mode-Test"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
