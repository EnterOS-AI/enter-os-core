#!/usr/bin/env bash
# LOCAL E2E — fresh-provision peer-visibility gate via the LITERAL MCP path.
#
# WHY THIS EXISTS
# ---------------
# tests/e2e/test_peer_visibility_mcp_staging.sh (PR #1298) codified the
# literal user-facing peer-visibility path — but staging-only. The
# standing rule is that the local prod-mimic stack runs a MANDATORY
# local-Postgres E2E BEFORE staging E2E (memory:
# feedback_local_must_mimic_production, feedback_mandatory_local_e2e_
# before_ship, feedback_local_test_before_staging_e2e,
# feedback_real_subprocess_test_for_boot_path). A staging-only gate means
# regressions are caught late and expensively on EC2. This is the LOCAL
# backend: same byte-identical assertion, local docker-compose stack.
#
# THE ASSERTION IS NOT A PROXY and is BYTE-IDENTICAL to staging — it is
# the SAME tests/e2e/lib/peer_visibility_assert.sh::pv_assert_runtime that
# the staging script calls. It issues the byte-for-byte JSON-RPC
# `tools/call name=list_peers` envelope to `POST /workspaces/:id/mcp`
# using each workspace's OWN bearer token, through the real WorkspaceAuth
# + MCPRateLimiter middleware chain — the exact call
# mcp_molecule_list_peers makes from a canvas agent. It does NOT read a
# registry row, /health, the heartbeat table, or GET /registry/:id/peers.
#
# Only PROVISIONING differs from staging:
#   - staging: POST /cp/admin/orgs (cold EC2 tenant) + per-tenant admin
#     token + each workspace's MCP bearer from the POST /workspaces
#     create response.
#   - local:   POST /workspaces directly against the local stack
#     (BASE, default http://localhost:8080), MCP bearer consumed inline
#     from the create response (auth_token field). Same model every
#     other local E2E uses; no new credential/provision flow.
#
# By default the local backend creates external-mode workspace rows and
# drives the literal MCP path directly. That keeps the local peer-visibility
# gate focused on platform auth + MCP list_peers semantics instead of local
# template container boot/heartbeat. Set PV_LOCAL_PROVISION_MODE=container
# for targeted runtime-boot debugging. NON-required by design until the
# flip-to-required tracked at molecule-core#1296, and NOT masked with
# continue-on-error (feedback_fix_root_not_symptom).
#
# Required env: none (local stack only).
# Optional env:
#   BASE                    default http://localhost:8080
#   PV_RUNTIMES             space list; default "hermes openclaw claude-code"
#   PV_LOCAL_PROVISION_MODE default external; set container to also require
#                            local template containers to boot online
#   PV_PARENT_RUNTIME       parent runtime; default claude-code when keyed,
#                            otherwise first keyed runtime in PV_RUNTIMES
#   E2E_PROVISION_TIMEOUT_SECS  per-workspace online budget; default 900
#                            (hermes cold apt+uv is the slow path locally)
#   E2E_KEEP_WS             1 → skip teardown (local debugging only)
#   LLM provider keys (a workspace boots only if its provider key is set;
#   a runtime whose key is absent is SKIPPED, not failed — a partially
#   keyed local env must not false-fail the gate):
#     CLAUDE_CODE_OAUTH_TOKEN  claude-code
#     E2E_MINIMAX_API_KEY      hermes/openclaw (MiniMax, preferred)
#     E2E_ANTHROPIC_API_KEY    hermes/openclaw (direct Anthropic)
#     E2E_OPENAI_API_KEY       hermes/openclaw (OpenAI)
#
# Exit codes (match the staging script):
#   0  every runtime under test saw its peers via the literal MCP call
#   1  generic failure
#   3  a workspace never reached online within the budget
#   10 peer-visibility regression reproduced (the gate firing as designed)

set -uo pipefail

source "$(dirname "$0")/_lib.sh"
# Byte-identical assertion shared with the staging backend.
# shellcheck source=tests/e2e/lib/peer_visibility_assert.sh
source "$(dirname "$0")/lib/peer_visibility_assert.sh"

PV_RUNTIMES="${PV_RUNTIMES:-hermes openclaw claude-code}"
PV_LOCAL_PROVISION_MODE="${PV_LOCAL_PROVISION_MODE:-external}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"
NAME_PREFIX="PV-Local-$$-$(date +%H%M%S)"

log()  { echo "[$(date +%H:%M:%S)] $*"; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

extract_auth_token() {
  python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print(''); sys.exit(0)
print(d.get('auth_token') or d.get('connection', {}).get('auth_token') or '')
" 2>/dev/null
}

CREATED_WSIDS=()
ADMIN_BEARER="${MOLECULE_ADMIN_TOKEN:-${ADMIN_TOKEN:-}}"
ADMIN_AUTH=()
[ -n "$ADMIN_BEARER" ] && ADMIN_AUTH=(-H "Authorization: Bearer $ADMIN_BEARER")

# ─── Scoped teardown ───────────────────────────────────────────────────
# Deletes ONLY the workspaces THIS run created (tracked in CREATED_WSIDS),
# one DELETE /workspaces/:id?confirm=true each. NEVER e2e_cleanup_all_
# workspaces / any blanket sweep — honors feedback_cleanup_after_each_test
# and feedback_never_run_cluster_cleanup_tests_on_live_platform (a local
# stack can still be shared with other concurrent local E2E).
teardown() {
  local rc=$?
  set +e
  if [ "${E2E_KEEP_WS:-0}" = "1" ]; then
    echo ""
    log "[teardown] E2E_KEEP_WS=1 — leaving ${#CREATED_WSIDS[@]} ws for debugging (REMEMBER TO DELETE)"
    exit $rc
  fi
  echo ""
  log "[teardown] deleting ${#CREATED_WSIDS[@]} workspace(s) this run created (scoped)"
  for wid in ${CREATED_WSIDS[@]+"${CREATED_WSIDS[@]}"}; do
    [ -n "$wid" ] || continue
    e2e_delete_workspace "$wid" "" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"}
  done
  exit $rc
}
trap teardown EXIT INT TERM

# Pre-sweep workspaces a prior crashed run of THIS script left behind
# (name prefix match only — never a blanket delete). The trap fires on
# normal exit, but a kill -9 / SIGPIPE can bypass it.
PRIOR=$(curl -s "$BASE/workspaces" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} | python3 -c '
import json, sys
try:
    print(" ".join(w["id"] for w in json.load(sys.stdin) if w.get("name","").startswith("PV-Local-")))
except Exception:
    pass
' 2>/dev/null)
for _wid in $PRIOR; do
  log "Pre-sweeping prior PV-Local workspace: $_wid"
  e2e_delete_workspace "$_wid" "" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"}
done

# ─── Local-stack preflight ─────────────────────────────────────────────
log "0/5 local stack preflight: $BASE/health"
if ! curl -fsS "$BASE/health" -m 5 >/dev/null 2>&1; then
  echo "::error::Local stack not healthy at $BASE/health — bring it up (make up) before this gate. Infra, not a workspace bug (feedback_fix_root_not_symptom)." >&2
  exit 1
fi
ok "    local stack healthy"

# ─── Resolve per-runtime provisioning secrets ──────────────────────────
# Mirrors test_priority_runtimes_e2e.sh / test_staging_full_saas.sh's
# provider-key chain. A runtime whose key is absent is SKIPPED (not
# failed) so a partially keyed local env doesn't false-fail the gate.
runtime_secrets() {
  local rt="$1"
  case "$rt" in
    claude-code)
      [ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] || { echo ""; return 1; }
      python3 -c "import json,os;print(json.dumps({'CLAUDE_CODE_OAUTH_TOKEN':os.environ['CLAUDE_CODE_OAUTH_TOKEN']}))"
      ;;
    hermes|openclaw)
      if [ -n "${E2E_MINIMAX_API_KEY:-}" ]; then
        python3 -c "import json,os;k=os.environ['E2E_MINIMAX_API_KEY'];print(json.dumps({'ANTHROPIC_BASE_URL':'https://api.minimax.io/anthropic','ANTHROPIC_AUTH_TOKEN':k,'MINIMAX_API_KEY':k}))"
      elif [ -n "${E2E_ANTHROPIC_API_KEY:-}" ]; then
        python3 -c "import json,os;k=os.environ['E2E_ANTHROPIC_API_KEY'];print(json.dumps({'ANTHROPIC_API_KEY':k}))"
      elif [ -n "${E2E_OPENAI_API_KEY:-}" ]; then
        python3 -c "import json,os;k=os.environ['E2E_OPENAI_API_KEY'];print(json.dumps({'OPENAI_API_KEY':k,'OPENAI_BASE_URL':'https://api.openai.com/v1','MODEL_PROVIDER':'openai:gpt-4o','HERMES_INFERENCE_PROVIDER':'custom','HERMES_CUSTOM_BASE_URL':'https://api.openai.com/v1','HERMES_CUSTOM_API_KEY':k,'HERMES_CUSTOM_API_MODE':'chat_completions'}))"
      else
        echo ""; return 1
      fi
      ;;
    *)
      # Unknown runtime: provision with empty secrets and let the stack
      # decide (kept permissive so PV_RUNTIMES can be widened later).
      echo "{}"
      ;;
  esac
}

choose_parent_runtime() {
  local rt
  if [ -n "${PV_PARENT_RUNTIME:-}" ]; then
    runtime_secrets "$PV_PARENT_RUNTIME" >/dev/null || return 1
    echo "$PV_PARENT_RUNTIME"
    return 0
  fi

  if runtime_secrets claude-code >/dev/null; then
    echo "claude-code"
    return 0
  fi

  for rt in $PV_RUNTIMES; do
    if runtime_secrets "$rt" >/dev/null; then
      echo "$rt"
      return 0
    fi
  done
  return 1
}

# Block until $1 reaches one of $2 (space-separated), or $3 sec elapse.
wait_for_status() {
  local wsid="$1" want="$2" budget="$3" start=$SECONDS last=""
  while [ $((SECONDS - start)) -lt "$budget" ]; do
    local s
    s=$(curl -s "$BASE/workspaces/$wsid" | python3 -c 'import json,sys
try:
  d=json.load(sys.stdin); w=d.get("workspace") if isinstance(d.get("workspace"),dict) else d; print(w.get("status",""))
except Exception:
  print("")' 2>/dev/null || echo "")
    [ "$s" != "$last" ] && { log "    $wsid → ${s:-<none>}"; last="$s"; }
    for w in $want; do [ "$s" = "$w" ] && { echo "$s"; return 0; }; done
    sleep 5
  done
  echo "$last"
  return 1
}

# ─── 1. Provision parent + one sibling per runtime ──────────────────────
# Same topology as the staging script: one parent plus one sibling per
# runtime under test, so each runtime should see all others. The default
# local backend uses external-mode rows because the literal MCP list_peers
# path is platform-local and must not depend on local template boot/heartbeat.
if [ "$PV_LOCAL_PROVISION_MODE" = "external" ]; then
  PARENT_RUNTIME="external"
  PARENT_SECRETS="{}"
  PARENT_EXTRA=',"external":true'
else
  # Container mode is still available for local runtime-boot debugging.
  # Prefer a claude-code parent for staging parity, but local CI is
  # intentionally allowed to be partially keyed; an unkeyed parent can
  # never heartbeat.
  PARENT_RUNTIME=$(choose_parent_runtime) || {
    echo "::error::No keyed runtime available for parent — cannot run the local peer-visibility gate. Set CLAUDE_CODE_OAUTH_TOKEN and/or E2E_MINIMAX_API_KEY (or ANTHROPIC/OPENAI)." >&2
    exit 1
  }
  PARENT_SECRETS=$(runtime_secrets "$PARENT_RUNTIME") || PARENT_SECRETS=""
  if [ -z "$PARENT_SECRETS" ]; then
    echo "::error::parent runtime $PARENT_RUNTIME has no provider secrets" >&2
    exit 1
  fi
  PARENT_EXTRA=""
fi
log "1/5 provisioning parent ($PARENT_RUNTIME, mode=$PV_LOCAL_PROVISION_MODE) + one sibling per runtime under test..."

# Map runtime → model per the CTO 2026-05-22 SSOT directive (model is
# required, no platform default). External runtimes are exempt by the
# Create-handler gate — for them the URL is the contract — but we still
# pass model="external:custom" defensively in case a downstream consumer
# of the create body asserts presence.
_model_for_runtime() {
  case "$1" in
    claude-code) echo "sonnet" ;;
    codex)       echo "gpt-5.5" ;;
    kimi)        echo "kimi-coding/kimi-k2-coding-6" ;;
    minimax)     echo "minimax/MiniMax-M2.7" ;;
    external)    echo "external:custom" ;;
    *)           echo "anthropic:claude-opus-4-7" ;;
  esac
}
PARENT_MODEL=$(_model_for_runtime "$PARENT_RUNTIME")
P_RESP=$(curl -s -X POST "$BASE/workspaces" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} -H "Content-Type: application/json" \
  -d "{\"name\":\"${NAME_PREFIX}-parent\",\"runtime\":\"$PARENT_RUNTIME\",\"model\":\"$PARENT_MODEL\",\"tier\":3$PARENT_EXTRA,\"secrets\":$PARENT_SECRETS}")
PARENT_ID=$(echo "$P_RESP" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))' 2>/dev/null)
# PARENT_TOKEN captured for symmetry with the per-sibling auth-token
# capture in the runtime loop below + reserved for follow-up steps
# that need parent-side auth. Current downstream steps reach the parent
# via admin token, so the variable isn't dereferenced — SC2034.
# shellcheck disable=SC2034 # captured for downstream parent-auth use; see #1644 follow-up
PARENT_TOKEN=$(echo "$P_RESP" | extract_auth_token)
if [ -z "$PARENT_ID" ]; then
  echo "::error::parent create failed: $(echo "$P_RESP" | head -c 300)" >&2
  exit 1
fi
CREATED_WSIDS+=("$PARENT_ID")
log "    PARENT_ID=$PARENT_ID runtime=$PARENT_RUNTIME"

# NOTE: no `declare -A` — this script must also run on a local macOS dev
# box (bash 3.2, no associative arrays) per feedback_local_must_mimic_
# production. WS_IDS / VERDICT are kept as newline-delimited "rt<TAB>val"
# maps with tiny get/set helpers (portable to bash 3.2+ AND ubuntu CI).
# shellcheck disable=SC2034 # map values are updated through portable eval-based helpers.
WS_IDS_MAP=""
# shellcheck disable=SC2034 # map values are updated through portable eval-based helpers.
VERDICT_MAP=""
# shellcheck disable=SC2034 # map values are updated through portable eval-based helpers.
WS_TOKENS_MAP=""
_map_set() { # _map_set <mapvarname> <key> <value>
  local __m="$1" __k="$2" __v="$3" __cur
  eval "__cur=\$$__m"
  __cur=$(printf '%s' "$__cur" | grep -v "^${__k}	" || true)
  if [ -n "$__cur" ]; then
    eval "$__m=\$(printf '%s\n%s\t%s' \"\$__cur\" \"\$__k\" \"\$__v\")"
  else
    eval "$__m=\$(printf '%s\t%s' \"\$__k\" \"\$__v\")"
  fi
}
_map_get() { # _map_get <mapvarname> <key>  -> stdout value (empty if absent)
  local __m="$1" __k="$2" __cur
  eval "__cur=\$$__m"
  printf '%s\n' "$__cur" | awk -F'\t' -v k="$__k" '$1==k {print $2; exit}'
}

ALL_WS_IDS="$PARENT_ID"
ACTIVE_RUNTIMES=""
for rt in $PV_RUNTIMES; do
  if [ "$PV_LOCAL_PROVISION_MODE" = "external" ]; then
    SEC="{}"
    CREATE_RUNTIME="external"
    CREATE_EXTRA=',"external":true'
  else
    SEC=$(runtime_secrets "$rt") || SEC=""
    if [ -z "$SEC" ]; then
      log "    SKIP $rt — no provider key in env (partially-keyed local env; not a failure)"
      continue
    fi
    CREATE_RUNTIME="$rt"
    CREATE_EXTRA=""
  fi
  CREATE_MODEL=$(_model_for_runtime "$CREATE_RUNTIME")
  R=$(curl -s -X POST "$BASE/workspaces" ${ADMIN_AUTH[@]+"${ADMIN_AUTH[@]}"} -H "Content-Type: application/json" \
    -d "{\"name\":\"${NAME_PREFIX}-$rt\",\"runtime\":\"$CREATE_RUNTIME\",\"model\":\"$CREATE_MODEL\",\"tier\":2,\"parent_id\":\"$PARENT_ID\"$CREATE_EXTRA,\"secrets\":$SEC}")
  WID=$(echo "$R" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))' 2>/dev/null)
  WTOK=$(echo "$R" | extract_auth_token)
  if [ -z "$WID" ]; then
    echo "::error::$rt workspace create failed: $(echo "$R" | head -c 300)" >&2
    exit 1
  fi
  if [ -z "$WTOK" ]; then
    echo "::error::$rt workspace create did not return an auth_token — cannot drive the literal MCP call" >&2
    exit 1
  fi
  _map_set WS_IDS_MAP "$rt" "$WID"
  _map_set WS_TOKENS_MAP "$rt" "$WTOK"
  CREATED_WSIDS+=("$WID")
  ALL_WS_IDS="$ALL_WS_IDS $WID"
  ACTIVE_RUNTIMES="$ACTIVE_RUNTIMES $rt"
  log "    $rt → $WID"
done
ACTIVE_RUNTIMES="$(echo "$ACTIVE_RUNTIMES" | xargs)"

if [ -z "$ACTIVE_RUNTIMES" ]; then
  echo "::error::No runtime had a provider key set — cannot run the local peer-visibility gate. Set CLAUDE_CODE_OAUTH_TOKEN and/or E2E_MINIMAX_API_KEY (or ANTHROPIC/OPENAI)." >&2
  exit 1
fi

# ─── 2. Wait for the parent online (it is a peer target) ───────────────
REGRESSED=0
ONLINE_RUNTIMES=""
if [ "$PV_LOCAL_PROVISION_MODE" = "external" ]; then
  log "2/5 external-mode local backend: parent is awaiting_agent; no container-online wait needed"
  ok "    parent created"
  log "3/5 external-mode local backend: siblings are awaiting_agent; driving MCP directly"
  ONLINE_RUNTIMES="$ACTIVE_RUNTIMES"
else
  log "2/5 waiting for parent online (peer target)..."
  PF=$(wait_for_status "$PARENT_ID" "online" "$PROVISION_TIMEOUT_SECS") || true
  if [ "$PF" != "online" ]; then
    echo "::error::parent ($PARENT_ID) never reached online (last=$PF) within ${PROVISION_TIMEOUT_SECS}s" >&2
    exit 3
  fi
  ok "    parent online"

  # ─── 3. Wait for every sibling online ──────────────────────────────────
  # A runtime that never comes online locally is itself a finding in
  # container mode. The default external mode keeps this gate focused on
  # literal MCP peer visibility.
  log "3/5 waiting for all siblings online (up to ${PROVISION_TIMEOUT_SECS}s each — cold boot)..."
  for rt in $ACTIVE_RUNTIMES; do
    wid="$(_map_get WS_IDS_MAP "$rt")"
    S=$(wait_for_status "$wid" "online" "$PROVISION_TIMEOUT_SECS") || true
    if [ "$S" != "online" ]; then
      echo "  ✗ $rt ($wid): never reached online (last=$S) — reproduces the never-online class locally"
      _map_set VERDICT_MAP "$rt" "FAIL(never-online:last=$S)"
      REGRESSED=1
      continue
    fi
    ok "    $rt online"
    ONLINE_RUNTIMES="$ONLINE_RUNTIMES $rt"
  done
fi

# ─── 4. THE GATE — literal mcp_molecule_list_peers via POST /:id/mcp ────
# Shared, byte-identical assertion. Local passes "" for the org id (the
# single-tenant local stack does not gate on X-Molecule-Org-Id); the
# literal MCP call + every anti-proxy / anti-native-fallback guarantee is
# the SAME code the staging backend runs.
log "4/5 driving the LITERAL list_peers MCP call per online runtime..."
echo ""
for rt in $ONLINE_RUNTIMES; do
  wid="$(_map_get WS_IDS_MAP "$rt")"
  WTOK="$(_map_get WS_TOKENS_MAP "$rt")"
  if [ -z "$WTOK" ]; then
    echo "--- $rt (ws=$wid) ---"
    echo "  ✗ $rt: workspace create did not return an auth_token — cannot drive the literal call"
    _map_set VERDICT_MAP "$rt" "FAIL(no-bearer)"
    REGRESSED=1
    echo ""
    continue
  fi
  PV_VERDICT=""
  pv_assert_runtime "$rt" "$wid" "$WTOK" "$BASE" "" "$ALL_WS_IDS" || REGRESSED=1
  _map_set VERDICT_MAP "$rt" "$PV_VERDICT"
  echo ""
done

# ─── 5. Summary + honest gate exit ─────────────────────────────────────
echo "=== SUMMARY — LOCAL fresh-provision peer-visibility (literal MCP list_peers) ==="
for rt in $ACTIVE_RUNTIMES; do
  _v="$(_map_get VERDICT_MAP "$rt")"
  printf '  %-14s %s\n' "$rt" "${_v:-NO_RUN}"
done
echo ""

if [ "$REGRESSED" -ne 0 ]; then
  echo "✗ GATE FAILED (LOCAL) — at least one runtime cannot see its peers via"
  echo "  the literal mcp_molecule_list_peers call on the local prod-mimic"
  echo "  stack. This is the SAME user-facing failure the proxy signals were"
  echo "  hiding, reproduced locally (far faster than EC2). Expected RED until"
  echo "  the Hermes-401 (#162) + OpenClaw-never-online/MCP-wiring (#165)"
  echo "  root-cause fixes land; goes green only when they actually do."
  exit 10
fi

ok "GATE PASSED (LOCAL) — every runtime under test sees its platform peers via the literal MCP call."
exit 0
