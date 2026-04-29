#!/usr/bin/env bash
# E2E for the v2 chat upload path (RFC #2312):
#
#   POST /workspaces/:id/chat/uploads
#       └─▶ platform Go workspace-server (proxies)
#               └─▶ workspace's own /internal/chat/uploads/ingest
#                       └─▶ writes to /workspace/.molecule/chat-uploads
#
# The same script runs against ANY environment because the architecture
# is now uniform — local docker-compose, staging tenant, production
# health-probe — all hit the same call site with the same expected
# behavior. This is the design goal RFC #2312 set: "test local will
# pretty much match production."
#
# Required env:
#   BASE                   default http://localhost:8080
#                          override to https://<id>.<tenant>.staging...
#   WORKSPACE_RUNTIME      default langgraph (any internal runtime)
#
# Exit codes:
#   0  upload + read-back round-trip succeeded
#   1  setup failed (couldn't create workspace, never came online, etc.)
#   2  upload returned non-2xx
#   3  upload succeeded but the file isn't readable via download

set -uo pipefail

BASE="${BASE:-http://localhost:8080}"
RUNTIME="${WORKSPACE_RUNTIME:-langgraph}"

PARENT=""
PARENT_TOK=""

# shellcheck disable=SC1091
source "$(dirname "$0")/_lib.sh"

cleanup() {
    local rc=$?
    set +e
    if [ -n "$PARENT" ]; then
        curl -sS -X DELETE "$BASE/workspaces/$PARENT?confirm=true&purge=true" \
            ${PARENT_TOK:+-H "Authorization: Bearer $PARENT_TOK"} >/dev/null 2>&1
    fi
    exit $rc
}
trap cleanup EXIT INT TERM

# ─── 1. Create workspace ───────────────────────────────────────────────
echo "[1/5] POST /workspaces (runtime=$RUNTIME)..."
P_RESP=$(curl -sS -X POST "$BASE/workspaces" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"e2e-chat-upload\",\"runtime\":\"$RUNTIME\",\"tier\":2}")
PARENT=$(echo "$P_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$PARENT" ] || { echo "  ✗ workspace create failed: $P_RESP"; exit 1; }
echo "  ✓ workspace=$PARENT"

# ─── 2. Wait for online ────────────────────────────────────────────────
echo "[2/5] waiting for workspace online (up to 5min)..."
for i in $(seq 1 60); do
    S=$(curl -sS "$BASE/workspaces/$PARENT" 2>/dev/null \
        | python3 -c "import sys,json; d=json.load(sys.stdin); w=d.get('workspace') if isinstance(d.get('workspace'),dict) else d; print(w.get('status') or '')" 2>/dev/null)
    [ $((i % 6)) -eq 1 ] && echo "  attempt $i: status=$S"
    [ "$S" = "online" ] && break
    sleep 5
done
[ "$S" = "online" ] || { echo "  ✗ workspace never online (last=$S)"; exit 1; }
echo "  ✓ online"

# Mint a workspace bearer for the test (the auth needed to call
# /workspaces/:id/chat/uploads, which is wsAuth-gated).
PARENT_TOK=$(e2e_mint_test_token "$PARENT") || {
    echo "  ✗ couldn't mint test token (MOLECULE_ENV=production?)"
    exit 1
}

# ─── 3. Upload a fixture ───────────────────────────────────────────────
echo "[3/5] POST /workspaces/$PARENT/chat/uploads ..."
FIXTURE=$(mktemp)
echo "e2e fixture content $(date +%s)" > "$FIXTURE"
EXPECTED=$(cat "$FIXTURE")

UPLOAD=$(curl -sS -X POST "$BASE/workspaces/$PARENT/chat/uploads" \
    -H "Authorization: Bearer $PARENT_TOK" \
    -F "files=@$FIXTURE;filename=greeting.txt;type=text/plain" \
    -w "\nHTTP_CODE=%{http_code}\n")
CODE=$(echo "$UPLOAD" | grep -oE 'HTTP_CODE=[0-9]+' | cut -d= -f2)
BODY=$(echo "$UPLOAD" | sed '/^HTTP_CODE=/,$d')
echo "  status=$CODE"
echo "  body=$(echo "$BODY" | head -c 300)"

if [ "$CODE" != "200" ]; then
    echo "  ✗ upload returned $CODE"
    rm -f "$FIXTURE"
    exit 2
fi

URI=$(echo "$BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['files'][0]['uri'])" 2>/dev/null)
NAME=$(echo "$BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['files'][0]['name'])" 2>/dev/null)
SIZE=$(echo "$BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['files'][0]['size'])" 2>/dev/null)
[ -n "$URI" ] || { echo "  ✗ no URI in response"; rm -f "$FIXTURE"; exit 2; }
[ "$NAME" = "greeting.txt" ] || { echo "  ✗ name mismatch: $NAME"; rm -f "$FIXTURE"; exit 2; }
[ "$SIZE" = "$(wc -c <"$FIXTURE" | tr -d ' ')" ] || { echo "  ✗ size mismatch: $SIZE"; rm -f "$FIXTURE"; exit 2; }
echo "  ✓ uri=$URI"
echo "  ✓ name=$NAME size=$SIZE"

# Extract the absolute path inside the workspace (strip workspace: scheme).
PATH_IN_WS="${URI#workspace:}"

# ─── 4. Read it back via /chat/download ────────────────────────────────
echo "[4/5] GET /workspaces/$PARENT/chat/download?path=$PATH_IN_WS"
DOWNLOADED=$(curl -sS "$BASE/workspaces/$PARENT/chat/download?path=$PATH_IN_WS" \
    -H "Authorization: Bearer $PARENT_TOK")
if [ "$DOWNLOADED" != "$EXPECTED" ]; then
    echo "  ✗ content mismatch"
    echo "    expected: $EXPECTED"
    echo "    got:      $DOWNLOADED"
    rm -f "$FIXTURE"
    exit 3
fi
echo "  ✓ round-trip content matches"

# ─── 5. Auth: bare upload without bearer is rejected ───────────────────
echo "[5/5] POST without bearer must be 401..."
NA_CODE=$(curl -sS -o /dev/null -w "%{http_code}" -X POST "$BASE/workspaces/$PARENT/chat/uploads" \
    -F "files=@$FIXTURE")
if [ "$NA_CODE" != "401" ]; then
    echo "  ✗ expected 401 without bearer, got $NA_CODE"
    rm -f "$FIXTURE"
    exit 2
fi
echo "  ✓ 401 without bearer"

rm -f "$FIXTURE"
echo ""
echo "✓ chat upload v2 (RFC #2312) end-to-end passed against $BASE"
