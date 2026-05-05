#!/usr/bin/env bash
# E2E test: chat file attachment round-trip
#
# Proves the full drag-drop → agent-reads → agent-returns-file → download
# path against a live workspace. Runs against the local workspace-server
# on :8080 with a hermes workspace already online. The test is provider-
# agnostic as long as the agent has a valid API key — it only asserts
# that attachments surface on both ends, not a specific reply shape.
#
# Usage:  WSID=<workspace-id> tests/e2e/test_chat_attachments_e2e.sh
#         (pass WSID for an existing hermes workspace)
#
# Prereqs:
#   - workspace-server on http://localhost:8080
#   - the WSID workspace is online, runtime=hermes
#   - a working provider key (MINIMAX_API_KEY / ANTHROPIC_API_KEY / etc.)
#   - /workspace writable by the agent user (some templates ship it
#     root-owned; chmod 777 for the E2E or use a writable template)

set -euo pipefail

WSID="${WSID:?WSID=<workspace-id> required}"
BASE="${BASE:-http://localhost:8080}"

# Per-run scratch dir collected under one trap so every mktemp leak path
# (assertion failure, SIGINT, exit non-zero) is plugged. Pre-fix this test
# created a /tmp/hermes-e2e-XXXXXX.txt and never deleted it — ~10 KB ×
# every CI run leaked into the runner. RFC #2873 cleanup-hygiene PR.
TMPDIR_E2E=$(mktemp -d -t chat-attachments-e2e-XXXXXX)
trap 'rm -rf "$TMPDIR_E2E"' EXIT INT TERM

log() { printf "\n=== %s ===\n" "$*"; }

log "Preflight: workspace online?"
STATUS=$(curl -s "$BASE/workspaces/$WSID" | python3 -c 'import json,sys;print(json.load(sys.stdin)["status"])')
[ "$STATUS" = "online" ] || { echo "workspace not online ($STATUS)"; exit 1; }

log "Step 1 — Upload a text file via /chat/uploads"
# `mktemp <full-template>` is portable across BSD (macOS) + GNU; -p is
# GNU-only and breaks local dev runs on Mac.
TEST_FILE=$(mktemp "$TMPDIR_E2E/hermes-e2e-XXXXXX.txt")
echo "secret code: $(openssl rand -hex 4)-$(openssl rand -hex 4)" > "$TEST_FILE"
EXPECTED=$(cat "$TEST_FILE" | awk '{print $NF}')
UPLOAD=$(curl -s -X POST "$BASE/workspaces/$WSID/chat/uploads" -F "files=@$TEST_FILE")
URI=$(echo "$UPLOAD" | python3 -c 'import json,sys;print(json.load(sys.stdin)["files"][0]["uri"])')
[ -n "$URI" ] || { echo "upload failed: $UPLOAD"; exit 1; }
echo "uploaded: $URI"

log "Step 2 — A2A message with file part; expect agent to quote the code"
# Build the JSON via a python helper so the URI value doesn't have to be
# shell-interpolated through a heredoc (the { } tokens in a JSON body
# collide with bash brace-expansion when quoted wrong).
PAYLOAD=$(URI="$URI" python3 -c '
import json, os
uri = os.environ["URI"]
print(json.dumps({
  "jsonrpc":"2.0","id":"e2e-up","method":"message/send",
  "params":{"message":{"role":"user","messageId":"e2e-up","kind":"message","parts":[
    {"kind":"text","text":"Read the attached file and tell me the exact secret code."},
    {"kind":"file","file":{"name":"test.txt","mimeType":"text/plain","uri":uri}},
  ]},"configuration":{"acceptedOutputModes":["text/plain"],"blocking":True}}}))
')
REPLY=$(curl -s -X POST "$BASE/workspaces/$WSID/a2a" \
  -H 'Content-Type: application/json' \
  --max-time 120 \
  -d "$PAYLOAD")
REPLY_TEXT=$(echo "$REPLY" | python3 -c 'import json,sys;d=json.load(sys.stdin);[print(p.get("text","")) for p in d["result"]["parts"] if p.get("kind")=="text"]')
echo "agent reply: $REPLY_TEXT"
if echo "$REPLY_TEXT" | grep -qF "$EXPECTED"; then
  echo "PASS: agent saw the attached file"
else
  echo "FAIL: agent reply missing expected code '$EXPECTED'"
  exit 1
fi

log "Step 3 — Seed a file inside /workspace and ask agent to reference it"
# Relies on /workspace being writable by the platform (we copy as root via
# docker exec, mimicking the path a real agent would use through its tools).
CONTAINER=$(docker ps --format '{{.Names}}' | grep -E "^ws-${WSID:0:12}" | head -1)
[ -n "$CONTAINER" ] || { echo "container not found"; exit 1; }
docker exec "$CONTAINER" sh -c 'echo "E2E report body $(date -u +%s)" > /workspace/e2e-report.txt'

REPLY=$(curl -s -X POST "$BASE/workspaces/$WSID/a2a" \
  -H 'Content-Type: application/json' \
  --max-time 120 \
  -d '{"jsonrpc":"2.0","id":"e2e-down","method":"message/send","params":{"message":{"role":"user","messageId":"e2e-down","kind":"message","parts":[{"kind":"text","text":"There is a file at /workspace/e2e-report.txt. Mention its exact path in your reply so I can download it."}]},"configuration":{"acceptedOutputModes":["text/plain"],"blocking":true}}}')
FILE_URI=$(echo "$REPLY" | python3 -c 'import json,sys,re;d=json.load(sys.stdin);[print(p["file"]["uri"]) for p in d["result"]["parts"] if p.get("kind")=="file"]' | head -1)
[ -n "$FILE_URI" ] || { echo "FAIL: agent reply had no file part"; echo "$REPLY"; exit 1; }
echo "agent attached: $FILE_URI"

log "Step 4 — Download via /chat/download"
DL_PATH=${FILE_URI#workspace:}
BODY=$(curl -s "$BASE/workspaces/$WSID/chat/download?path=$DL_PATH")
echo "downloaded: $BODY"
if echo "$BODY" | grep -q "E2E report body"; then
  echo "PASS: downloaded the agent-returned file"
else
  echo "FAIL: download did not return expected body"
  exit 1
fi

log "ALL E2E CHECKS PASSED"
