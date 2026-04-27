#!/bin/bash
# Full nuke + rebuild — one command to reset everything.
#
# What "everything" means:
#   1. The compose stack (containers + named volumes + network).
#   2. Dynamically-spawned ws-* workspace containers + their volumes.
#      These are NOT in docker-compose.yml — the provisioner creates them
#      at workspace-create time, so `compose down -v` leaves them behind.
#      Without this step, a fresh DB plus old ws-* containers = ghost
#      containers Canvas can't see, eating CPU + memory.
#   3. Repopulating the manifest-managed dirs (workspace-configs-templates/,
#      org-templates/, plugins/). These are .gitignored — fresh checkouts
#      and post-deletion runs leave them empty, which silently hides the
#      entire template palette in Canvas. clone-manifest.sh is idempotent,
#      so re-running with already-populated dirs is a fast no-op.
#
# Usage:
#   bash scripts/nuke-and-rebuild.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "=== NUKE ==="
docker compose -f "$ROOT/docker-compose.yml" down -v 2>/dev/null || true
docker ps -a --format "{{.Names}}" | grep "^ws-" | xargs -r docker rm -f 2>/dev/null || true
docker volume ls --format "{{.Name}}" | grep "^ws-" | xargs -r docker volume rm 2>/dev/null || true
docker network rm molecule-monorepo-net 2>/dev/null || true
echo "  cleaned"

echo "=== POPULATE MANIFEST DIRS ==="
# Idempotent: clone-manifest.sh skips dirs that already have content, so a
# re-nuke after templates are populated is a fast no-op (a few stat calls).
# Skip with a clear warning if jq is missing — installing it is a one-time
# step documented in the README quickstart.
if command -v jq >/dev/null 2>&1; then
  bash "$ROOT/scripts/clone-manifest.sh" \
    "$ROOT/manifest.json" \
    "$ROOT/workspace-configs-templates" \
    "$ROOT/org-templates" \
    "$ROOT/plugins" 2>&1 | tail -3
else
  echo "  WARNING: jq not installed — skipping template/plugin clone."
  echo "           Install (brew install jq) and rerun, or Canvas's template"
  echo "           palette will be empty and provisioning falls back to defaults."
fi

echo "=== REBUILD ==="
docker compose -f "$ROOT/docker-compose.yml" up -d --build
echo "  platform + canvas up"

echo "=== POST-REBUILD SETUP ==="
bash "$ROOT/scripts/post-rebuild-setup.sh"
