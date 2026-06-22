#!/usr/bin/env bash
# Standalone runner for Issue 4 reproduction (RFC #2251) — exists alongside
# `measure-coordinator-task-bounds.sh` to support arbitrary template + secret
# combinations without modifying the canonical harness. The canonical harness
# stays focused on its v1 contract (claude-code-default + claude-code + OpenRouter);
# this runner wraps the same workspace-server API calls but takes everything as
# env-var inputs so a Hermes/MiniMax run can share the measurement code path.
#
# Two routing modes:
#   MODE=local (default) — direct workspace-server API
#   MODE=saas            — placeholder; populates same vars but expects
#                          PLATFORM=<tenant-subdomain> with X-Tenant-Id +
#                          Authorization headers from CP_ADMIN_API_TOKEN
#
# Required env:
#   PLATFORM            workspace-server base URL (default http://localhost:8080)
#   PM_TEMPLATE         template slug for coordinator
#   CHILD_TEMPLATE      template slug for researcher child
#   SECRET_NAME         workspace_secrets key (e.g. MINIMAX_API_KEY)
#   SECRET_VALUE        the secret value (or read from $SECRET_NAME if unset)
#
# Optional:
#   MODEL               PUT /workspaces/:id/model after provision
#   SYNTHESIS_DEPTH=3   number of delegation rounds in the kickoff task
#   A2A_TIMEOUT=600     ceiling on measurement-side wait (seconds)
#   KEEP_WORKSPACES=0   skip cleanup-on-exit when 1 (for log inspection)
#   MODE=local|saas     local-dev vs SaaS routing posture
#   CP_ADMIN_API_TOKEN  required when MODE=saas; sent as Authorization bearer
#   TENANT_ID           required when MODE=saas; sent as X-Tenant-Id
#
# Output: NDJSON event stream on stdout + a human summary on stderr.
#
set -euo pipefail

PLATFORM="${PLATFORM:-http://localhost:8080}"
MODE="${MODE:-local}"
PM_TEMPLATE="${PM_TEMPLATE:?PM_TEMPLATE is required (e.g. claude-code-default, hermes)}"
CHILD_TEMPLATE="${CHILD_TEMPLATE:?CHILD_TEMPLATE is required}"
SECRET_NAME="${SECRET_NAME:?SECRET_NAME is required (e.g. MINIMAX_API_KEY)}"
MODEL="${MODEL:-}"
SYNTHESIS_DEPTH="${SYNTHESIS_DEPTH:-3}"
A2A_TIMEOUT="${A2A_TIMEOUT:-600}"
KEEP_WORKSPACES="${KEEP_WORKSPACES:-0}"

# SaaS-mode auth chain: workspace-server (per-tenant Go binary on EC2)
# requires BOTH headers:
#   Authorization: Bearer <tenant-admin-token>      (per-tenant secret)
#   X-Molecule-Org-Id:  <org-uuid>                  (TenantGuard middleware)
# The tenant-admin-token is provisioned by controlplane and retrievable via:
#   GET /cp/admin/orgs/<slug>/admin-token   (CP_ADMIN_API_TOKEN bearer-gated)
# The runner can either:
#   1. Take ORG_SLUG + CP_ADMIN_API_TOKEN and fetch the tenant token itself, or
#   2. Take ORG_ID + TENANT_ADMIN_TOKEN directly.
ORG_ID="${ORG_ID:-}"
ORG_SLUG="${ORG_SLUG:-}"
TENANT_ADMIN_TOKEN="${TENANT_ADMIN_TOKEN:-}"
CP_ADMIN_API_TOKEN="${CP_ADMIN_API_TOKEN:-}"
CP_API_URL="${CP_API_URL:-https://staging-api.moleculesai.app}"

# Resolve secret value: ${SECRET_VALUE} > $${SECRET_NAME} > error.
SECRET_VALUE="${SECRET_VALUE:-}"
if [ -z "$SECRET_VALUE" ]; then
  SECRET_VALUE="$(printenv "$SECRET_NAME" 2>/dev/null || true)"
fi
[ -n "$SECRET_VALUE" ] || { echo "ERROR: set \$$SECRET_NAME or \$SECRET_VALUE" >&2; exit 1; }

# SaaS-mode preflight + format validation.
# Validating ORG_ID + ORG_SLUG client-side gives an actionable error
# before the request hits TenantGuard's intentionally-opaque 404
# (which doesn't tell the operator whether the slug is wrong, the
# UUID is wrong, or auth is wrong).
if [ "$MODE" = "saas" ]; then
  [ -n "$ORG_ID" ] || { echo "ERROR: MODE=saas requires ORG_ID (the org UUID)" >&2; exit 1; }
  case "$ORG_ID" in
    [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
    *) echo "ERROR: ORG_ID must be a UUID (got '$ORG_ID')" >&2; exit 1;;
  esac
  if [ -n "$ORG_SLUG" ]; then
    case "$ORG_SLUG" in
      *[!a-z0-9-]* | -* | *-) echo "ERROR: ORG_SLUG must match ^[a-z0-9][a-z0-9-]*[a-z0-9]\$ (got '$ORG_SLUG')" >&2; exit 1;;
    esac
  fi
  if [ -z "$TENANT_ADMIN_TOKEN" ]; then
    [ -n "$ORG_SLUG" ]          || { echo "ERROR: MODE=saas needs TENANT_ADMIN_TOKEN or ORG_SLUG (to fetch it via CP)" >&2; exit 1; }
    [ -n "$CP_ADMIN_API_TOKEN" ] || { echo "ERROR: ORG_SLUG path needs CP_ADMIN_API_TOKEN to fetch tenant token from $CP_API_URL" >&2; exit 1; }
    TENANT_ADMIN_TOKEN=$(curl -s -H "Authorization: Bearer $CP_ADMIN_API_TOKEN" \
      "$CP_API_URL/cp/admin/orgs/$ORG_SLUG/admin-token" \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null || echo "")
    [ -n "$TENANT_ADMIN_TOKEN" ] || { echo "ERROR: failed to resolve tenant admin token via $CP_API_URL/cp/admin/orgs/$ORG_SLUG/admin-token" >&2; exit 1; }
  fi
fi

ts() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ; }
emit() { printf '{"ts":"%s","event":"%s","data":%s}\n' "$(ts)" "$1" "${2:-null}"; }

api() {
  local args=()
  if [ "$MODE" = "saas" ]; then
    args+=(-H "Authorization: Bearer $TENANT_ADMIN_TOKEN")
    args+=(-H "X-Molecule-Org-Id: $ORG_ID")
  fi
  curl -s ${args[@]+"${args[@]}"} "$@"
}

PM_ID=""
CHILD_ID=""
cleanup() {
  local rc=$?
  set +e
  if [ "$KEEP_WORKSPACES" = "1" ]; then
    emit "cleanup_skipped" "{\"reason\":\"KEEP_WORKSPACES=1\",\"pm_id\":\"$PM_ID\",\"child_id\":\"$CHILD_ID\"}"
    return $rc
  fi
  for id in "$CHILD_ID" "$PM_ID"; do
    [ -z "$id" ] && continue
    code=$(api -o /dev/null -w '%{http_code}' -X DELETE "$PLATFORM/workspaces/$id" 2>/dev/null || echo "curl_err")
    if [ "$code" = "200" ] || [ "$code" = "204" ] || [ "$code" = "404" ]; then
      emit "cleanup_deleted" "{\"workspace_id\":\"$id\",\"http_code\":\"$code\"}"
    else
      emit "cleanup_failed" "{\"workspace_id\":\"$id\",\"http_code\":\"$code\"}"
    fi
  done
  return $rc
}
trap cleanup EXIT INT TERM

emit "run_started" "{\"platform\":\"$PLATFORM\",\"mode\":\"$MODE\",\"pm_template\":\"$PM_TEMPLATE\",\"child_template\":\"$CHILD_TEMPLATE\",\"model\":\"$MODEL\",\"secret_name\":\"$SECRET_NAME\",\"synthesis_depth\":$SYNTHESIS_DEPTH,\"a2a_timeout_secs\":$A2A_TIMEOUT}"

# ---- Provision via JSON-encoded bodies (defends against templates/values
# with embedded shell-special chars). ----
pm_body=$(python3 -c '
import json, sys
print(json.dumps({"name":"PM","role":"Coordinator — delegates and synthesizes","tier":2,"template":sys.argv[1]}))' "$PM_TEMPLATE")
child_body=$(python3 -c '
import json, sys
print(json.dumps({"name":"Researcher","role":"Returns short research findings","tier":2,"template":sys.argv[1]}))' "$CHILD_TEMPLATE")
secret_body=$(python3 -c '
import json, sys
print(json.dumps({"key":sys.argv[1],"value":sys.argv[2]}))' "$SECRET_NAME" "$SECRET_VALUE")

emit "provisioning_pm" "{\"template\":\"$PM_TEMPLATE\"}"
R=$(api -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' -d "$pm_body")
PM_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
[ -n "$PM_ID" ] || { echo "ERROR: PM create failed — response: $R" >&2; exit 1; }
emit "pm_provisioned" "{\"workspace_id\":\"$PM_ID\"}"

emit "provisioning_child" "{\"template\":\"$CHILD_TEMPLATE\"}"
R=$(api -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' -d "$child_body")
CHILD_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
[ -n "$CHILD_ID" ] || { echo "ERROR: child create failed — response: $R" >&2; exit 1; }
emit "child_provisioned" "{\"workspace_id\":\"$CHILD_ID\"}"

api -X PATCH "$PLATFORM/workspaces/$CHILD_ID" -H 'Content-Type: application/json' \
  -d "{\"parent_id\":\"$PM_ID\"}" > /dev/null

# Seed secret on BOTH workspaces. Hermes/MiniMax both sides need it; templates
# that ignore unknown env vars treat extras as no-op.
for id in "$PM_ID" "$CHILD_ID"; do
  api -X POST "$PLATFORM/workspaces/$id/secrets" -H 'Content-Type: application/json' -d "$secret_body" > /dev/null
done
emit "secrets_seeded" "{\"key\":\"$SECRET_NAME\",\"workspaces\":[\"$PM_ID\",\"$CHILD_ID\"]}"

if [ -n "$MODEL" ]; then
  model_body=$(python3 -c 'import json,sys; print(json.dumps({"model":sys.argv[1]}))' "$MODEL")
  for id in "$PM_ID" "$CHILD_ID"; do
    api -X PUT "$PLATFORM/workspaces/$id/model" -H 'Content-Type: application/json' -d "$model_body" > /dev/null
  done
  emit "model_set" "{\"model\":\"$MODEL\",\"workspaces\":[\"$PM_ID\",\"$CHILD_ID\"]}"
fi

# ---- Wait for both online ----
WAIT_ONLINE_SECS="${WAIT_ONLINE_SECS:-180}"
wait_online() {
  local id="$1" label="$2"
  # Round up so a non-multiple-of-3 budget waits at least the requested
  # seconds (200 → 67 polls × 3s = 201s, not 198s).
  local polls=$(( (WAIT_ONLINE_SECS + 2) / 3 ))
  local last_status=""
  for i in $(seq 1 "$polls"); do
    s=$(api "$PLATFORM/workspaces/$id" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
    if [ "$s" != "$last_status" ]; then
      emit "status_change" "{\"workspace\":\"$label\",\"from\":\"$last_status\",\"to\":\"$s\",\"poll\":$i}"
      last_status="$s"
    fi
    [ "$s" = "online" ] && { emit "online" "{\"workspace\":\"$label\",\"after_polls\":$i,\"after_secs\":$((i * 3))}"; return 0; }
    [ "$s" = "failed" ] && { emit "failed" "{\"workspace\":\"$label\"}"; return 1; }
    sleep 3
  done
  emit "online_timeout" "{\"workspace\":\"$label\",\"last_status\":\"$last_status\",\"waited_secs\":$WAIT_ONLINE_SECS}"
  return 1
}
wait_online "$PM_ID"    "PM"    || exit 2
wait_online "$CHILD_ID" "child" || exit 2

# ---- Build a synthesis-heavy kickoff task ----
TASK="You are coordinating a research analysis. Delegate $SYNTHESIS_DEPTH separate sub-questions to the Researcher (one at a time, sequentially — wait for each response before sending the next), then synthesize all findings into a single coherent report. Sub-questions: (a) historical context of distributed consensus, (b) modern Byzantine-fault-tolerant protocols, (c) practical trade-offs between Raft and Paxos. After all delegations complete, write a 600-word synthesis comparing the three responses and drawing one cross-cutting insight. Do not respond until the synthesis is complete."

# ---- A2A kickoff round-trip ----
emit "a2a_kickoff_sent" "{\"to\":\"$PM_ID\",\"task_chars\":${#TASK}}"
START_NS=$(python3 -c 'import time; print(int(time.time_ns()))')

a2a_body=$(python3 -c '
import json, sys
print(json.dumps({"method":"message/send","params":{"message":{"role":"user","parts":[{"type":"text","text":sys.argv[1]}]}}}))' "$TASK")

RESP=$(api --max-time "$A2A_TIMEOUT" -X POST "$PLATFORM/workspaces/$PM_ID/a2a" \
  -H "Content-Type: application/json" -d "$a2a_body" || echo "<curl_failed_or_timed_out>")

END_NS=$(python3 -c 'import time; print(int(time.time_ns()))')
ELAPSED_SECS=$(python3 -c "print(round(($END_NS - $START_NS) / 1e9, 2))")

emit "a2a_response_observed" "{\"elapsed_secs\":$ELAPSED_SECS,\"response_chars\":${#RESP},\"response_head\":$(python3 -c "import json,sys; print(json.dumps(sys.argv[1][:200]))" "$RESP")}"

# ---- Activity trace ----
# Earlier versions of this runner called /workspaces/:id/heartbeat-history,
# which doesn't exist on workspace-server. On local dev that returned 404,
# on tenant builds the platform's canvas-proxy fallback intercepted it and
# returned 28KB of Next.js HTML — neither of which is useful trace data.
# /workspaces/:id/activity is the existing endpoint that reads the
# activity_logs table (a2a_send / a2a_receive / task_update / agent_log /
# error events with duration_ms + status). That's the data the RFC's
# §V1.0 step 6 'platform-side transition' check actually needs.
emit "fetching_activity_trace" "{\"mode\":\"$MODE\"}"
ACTIVITY=$(api "$PLATFORM/workspaces/$PM_ID/activity?since_secs=$A2A_TIMEOUT" 2>&1 || echo "<endpoint_unavailable>")
emit "activity_trace" "{\"raw\":$(python3 -c "import json,sys; print(json.dumps(sys.argv[1]))" "$ACTIVITY")}"

# ---- rfc2251_phase log lines from the workspace container ----
# Local Docker provisioner: workspace container name is workspace-<id>.
# SaaS: container is on EC2 — skip log capture, fall back to heartbeat only.
if [ "$MODE" = "local" ] && command -v docker >/dev/null 2>&1; then
  for id in "$PM_ID"; do
    container=$(docker ps --filter "name=workspace-$id" --format '{{.Names}}' | head -1)
    if [ -n "$container" ]; then
      phase_log=$(docker logs --since "${A2A_TIMEOUT}s" "$container" 2>&1 | grep 'rfc2251_phase=' || echo "<no rfc2251_phase log lines — container running stale image without #2255 instrumentation>")
      emit "phase_log" "{\"workspace_id\":\"$id\",\"container\":\"$container\",\"raw\":$(python3 -c "import json,sys; print(json.dumps(sys.argv[1]))" "$phase_log")}"
    fi
  done
fi

emit "run_completed" "{\"elapsed_secs\":$ELAPSED_SECS,\"pm_id\":\"$PM_ID\",\"child_id\":\"$CHILD_ID\"}"

cat <<EOF >&2

=========================================
  Measurement complete. (RFC #2251 / Issue 4 repro)
  Mode:                  $MODE
  Coordinator template:  $PM_TEMPLATE
  Child template:        $CHILD_TEMPLATE
  Model:                 ${MODEL:-<template default>}
  Coordinator response:  ${ELAPSED_SECS}s
  PM workspace:          $PM_ID
  Child workspace:       $CHILD_ID
=========================================

Interpretation:

  ELAPSED < 60   → Synthesis fast; not informative about platform bounds.
                   Re-run with SYNTHESIS_DEPTH=8 for longer synthesis.

  60 <= ELAPSED < 300 → Within DELEGATION_TIMEOUT. Doesn't prove or refute
                   Issue 4 — HTTP-level timeout would be sufficient.

  ELAPSED >= 300 → BUG CONFIRMED IF activity_trace shows no platform-side
                   transition. Coordinator ran past DELEGATION_TIMEOUT without
                   any platform ceiling kicking in — exactly the gap V1.0
                   plans to close with MAX_TASK_EXECUTION_SECS.

  curl_failed_or_timed_out → \$A2A_TIMEOUT exceeded. Coordinator likely hung
                   or synthesis is just very slow.

EOF
