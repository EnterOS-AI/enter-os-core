#!/usr/bin/env bash
# E2E for poll-mode chat upload (RFC #2891 phases 1-5b).
#
# Round-trip: register a workspace as poll-mode (no callback URL) → POST a
# multi-file chat upload → verify each file becomes (a) one
# `chat_upload_receive` activity row and (b) one /pending-uploads row → fetch
# the bytes back via the poll endpoint → ack → verify the row 404s on
# subsequent fetch. Also pins cross-workspace bleed protection: workspace B
# cannot read workspace A's pending uploads even with its own valid bearer.
#
# Why this exists separately from test_chat_upload_e2e.sh: that script
# covers the PUSH path (the workspace's own /internal/chat/uploads/ingest).
# This script covers the POLL path: the same canvas-side request lands on
# the platform's pendinguploads.Storage instead, and the workspace fetches
# it later. The two paths share zero handler code on the platform side, so
# both need their own E2E.
#
# Requires: platform running on localhost:8080 with migrations applied.
#   bash workspace-server/scripts/dev-start.sh
#   bash workspace-server/scripts/run-migrations.sh
#
# Idempotent: each run uses fresh per-script workspace UUIDs so reruns
# don't collide. Best-effort cleanup on EXIT — does NOT call
# e2e_cleanup_all_workspaces (see
# `feedback_never_run_cluster_cleanup_tests_on_live_platform.md`).

set -euo pipefail

source "$(dirname "$0")/_lib.sh"

PASS=0
FAIL=0
TIMEOUT="${A2A_TIMEOUT:-30}"

gen_uuid() {
  if command -v uuidgen >/dev/null 2>&1; then
    uuidgen | tr '[:upper:]' '[:lower:]'
  else
    python3 -c 'import uuid; print(uuid.uuid4())'
  fi
}
WS_A="$(gen_uuid)"
WS_B="$(gen_uuid)"

# Per-run scratch dir collected under one trap so every assertion-failure
# path drops the temp files it made (see test_chat_attachments_e2e.sh).
TMPDIR_E2E=$(mktemp -d -t poll-chat-upload-e2e-XXXXXX)

cleanup() {
  local rc=$?
  curl -s -X DELETE "$BASE/workspaces/$WS_A?confirm=true" >/dev/null 2>&1 || true
  curl -s -X DELETE "$BASE/workspaces/$WS_B?confirm=true" >/dev/null 2>&1 || true
  rm -rf "$TMPDIR_E2E"
  exit $rc
}
trap cleanup EXIT INT TERM

check() {
  local desc="$1" expected="$2" actual="$3"
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
  local desc="$1" expected="$2" actual="$3"
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

echo "=== Poll-Mode Chat Upload E2E ==="
echo "  base:        $BASE"
echo "  workspace A: $WS_A"
echo "  workspace B: $WS_B"
echo ""

# ---------- Phase 1: register poll-mode workspace ----------
echo "--- Phase 1: Register poll-mode workspace A ---"

REG_A=$(curl -s -X POST "$BASE/registry/register" \
  -H "Content-Type: application/json" \
  -d "{
    \"id\": \"$WS_A\",
    \"delivery_mode\": \"poll\",
    \"agent_card\": {\"name\": \"poll-chat-upload-test-a\"}
  }")
check "register accepts poll mode without URL" '"status":"registered"' "$REG_A"
TOK_A=$(echo "$REG_A" | e2e_extract_token || true)
[ -n "$TOK_A" ] || { echo "FAIL: no auth_token in register response (ws A)"; FAIL=$((FAIL + 1)); exit 1; }

# ---------- Phase 2: multi-file chat upload ----------
echo ""
echo "--- Phase 2: POST /chat/uploads with two files ---"

FILE1="$TMPDIR_E2E/alpha.txt"
FILE2="$TMPDIR_E2E/beta.txt"
EXPECTED1="alpha-secret-$(openssl rand -hex 4)"
EXPECTED2="beta-secret-$(openssl rand -hex 4)"
printf '%s' "$EXPECTED1" > "$FILE1"
printf '%s' "$EXPECTED2" > "$FILE2"

UPLOAD=$(curl -s -X POST "$BASE/workspaces/$WS_A/chat/uploads" \
  -H "Authorization: Bearer $TOK_A" \
  -F "files=@$FILE1;filename=alpha.txt;type=text/plain" \
  -F "files=@$FILE2;filename=beta.txt;type=text/plain" \
  -w "\nHTTP_CODE=%{http_code}\n")
UPLOAD_CODE=$(echo "$UPLOAD" | grep -oE 'HTTP_CODE=[0-9]+' | cut -d= -f2)
UPLOAD_BODY=$(echo "$UPLOAD" | sed '/^HTTP_CODE=/,$d')

check_eq "upload returns 200" "200" "$UPLOAD_CODE"
check "upload response has files array" '"files":' "$UPLOAD_BODY"

# Pull file_ids out of the URI in the response. URI shape is
# `platform-pending:<wsid>/<file_id>` — proves the response came from the
# poll-mode branch, not the push-mode internal-ingest branch.
URI1=$(echo "$UPLOAD_BODY" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d["files"][0]["uri"])')
URI2=$(echo "$UPLOAD_BODY" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d["files"][1]["uri"])')
check "URI 1 has platform-pending: scheme" "platform-pending:$WS_A/" "$URI1"
check "URI 2 has platform-pending: scheme" "platform-pending:$WS_A/" "$URI2"

FID1="${URI1##*/}"
FID2="${URI2##*/}"
[ -n "$FID1" ] && [ -n "$FID2" ] || { echo "FAIL: could not extract file IDs"; FAIL=$((FAIL + 1)); exit 1; }
echo "  file_id 1: $FID1"
echo "  file_id 2: $FID2"

# ---------- Phase 3: activity rows visible to the workspace ----------
echo ""
echo "--- Phase 3: /activity shows two chat_upload_receive rows ---"

# activity_logs INSERTs run in a goroutine — give them a moment.
sleep 1
ACT=$(curl -s --max-time "$TIMEOUT" -H "Authorization: Bearer $TOK_A" \
  "$BASE/workspaces/$WS_A/activity?type=a2a_receive&limit=20")
check "activity feed has the alpha file"      "$FID1" "$ACT"
check "activity feed has the beta file"       "$FID2" "$ACT"
check "activity rows tagged chat_upload_receive" '"method":"chat_upload_receive"' "$ACT"
check "activity rows record alpha mimetype"   '"mimeType":"text/plain"' "$ACT"

CHAT_UPLOAD_COUNT=$(echo "$ACT" | python3 -c '
import json, sys
rows = json.load(sys.stdin)
n = sum(1 for r in rows if (r.get("method") or "") == "chat_upload_receive")
print(n)
')
check_eq "exactly two chat_upload_receive rows" "2" "$CHAT_UPLOAD_COUNT"

# ---------- Phase 4: GET /pending-uploads/:file_id/content ----------
echo ""
echo "--- Phase 4: Fetch content for each pending upload ---"

GOT1=$(curl -s --max-time "$TIMEOUT" -H "Authorization: Bearer $TOK_A" \
  "$BASE/workspaces/$WS_A/pending-uploads/$FID1/content")
check_eq "alpha bytes round-trip" "$EXPECTED1" "$GOT1"

GOT2=$(curl -s --max-time "$TIMEOUT" -H "Authorization: Bearer $TOK_A" \
  "$BASE/workspaces/$WS_A/pending-uploads/$FID2/content")
check_eq "beta bytes round-trip" "$EXPECTED2" "$GOT2"

# Mimetype + Content-Disposition headers should match what was uploaded.
HEAD1=$(curl -s -D - -o /dev/null --max-time "$TIMEOUT" -H "Authorization: Bearer $TOK_A" \
  "$BASE/workspaces/$WS_A/pending-uploads/$FID1/content")
check "alpha response carries text/plain Content-Type" "Content-Type: text/plain" "$HEAD1"
check "alpha response carries Content-Disposition with filename" 'filename="alpha.txt"' "$HEAD1"

# ---------- Phase 5: idempotent re-fetch (until ack) ----------
echo ""
echo "--- Phase 5: Re-fetch before ack returns the same bytes ---"

RE_GOT1=$(curl -s --max-time "$TIMEOUT" -H "Authorization: Bearer $TOK_A" \
  "$BASE/workspaces/$WS_A/pending-uploads/$FID1/content")
check_eq "re-fetch returns same alpha bytes" "$EXPECTED1" "$RE_GOT1"

# ---------- Phase 6: ack each row ----------
echo ""
echo "--- Phase 6: Ack each pending upload ---"

ACK1=$(curl -s -X POST --max-time "$TIMEOUT" -H "Authorization: Bearer $TOK_A" \
  "$BASE/workspaces/$WS_A/pending-uploads/$FID1/ack")
check "alpha ack returns acked:true" '"acked":true' "$ACK1"

ACK2=$(curl -s -X POST --max-time "$TIMEOUT" -H "Authorization: Bearer $TOK_A" \
  "$BASE/workspaces/$WS_A/pending-uploads/$FID2/ack")
check "beta ack returns acked:true" '"acked":true' "$ACK2"

# Re-ack should still 200 (idempotent — the row's gone but the workspace's
# at-least-once intent was already honored, and the second ack hits the
# raced path which also returns 200).
RE_ACK1=$(curl -s -w '\n%{http_code}' -X POST --max-time "$TIMEOUT" \
  -H "Authorization: Bearer $TOK_A" \
  "$BASE/workspaces/$WS_A/pending-uploads/$FID1/ack")
RE_ACK1_CODE=$(printf '%s' "$RE_ACK1" | tail -n1)
# Acked rows return 404 on Get-before-Ack (the row's still in the table
# but Get filters acked_at IS NULL); workspace would not normally re-ack
# since it already saw the success. Accept both 200 and 404 here so the
# test pins the contract without being brittle on the inner ordering.
case "$RE_ACK1_CODE" in
  200|404)
    echo "PASS: re-ack returns 200 or 404 ($RE_ACK1_CODE)"
    PASS=$((PASS + 1))
    ;;
  *)
    echo "FAIL: re-ack returned unexpected $RE_ACK1_CODE"
    FAIL=$((FAIL + 1))
    ;;
esac

# ---------- Phase 7: GET content after ack returns 404 ----------
echo ""
echo "--- Phase 7: Acked file 404s on subsequent fetch ---"

POST_ACK=$(curl -s -w '\n%{http_code}' --max-time "$TIMEOUT" -H "Authorization: Bearer $TOK_A" \
  "$BASE/workspaces/$WS_A/pending-uploads/$FID1/content")
POST_ACK_CODE=$(printf '%s' "$POST_ACK" | tail -n1)
check_eq "acked alpha returns HTTP 404" "404" "$POST_ACK_CODE"

# ---------- Phase 8: cross-workspace bleed protection ----------
echo ""
echo "--- Phase 8: Workspace B cannot read workspace A's pending uploads ---"

# Stage a fresh upload on workspace A so we have an UN-acked row to probe.
PROBE_FILE="$TMPDIR_E2E/probe.txt"
printf '%s' "probe-bytes-$(openssl rand -hex 4)" > "$PROBE_FILE"
PROBE_UP=$(curl -s -X POST "$BASE/workspaces/$WS_A/chat/uploads" \
  -H "Authorization: Bearer $TOK_A" \
  -F "files=@$PROBE_FILE;filename=probe.txt;type=text/plain")
PROBE_FID=$(echo "$PROBE_UP" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d["files"][0]["uri"].split("/")[-1])')
[ -n "$PROBE_FID" ] || { echo "FAIL: probe upload returned no file_id"; FAIL=$((FAIL + 1)); exit 1; }

# Register a SECOND poll-mode workspace and capture its bearer.
REG_B=$(curl -s -X POST "$BASE/registry/register" \
  -H "Content-Type: application/json" \
  -d "{
    \"id\": \"$WS_B\",
    \"delivery_mode\": \"poll\",
    \"agent_card\": {\"name\": \"poll-chat-upload-test-b\"}
  }")
check "second workspace registers" '"status":"registered"' "$REG_B"
TOK_B=$(echo "$REG_B" | e2e_extract_token || true)
[ -n "$TOK_B" ] || { echo "FAIL: no auth_token (ws B)"; FAIL=$((FAIL + 1)); exit 1; }

# B's bearer hitting B's URL with A's file_id → 404 (handler checks the row's
# workspace_id matches the URL :id, not the bearer's workspace).
CROSS_RESP=$(curl -s -w '\n%{http_code}' --max-time "$TIMEOUT" \
  -H "Authorization: Bearer $TOK_B" \
  "$BASE/workspaces/$WS_B/pending-uploads/$PROBE_FID/content")
CROSS_CODE=$(printf '%s' "$CROSS_RESP" | tail -n1)
check_eq "B's URL with A's file_id returns 404" "404" "$CROSS_CODE"

# B's bearer hitting A's URL → 401 (wsAuth pins bearer to :id). This is the
# strictest cross-workspace check: a presented-but-wrong bearer is rejected
# in EVERY platform posture (dev-mode fail-open only triggers when no bearer
# is presented at all — invalid tokens always 401).
WRONG_BEARER=$(curl -s -w '\n%{http_code}' --max-time "$TIMEOUT" \
  -H "Authorization: Bearer $TOK_B" \
  "$BASE/workspaces/$WS_A/pending-uploads/$PROBE_FID/content")
WRONG_CODE=$(printf '%s' "$WRONG_BEARER" | tail -n1)
check_eq "B's bearer on A's URL returns 401" "401" "$WRONG_CODE"

# NB: a fully bearerless request to /pending-uploads/:fid/content returns
# 401 ONLY when the platform has MOLECULE_ENV != development (production /
# staging). On local-dev with MOLECULE_ENV=development the wsauth middleware
# fail-opens for bearerless requests so the canvas at :3000 can talk to the
# platform at :8080 without per-call token plumbing — see middleware/
# devmode.go. The strict bearerless-401 contract is covered by the wsauth
# unit + middleware tests; we don't reassert it here because the result
# depends on platform posture, not the poll-mode upload contract.

# ---------- Phase 9: invalid file_id rejected at the URL parser ----------
echo ""
echo "--- Phase 9: Invalid file_id returns 400 ---"

BAD_FID=$(curl -s -w '\n%{http_code}' --max-time "$TIMEOUT" \
  -H "Authorization: Bearer $TOK_A" \
  "$BASE/workspaces/$WS_A/pending-uploads/not-a-uuid/content")
BAD_FID_CODE=$(printf '%s' "$BAD_FID" | tail -n1)
check_eq "invalid file_id UUID returns 400" "400" "$BAD_FID_CODE"

# ---------- Results ----------
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
