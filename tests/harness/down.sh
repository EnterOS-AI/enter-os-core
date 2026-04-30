#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"
docker compose -f compose.yml down -v --remove-orphans
echo "[harness] down + volumes removed."
