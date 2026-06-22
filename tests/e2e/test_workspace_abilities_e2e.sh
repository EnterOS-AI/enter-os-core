#!/usr/bin/env bash
# E2E test: workspace broadcast and talk-to-user platform abilities.
#
# What this proves:
#   1. talk_to_user_enabled (default true) — POST /notify works out-of-the-box.
#   2. PATCH /workspaces/:id/abilities { talk_to_user_enabled: false } disables
#      delivery: /notify → 403 with error="talk_to_user_disabled" + delegate hint.
#   3. Re-enabling talk_to_user_enabled restores delivery.
#   4. broadcast_enabled (default false) — POST /broadcast → 403 when disabled.
#   5. PATCH { broadcast_enabled: true } enables fan-out.
#   6. POST /broadcast delivers to all non-sender, non-removed workspaces:
#      - Returns {"status":"sent","delivered":N}
#      - Receiver's activity log has a broadcast_receive entry with the message.
#      - Sender's activity log has a broadcast_sent entry.
#   7. The sender itself does NOT receive a broadcast_receive entry.
#
# Usage:  tests/e2e/test_workspace_abilities_e2e.sh
# Prereqs: workspace-server on http://localhost:8080, MOLECULE_ENV != production

set -euo pipefail

source "$(dirname "$0")/_lib.sh"

PASS=0
FAIL=0
SENDER_ID=""
RECEIVER_ID=""
SENDER_TOKEN=""
RECEIVER_TOKEN=""

cleanup() {
  for wid in "$SENDER_ID" "$RECEIVER_ID"; do
    if [ -n "$wid" ]; then
      if [ "$wid" = "$SENDER_ID" ]; then
        e2e_delete_workspace "$wid" "Abilities Sender"
      else
        e2e_delete_workspace "$wid" "Abilities Receiver"
      fi
    fi
  done
}
trap cleanup EXIT INT TERM

assert() {
  local label="$1" actual="$2" expected="$3"
  if [ "$actual" = "$expected" ]; then
    echo "  PASS — $label"
    PASS=$((PASS+1))
  else
    echo "  FAIL — $label"
    echo "         expected: $expected"
    echo "         actual:   $actual"
    FAIL=$((FAIL+1))
  fi
}

assert_contains() {
  local label="$1" haystack="$2" needle="$3"
  if echo "$haystack" | grep -qF "$needle"; then
    echo "  PASS — $label"
    PASS=$((PASS+1))
  else
    echo "  FAIL — $label"
    echo "         needle:   $needle"
    echo "         haystack: $haystack"
    FAIL=$((FAIL+1))
  fi
}

assert_not_contains() {
  local label="$1" haystack="$2" needle="$3"
  if ! echo "$haystack" | grep -qF "$needle"; then
    echo "  PASS — $label"
    PASS=$((PASS+1))
  else
    echo "  FAIL — $label (unexpected match)"
    echo "         needle:   $needle"
    echo "         haystack: $haystack"
    FAIL=$((FAIL+1))
  fi
}

# ── Pre-sweep: remove any stale leftover workspaces from a prior aborted run ──
echo "=== Setup ==="
for NAME in "Abilities Sender" "Abilities Receiver"; do
  PRIOR=$(curl -s "$BASE/workspaces" | python3 -c "
import json, sys
try:
    print(' '.join(w['id'] for w in json.load(sys.stdin) if w.get('name') == '$NAME'))
except Exception:
    pass
")
  for _wid in $PRIOR; do
    echo "Sweeping leftover '$NAME' workspace: $_wid"
    e2e_delete_workspace "$_wid" "$NAME"
  done
done

R=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
  -d '{"name":"Abilities Sender","tier":1,"runtime":"external","external":true}')
SENDER_ID=$(echo "$R" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])' 2>/dev/null || true)
[ -n "$SENDER_ID" ] || { echo "Failed to create sender workspace: $R"; exit 1; }
SENDER_TOKEN=$(echo "$R" | e2e_extract_token)
echo "Created sender   workspace: $SENDER_ID"

# Admin token — any live workspace bearer satisfies AdminAuth in local dev.
# In production-like envs, set MOLECULE_ADMIN_TOKEN.
if [ -z "$SENDER_TOKEN" ]; then
  SENDER_TOKEN=$(e2e_mint_workspace_token "$SENDER_ID")
fi
[ -n "$SENDER_TOKEN" ] || { echo "Failed to mint sender token"; exit 1; }
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:-$SENDER_TOKEN}"
ADMIN_AUTH="Authorization: Bearer $ADMIN_TOKEN"

R=$(curl -s -X POST "$BASE/workspaces" -H "$ADMIN_AUTH" -H "Content-Type: application/json" \
  -d '{"name":"Abilities Receiver","tier":1,"runtime":"external","external":true}')
RECEIVER_ID=$(echo "$R" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])' 2>/dev/null || true)
[ -n "$RECEIVER_ID" ] || { echo "Failed to create receiver workspace: $R"; exit 1; }
RECEIVER_TOKEN=$(echo "$R" | e2e_extract_token)
echo "Created receiver workspace: $RECEIVER_ID"

SENDER_AUTH="Authorization: Bearer $SENDER_TOKEN"

# ─────────────────────────────────────────────────────────────────────────────
echo ""
echo "=== Part 1: talk_to_user ability ==="

echo ""
echo "--- 1a: /notify works with default talk_to_user_enabled=true ---"
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/workspaces/$SENDER_ID/notify" \
  -H "Content-Type: application/json" -H "$SENDER_AUTH" \
  -d '{"message":"Hello from sender"}')
assert "POST /notify returns 200 when talk_to_user_enabled=true (default)" "$CODE" "200"

echo ""
echo "--- 1b: Disable talk_to_user ---"
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$BASE/workspaces/$SENDER_ID/abilities" \
  -H "Content-Type: application/json" -H "$ADMIN_AUTH" \
  -d '{"talk_to_user_enabled": false}')
assert "PATCH /abilities talk_to_user_enabled=false returns 200" "$CODE" "200"

# Verify the flag is reflected in the workspace GET response.
WS=$(curl -s "$BASE/workspaces/$SENDER_ID" -H "$SENDER_AUTH")
FLAG=$(echo "$WS" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("talk_to_user_enabled","MISSING"))')
assert "GET /workspaces/:id reflects talk_to_user_enabled=false" "$FLAG" "False"

echo ""
echo "--- 1c: /notify blocked when talk_to_user disabled ---"
BODY=$(curl -s -w "" -X POST "$BASE/workspaces/$SENDER_ID/notify" \
  -H "Content-Type: application/json" -H "$SENDER_AUTH" \
  -d '{"message":"Should be blocked"}')
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/workspaces/$SENDER_ID/notify" \
  -H "Content-Type: application/json" -H "$SENDER_AUTH" \
  -d '{"message":"Should be blocked"}')
assert "POST /notify returns 403 when talk_to_user_enabled=false" "$CODE" "403"

ERR=$(echo "$BODY" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("error",""))' 2>/dev/null || echo "")
assert_contains "403 body contains talk_to_user_disabled error code" "$ERR" "talk_to_user_disabled"

HINT=$(echo "$BODY" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("hint",""))' 2>/dev/null || echo "")
assert_contains "403 body contains delegate_task hint" "$HINT" "delegate_task"

echo ""
echo "--- 1d: Re-enable talk_to_user and verify /notify works again ---"
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$BASE/workspaces/$SENDER_ID/abilities" \
  -H "Content-Type: application/json" -H "$ADMIN_AUTH" \
  -d '{"talk_to_user_enabled": true}')
assert "PATCH /abilities talk_to_user_enabled=true returns 200" "$CODE" "200"

CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/workspaces/$SENDER_ID/notify" \
  -H "Content-Type: application/json" -H "$SENDER_AUTH" \
  -d '{"message":"Re-enabled, should work"}')
assert "POST /notify returns 200 after re-enabling talk_to_user" "$CODE" "200"

# ─────────────────────────────────────────────────────────────────────────────
echo ""
echo "=== Part 2: broadcast ability ==="

echo ""
echo "--- 2a: Broadcast blocked by default (broadcast_enabled=false) ---"
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/workspaces/$SENDER_ID/broadcast" \
  -H "Content-Type: application/json" -H "$SENDER_AUTH" \
  -d '{"message":"Should be blocked"}')
assert "POST /broadcast returns 403 when broadcast_enabled=false (default)" "$CODE" "403"

echo ""
echo "--- 2b: Enable broadcast ---"
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$BASE/workspaces/$SENDER_ID/abilities" \
  -H "Content-Type: application/json" -H "$ADMIN_AUTH" \
  -d '{"broadcast_enabled": true}')
assert "PATCH /abilities broadcast_enabled=true returns 200" "$CODE" "200"

WS=$(curl -s "$BASE/workspaces/$SENDER_ID" -H "$SENDER_AUTH")
FLAG=$(echo "$WS" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("broadcast_enabled","MISSING"))')
assert "GET /workspaces/:id reflects broadcast_enabled=true" "$FLAG" "True"

echo ""
echo "--- 2c: Successful broadcast fan-out ---"
BCAST=$(curl -s -X POST "$BASE/workspaces/$SENDER_ID/broadcast" \
  -H "Content-Type: application/json" -H "$SENDER_AUTH" \
  -d '{"message":"Org-wide notice: scheduled maintenance in 5 minutes."}')
BSTATUS=$(echo "$BCAST" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("status",""))' 2>/dev/null || echo "")
BDELIVERED=$(echo "$BCAST" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("delivered","-1"))' 2>/dev/null || echo "-1")
assert "POST /broadcast returns status=sent" "$BSTATUS" "sent"

# delivered count must be >= 1 (the receiver workspace).
echo "  INFO — broadcast delivered=$BDELIVERED"
if python3 -c "import sys; sys.exit(0 if int('$BDELIVERED') >= 1 else 1)" 2>/dev/null; then
  echo "  PASS — delivered count >= 1"
  PASS=$((PASS+1))
else
  echo "  FAIL — expected delivered >= 1, got $BDELIVERED"
  FAIL=$((FAIL+1))
fi

echo ""
echo "--- 2d: Receiver activity log has broadcast_receive entry ---"
if [ -z "$RECEIVER_TOKEN" ]; then
  RECEIVER_TOKEN=$(e2e_mint_workspace_token "$RECEIVER_ID")
fi
[ -n "$RECEIVER_TOKEN" ] || { echo "Failed to mint receiver token"; exit 1; }
RECEIVER_AUTH="Authorization: Bearer $RECEIVER_TOKEN"

ACT=$(curl -s -H "$RECEIVER_AUTH" "$BASE/workspaces/$RECEIVER_ID/activity?source=agent&limit=20")
ROW=$(echo "$ACT" | python3 -c '
import json, sys
rows = json.load(sys.stdin) or []
for r in rows:
    if r.get("activity_type") == "broadcast_receive":
        print(json.dumps(r))
        break
')
[ -n "$ROW" ] || {
  echo "  FAIL — could not find broadcast_receive row in receiver activity"
  FAIL=$((FAIL+1))
}

if [ -n "$ROW" ]; then
  # Message is stored in summary field.
  MSG=$(echo "$ROW" | python3 -c 'import json,sys;r=json.load(sys.stdin);print(r.get("summary",""))')
  assert_contains "broadcast_receive row summary has original message" "$MSG" "scheduled maintenance"
  # Sender ID is stored in source_id field.
  SRC=$(echo "$ROW" | python3 -c 'import json,sys;r=json.load(sys.stdin);print(r.get("source_id",""))')
  assert "broadcast_receive row source_id is sender workspace" "$SRC" "$SENDER_ID"
fi

echo ""
echo "--- 2e: Sender activity log has broadcast_sent entry ---"
ACT_SENDER=$(curl -s -H "$SENDER_AUTH" "$BASE/workspaces/$SENDER_ID/activity?limit=20")
SENT_ROW=$(echo "$ACT_SENDER" | python3 -c '
import json, sys
rows = json.load(sys.stdin) or []
for r in rows:
    if r.get("activity_type") == "broadcast_sent":
        print(json.dumps(r))
        break
')
[ -n "$SENT_ROW" ] || {
  echo "  FAIL — could not find broadcast_sent row in sender activity"
  FAIL=$((FAIL+1))
}

if [ -n "$SENT_ROW" ]; then
  # Delivered count is baked into the summary field (no response_body for sender row).
  SUMMARY=$(echo "$SENT_ROW" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("summary",""))')
  assert_contains "broadcast_sent summary mentions workspace count" "$SUMMARY" "workspace"
fi

echo ""
echo "--- 2f: Sender does NOT receive a broadcast_receive entry ---"
SELF_RECV=$(echo "$ACT_SENDER" | python3 -c '
import json, sys
rows = json.load(sys.stdin) or []
for r in rows:
    if r.get("activity_type") == "broadcast_receive":
        print("found")
        break
')
assert_not_contains "sender has no broadcast_receive in own activity log" "${SELF_RECV:-}" "found"

# ─────────────────────────────────────────────────────────────────────────────
echo ""
echo "--- 2g: Empty message is rejected ---"
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/workspaces/$SENDER_ID/broadcast" \
  -H "Content-Type: application/json" -H "$SENDER_AUTH" \
  -d '{"message":""}')
assert "POST /broadcast with empty message returns 400" "$CODE" "400"

echo ""
echo "--- 2h: Partial PATCH does not clobber other flags ---"
# Set talk_to_user=false, then patch only broadcast — talk_to_user must stay false.
curl -s -o /dev/null -X PATCH "$BASE/workspaces/$SENDER_ID/abilities" \
  -H "Content-Type: application/json" -H "$ADMIN_AUTH" \
  -d '{"talk_to_user_enabled": false}'
curl -s -o /dev/null -X PATCH "$BASE/workspaces/$SENDER_ID/abilities" \
  -H "Content-Type: application/json" -H "$ADMIN_AUTH" \
  -d '{"broadcast_enabled": false}'
WS=$(curl -s "$BASE/workspaces/$SENDER_ID" -H "$SENDER_AUTH")
TUF=$(echo "$WS" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("talk_to_user_enabled","MISSING"))')
BEF=$(echo "$WS" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("broadcast_enabled","MISSING"))')
assert "partial PATCH preserves talk_to_user_enabled=false" "$TUF" "False"
assert "partial PATCH sets broadcast_enabled=false" "$BEF" "False"

# ─────────────────────────────────────────────────────────────────────────────
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
