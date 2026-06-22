#!/usr/bin/env bash
# E2E test: scripts/nuke-and-rebuild.sh self-bootstraps a clean dev stack.
#
# What this asserts (and why each one matters):
#   1. After nuke+rebuild, workspace-configs-templates/ is populated.
#      Regression target: someone deletes the manifest-clone step and
#      Canvas silently shows zero templates.
#   2. After nuke+rebuild, no orphan ws-* containers survive on the
#      Docker daemon. Regression target: someone removes the ws-*
#      reaping lines from the script and old containers haunt every
#      future stack with a wiped DB.
#   3. Platform serves /health 200. Regression target: env wiring drift
#      or a Dockerfile change that breaks platform startup.
#   4. Platform exposes the templates it sees on disk. Regression target:
#      bind-mount drift between docker-compose.yml and the platform
#      config (CONFIGS_HOST_DIR / CONFIGS_DIR misalignment).
#   5. The image-auto-refresh watcher (PR #2114) starts. Regression
#      target: someone defaults IMAGE_AUTO_REFRESH back to false in
#      compose, breaking the runtime CD chain users now rely on.
#
# Usage:
#   bash scripts/test-nuke-and-rebuild.sh
#
# Cost: ~3-6 min on a warm cache (plugin clones are the slow part on
# a cold cache, ~30-60s).
#
# Caveats:
#   - Requires Docker daemon + jq + curl on PATH.
#   - Spawns a fake `ws-deadbeef-test` container with a sleep-forever
#     command so we have a known orphan to assert against. Cleanup
#     runs in a trap.
#   - Does NOT test the runtime CD propagation end-to-end (that's
#     issue #2118). Scope here is the local nuke+rebuild loop only.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PLATFORM="${PLATFORM:-http://localhost:8080}"
PASS=0
FAIL=0
FAKE_WS="ws-deadbeeftest"

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing dependency: $1"; exit 2; }
}
require docker
require jq
require curl

cleanup() {
  docker rm -f "$FAKE_WS" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Pre-flight: if another compose project already holds the ports we need,
# bail with a clear message rather than letting the rebuild step fail
# halfway through with a confusing "port already allocated" error. This
# happens routinely when a parallel monorepo checkout has its stack up.
PROJECT="$(basename "$ROOT")"
for port in 5432 6379 8080; do
  HOLDER=$(docker ps --filter "publish=$port" --format '{{.Names}}' | head -1)
  if [ -n "$HOLDER" ] && [[ "$HOLDER" != "${PROJECT}-"* ]]; then
    echo "SKIP: port $port held by container '$HOLDER' from a different compose project."
    echo "      This test rebuilds the '$PROJECT' stack, which would conflict."
    echo "      Stop the other stack first (in its own checkout):"
    echo "        docker compose down -v"
    exit 0
  fi
done

check() {
  local label="$1" cond="$2"
  if eval "$cond"; then
    echo "PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $label"
    echo "  cond: $cond"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== Setup: plant a fake orphan ws-* container ==="
# alpine because it's already on most Docker hosts; sleep so Docker treats
# it as a long-running container worth listing in `docker ps`.
docker run -d --name "$FAKE_WS" --rm=false alpine sleep 3600 >/dev/null
docker ps --filter name="^${FAKE_WS}$" --format '{{.Names}}' | grep -q "^${FAKE_WS}$" || {
  echo "FAIL: setup — fake orphan container did not start"
  exit 2
}
echo "  planted $FAKE_WS"

echo ""
echo "=== Setup: wipe the manifest-managed dirs to simulate a fresh checkout ==="
# Don't actually delete — rename to a sentinel, restore on exit. Avoids
# unrecoverable damage if the test crashes after the rename and operator
# Ctrl-Cs the trap.
for d in workspace-configs-templates org-templates plugins; do
  if [ -d "$ROOT/$d" ]; then
    mv "$ROOT/$d" "$ROOT/${d}.testbak"
  fi
done
restore_dirs() {
  for d in workspace-configs-templates org-templates plugins; do
    if [ -d "$ROOT/${d}.testbak" ] && [ ! -d "$ROOT/$d" ]; then
      mv "$ROOT/${d}.testbak" "$ROOT/$d"
    fi
  done
}
trap 'cleanup; restore_dirs' EXIT

echo ""
echo "=== Run nuke-and-rebuild.sh (this is what we're testing) ==="
bash "$ROOT/scripts/nuke-and-rebuild.sh" >/tmp/nuke.log 2>&1 || {
  echo "FAIL: nuke-and-rebuild.sh exited non-zero. Tail of log:"
  tail -30 /tmp/nuke.log
  exit 2
}
echo "  ran (full log: /tmp/nuke.log)"

echo ""
echo "=== Assertions ==="

check "templates dir populated (8 entries expected)" \
  "[ \"\$(ls $ROOT/workspace-configs-templates 2>/dev/null | wc -l | tr -d ' ')\" -ge 8 ]"

check "fake orphan ws-* container reaped" \
  "! docker ps -a --filter name=^${FAKE_WS}\$ --format '{{.Names}}' | grep -q ."

# Wait for platform health (compose startup + migrations can take a beat).
echo "  waiting for platform /health..."
for _ in $(seq 1 30); do
  if curl -sf "$PLATFORM/health" >/dev/null 2>&1; then break; fi
  sleep 2
done

check "platform /health returns 200" \
  "[ \"\$(curl -s -o /dev/null -w '%{http_code}' $PLATFORM/health)\" = '200' ]"

# Compare templates the platform sees vs. what's on disk. If the bind
# mount is broken, on-disk count won't match in-container count.
DISK_COUNT=$(find "$ROOT/workspace-configs-templates" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l | tr -d ' ')
PLATFORM_COUNT=$(docker exec molecule-monorepo-platform-1 sh -c 'find /configs -mindepth 1 -maxdepth 1 2>/dev/null | wc -l' | tr -d ' ' || echo 0)
check "platform sees same template count as disk ($DISK_COUNT)" \
  "[ \"$PLATFORM_COUNT\" = \"$DISK_COUNT\" ]"

# IMAGE_AUTO_REFRESH watcher should log its startup line (PR #2114).
check "image-auto-refresh watcher started" \
  "docker logs molecule-monorepo-platform-1 2>&1 | grep -q 'image-auto-refresh: started'"

echo ""
echo "=== Result: $PASS passed, $FAIL failed ==="
[ $FAIL -eq 0 ]
