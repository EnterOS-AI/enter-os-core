#!/usr/bin/env bash
# local-tenant-smoke.sh — run the tenant-image build-and-push SMOKE GATE locally.
#
# This is the dev-runnable companion to the FULL-ENV smoke in
# .gitea/workflows/publish-workspace-server-image.yml. That gate only runs in CI
# on merge — so a broken tenant boot (or a smoke/path mismatch) is only caught
# AFTER the PR lands. Run this before pushing to catch the same class of defect:
#   - tenant fails to boot under FULL env (DB + Redis + MEMORY_PLUGIN sidecar)
#     — the prod-onboarding root cause (RCA 107680).
#   - the platform health route is /health (NOT /healthz) — the route the server
#     actually mounts (router.go: r.GET("/health", ...)). The CI smoke polled
#     /healthz and got 404 for 180s (#3121); this script asserts the real path.
#
# Usage:
#   scripts/local-tenant-smoke.sh             # build image, then smoke
#   scripts/local-tenant-smoke.sh --no-build  # smoke an already-built IMAGE
#   IMAGE=ghcr.io/.../molecule-tenant:sha-x scripts/local-tenant-smoke.sh --no-build
#
# Requires: docker (+ buildx). Mirrors the CI build args + smoke env exactly.

set -euo pipefail

IMAGE="${IMAGE:-molecule-tenant-localsmoke:latest}"
HEALTH_PATH="${HEALTH_PATH:-/health}"   # the route the server mounts; NOT /healthz
BUILD=1
[ "${1:-}" = "--no-build" ] && BUILD=0

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

SUF="local-$$"
NET="smoke-net-${SUF}"; PGV="smoke-pgv-${SUF}"; RED="smoke-redis-${SUF}"; TEN="smoke-tenant-${SUF}"
cleanup() { docker rm -f "$TEN" "$PGV" "$RED" >/dev/null 2>&1 || true; docker network rm "$NET" >/dev/null 2>&1 || true; }
trap cleanup EXIT

if [ "$BUILD" = "1" ]; then
  echo ">> Building tenant image (${IMAGE}) — mirrors CI build args ..."
  docker buildx build \
    --file ./workspace-server/Dockerfile.tenant \
    --provenance=false --sbom=false \
    --build-arg NEXT_PUBLIC_PLATFORM_URL= \
    --build-arg GIT_SHA="$(git rev-parse HEAD 2>/dev/null || echo local)" \
    -t "${IMAGE}" --load .
fi

echo ">> Starting sidecars (pgvector + redis) ..."
docker network create "$NET" >/dev/null
docker run -d --rm --name "$PGV" --network "$NET" \
  -e POSTGRES_PASSWORD=smoketest -e POSTGRES_USER=smoke -e POSTGRES_DB=smoke \
  pgvector/pgvector:pg16 >/dev/null
# Mirror CI smoke exactly (CR2 RC 13003 / PR #3120 pattern):
#   --bind 0.0.0.0 avoids the default [::1]:6379 IPv6-loopback bind that
#   breaks cross-container go-redis connectivity on the user-defined bridge
#   network. --protected-mode no lets the tenant (no AUTH) connect. The
#   --save/--appendonly flags turn off disk persistence (smoke data is
#   throwaway). The PING readiness probe below waits for Redis to accept
#   connections before the tenant boots — same pattern as #3120.
docker run -d --rm --name "$RED" --network "$NET" \
  redis:7-alpine \
  redis-server \
    --bind 0.0.0.0 \
    --protected-mode no \
    --save "" \
    --appendonly no >/dev/null

for _ in $(seq 1 30); do docker exec "$PGV" pg_isready -U smoke >/dev/null 2>&1 && break; sleep 2; done
docker exec "$PGV" psql -U smoke -d smoke -c "CREATE EXTENSION IF NOT EXISTS vector;" >/dev/null

# PING readiness probe for redis (matches #3120 CI smoke pattern).
# Without this, the tenant can race Redis startup and hit a
# `connection refused` that fails the smoke.
redis_ok=0
for _ in $(seq 1 15); do
  if docker exec "$RED" redis-cli -h 127.0.0.1 -p 6379 PING 2>/dev/null | grep -q PONG; then
    redis_ok=1
    break
  fi
  sleep 2
done
if [ "$redis_ok" -ne 1 ]; then
  echo "FAIL: redis sidecar never responded to PING in 30s"
  docker logs --tail 40 "$RED" 2>&1 | tail -40 || true
  exit 1
fi
echo ">> redis sidecar ready (PING ok)"

echo ">> Starting tenant (FULL ENV: DB + Redis + MEMORY_PLUGIN sidecar) ..."
docker run -d --rm --name "$TEN" --network "$NET" \
  -e PORT=8080 -e MOLECULE_TENANT_MODE=smoke \
  -e DATABASE_URL="postgres://smoke:smoketest@${PGV}:5432/smoke?sslmode=disable" \
  -e REDIS_URL="redis://${RED}:6379" \
  -e MEMORY_PLUGIN_URL="http://localhost:9100" -e MEMORY_PLUGIN_LISTEN_ADDR=":9100" \
  -p 18080:8080 -p 19100:9100 \
  "${IMAGE}" >/dev/null

echo ">> Polling platform ${HEALTH_PATH} (180s budget) ..."
code=000
for _ in $(seq 1 90); do
  code=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 2 "http://localhost:18080${HEALTH_PATH}" 2>/dev/null || echo 000)
  [ "$code" = "200" ] && break
  sleep 2
done
if [ "$code" != "200" ]; then
  echo "FAIL: platform ${HEALTH_PATH} never returned 200 (last: ${code})"
  docker logs --tail 80 "$TEN" 2>&1 | tail -80 || true
  exit 1
fi
echo "PASS: platform ${HEALTH_PATH} = 200"

echo ">> Polling memory-plugin sidecar /v1/health ..."
sc=000
for _ in $(seq 1 30); do
  sc=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 2 "http://localhost:19100/v1/health" 2>/dev/null || echo 000)
  [ "$sc" = "200" ] && break
  sleep 2
done
[ "$sc" = "200" ] && echo "PASS: memory-plugin /v1/health = 200" || { echo "FAIL: memory-plugin /v1/health = ${sc}"; exit 1; }

echo ">> SMOKE PASSED."
