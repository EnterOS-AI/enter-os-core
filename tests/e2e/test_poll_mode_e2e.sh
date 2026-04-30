#!/usr/bin/env bash
# E2E for delivery_mode=poll + since_id cursor (#2339).
#
# Round-trip: register a workspace as poll-mode (no URL) → POST A2A to it →
# verify the proxy short-circuits to {status:"queued"} → verify the message
# appears in /activity → verify the since_id cursor returns ONLY new events
# in ASC order → verify a stale cursor returns 410.
#
# Requires: platform running on localhost:8080 with migrations applied.
#   bash workspace-server/scripts/dev-start.sh
#   bash workspace-server/scripts/run-migrations.sh
#
# Idempotent: each run uses fresh per-script workspace ids so reruns don't
# collide. Does NOT call e2e_cleanup_all_workspaces — see
# `feedback_never_run_cluster_cleanup_tests_on_live_platform.md`.

set -euo pipefail

source "$(dirname "$0")/_lib.sh"

PASS=0
FAIL=0
TIMEOUT="${A2A_TIMEOUT:-30}"

# Per-run unique ids — workspaces.id is a UUID column, so we generate
# real v4 UUIDs. A "ws-<tag>" string fails the pq UUID cast and surfaces
# as opaque "registration failed" (caught against this very test in CI
# before merge — the failure mode that motivates the helper).
gen_uuid() {
  if command -v uuidgen >/dev/null 2>&1; then
    uuidgen | tr '[:upper:]' '[:lower:]'
  else
    python3 -c 'import uuid; print(uuid.uuid4())'
  fi
}
POLL_WS_ID="$(gen_uuid)"
CALLER_WS_ID="$(gen_uuid)"
# Phase 2 uses a separate UUID for its invalid-mode probe so a rerun
# can't poison POLL_WS_ID's row with a bad upsert (the 400 path doesn't
# touch DB, but defense in depth).
INVALID_PROBE_ID="$(gen_uuid)"

cleanup() {
  local rc=$?
  # Best-effort delete; non-fatal if the row was never created.
  curl -s -X DELETE "$BASE/workspaces/$POLL_WS_ID" >/dev/null || true
  curl -s -X DELETE "$BASE/workspaces/$CALLER_WS_ID" >/dev/null || true
  exit $rc
}
trap cleanup EXIT

check() {
  local desc="$1"
  local expected="$2"
  local actual="$3"
  if echo "$actual" | grep -qF -- "$expected"; then
    echo "PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $desc"
    echo "  expected to contain: $expected"
    echo "  got: $(echo "$actual" | head -10)"
    FAIL=$((FAIL + 1))
  fi
}

check_eq() {
  local desc="$1"
  local expected="$2"
  local actual="$3"
  if [ "$actual" = "$expected" ]; then
    echo "PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $desc"
    echo "  expected: $expected"
    echo "  got:      $actual"
    FAIL=$((FAIL + 1))
  fi
}

check_not_contains() {
  local desc="$1"
  local unexpected="$2"
  local actual="$3"
  if echo "$actual" | grep -qF -- "$unexpected"; then
    echo "FAIL: $desc"
    echo "  should NOT contain: $unexpected"
    FAIL=$((FAIL + 1))
  else
    echo "PASS: $desc"
    PASS=$((PASS + 1))
  fi
}

echo "=== Poll-Mode + since_id Cursor E2E (#2339) ==="
echo "  base: $BASE"
echo "  poll workspace: $POLL_WS_ID"
echo "  caller workspace: $CALLER_WS_ID"
echo ""

# ---------- Phase 1: register as poll-mode ----------
echo "--- Phase 1: Register poll-mode workspace (no URL) ---"

# A poll-mode workspace registers WITHOUT a URL — that's the contract from
# PR 1 (#2348). The agent_card is required; everything else is optional.
REG_RESP=$(curl -s -X POST "$BASE/registry/register" \
  -H "Content-Type: application/json" \
  -d "{
    \"id\": \"$POLL_WS_ID\",
    \"delivery_mode\": \"poll\",
    \"agent_card\": {\"name\": \"poll-mode-test\"}
  }")

check "register accepts poll mode without URL" '"status":"registered"' "$REG_RESP"
check "register response echoes delivery_mode=poll"  '"delivery_mode":"poll"' "$REG_RESP"

# Capture the auth token for subsequent /activity reads (Phase 30.1).
POLL_TOKEN=$(echo "$REG_RESP" | e2e_extract_token || true)
if [ -z "$POLL_TOKEN" ]; then
  echo "WARN: no auth_token in register response — token-required reads will fail"
fi

# ---------- Phase 2: invalid mode rejected ----------
echo ""
echo "--- Phase 2: Invalid delivery_mode rejected ---"

INVALID_RESP=$(curl -s -w '\n%{http_code}' -X POST "$BASE/registry/register" \
  -H "Content-Type: application/json" \
  -d "{
    \"id\": \"$INVALID_PROBE_ID\",
    \"delivery_mode\": \"webhook\",
    \"agent_card\": {\"name\": \"bad\"}
  }")
INVALID_CODE=$(printf '%s' "$INVALID_RESP" | tail -n1)
INVALID_BODY=$(printf '%s' "$INVALID_RESP" | sed '$d')

check_eq "register rejects unknown delivery_mode (HTTP 400)" "400" "$INVALID_CODE"
check "error mentions delivery_mode" "delivery_mode" "$INVALID_BODY"

# ---------- Phase 3: A2A short-circuits to {status:"queued"} ----------
echo ""
echo "--- Phase 3: A2A to poll-mode workspace short-circuits ---"

A2A_RESP=$(curl -s --max-time "$TIMEOUT" -X POST "$BASE/workspaces/$POLL_WS_ID/a2a" \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "id": "msg-1",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "parts": [{"type": "text", "text": "hello-from-e2e-1"}]
      }
    }
  }')

check "poll-mode A2A returns queued status" '"status":"queued"' "$A2A_RESP"
check "queued response echoes delivery_mode=poll" '"delivery_mode":"poll"' "$A2A_RESP"
check "queued response echoes the JSON-RPC method" '"method":"message/send"' "$A2A_RESP"

# ---------- Phase 4: queued message appears in /activity ----------
echo ""
echo "--- Phase 4: Queued message visible via /activity ---"

# The activity_logs INSERT runs in a goroutine — give it a moment.
sleep 1

# Use bearer token if we got one; some platforms require it on /activity.
ACTIVITY_AUTH=()
[ -n "${POLL_TOKEN:-}" ] && ACTIVITY_AUTH=(-H "Authorization: Bearer $POLL_TOKEN")

ACT_RESP=$(curl -s --max-time "$TIMEOUT" "${ACTIVITY_AUTH[@]}" \
  "$BASE/workspaces/$POLL_WS_ID/activity?type=a2a_receive&limit=10")

check "activity feed has the queued message text" "hello-from-e2e-1" "$ACT_RESP"
check "activity_type is a2a_receive"             '"activity_type":"a2a_receive"' "$ACT_RESP"
check "method preserved on the activity row"     '"method":"message/send"' "$ACT_RESP"

# Pull the most-recent activity_id for use as a cursor.
FIRST_ACTIVITY_ID=$(echo "$ACT_RESP" | python3 -c "
import json, sys
rows = json.load(sys.stdin)
if not rows:
    print('')
else:
    # Default ordering is DESC (newest-first) when no since_id is set.
    print(rows[0]['id'])
")

if [ -z "$FIRST_ACTIVITY_ID" ]; then
  echo "FAIL: could not extract activity_id from /activity response"
  FAIL=$((FAIL + 1))
  exit 1
fi
echo "  cursor candidate: $FIRST_ACTIVITY_ID"

# ---------- Phase 5: since_id returns only events strictly after ----------
echo ""
echo "--- Phase 5: since_id cursor returns ASC, strictly-after ---"

# Send a SECOND A2A message; it must appear in the cursor-filtered feed,
# the FIRST message must NOT (cursor is strictly-after).
A2A_RESP2=$(curl -s --max-time "$TIMEOUT" -X POST "$BASE/workspaces/$POLL_WS_ID/a2a" \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "id": "msg-2",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "parts": [{"type": "text", "text": "hello-from-e2e-2"}]
      }
    }
  }')
check "second A2A also queues" '"status":"queued"' "$A2A_RESP2"

sleep 1

CURSOR_RESP=$(curl -s --max-time "$TIMEOUT" "${ACTIVITY_AUTH[@]}" \
  "$BASE/workspaces/$POLL_WS_ID/activity?type=a2a_receive&since_id=$FIRST_ACTIVITY_ID&limit=10")

check              "since_id feed includes the new message"          "hello-from-e2e-2" "$CURSOR_RESP"
check_not_contains "since_id feed excludes the cursor row itself"  "hello-from-e2e-1" "$CURSOR_RESP"

# Verify ASC ordering: in a fresh cursor window with two new events the
# array's first element must be the OLDER one (the test only sends one
# event after the cursor, so this case is trivially "exactly one row";
# the next sub-phase strengthens this with a second event).
A2A_RESP3=$(curl -s --max-time "$TIMEOUT" -X POST "$BASE/workspaces/$POLL_WS_ID/a2a" \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "id": "msg-3",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "parts": [{"type": "text", "text": "hello-from-e2e-3"}]
      }
    }
  }')
check "third A2A queues" '"status":"queued"' "$A2A_RESP3"

sleep 1

ASC_RESP=$(curl -s --max-time "$TIMEOUT" "${ACTIVITY_AUTH[@]}" \
  "$BASE/workspaces/$POLL_WS_ID/activity?type=a2a_receive&since_id=$FIRST_ACTIVITY_ID&limit=10")

# rows[0] should be msg-2 (older), rows[-1] should be msg-3 (newer) — that's
# ASC. If the server still defaulted to DESC, rows[0] would be msg-3.
ASC_FIRST=$(echo "$ASC_RESP" | python3 -c "
import json, sys
rows = json.load(sys.stdin)
def text_of(r):
    body = r.get('request_body') or {}
    parts = (body.get('params') or {}).get('message', {}).get('parts') or []
    return ''.join(p.get('text','') for p in parts if p.get('type')=='text')
if len(rows) < 2:
    print('NEED2_GOT_'+str(len(rows)))
else:
    print(text_of(rows[0]) + '|' + text_of(rows[-1]))
")
check_eq "since_id feed orders ASC (oldest-new first, newest-new last)" \
  "hello-from-e2e-2|hello-from-e2e-3" "$ASC_FIRST"

# ---------- Phase 6: stale cursor returns 410 ----------
echo ""
echo "--- Phase 6: Stale / unknown cursor returns 410 ---"

GONE_RESP=$(curl -s -w '\n%{http_code}' --max-time "$TIMEOUT" "${ACTIVITY_AUTH[@]}" \
  "$BASE/workspaces/$POLL_WS_ID/activity?since_id=00000000-0000-0000-0000-000000000000")
GONE_CODE=$(printf '%s' "$GONE_RESP" | tail -n1)
GONE_BODY=$(printf '%s' "$GONE_RESP" | sed '$d')

check_eq "unknown since_id returns HTTP 410 Gone" "410" "$GONE_CODE"
check "410 body explains how to recover" "since_id" "$GONE_BODY"

# ---------- Phase 7: cross-workspace cursor isolation ----------
echo ""
echo "--- Phase 7: Cross-workspace cursor isolation ---"

# Register a SECOND poll-mode workspace and try to read its activity
# feed using a cursor from the FIRST workspace. Must 410 — the cursor
# is workspace-scoped to prevent UUID-guessing peeks.
REG2=$(curl -s -X POST "$BASE/registry/register" \
  -H "Content-Type: application/json" \
  -d "{
    \"id\": \"$CALLER_WS_ID\",
    \"delivery_mode\": \"poll\",
    \"agent_card\": {\"name\": \"poll-cross-test\"}
  }")
check "second poll-mode workspace registers" '"status":"registered"' "$REG2"
CALLER_TOKEN=$(echo "$REG2" | e2e_extract_token || true)
CROSS_AUTH=()
[ -n "${CALLER_TOKEN:-}" ] && CROSS_AUTH=(-H "Authorization: Bearer $CALLER_TOKEN")

CROSS_RESP=$(curl -s -w '\n%{http_code}' --max-time "$TIMEOUT" "${CROSS_AUTH[@]}" \
  "$BASE/workspaces/$CALLER_WS_ID/activity?since_id=$FIRST_ACTIVITY_ID")
CROSS_CODE=$(printf '%s' "$CROSS_RESP" | tail -n1)
check_eq "cross-workspace cursor blocked with 410 (no info leak)" "410" "$CROSS_CODE"

# ---------- Results ----------
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
