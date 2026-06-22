#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "==> Ensuring shared docker network exists..."
docker network create molecule-core-net 2>/dev/null || true

# Populate the template / plugin registry.
# workspace-configs-templates/, org-templates/, and plugins/ are intentionally
# gitignored — the curated set lives in manifest.json as external repos. Without
# them the Canvas template palette is empty and workspace provisioning falls
# through to a bare default. The script itself is idempotent (skips dirs that
# already have content), so re-running setup.sh is safe.
if [ -f "$ROOT_DIR/manifest.json" ] && [ -f "$ROOT_DIR/scripts/clone-manifest.sh" ]; then
  if ! command -v jq >/dev/null 2>&1; then
    echo "==> NOTE: jq not installed — skipping template registry populate."
    echo "    Install with: brew install jq   (macOS) / apt install jq (Debian)"
    echo "    Then rerun:   bash scripts/clone-manifest.sh manifest.json \\"
    echo "                    workspace-configs-templates/ org-templates/ plugins/"
  else
    echo "==> Populating template / plugin registry from manifest.json..."
    bash "$ROOT_DIR/scripts/clone-manifest.sh" \
      "$ROOT_DIR/manifest.json" \
      "$ROOT_DIR/workspace-configs-templates" \
      "$ROOT_DIR/org-templates" \
      "$ROOT_DIR/plugins"
  fi
fi

echo "==> Starting infrastructure..."
docker compose -f "$ROOT_DIR/docker-compose.infra.yml" up -d

echo "==> Waiting for Postgres..."
until docker compose -f "$ROOT_DIR/docker-compose.infra.yml" exec -T postgres pg_isready -U "${POSTGRES_USER:-dev}" 2>/dev/null; do
  sleep 1
done
echo "    Postgres is ready."

echo "==> Waiting for Redis..."
until docker compose -f "$ROOT_DIR/docker-compose.infra.yml" exec -T redis redis-cli ping 2>/dev/null | grep -q PONG; do
  sleep 1
done
echo "    Redis is ready."

echo "==> Verifying Redis KEA config..."
KEA=$(docker compose -f "$ROOT_DIR/docker-compose.infra.yml" exec -T redis redis-cli config get notify-keyspace-events | tail -1)
echo "    notify-keyspace-events = $KEA"

# Migrations are intentionally not applied here. The platform's own runner
# (workspace-server/internal/db/postgres.go::RunMigrations) tracks applied
# files in `schema_migrations` on every boot. Applying them out-of-band via
# psql leaves that table empty, so the platform re-applies everything and
# fails on non-idempotent ALTER TABLE statements. Let `go run ./cmd/server`
# handle it.

echo "==> Infrastructure ready!"
echo "    Postgres: localhost:5432"
echo "    Redis:    localhost:6379"
echo "    Langfuse: localhost:3001"
echo "    Temporal: localhost:7233 (gRPC) / localhost:8233 (UI)"
echo ""
echo "    Next: cd workspace-server && go run ./cmd/server"
echo "          (the platform applies pending migrations on first boot)"

# Source .env if it exists so the ADMIN_TOKEN check below reflects what the
# platform will actually see at startup, not just the current shell env.
if [ -f "$ROOT_DIR/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$ROOT_DIR/.env"
  set +a
fi

# Security check — issue #684 (AdminAuth bearer bypass, PR #729).
# Without ADMIN_TOKEN, any valid workspace bearer token can call /admin/* routes.
if [ -z "${ADMIN_TOKEN:-}" ]; then
  echo ""
  echo "  ⚠  WARNING: ADMIN_TOKEN is not set."
  echo "     Until it is, AdminAuth falls back to accepting any workspace bearer token"
  echo "     — the #684 vulnerability is NOT closed in this deployment."
  echo "     Generate one:  openssl rand -base64 32"
  echo "     Then export ADMIN_TOKEN=<value> or add it to your .env before starting the platform."
fi
