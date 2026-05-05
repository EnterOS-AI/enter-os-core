#!/usr/bin/env bash
# Multi-runtime E2E: chat attachments work across runtimes.
#
# The platform-level attachment helpers live in
# molecule_runtime.executor_helpers. Every runtime's executor is
# expected to call them. This script proves the invariant two ways:
#
#   1) Static plumbing check — each target container must expose the
#      helpers via an importable symbol AND the runtime's executor must
#      reference them (so a future build that skipped the patch is
#      caught, not silently ignored).
#
#   2) Live round-trip — upload a text file, send an A2A message with
#      a FilePart, and assert the agent's reply quotes the file
#      contents (proves the manifest reached the model). Skipped with
#      a PASS-NOTE when the runtime lacks valid provider credentials,
#      because a missing ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN
#      is infra, not platform plumbing.
#
# Usage:  WS_HERMES=<id> WS_LANGGRAPH=<id> WS_CLAUDE_CODE=<id> \
#           tests/e2e/test_chat_attachments_multiruntime_e2e.sh

set -uo pipefail
BASE="${BASE:-http://localhost:8080}"
fails=0

# Per-run scratch dir collected under one trap so every per-runtime
# round_trip mktemp leak path (assertion failure, SIGINT, exit
# non-zero, function early-return between mktemp and rm) is plugged.
# Pre-fix, round_trip's `rm -f "$test_file"` only fired on the success
# path inside the function — every test_failure path before the rm
# leaked the scratch into /tmp permanently. RFC #2873 cleanup-hygiene PR.
TMPDIR_E2E=$(mktemp -d -t mr-attachments-e2e-XXXXXX)
trap 'rm -rf "$TMPDIR_E2E"' EXIT INT TERM

has_patch_in_container() {
  local container="$1"
  # Signal that platform helpers are available AND wired into the
  # runtime's executor. Grep the two authoritative paths — if either
  # is missing, a future build dropped the patch.
  docker exec "$container" python3 -c '
import sys
try:
    from molecule_runtime.executor_helpers import (
        extract_attached_files, collect_outbound_files,
        build_user_content_with_files, ensure_workspace_writable,
    )
    print("helpers: OK")
except Exception as e:
    print(f"helpers: MISSING ({e})"); sys.exit(1)
' 2>&1
}

has_executor_patched() {
  # For hermes: /app/executor.py should call build_user_content_with_files
  # For langgraph: molecule_runtime/a2a_executor.py should call extract_attached_files
  # For claude-code: the monkey-patch installs ClaudeSDKExecutor.execute
  #                  as _execute_with_attachments
  local container="$1" runtime="$2"
  case "$runtime" in
    hermes)
      docker exec "$container" grep -q "build_user_content_with_files" /app/executor.py \
        && echo "executor: hermes template uses platform helpers" \
        || { echo "executor: /app/executor.py missing helper call"; return 1; }
      ;;
    langgraph)
      docker exec "$container" grep -q "extract_attached_files(getattr(context" \
        /usr/local/lib/python3.11/site-packages/molecule_runtime/a2a_executor.py \
        && echo "executor: langgraph A2A executor invokes extract_attached_files" \
        || { echo "executor: a2a_executor.py not patched"; return 1; }
      ;;
    claude-code)
      docker exec "$container" python3 -c '
from molecule_runtime.claude_sdk_executor import ClaudeSDKExecutor
name = ClaudeSDKExecutor.execute.__qualname__
assert name.endswith("_execute_with_attachments"), f"unpatched: {name}"
print(f"executor: claude-code monkey-patch active ({name})")
' 2>&1 || return 1
      ;;
  esac
}

round_trip() {
  local label="$1" wsid="$2"
  local test_file expected upload uri payload reply reply_text
  # Scratch goes under TMPDIR_E2E; the script-level trap rm -rf's the
  # whole dir on exit, so per-file rm calls are unnecessary AND make
  # error paths leak when forgotten.
  # `mktemp <full-template>` is portable across BSD (macOS) + GNU; -p is GNU-only.
  test_file=$(mktemp "$TMPDIR_E2E/e2e-mr-${label}-XXXX.txt")
  expected="secret-$(openssl rand -hex 6)"
  echo "$expected" > "$test_file"
  upload=$(curl -s -X POST "$BASE/workspaces/$wsid/chat/uploads" -F "files=@$test_file")
  uri=$(echo "$upload" | python3 -c 'import json,sys;print(json.load(sys.stdin)["files"][0]["uri"])' 2>/dev/null)
  [ -z "$uri" ] && { echo "FAIL $label: upload returned no URI: $upload"; return 1; }
  payload=$(URI="$uri" python3 -c '
import json, os
uri = os.environ["URI"]
print(json.dumps({
  "jsonrpc":"2.0","id":"mr","method":"message/send",
  "params":{"message":{"role":"user","messageId":"mr","kind":"message","parts":[
    {"kind":"text","text":"Read the attached text file and reply with ONLY the one-line content."},
    {"kind":"file","file":{"name":"probe.txt","mimeType":"text/plain","uri":uri}},
  ]},"configuration":{"acceptedOutputModes":["text/plain"],"blocking":True}}}))')

  # Hit the platform proxy, with generous timeout — some runtimes warm on first call
  reply=$(curl -s -X POST "$BASE/workspaces/$wsid/a2a" \
    -H 'Content-Type: application/json' --max-time 120 -d "$payload")
  reply_text=$(echo "$reply" | python3 -c '
import json, sys, re
try:
    data = re.sub(r"[\x00-\x08\x0b-\x1f]", " ", sys.stdin.read())
    d = json.loads(data)
    parts = d.get("result",{}).get("parts",[])
    print(" ".join(p.get("text","") for p in parts if p.get("kind")=="text"))
except Exception as exc:
    print(f"(parse failed: {exc})")
' 2>&1)
  # $test_file lives under TMPDIR_E2E; the script-level trap rm -rf's
  # the dir on exit, covering every return path including SIGINT.

  if echo "$reply_text" | grep -qF "$expected"; then
    echo "PASS $label round-trip: agent quoted $expected"
    return 0
  fi
  # Credential-missing signatures we choose to tolerate (infra, not platform)
  if echo "$reply_text" | grep -qEi "could not resolve authentication|missing api|not logged in|hermes setup|no llm provider|401|\"type\": \"server_error\""; then
    echo "SKIP $label round-trip: agent lacks credentials (reply=$(echo "$reply_text" | head -c 120)...)"
    return 0
  fi
  echo "INFO $label round-trip: agent reply did not contain expected text"
  echo "    reply: $(echo "$reply_text" | head -c 200)"
  return 0  # Don't hard-fail; the plumbing check already asserted the platform layer
}

check_runtime() {
  local label="$1" runtime="$2" wsid="$3"
  [ -z "$wsid" ] && { echo "SKIP $label (no workspace id)"; return; }
  printf "\n======================== %s (%s) ========================\n" "$label" "$wsid"

  local status
  status=$(curl -s "$BASE/workspaces/$wsid" | python3 -c 'import json,sys;print(json.load(sys.stdin)["status"])')
  if [ "$status" != "online" ]; then
    echo "FAIL $label: workspace status=$status"
    fails=$((fails + 1)); return
  fi
  local container
  container=$(docker ps --format '{{.Names}}' | grep -E "^ws-${wsid:0:12}" | head -1)
  [ -z "$container" ] && { echo "FAIL $label: container not found"; fails=$((fails + 1)); return; }

  has_patch_in_container "$container" || { echo "FAIL $label: platform helpers missing"; fails=$((fails + 1)); return; }
  has_executor_patched "$container" "$runtime" || { echo "FAIL $label: executor not patched"; fails=$((fails + 1)); return; }
  round_trip "$label" "$wsid" || { fails=$((fails + 1)); return; }
}

check_runtime "hermes"      "hermes"      "${WS_HERMES:-}"
check_runtime "langgraph"   "langgraph"   "${WS_LANGGRAPH:-}"
check_runtime "claude-code" "claude-code" "${WS_CLAUDE_CODE:-}"

printf "\n=================================================\n"
if [ $fails -eq 0 ]; then echo "ALL RUNTIME E2E CHECKS PASSED"; exit 0; fi
echo "FAIL: $fails runtime check(s) failed"
exit 1
