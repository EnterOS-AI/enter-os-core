#!/usr/bin/env bash
# E2E test: A2A round-trip parity across all four runtimes.
#
# Validates that for each of {claude-code, hermes, codex, openclaw}:
#   1. A workspace can be provisioned + brought online
#   2. The adapter responds to A2A message/send
#   3. The reply contains expected content (echo of the prompt)
#   4. A SECOND message preserves session state where the runtime
#      supports it (currently: hermes via plugin path)
#
# Targets a SaaS tenant subdomain. Provisions workspaces in the calling
# tenant, runs the round-trip, deletes them on success.
#
# Pre-reqs:
#   - PLATFORM env or first arg pointing at a tenant subdomain
#       (e.g. https://demo-tenant.staging.moleculesai.app)
#   - $OPENROUTER_API_KEY (or $HERMES_API_KEY) for non-claude runtimes
#   - $OPENAI_API_KEY for claude-code peer
#   - SaaS edge requires Origin header — see auto-memory
#       reference_saas_waf_origin_header.md
#
# Run:
#   PLATFORM=https://my-tenant.staging.moleculesai.app \
#       ./scripts/test-all-runtimes-a2a-e2e.sh
#
# Skip individual runtimes:
#   SKIP_HERMES=1 SKIP_OPENCLAW=1 ./scripts/test-all-runtimes-a2a-e2e.sh
set -euo pipefail

PLATFORM="${PLATFORM:-${1:-http://localhost:8080}}"
HERMES_PROVIDER_KEY="${OPENROUTER_API_KEY:-${HERMES_API_KEY:-}}"
PEER_OPENAI_KEY="${OPENAI_API_KEY:-}"
# SaaS auth chain — TENANT_ADMIN_TOKEN + TENANT_ORG_ID required when
# hitting *.moleculesai.app (per-tenant ADMIN_TOKEN, NOT
# CP_ADMIN_API_TOKEN). Optional for localhost.
TENANT_ADMIN_TOKEN="${TENANT_ADMIN_TOKEN:-}"
TENANT_ORG_ID="${TENANT_ORG_ID:-}"
EXTRA_HEADERS=()
case "$PLATFORM" in
  https://*.moleculesai.app|https://*.moleculesai.app/*)
    EXTRA_HEADERS+=("-H" "Origin: $PLATFORM")
    [ -n "$TENANT_ADMIN_TOKEN" ] && EXTRA_HEADERS+=("-H" "Authorization: Bearer $TENANT_ADMIN_TOKEN")
    [ -n "$TENANT_ORG_ID" ] && EXTRA_HEADERS+=("-H" "X-Molecule-Org-Id: $TENANT_ORG_ID")
    ;;
esac

if [ -z "$HERMES_PROVIDER_KEY" ] && [ -z "${SKIP_HERMES:-}${SKIP_CODEX:-}${SKIP_OPENCLAW:-}" ]; then
  echo "FAIL: set OPENROUTER_API_KEY or HERMES_API_KEY for non-claude runtimes"
  exit 2
fi

PASS=0
FAIL=0
declare -A WS_IDS

check() {
  local label="$1" expected="$2" actual="$3"
  if echo "$actual" | grep -qiE "$expected"; then
    echo "PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $label"
    echo "  expected to contain: $expected"
    echo "  got: $actual"
    FAIL=$((FAIL + 1))
  fi
}

curl_p() {
  /usr/bin/curl -s "${EXTRA_HEADERS[@]}" "$@"
}

wait_online() {
  local id="$1" name="$2" max="${3:-60}"
  for i in $(seq 1 "$max"); do
    local s
    s=$(curl_p "$PLATFORM/workspaces/$id" \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
    [ "$s" = "online" ] && return 0
    [ "$s" = "failed" ] && echo "  $name FAILED" && return 1
    [ $((i % 6)) -eq 0 ] && echo "  [$name] ${i}/${max}... ($s)"
    sleep 5
  done
  echo "  $name did not come online within $((max*5))s"
  return 1
}

a2a_send() {
  local id="$1" message="$2"
  local resp text
  resp=$(curl_p -X POST "$PLATFORM/workspaces/$id/a2a" \
    -H 'Content-Type: application/json' \
    -d "$(python3 -c "import json,sys; print(json.dumps({
      'method': 'message/send',
      'params': {'message': {'role': 'user', 'parts': [{'kind': 'text', 'text': sys.argv[1]}]}}
    }))" "$message")")
  text=$(echo "$resp" | python3 -c "
import sys, json
try:
  r = json.load(sys.stdin)
  print(r.get('result', {}).get('parts', [{}])[0].get('text', ''))
except Exception:
  print('')
" 2>/dev/null)
  echo "$text"
}

provision() {
  local name="$1" template="$2" role="$3"
  local r id
  r=$(curl_p -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\",\"role\":\"$role\",\"tier\":2,\"template\":\"$template\"}")
  id=$(echo "$r" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))")
  if [ -z "$id" ]; then
    echo "FAIL: provision $name returned no id: $r" >&2
    return 1
  fi
  echo "$id"
}

set_secret() {
  local id="$1" key="$2" value="$3"
  curl_p -X POST "$PLATFORM/workspaces/$id/secrets" \
    -H 'Content-Type: application/json' \
    -d "{\"key\":\"$key\",\"value\":\"$value\"}" > /dev/null
}

cleanup() {
  echo ""
  echo "--- Cleanup ---"
  for runtime in "${!WS_IDS[@]}"; do
    id="${WS_IDS[$runtime]}"
    [ -n "$id" ] && curl_p -X DELETE "$PLATFORM/workspaces/$id" >/dev/null && \
      echo "  Deleted $runtime ($id)" || echo "  Cleanup skipped for $runtime"
  done
}
trap cleanup EXIT

echo "=========================================="
echo "  All-runtimes A2A parity E2E"
echo "  Platform: $PLATFORM"
echo "=========================================="
echo ""

# -------------------------------------------------------
# 1. Provision the four runtimes (skip via SKIP_* flags)
# -------------------------------------------------------
echo "--- 1. Provision workspaces ---"
if [ -z "${SKIP_CLAUDE_CODE:-}" ]; then
  WS_IDS[claude-code]=$(provision "ParityClaude" "claude-code-default" "claude-code peer")
  echo "  claude-code: ${WS_IDS[claude-code]}"
fi
if [ -z "${SKIP_HERMES:-}" ]; then
  WS_IDS[hermes]=$(provision "ParityHermes" "hermes" "hermes peer")
  echo "  hermes:      ${WS_IDS[hermes]}"
fi
if [ -z "${SKIP_CODEX:-}" ]; then
  WS_IDS[codex]=$(provision "ParityCodex" "codex" "codex peer")
  echo "  codex:       ${WS_IDS[codex]}"
fi
if [ -z "${SKIP_OPENCLAW:-}" ]; then
  WS_IDS[openclaw]=$(provision "ParityOpenClaw" "openclaw" "openclaw peer")
  echo "  openclaw:    ${WS_IDS[openclaw]}"
fi

# -------------------------------------------------------
# 2. Set provider keys
# -------------------------------------------------------
echo ""
echo "--- 2. Set provider keys ---"
for runtime in hermes codex openclaw; do
  id="${WS_IDS[$runtime]:-}"
  [ -n "$id" ] && set_secret "$id" "OPENROUTER_API_KEY" "$HERMES_PROVIDER_KEY" && \
    echo "  $runtime: OPENROUTER_API_KEY set"
done
if [ -n "${WS_IDS[claude-code]:-}" ] && [ -n "$PEER_OPENAI_KEY" ]; then
  set_secret "${WS_IDS[claude-code]}" "OPENAI_API_KEY" "$PEER_OPENAI_KEY"
  echo "  claude-code: OPENAI_API_KEY set"
fi

# -------------------------------------------------------
# 3. Wait for online
# -------------------------------------------------------
echo ""
echo "--- 3. Wait online (hermes cold-boot ~3-7 min) ---"
for runtime in "${!WS_IDS[@]}"; do
  id="${WS_IDS[$runtime]}"
  [ -z "$id" ] && continue
  max=60
  [ "$runtime" = "hermes" ] && max=120
  if wait_online "$id" "$runtime" "$max"; then
    check "$runtime online" "ok" "ok"
  else
    check "$runtime online" "online" "timeout"
  fi
done

# -------------------------------------------------------
# 4. A2A round-trip — first message
# -------------------------------------------------------
echo ""
echo "--- 4. A2A round-trip (first message) ---"
for runtime in claude-code hermes codex openclaw; do
  id="${WS_IDS[$runtime]:-}"
  [ -z "$id" ] && continue
  reply=$(a2a_send "$id" "Reply with just the word OK so we know you got this.")
  echo "  [$runtime] reply: ${reply:0:80}"
  check "$runtime A2A reply" "ok|got|received|reply|response" "$reply"
done

# -------------------------------------------------------
# 5. Session continuity — second message recalls first
# -------------------------------------------------------
echo ""
echo "--- 5. Session continuity (second message recalls first) ---"
for runtime in claude-code hermes codex openclaw; do
  id="${WS_IDS[$runtime]:-}"
  [ -z "$id" ] && continue
  # Set up: tell the agent a name.
  a2a_send "$id" "My name is Carol. Reply with just the word OK." > /dev/null
  # Recall: ask for the name back. Hermes plugin path keeps session
  # state across turns; chat-completions path forgets between turns.
  reply=$(a2a_send "$id" "What name did I introduce myself with one message ago? One word answer.")
  echo "  [$runtime] recall reply: ${reply:0:80}"
  check "$runtime session continuity" "carol" "$reply"
done

# -------------------------------------------------------
# Results
# -------------------------------------------------------
echo ""
echo "=========================================="
echo "  Pass: $PASS    Fail: $FAIL"
echo "=========================================="
[ "$FAIL" -eq 0 ]
