#!/usr/bin/env bash
# Replay for the chat_history MCP tool — exercises the full SaaS-shape
# wire that PRs #2472 (peer_id filter), #2474 (chat_history client), and
# #2476 (before_ts paging) ride on. Runs against the prod-shape tenant
# image, not unit-mock'd handlers, so any drift between the Go handler
# and the Python tool's expectations surfaces here.
#
# What this catches that unit tests don't:
#   - Real Postgres planner behaviour on the (source_id = $X OR target_id = $X)
#     OR clause (issue #2478 — both indexes missing).
#   - cf-proxy header rewrites + TenantGuard middleware in the path.
#   - lib/pq + Postgres driver type binding for time.Time parameters.
#   - JSON encoding of created_at across the wire (timezone, precision).
#
# Phases:
#   A. Seed three a2a_receive rows for alpha with peer_id=beta, spread
#      across distinct timestamps.
#   B. Basic peer_id filter: GET ?type=a2a_receive&peer_id=beta&limit=10
#      → assert 3 rows DESC.
#   C. Limit cap: limit=2 → assert 2 newest rows.
#   D. before_ts paging: take the 2nd-newest's created_at, GET with
#      before_ts=that → assert the 1 strictly-older row.
#   E. OR clause (target side): seed an a2a_send row where source=alpha,
#      target=beta. GET with type unset, peer_id=beta → assert that row
#      surfaces too (target_id match, not just source_id).
#   F. Trust-boundary: peer_id="not-a-uuid" → 400 + "peer_id must be a UUID".
#   G. Trust-boundary: before_ts="garbage" → 400 + RFC3339 example.
#   H. URL-encoded SQL-injection-shape peer_id → 400 (matches activity_test.go's
#      malicious-peer-id panel).

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

assert_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if echo "$haystack" | grep -qF "$needle"; then
        printf "  PASS %s\n" "$desc"
        PASS=$((PASS + 1))
    else
        printf "  FAIL %s\n    expected to contain: %s\n    got: %s\n" "$desc" "$needle" "$haystack" >&2
        FAIL=$((FAIL + 1))
    fi
}

echo "[replay] alpha=$ALPHA_ID beta=$BETA_ID"

# ─── Phase A: seed the activity_logs table ─────────────────────────────
# Inserted via psql so the seed is independent of the platform's HTTP
# Notify path — that path itself ships through the same handler chain
# we want to test, and seeding through it would conflate setup and
# assertion.
echo ""
echo "[replay] A. seeding 3 a2a_receive rows for alpha←beta at distinct timestamps..."
psql_exec >/dev/null <<SQL
DELETE FROM activity_logs WHERE workspace_id = '$ALPHA_ID';
INSERT INTO activity_logs (workspace_id, activity_type, source_id, target_id, method, summary, created_at)
VALUES
  ('$ALPHA_ID', 'a2a_receive', '$BETA_ID', '$ALPHA_ID', 'message/send', 'oldest from beta',  NOW() - INTERVAL '4 hours'),
  ('$ALPHA_ID', 'a2a_receive', '$BETA_ID', '$ALPHA_ID', 'message/send', 'middle from beta',  NOW() - INTERVAL '2 hours'),
  ('$ALPHA_ID', 'a2a_receive', '$BETA_ID', '$ALPHA_ID', 'message/send', 'newest from beta',  NOW() - INTERVAL '1 hour');
SQL
echo "[replay]   inserted 3 rows"

# ─── Phase B: basic peer_id filter ─────────────────────────────────────
echo ""
echo "[replay] B. GET ?type=a2a_receive&peer_id=beta&limit=10 ..."
RESP=$(curl_admin "$BASE/workspaces/$ALPHA_ID/activity?type=a2a_receive&peer_id=$BETA_ID&limit=10")
COUNT=$(echo "$RESP" | jq 'length')
assert "B1: returns 3 rows" "3" "$COUNT"

# DESC order — newest first
NEWEST_SUMMARY=$(echo "$RESP" | jq -r '.[0].summary')
assert "B2: newest first (DESC ordering)" "newest from beta" "$NEWEST_SUMMARY"

OLDEST_SUMMARY=$(echo "$RESP" | jq -r '.[2].summary')
assert "B3: oldest last" "oldest from beta" "$OLDEST_SUMMARY"

# ─── Phase C: limit cap ────────────────────────────────────────────────
echo ""
echo "[replay] C. limit=2 (expecting 2 newest) ..."
RESP=$(curl_admin "$BASE/workspaces/$ALPHA_ID/activity?type=a2a_receive&peer_id=$BETA_ID&limit=2")
assert "C1: limit clamps to 2" "2" "$(echo "$RESP" | jq 'length')"
assert "C2: kept newest" "newest from beta" "$(echo "$RESP" | jq -r '.[0].summary')"
assert "C3: kept middle" "middle from beta" "$(echo "$RESP" | jq -r '.[1].summary')"

# ─── Phase D: before_ts paging ─────────────────────────────────────────
echo ""
echo "[replay] D. before_ts paging — walk backwards from middle row's created_at ..."
# Take the newest row's created_at, page from there.
NEWEST_TS=$(curl_admin "$BASE/workspaces/$ALPHA_ID/activity?type=a2a_receive&peer_id=$BETA_ID&limit=1" \
    | jq -r '.[0].created_at')
# RFC3339 with timezone — Go's time.Parse(RFC3339) handles `2026-...Z` AND
# `2026-...+00:00`. Postgres returns the latter; URL-encode the +.
NEWEST_TS_ENCODED=$(echo "$NEWEST_TS" | python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.stdin.read().strip(), safe=""))')
RESP=$(curl_admin "$BASE/workspaces/$ALPHA_ID/activity?type=a2a_receive&peer_id=$BETA_ID&before_ts=$NEWEST_TS_ENCODED&limit=10")
assert "D1: 2 rows older than newest" "2" "$(echo "$RESP" | jq 'length')"
assert "D2: middle is now newest in the slice" "middle from beta" "$(echo "$RESP" | jq -r '.[0].summary')"
# Strict less-than — the row at exactly NEWEST_TS must NOT come back.
NOT_INCLUDED=$(echo "$RESP" | jq -r '[.[].summary] | index("newest from beta") // "absent"')
assert "D3: strictly older — newest excluded" "absent" "$NOT_INCLUDED"

# ─── Phase E: OR clause covers target_id direction ─────────────────────
echo ""
echo "[replay] E. OR clause: seed an a2a_send row (alpha→beta) and confirm it surfaces ..."
psql_exec >/dev/null <<SQL
INSERT INTO activity_logs (workspace_id, activity_type, source_id, target_id, method, summary, created_at)
VALUES ('$ALPHA_ID', 'a2a_send', '$ALPHA_ID', '$BETA_ID', 'message/send', 'sent to beta', NOW());
SQL
# No type filter — we want both a2a_receive AND a2a_send rows back.
RESP=$(curl_admin "$BASE/workspaces/$ALPHA_ID/activity?peer_id=$BETA_ID&limit=10")
HAS_SENT=$(echo "$RESP" | jq '[.[].summary] | any(. == "sent to beta")')
assert "E1: a2a_send (alpha→beta) returned via target_id match" "true" "$HAS_SENT"
TOTAL=$(echo "$RESP" | jq 'length')
assert "E2: total = 4 (3 receives + 1 send)" "4" "$TOTAL"

# ─── Phase F: malformed peer_id → 400 ──────────────────────────────────
echo ""
echo "[replay] F. malformed peer_id → 400 ..."
HTTP_CODE=$(curl_admin -o /tmp/cha-bad-peer.json -w '%{http_code}' \
    "$BASE/workspaces/$ALPHA_ID/activity?type=a2a_receive&peer_id=not-a-uuid")
assert "F1: HTTP 400" "400" "$HTTP_CODE"
assert_contains "F2: error names the param" "peer_id must be a UUID" "$(cat /tmp/cha-bad-peer.json)"

# ─── Phase G: malformed before_ts → 400 ────────────────────────────────
echo ""
echo "[replay] G. malformed before_ts → 400 ..."
HTTP_CODE=$(curl_admin -o /tmp/cha-bad-ts.json -w '%{http_code}' \
    "$BASE/workspaces/$ALPHA_ID/activity?type=a2a_receive&before_ts=garbage")
assert "G1: HTTP 400" "400" "$HTTP_CODE"
assert_contains "G2: error mentions RFC3339" "RFC3339" "$(cat /tmp/cha-bad-ts.json)"

# ─── Phase H: SQL-injection-shape peer_id is rejected ──────────────────
echo ""
echo "[replay] H. URL-encoded SQLi-shape peer_id → 400 ..."
SQLI_ENCODED="%27%20OR%201%3D1%20--"  # ' OR 1=1 --
HTTP_CODE=$(curl_admin -o /tmp/cha-sqli.json -w '%{http_code}' \
    "$BASE/workspaces/$ALPHA_ID/activity?type=a2a_receive&peer_id=$SQLI_ENCODED")
assert "H1: HTTP 400 (UUID validation rejects before SQL builder sees it)" "400" "$HTTP_CODE"

# ─── Cleanup: tear down seeded rows so subsequent runs don't accumulate ─
psql_exec >/dev/null <<SQL
DELETE FROM activity_logs WHERE workspace_id = '$ALPHA_ID';
SQL

echo ""
if [ "$FAIL" -gt 0 ]; then
    echo "[replay] FAIL: $PASS pass, $FAIL fail"
    exit 1
fi
echo "[replay] PASS: $PASS/$PASS — chat_history wire (peer_id filter + before_ts paging + trust boundary + OR clause)"
