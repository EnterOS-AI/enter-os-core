#!/bin/sh
# Tenant entrypoint — starts both Go platform (API) and Canvas (UI).
#
# Container runs as non-root 'canvas' user (USER directive in Dockerfile.tenant).
# Both processes start as non-root. SIGTERM propagates to child processes via the
# shell's trap + wait -n pattern below.
#
# Go platform listens on :8080 (Fly health checks hit this port).
# Canvas Node.js listens on :3000 (internal only).
# The Go platform's fallback handler proxies non-API routes to :3000
# so the browser only ever talks to :8080.
#
# If either process dies, we kill the other and exit non-zero so Fly
# restarts the machine.

set -e

# Start Canvas in background
cd /canvas
PORT=3000 HOSTNAME=0.0.0.0 node server.js &
CANVAS_PID=$!

# Memory v2 sidecar (built-in postgres plugin). See Dockerfile entrypoint
# comment for rationale.
#
# Spawn-gating: only start the sidecar when the operator has indicated
# they want it (MEMORY_V2_CUTOVER=true OR MEMORY_PLUGIN_URL set).
# Without that signal, the sidecar adds zero value and risks aborting
# tenant boot via the 30s health gate when the tenant Postgres lacks
# pgvector. Caught on staging redeploy 2026-05-05:
#   pq: extension "vector" is not available
#
# Defaults (when sidecar IS spawned): MEMORY_PLUGIN_DATABASE_URL
# falls back to the tenant's DATABASE_URL.
MEMORY_PLUGIN_PID=""
memory_plugin_wanted=""
if [ "$MEMORY_V2_CUTOVER" = "true" ] || [ -n "$MEMORY_PLUGIN_URL" ]; then
  memory_plugin_wanted=1
fi
if [ -z "$MEMORY_PLUGIN_DISABLE" ] && [ -n "$memory_plugin_wanted" ] && [ -n "$DATABASE_URL" ]; then
  : "${MEMORY_PLUGIN_DATABASE_URL:=$DATABASE_URL}"
  : "${MEMORY_PLUGIN_LISTEN_ADDR:=:9100}"
  export MEMORY_PLUGIN_DATABASE_URL MEMORY_PLUGIN_LISTEN_ADDR
  echo "memory-plugin: starting sidecar on $MEMORY_PLUGIN_LISTEN_ADDR" >&2
  /memory-plugin &
  MEMORY_PLUGIN_PID=$!
  # Wait up to 30s for /v1/health. Boot failure is fatal so a misconfigured
  # tenant crash-loops instead of silently serving cutover traffic against
  # a dead plugin.
  health_port=${MEMORY_PLUGIN_LISTEN_ADDR#:}
  ready=0
  for _ in $(seq 1 30); do
    if wget -qO- --timeout=2 "http://localhost:${health_port}/v1/health" >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 1
  done
  if [ "$ready" != "1" ]; then
    echo "memory-plugin: ❌ /v1/health never returned 200 after 30s — aborting boot. Check DATABASE_URL reachability + pgvector extension + migrations." >&2
    kill "$MEMORY_PLUGIN_PID" 2>/dev/null || true
    kill "$CANVAS_PID" 2>/dev/null || true
    exit 1
  fi
  echo "memory-plugin: ✅ sidecar healthy on :$health_port" >&2
fi

# Start Go platform in foreground-ish (we trap signals)
# CANVAS_PROXY_URL tells the platform to proxy unmatched routes to Canvas.
# CONTAINER_BACKEND: empty = Docker (default for self-hosted/local).
# Set to "flyio" via Fly machine env to use Fly Machines API instead.
export CANVAS_PROXY_URL="${CANVAS_PROXY_URL:-http://localhost:3000}"
cd /
/platform &
PLATFORM_PID=$!

# If any process exits, kill the others
cleanup() {
  kill $CANVAS_PID 2>/dev/null || true
  kill $PLATFORM_PID 2>/dev/null || true
  [ -n "$MEMORY_PLUGIN_PID" ] && kill $MEMORY_PLUGIN_PID 2>/dev/null || true
}
trap cleanup EXIT SIGTERM SIGINT

# Wait for any to exit — whichever exits first triggers cleanup
if [ -n "$MEMORY_PLUGIN_PID" ]; then
  wait -n $CANVAS_PID $PLATFORM_PID $MEMORY_PLUGIN_PID
else
  wait -n $CANVAS_PID $PLATFORM_PID
fi
EXIT_CODE=$?
cleanup
exit $EXIT_CODE
