#!/usr/bin/env bash
# run-canary.sh — one-shot orchestration for the local-e2e session-continuity
# canary harness. Used by both interactive local runs and the per-template
# .gitea/workflows/session-continuity-e2e.yml.
#
# Usage:
#   TEMPLATE_IMAGE=ghcr.io/molecule-ai/workspace-template-hermes:latest \
#       ./local-e2e/scripts/run-canary.sh
#
# Optional env:
#   CANARY_RUN_ID   — disambiguator for parallel CI runs (default: random)
#   RUNTIME_PORT    — host port for runtime :8000 (default: 18000)
#   KEEP_RUNNING    — set =1 to leave containers up for post-mortem
#
# Exit codes:
#   0   — all 4 canaries passed
#   1   — at least one canary failed (artifacts/ has the dump)
#   2   — harness infrastructure failure (image pull / compose / etc.)
#
# Cross-refs:
#   feedback_image_promote_is_not_user_live — we verify at the running
#     container layer, NOT at the pipeline-green layer.
#   feedback_verify_actual_endstate_not_ack_follow_sop — every assert
#     reads state back; no side-effect-ack claims success.

set -euo pipefail

: "${TEMPLATE_IMAGE:?TEMPLATE_IMAGE env required (the runtime image under test)}"

# ----------------------------------------------------------------- paths
HARNESS_ROOT="$( cd "$( dirname "${BASH_SOURCE[0]}" )/.." && pwd )"
ARTIFACTS_DIR="$HARNESS_ROOT/artifacts"
mkdir -p "$ARTIFACTS_DIR"

export CANARY_RUN_ID="${CANARY_RUN_ID:-$(uuidgen 2>/dev/null | tr A-Z a-z | tr -d - | cut -c1-12 || date +%s)}"
export RUNTIME_PORT="${RUNTIME_PORT:-18000}"
export TEMPLATE_IMAGE
COMPOSE_PROJECT="canary-${CANARY_RUN_ID}"
COMPOSE_FILE="$HARNESS_ROOT/docker-compose.yml"

log() { printf "\n=== [%s] %s ===\n" "$(date +%H:%M:%S)" "$*"; }

# ----------------------------------------------------------- cleanup hook
cleanup() {
    local rc=$?
    if [ "${KEEP_RUNNING:-0}" = "1" ]; then
        log "KEEP_RUNNING=1 — leaving containers up (project=$COMPOSE_PROJECT)"
        return $rc
    fi
    log "Tearing down compose project $COMPOSE_PROJECT"
    # On non-zero exit, capture logs FIRST. Per feedback_image_promote_is_
    # not_user_live: dump state from the actually-running container, not
    # an inferred pipeline state.
    if [ $rc -ne 0 ]; then
        log "Canary FAILED — dumping artifacts to $ARTIFACTS_DIR"
        docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" logs \
            --no-color --tail=200 runtime \
            > "$ARTIFACTS_DIR/runtime.log" 2>&1 || true
        # SessionStore state probe — runtime exposes /admin/session-store
        # in canary mode; if not present this 404s and the file is empty.
        docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" exec -T runtime \
            sh -c 'ls -la /tmp/canary-memory 2>/dev/null; find /tmp -name "session*.json" -exec cat {} \; 2>/dev/null' \
            > "$ARTIFACTS_DIR/session-store.txt" 2>&1 || true
    fi
    docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" down --volumes --remove-orphans >/dev/null 2>&1 || true
    return $rc
}
trap cleanup EXIT

# ------------------------------------------------------ stack bring-up
log "Building cp_sim image"
docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" build cp_sim

log "Pulling runtime image: $TEMPLATE_IMAGE"
docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" pull runtime 2>&1 \
    | tail -5 || true

log "Starting runtime (host port $RUNTIME_PORT)"
docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" up -d runtime

# Wait for healthcheck — docker-compose `--wait` is the canonical mechanism
# (introduced in v2.1.1 in 2021, available on every supported runner pool).
log "Waiting for runtime healthcheck"
if ! docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" up -d --wait runtime; then
    log "Runtime never went healthy — dumping logs"
    docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" logs --no-color --tail=200 runtime \
        > "$ARTIFACTS_DIR/runtime-boot-failure.log" 2>&1 || true
    exit 2
fi

# -------------------------------------------------------------- run tests
log "Running canary suite"
# Run cp_sim under the same compose project so DNS (runtime hostname)
# resolves on the molecule-core-net bridge. --rm cleans the driver container
# after pytest exits; volume bind mounts pytest's junit-xml back to host.
if docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" --profile driver run \
        --rm \
        -v "$ARTIFACTS_DIR:/harness/artifacts" \
        cp_sim; then
    log "All canaries PASSED"
    exit 0
else
    log "At least one canary FAILED — see $ARTIFACTS_DIR/junit.xml"
    exit 1
fi
