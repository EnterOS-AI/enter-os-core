#!/usr/bin/env bash
#
# Measure platform-side bounds (or absence thereof) on a coordinator's
# task execution. Reproduction harness for Issue 4 of the 2026-04-28
# CP review, surfaced in the RFC at molecule-core#2251.
#
# What Issue 4 hypothesized
# -------------------------
# A coordinator workspace receives an A2A kickoff, delegates to children,
# then enters a synthesis phase whose duration the platform does not
# bound. `DELEGATION_TIMEOUT` (300s, in workspace/builtin_tools/
# delegation.py) governs the parent→child HTTP request, NOT the
# coordinator's own task-execution budget. So a coordinator that's
# spent 10min synthesizing past delegation will keep going until the
# LLM returns or its host runtime crashes — never bounded by a platform
# ceiling.
#
# Issue 4 explicitly hedged ("This isn't necessarily a platform bug —
# could be that the Design Director's system prompt told it to do
# complex synthesis work that exceeded the A2A response window"). This
# script is the empirical test of which side that ambiguity lands on.
#
# What this script does NOT do
# ----------------------------
# - It does NOT assert pass/fail. The "bug" is absence-of-bound, which
#   is hard to assert in a single run. The script outputs measurement
#   data; the team interprets.
# - It does NOT simulate a coordinator hang via runtime modification.
#   Instead, it drives a real coordinator with a synthesis-heavy task
#   and observes the duration the platform tolerates.
# - It does NOT clean up on failure. Use scripts/cleanup-rogue-workspaces.sh.
#
# What "bug confirmed" looks like (per Issue 4)
# ---------------------------------------------
#   coordinator_response_secs > 300 AND no platform_intervention=true
#   in the heartbeat trace → coordinator ran past DELEGATION_TIMEOUT
#   (HTTP-level) without any platform ceiling kicking in. The RFC's
#   V1.0 operator ceiling would convert this into an explicit
#   `terminated` response at MAX_TASK_EXECUTION_SECS.
#
# What "bug refuted" looks like
# -----------------------------
#   coordinator_response_secs cleanly bounded by either the LLM API
#   timeout or some other platform mechanism → Issue 4's premise that
#   "no platform-enforced timeout" is wrong, V1.0 of the RFC needs
#   re-justification.
#
# Usage
# -----
#   PLATFORM=http://localhost:8080 OPENROUTER_API_KEY=... \
#     bash scripts/measure-coordinator-task-bounds.sh
#
# Or against staging-api (requires a tenant admin token):
#
#   PLATFORM=https://your-staging-tenant.example \
#   OPENROUTER_API_KEY=... \
#     bash scripts/measure-coordinator-task-bounds.sh
#
set -euo pipefail

PLATFORM="${PLATFORM:-http://localhost:8080}"
OR_KEY="${OPENROUTER_API_KEY:-${OPENAI_API_KEY:?Set OPENROUTER_API_KEY (or OPENAI_API_KEY)}}"
# Synthesis prompt knob — choose the size of the post-delegation work
# the coordinator is asked to do. Default exercises 3 delegation rounds
# with non-trivial aggregation.
SYNTHESIS_DEPTH="${SYNTHESIS_DEPTH:-3}"
# Max time we'll wait on the coordinator's A2A response before giving
# up on this measurement. Set generously (10min) so we don't truncate
# a slow-but-eventually-completing case.
A2A_TIMEOUT="${A2A_TIMEOUT:-600}"

ts() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ; }

emit() {
  # One JSON line per event so the output is machine-readable.
  printf '{"ts":"%s","event":"%s","data":%s}\n' "$(ts)" "$1" "${2:-null}"
}

emit "run_started" "{\"platform\":\"$PLATFORM\",\"synthesis_depth\":$SYNTHESIS_DEPTH,\"a2a_timeout_secs\":$A2A_TIMEOUT}"

# ---- Setup: coordinator + 1 child ----
emit "provisioning_pm" null
R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"PM","role":"Coordinator — delegates and synthesizes","tier":2,"template":"claude-code-default"}')
PM_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))")
[ -n "$PM_ID" ] || { echo "ERROR: PM create failed: $R" >&2; exit 1; }
emit "pm_provisioned" "{\"workspace_id\":\"$PM_ID\"}"

emit "provisioning_child" null
R=$(curl -s -X POST "$PLATFORM/workspaces" -H 'Content-Type: application/json' \
  -d '{"name":"Researcher","role":"Returns short research findings","tier":2,"template":"langgraph"}')
CHILD_ID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))")
[ -n "$CHILD_ID" ] || { echo "ERROR: child create failed: $R" >&2; exit 1; }
emit "child_provisioned" "{\"workspace_id\":\"$CHILD_ID\"}"

curl -s -X PATCH "$PLATFORM/workspaces/$CHILD_ID" -H 'Content-Type: application/json' \
  -d "{\"parent_id\":\"$PM_ID\"}" > /dev/null
curl -s -X POST "$PLATFORM/workspaces/$CHILD_ID/secrets" -H 'Content-Type: application/json' \
  -d "{\"key\":\"OPENROUTER_API_KEY\",\"value\":\"$OR_KEY\"}" > /dev/null

# ---- Wait for both online ----
wait_online() {
  local id="$1"; local label="$2"
  for i in $(seq 1 30); do
    s=$(curl -s "$PLATFORM/workspaces/$id" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
    [ "$s" = "online" ] && { emit "online" "{\"workspace\":\"$label\",\"after_polls\":$i}"; return 0; }
    sleep 3
  done
  emit "online_timeout" "{\"workspace\":\"$label\"}"
  return 1
}
wait_online "$PM_ID"    "PM"    || exit 2
wait_online "$CHILD_ID" "child" || exit 2

# ---- Build a synthesis-heavy kickoff task ----
# The task asks the coordinator to delegate N times, each time with a
# different sub-question, then aggregate findings into a single report.
# The synthesis phase happens entirely inside the coordinator's A2A
# handler post-delegation, which is the exact code path Issue 4 named.
TASK="You are coordinating a research analysis. Delegate $SYNTHESIS_DEPTH separate sub-questions to the Researcher (one at a time, sequentially — wait for each response before sending the next), then synthesize all findings into a single coherent report. Sub-questions: (a) historical context of distributed consensus, (b) modern Byzantine-fault-tolerant protocols, (c) practical trade-offs between Raft and Paxos. After all delegations complete, write a 600-word synthesis comparing the three responses and drawing one cross-cutting insight. Do not respond until the synthesis is complete."

# ---- Time the A2A kickoff round-trip ----
emit "a2a_kickoff_sent" "{\"to\":\"$PM_ID\",\"task_chars\":${#TASK}}"
START_NS=$(python3 -c 'import time; print(int(time.time_ns()))')

# Use --max-time to bound this measurement (else the script could itself
# hang past sensible limits). The bound is a measurement-side timeout,
# NOT a platform-side timeout — the latter is what we're trying to
# detect.
RESP=$(curl -s --max-time "$A2A_TIMEOUT" -X POST "$PLATFORM/workspaces/$PM_ID/a2a" \
  -H "Content-Type: application/json" \
  -d "$(python3 -c "
import json,sys
print(json.dumps({
  'method':'message/send',
  'params':{
    'message':{
      'role':'user',
      'parts':[{'type':'text','text':sys.argv[1]}]
    }
  }
}))
" "$TASK")" || RESP="<curl_failed_or_timed_out>")

END_NS=$(python3 -c 'import time; print(int(time.time_ns()))')
ELAPSED_SECS=$(python3 -c "print(round(($END_NS - $START_NS) / 1e9, 2))")

emit "a2a_response_observed" "{\"elapsed_secs\":$ELAPSED_SECS,\"response_chars\":${#RESP},\"response_head\":$(python3 -c "import json,sys; print(json.dumps(sys.argv[1][:200]))" "$RESP")}"

# ---- Pull heartbeat trace from the platform ----
# The heartbeat endpoint records workspace liveness pings. If the
# platform implements per-task bounds, the trace will show a status
# transition (e.g. terminated) within the run window. Absence of any
# such transition over a 10min synthesis is the empirical evidence
# that no platform ceiling fired.
emit "fetching_heartbeat_trace" null
HB=$(curl -s "$PLATFORM/workspaces/$PM_ID/heartbeat-history?since_secs=$A2A_TIMEOUT" 2>&1 || echo "<endpoint_unavailable>")
emit "heartbeat_trace" "{\"raw\":$(python3 -c "import json,sys; print(json.dumps(sys.argv[1]))" "$HB")}"

# ---- Summary ----
emit "run_completed" "{\"elapsed_secs\":$ELAPSED_SECS,\"pm_id\":\"$PM_ID\",\"child_id\":\"$CHILD_ID\"}"

cat <<EOF >&2

=========================================
  Measurement complete.
  Coordinator response time: ${ELAPSED_SECS}s
  PM workspace:    $PM_ID
  Child workspace: $CHILD_ID
=========================================

Interpretation guide:

  ELAPSED_SECS < 60   → Synthesis completed quickly; not informative
                        about platform bounds (LLM was just fast).
                        Re-run with SYNTHESIS_DEPTH=8 to force longer
                        synthesis.

  60 <= ELAPSED < 300 → Within DELEGATION_TIMEOUT. Doesn't prove or
                        refute Issue 4 — the HTTP-level timeout would
                        be sufficient if synthesis happened to fall
                        under it.

  ELAPSED >= 300      → BUG CONFIRMED IF heartbeat_trace shows no
                        platform-side transition. Coordinator ran past
                        DELEGATION_TIMEOUT without any platform ceiling
                        kicking in — exactly the gap the RFC V1.0 plans
                        to close with MAX_TASK_EXECUTION_SECS.

  curl_failed_or_timed_out → \$A2A_TIMEOUT exceeded. Either the
                        coordinator is genuinely hung (likely) or
                        synthesis is just very slow. Pull workspace
                        status separately to disambiguate.

Cleanup:
  curl -X DELETE $PLATFORM/workspaces/$PM_ID
  curl -X DELETE $PLATFORM/workspaces/$CHILD_ID

EOF
