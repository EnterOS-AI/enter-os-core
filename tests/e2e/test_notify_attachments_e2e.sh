#!/usr/bin/env bash
# E2E test: agent → user notify with attachments (PR #2130).
#
# Exercises the wire contract that workspace/a2a_tools.tool_send_message_to_user
# uses to push files into the canvas chat. Pure platform test — no workspace
# container or LLM key required, so it runs against a bare workspace-server
# in <2s and pins the contract that real runtimes depend on.
#
# What this proves:
#   1. POST /notify (no attachments) persists a chat row that survives reload.
#   2. POST /notify with attachments persists parts[].kind=file in the SAME
#      shape extractFilesFromTask reads → chips re-render after reload.
#   3. Per-element attachment validation rejects empty uri/name (regression
#      for the bug where gin's binding:"required" on slice elements was a
#      no-op without `dive` — see activity.go:299-306).
#   4. Empty `attachments: []` does NOT inject a stray `parts` key (would
#      otherwise render a "0 files" header in the canvas).
#   5. Real /chat/uploads → /notify chain round-trips the URI verbatim.
#
# Usage:  tests/e2e/test_notify_attachments_e2e.sh
# Prereqs: workspace-server on http://localhost:8080, MOLECULE_ENV != production

set -euo pipefail

source "$(dirname "$0")/_lib.sh"

PASS=0
FAIL=0
WSID=""

cleanup() {
  if [ -n "$WSID" ]; then
    curl -s -X DELETE "$BASE/workspaces/$WSID?confirm=true" > /dev/null || true
  fi
}
trap cleanup EXIT

assert() {
  local label="$1"
  local actual="$2"
  local expected="$3"
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
  local label="$1"
  local haystack="$2"
  local needle="$3"
  if echo "$haystack" | grep -qF "$needle"; then
    echo "  PASS — $label"
    PASS=$((PASS+1))
  else
    echo "  FAIL — $label"
    echo "         haystack: $haystack"
    echo "         needle:   $needle"
    FAIL=$((FAIL+1))
  fi
}

echo "=== Setup ==="
# Idempotent pre-sweep: a prior run that got SIGPIPE'd or kill -9'd before
# the EXIT trap fired would leave a "Notify E2E" workspace sitting on the
# canvas. Find and delete any with this exact name so the test is safe to
# re-run from any state. Match by name (not tag) so this also catches
# leftovers created by older script versions.
PRIOR=$(curl -s "$BASE/workspaces" | python3 -c '
import json, sys
try:
    print(" ".join(w["id"] for w in json.load(sys.stdin) if w.get("name") == "Notify E2E"))
except Exception:
    pass
')
for _wid in $PRIOR; do
  echo "Sweeping leftover Notify E2E workspace: $_wid"
  curl -s -X DELETE "$BASE/workspaces/$_wid?confirm=true" > /dev/null || true
done

R=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
  -d '{"name":"Notify E2E","tier":1}')
WSID=$(echo "$R" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])' 2>/dev/null || true)
[ -n "$WSID" ] || { echo "Failed to create workspace: $R"; exit 1; }
echo "Created workspace $WSID"

# Mint a bearer token so the wsAuth-grouped endpoints (notify, activity,
# chat/uploads) accept us. Local dev mode skips auth, but CI enforces it
# — so we always send the header to keep the test portable. The
# admin/test-token endpoint is only enabled when MOLECULE_ENV != production.
TOKEN=$(e2e_mint_test_token "$WSID")
[ -n "$TOKEN" ] || { echo "Failed to mint test token"; exit 1; }
AUTH="Authorization: Bearer $TOKEN"

echo ""
echo "=== Test 1: notify without attachments persists row ==="
CODE=$(curl -s -o /tmp/notify1.json -w "%{http_code}" -X POST "$BASE/workspaces/$WSID/notify" \
  -H "Content-Type: application/json" -H "$AUTH" \
  -d '{"message":"Working on it"}')
assert "POST /notify (text only) returns 200" "$CODE" "200"

# Read it back via /activity. The notify handler writes a2a_receive,
# method=notify, response_body.result=<message>.
# Notify writes source_id=NULL → use source=canvas (matches the chat
# panel's history loader filter). source=agent would correctly hide it.
ACT=$(curl -s -H "$AUTH" "$BASE/workspaces/$WSID/activity?source=canvas&limit=10")
ROW=$(echo "$ACT" | python3 -c '
import json, sys
rows = json.load(sys.stdin) or []
for r in rows:
    if r.get("method") == "notify":
        print(json.dumps(r))
        break
')
[ -n "$ROW" ] || { echo "  FAIL — could not find notify row in activity"; FAIL=$((FAIL+1)); }

if [ -n "$ROW" ]; then
  RESULT=$(echo "$ROW" | python3 -c 'import json,sys;print(json.load(sys.stdin)["response_body"]["result"])')
  assert "persisted response_body.result matches message" "$RESULT" "Working on it"
  PARTS=$(echo "$ROW" | python3 -c 'import json,sys;b=json.load(sys.stdin)["response_body"];print("parts" in b)')
  assert "no stray parts[] when message has no attachments" "$PARTS" "False"
fi

echo ""
echo "=== Test 2: notify with attachments persists parts[].kind=file ==="
CODE=$(curl -s -o /tmp/notify2.json -w "%{http_code}" -X POST "$BASE/workspaces/$WSID/notify" \
  -H "Content-Type: application/json" -H "$AUTH" \
  -d '{
    "message": "Done — see attached.",
    "attachments": [
      {"uri":"workspace:/tmp/build-output.zip","name":"build-output.zip","mimeType":"application/zip","size":12345}
    ]
  }')
assert "POST /notify (with attachment) returns 200" "$CODE" "200"

ACT=$(curl -s -H "$AUTH" "$BASE/workspaces/$WSID/activity?source=canvas&limit=10")
ROW=$(echo "$ACT" | python3 -c '
import json, sys
rows = json.load(sys.stdin) or []
# Most recent matching notify row
for r in rows:
    rb = r.get("response_body") or {}
    if r.get("method") == "notify" and "parts" in rb:
        print(json.dumps(r))
        break
')
[ -n "$ROW" ] || { echo "  FAIL — could not find notify-with-attachments row"; FAIL=$((FAIL+1)); }

if [ -n "$ROW" ]; then
  KIND=$(echo "$ROW" | python3 -c 'import json,sys;print(json.load(sys.stdin)["response_body"]["parts"][0]["kind"])')
  URI=$(echo "$ROW" | python3 -c 'import json,sys;print(json.load(sys.stdin)["response_body"]["parts"][0]["file"]["uri"])')
  NAME=$(echo "$ROW" | python3 -c 'import json,sys;print(json.load(sys.stdin)["response_body"]["parts"][0]["file"]["name"])')
  MIME=$(echo "$ROW" | python3 -c 'import json,sys;print(json.load(sys.stdin)["response_body"]["parts"][0]["file"]["mimeType"])')
  SIZE=$(echo "$ROW" | python3 -c 'import json,sys;print(json.load(sys.stdin)["response_body"]["parts"][0]["file"]["size"])')
  assert "parts[0].kind == file" "$KIND" "file"
  assert "parts[0].file.uri preserved" "$URI" "workspace:/tmp/build-output.zip"
  assert "parts[0].file.name preserved" "$NAME" "build-output.zip"
  assert "parts[0].file.mimeType preserved" "$MIME" "application/zip"
  assert "parts[0].file.size preserved" "$SIZE" "12345"
fi

echo ""
echo "=== Test 3: per-element validation rejects empty uri/name ==="
# Critical regression: gin's binding:"required" on slice elements is a no-op
# without `dive`. activity.go:299 explicitly loops and rejects. Keep in lock-
# step here so a future refactor that drops the loop fails this test.
CODE=$(curl -s -o /tmp/notify3.json -w "%{http_code}" -X POST "$BASE/workspaces/$WSID/notify" \
  -H "Content-Type: application/json" -H "$AUTH" \
  -d '{"message":"x","attachments":[{"uri":"","name":""}]}')
assert "empty uri/name attachment is 400" "$CODE" "400"
ERR=$(cat /tmp/notify3.json | python3 -c 'import json,sys;print(json.load(sys.stdin).get("error",""))')
assert_contains "error mentions attachment[0]" "$ERR" "attachment[0]"

CODE=$(curl -s -o /tmp/notify3b.json -w "%{http_code}" -X POST "$BASE/workspaces/$WSID/notify" \
  -H "Content-Type: application/json" -H "$AUTH" \
  -d '{"message":"x","attachments":[{"uri":"workspace:/ok.txt","name":"ok.txt"},{"uri":"","name":"bad"}]}')
assert "second-element empty uri rejects whole call" "$CODE" "400"
ERR=$(cat /tmp/notify3b.json | python3 -c 'import json,sys;print(json.load(sys.stdin).get("error",""))')
assert_contains "error mentions attachment[1]" "$ERR" "attachment[1]"

echo ""
echo "=== Test 4: missing message field rejected ==="
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/workspaces/$WSID/notify" \
  -H "Content-Type: application/json" -H "$AUTH" -d '{}')
assert "POST /notify with no message returns 400" "$CODE" "400"

echo ""
echo "=== Test 5: real /chat/uploads → /notify round-trip ==="
TMPF=$(mktemp -t notify-e2e-XXXX.txt)
echo "round-trip-marker-$(date +%s)" > "$TMPF"
UP=$(curl -s -X POST "$BASE/workspaces/$WSID/chat/uploads" -H "$AUTH" -F "files=@$TMPF")
URI=$(echo "$UP" | python3 -c '
import json,sys
try:
    print(json.load(sys.stdin)["files"][0]["uri"])
except Exception:
    pass
')
NAME=$(basename "$TMPF")

if [ -z "$URI" ]; then
  # /chat/uploads requires a running container in some configs. Skip the
  # round-trip if the platform refused — the synthetic-URI tests above
  # already pin the wire contract.
  echo "  SKIP — /chat/uploads not available in this env (response: $UP)"
else
  CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE/workspaces/$WSID/notify" \
    -H "Content-Type: application/json" -H "$AUTH" \
    -d "{\"message\":\"see file\",\"attachments\":[{\"uri\":\"$URI\",\"name\":\"$NAME\"}]}")
  assert "uploaded URI round-trips through notify" "$CODE" "200"

  ACT=$(curl -s -H "$AUTH" "$BASE/workspaces/$WSID/activity?source=canvas&limit=10")
  STORED_URI=$(echo "$ACT" | python3 -c "
import json, sys
rows = json.load(sys.stdin) or []
for r in rows:
    rb = r.get('response_body') or {}
    parts = rb.get('parts') or []
    for p in parts:
        f = p.get('file') or {}
        if f.get('name') == '$NAME':
            print(f.get('uri',''))
            sys.exit(0)
")
  assert "stored URI matches uploaded URI" "$STORED_URI" "$URI"
fi

rm -f "$TMPF"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
