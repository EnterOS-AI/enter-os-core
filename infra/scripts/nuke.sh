#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "==> Tearing down all services..."

# Stop full stack if running
docker compose -f "$ROOT_DIR/docker-compose.yml" down -v --remove-orphans 2>/dev/null || true

# Stop infra if running separately
docker compose -f "$ROOT_DIR/docker-compose.infra.yml" down -v --remove-orphans 2>/dev/null || true

echo "==> All services stopped and volumes removed."
