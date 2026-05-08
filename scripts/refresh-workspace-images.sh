#!/usr/bin/env bash
# refresh-workspace-images.sh — pull the latest workspace template images
# from GHCR and recreate any running ws-* containers against the new digest.
#
# This is the local-dev / single-host equivalent of step 5 of the runtime CD
# chain (see docs/workspace-runtime-package.md). On a SaaS deployment the
# host's deploy pipeline does the pull on every release; this script is
# what to run on a local docker-compose host after a runtime release lands.
#
# Usage:
#   bash scripts/refresh-workspace-images.sh                     # pull all 8 + recreate running ws-*
#   bash scripts/refresh-workspace-images.sh --runtime claude-code  # pull just one template
#   bash scripts/refresh-workspace-images.sh --no-recreate          # pull only, leave containers
#
# Behavior:
#   - Always pulls fresh; docker is a no-op if local matches remote, so
#     repeated runs are cheap.
#   - Recreate is "kill + remove + let the next canvas interaction re-
#     provision" — simpler than `docker stop / docker run` because the
#     platform owns the run flags. Workspaces re-register on next probe.
#   - If a container is mid-conversation, the kill cancels in-flight work.
#     Run during a quiet window OR add --no-recreate and recreate manually
#     via canvas Restart buttons.

set -euo pipefail

GREEN='\033[0;32m'
YELLOW='\033[0;33m'
RED='\033[0;31m'
NC='\033[0m'
log()  { echo -e "${GREEN}[refresh]${NC} $1" >&2; }
warn() { echo -e "${YELLOW}[refresh]${NC} $1" >&2; }
err()  { echo -e "${RED}[refresh]${NC} $1" >&2; }

ALL_RUNTIMES=(claude-code langgraph crewai autogen deepagents hermes gemini-cli openclaw)
RUNTIMES=("${ALL_RUNTIMES[@]}")
RECREATE=true

while [ $# -gt 0 ]; do
  case "$1" in
    --runtime) RUNTIMES=("$2"); shift 2;;
    --no-recreate) RECREATE=false; shift;;
    -h|--help) sed -n '2,30p' "$0"; exit 0;;
    *) err "unknown arg: $1"; exit 2;;
  esac
done

# 1. Pull fresh tags. Soft-fail per runtime — one missing image (e.g., a
#    template that hasn't been published yet) shouldn't abort the others.
log "pulling latest images for: ${RUNTIMES[*]}"
PULLED=()
FAILED=()
for rt in "${RUNTIMES[@]}"; do
  IMG="153263036946.dkr.ecr.us-east-2.amazonaws.com/molecule-ai/workspace-template-$rt:latest"
  if docker pull "$IMG" >/dev/null 2>&1; then
    log "  ✓ $rt"
    PULLED+=("$rt")
  else
    warn "  ✗ $rt (pull failed — image may not exist or auth missing)"
    FAILED+=("$rt")
  fi
done

if [ "$RECREATE" = "false" ]; then
  log "skip-recreate set — leaving containers untouched"
  log "done. pulled=${#PULLED[@]} failed=${#FAILED[@]}"
  exit 0
fi

# 2. Find ws-* containers whose image is one of the runtimes we pulled.
#    `docker inspect` exposes the image tag/digest each was created from.
log "scanning ws-* containers for stale images..."
TO_RECREATE=()
for cn in $(docker ps -a --filter "name=ws-" --format "{{.Names}}"); do
  IMG=$(docker inspect "$cn" --format '{{.Config.Image}}' 2>/dev/null || echo "")
  for rt in "${PULLED[@]}"; do
    if [[ "$IMG" == *"workspace-template-$rt"* ]]; then
      TO_RECREATE+=("$cn")
      break
    fi
  done
done

if [ "${#TO_RECREATE[@]}" -eq 0 ]; then
  log "no running ws-* containers using a refreshed image — nothing to recreate"
  exit 0
fi

# 3. Kill + remove. Canvas next-interaction will re-provision.
log "recreating ${#TO_RECREATE[@]} containers (canvas will re-provision on next interaction)"
for cn in "${TO_RECREATE[@]}"; do
  docker rm -f "$cn" >/dev/null 2>&1 && log "  removed $cn" || warn "  failed to remove $cn"
done

log "done. open the canvas and the workspaces will re-provision against the new image."
