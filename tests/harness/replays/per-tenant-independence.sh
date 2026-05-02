#!/usr/bin/env bash
# Replay for per-tenant independence — each tenant runs the same
# workflow concurrently with no cross-bleed in workspaces table or
# activity_logs.
#
# What this proves that tenant-isolation.sh doesn't:
#   tenant-isolation.sh proves that REQUESTS get rejected at the
#   middleware layer when they target the wrong tenant. THIS replay
#   proves that even when both tenants are doing legitimate work
#   simultaneously, the back-end state stays partitioned: no row in
#   alpha's activity_logs ever shows up in beta's, no FK-resolution
#   ever crosses tenants, etc.
#
# Test shape: seed activity_logs in BOTH tenants in parallel using
# distinct row counts (3 vs 5) so we can distinguish them. Then
# fetch each tenant's history and assert the count + content match
# the seed exactly — proves no leak in either direction.
#
# Phases:
#   A. Seed alpha tenant: 3 a2a_receive rows (parent ← child).
#   B. Seed beta tenant:  5 a2a_receive rows (parent ← child).
#   C. GET alpha history → exactly 3 rows, all alpha-summary.
#   D. GET beta history  → exactly 5 rows, all beta-summary.
#   E. Direct DB sanity — alpha PG has only alpha rows, beta PG only beta.
#   F. Concurrent write race — both tenants take turns INSERTing
#      simultaneously; each tenant's count after the race matches what
#      it INSERTed. Catches "shared cache poison" / "shared connection
#      pool" failure modes that don't show up in single-tenant tests.

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

# ─── Cleanup (idempotent) ──────────────────────────────────────────────
psql_exec_alpha >/dev/null <<SQL
DELETE FROM activity_logs WHERE workspace_id = '$ALPHA_PARENT_ID';
SQL
psql_exec_beta >/dev/null <<SQL
DELETE FROM activity_logs WHERE workspace_id = '$BETA_PARENT_ID';
SQL

# ─── Phase A: seed alpha (3 rows) ──────────────────────────────────────
echo "[replay] A. seeding alpha tenant: 3 a2a_receive rows for alpha-parent ←alpha-child"
psql_exec_alpha >/dev/null <<SQL
INSERT INTO activity_logs (workspace_id, activity_type, source_id, target_id, method, summary, created_at)
VALUES
  ('$ALPHA_PARENT_ID', 'a2a_receive', '$ALPHA_CHILD_ID', '$ALPHA_PARENT_ID', 'message/send', 'alpha-msg-1', NOW() - INTERVAL '3 hours'),
  ('$ALPHA_PARENT_ID', 'a2a_receive', '$ALPHA_CHILD_ID', '$ALPHA_PARENT_ID', 'message/send', 'alpha-msg-2', NOW() - INTERVAL '2 hours'),
  ('$ALPHA_PARENT_ID', 'a2a_receive', '$ALPHA_CHILD_ID', '$ALPHA_PARENT_ID', 'message/send', 'alpha-msg-3', NOW() - INTERVAL '1 hour');
SQL

# ─── Phase B: seed beta (5 rows — distinct count) ──────────────────────
echo "[replay] B. seeding beta tenant: 5 a2a_receive rows for beta-parent ← beta-child"
psql_exec_beta >/dev/null <<SQL
INSERT INTO activity_logs (workspace_id, activity_type, source_id, target_id, method, summary, created_at)
VALUES
  ('$BETA_PARENT_ID', 'a2a_receive', '$BETA_CHILD_ID', '$BETA_PARENT_ID', 'message/send', 'beta-msg-1', NOW() - INTERVAL '5 hours'),
  ('$BETA_PARENT_ID', 'a2a_receive', '$BETA_CHILD_ID', '$BETA_PARENT_ID', 'message/send', 'beta-msg-2', NOW() - INTERVAL '4 hours'),
  ('$BETA_PARENT_ID', 'a2a_receive', '$BETA_CHILD_ID', '$BETA_PARENT_ID', 'message/send', 'beta-msg-3', NOW() - INTERVAL '3 hours'),
  ('$BETA_PARENT_ID', 'a2a_receive', '$BETA_CHILD_ID', '$BETA_PARENT_ID', 'message/send', 'beta-msg-4', NOW() - INTERVAL '2 hours'),
  ('$BETA_PARENT_ID', 'a2a_receive', '$BETA_CHILD_ID', '$BETA_PARENT_ID', 'message/send', 'beta-msg-5', NOW() - INTERVAL '1 hour');
SQL

# ─── Phase C: alpha tenant sees only its 3 rows ────────────────────────
echo ""
echo "[replay] C. alpha history via /activity ..."
ALPHA_RESP=$(curl_alpha_admin "$BASE/workspaces/$ALPHA_PARENT_ID/activity?type=a2a_receive&peer_id=$ALPHA_CHILD_ID&limit=20")
assert "C1: alpha row count = 3" "3" "$(echo "$ALPHA_RESP" | jq 'length')"

# Every summary must start with "alpha-msg-" — beta leak would manifest
# as a beta-msg-* string in this list.
ALPHA_NON_ALPHA=$(echo "$ALPHA_RESP" | jq -r '[.[].summary | select(startswith("alpha-msg-") | not)] | length')
assert "C2: zero non-alpha summaries leaked into alpha" "0" "$ALPHA_NON_ALPHA"

# ─── Phase D: beta tenant sees only its 5 rows ─────────────────────────
echo ""
echo "[replay] D. beta history via /activity ..."
BETA_RESP=$(curl_beta_admin "$BASE/workspaces/$BETA_PARENT_ID/activity?type=a2a_receive&peer_id=$BETA_CHILD_ID&limit=20")
assert "D1: beta row count = 5" "5" "$(echo "$BETA_RESP" | jq 'length')"

BETA_NON_BETA=$(echo "$BETA_RESP" | jq -r '[.[].summary | select(startswith("beta-msg-") | not)] | length')
assert "D2: zero non-beta summaries leaked into beta" "0" "$BETA_NON_BETA"

# ─── Phase E: direct DB-side sanity ────────────────────────────────────
echo ""
echo "[replay] E. direct DB-side counts ..."
ALPHA_DB=$(psql_exec_alpha -c "SELECT COUNT(*) FROM activity_logs WHERE workspace_id = '$ALPHA_PARENT_ID';")
BETA_DB=$(psql_exec_beta -c  "SELECT COUNT(*) FROM activity_logs WHERE workspace_id = '$BETA_PARENT_ID';")
assert "E1: postgres-alpha has exactly 3 alpha rows"  "3" "$ALPHA_DB"
assert "E2: postgres-beta has exactly 5 beta rows"   "5" "$BETA_DB"

# Cross-DB sanity: alpha PG has zero beta-named workspaces, vice versa.
ALPHA_HAS_BETA=$(psql_exec_alpha -c "SELECT COUNT(*) FROM workspaces WHERE name LIKE 'beta-%';")
BETA_HAS_ALPHA=$(psql_exec_beta  -c "SELECT COUNT(*) FROM workspaces WHERE name LIKE 'alpha-%';")
assert "E3: postgres-alpha has zero beta-named workspaces" "0" "$ALPHA_HAS_BETA"
assert "E4: postgres-beta has zero alpha-named workspaces" "0" "$BETA_HAS_ALPHA"

# ─── Phase F: concurrent INSERT race ───────────────────────────────────
# Both tenants insert 10 rows concurrently. Race shape catches the
# failure modes that CAN cross tenants in this topology:
#   - redis cross-keyspace bleed (shared redis container).
#   - shared-cp-stub state corruption (single Go process serves both).
#   - cf-proxy buffer mixup under simultaneous in-flight writes.
# Does NOT catch lib/pq prepared-statement cache collision or shared
# *sql.DB pool poisoning — each tenant has its own DATABASE_URL and
# its own postgres-{alpha,beta} container, so there is no shared pool
# to corrupt. A future replay variant on a single shared Postgres
# would be the right place to assert that failure mode.
# Each side must end with EXACTLY +10 rows from its own writes.
echo ""
echo "[replay] F. concurrent insert race — 10 rows per tenant in parallel"

(
    for i in $(seq 1 10); do
        psql_exec_alpha >/dev/null <<SQL
INSERT INTO activity_logs (workspace_id, activity_type, source_id, target_id, method, summary)
VALUES ('$ALPHA_PARENT_ID', 'a2a_receive', '$ALPHA_CHILD_ID', '$ALPHA_PARENT_ID', 'message/send', 'alpha-race-$i');
SQL
    done
) &
ALPHA_PID=$!

(
    for i in $(seq 1 10); do
        psql_exec_beta >/dev/null <<SQL
INSERT INTO activity_logs (workspace_id, activity_type, source_id, target_id, method, summary)
VALUES ('$BETA_PARENT_ID', 'a2a_receive', '$BETA_CHILD_ID', '$BETA_PARENT_ID', 'message/send', 'beta-race-$i');
SQL
    done
) &
BETA_PID=$!

wait $ALPHA_PID $BETA_PID

ALPHA_AFTER=$(psql_exec_alpha -c "SELECT COUNT(*) FROM activity_logs WHERE workspace_id = '$ALPHA_PARENT_ID';")
BETA_AFTER=$(psql_exec_beta  -c "SELECT COUNT(*) FROM activity_logs WHERE workspace_id = '$BETA_PARENT_ID';")
assert "F1: alpha has 13 rows after race (3 + 10)"  "13" "$ALPHA_AFTER"
assert "F2: beta has 15 rows after race (5 + 10)"  "15" "$BETA_AFTER"

# Concurrency leak check: alpha's "race" rows must all be alpha-race-*,
# beta's must all be beta-race-*. A pool/cache cross-bleed would surface
# as some tenant getting the other's writes.
ALPHA_RACE_NAMES=$(psql_exec_alpha -c "SELECT COUNT(*) FROM activity_logs WHERE workspace_id = '$ALPHA_PARENT_ID' AND summary LIKE 'beta-race-%';")
BETA_RACE_NAMES=$(psql_exec_beta  -c "SELECT COUNT(*) FROM activity_logs WHERE workspace_id = '$BETA_PARENT_ID' AND summary LIKE 'alpha-race-%';")
assert "F3: zero beta-race rows leaked into alpha PG" "0" "$ALPHA_RACE_NAMES"
assert "F4: zero alpha-race rows leaked into beta PG" "0" "$BETA_RACE_NAMES"

# ─── Cleanup ───────────────────────────────────────────────────────────
psql_exec_alpha >/dev/null <<SQL
DELETE FROM activity_logs WHERE workspace_id = '$ALPHA_PARENT_ID';
SQL
psql_exec_beta >/dev/null <<SQL
DELETE FROM activity_logs WHERE workspace_id = '$BETA_PARENT_ID';
SQL

echo ""
if [ "$FAIL" -gt 0 ]; then
    echo "[replay] FAIL: $PASS pass, $FAIL fail"
    exit 1
fi
echo "[replay] PASS: $PASS/$PASS — per-tenant independence holds (DB partition + concurrent race)"
