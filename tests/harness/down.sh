#!/usr/bin/env bash
# Tear down the harness and wipe per-tenant volumes.
#
# SECRETS_ENCRYPTION_KEY placeholder: docker compose validates the entire
# compose file even for `down -v` (a destructive read-only operation that
# doesn't read the env). up.sh generates a per-run key into its own
# shell — this script runs in a fresh shell that wouldn't see it. Without
# the placeholder, `compose down` exits non-zero before removing volumes,
# silently leaking workspaces+activity_logs into the next ./up.sh + seed.sh
# (verified 2026-05-02: tenant-isolation.sh F1/F2 saw 3× duplicate
# alpha-parent + alpha-child rows accumulated across three prior boots).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"
SECRETS_ENCRYPTION_KEY="${SECRETS_ENCRYPTION_KEY:-down-placeholder}" \
    docker compose -f compose.yml down -v --remove-orphans
echo "[harness] down + volumes removed."
