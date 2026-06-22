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
# Spawn-gating: start the sidecar when MEMORY_PLUGIN_URL is set.
# Without it, the sidecar adds zero value and risks aborting tenant
# boot via the 30s health gate when the tenant Postgres lacks
# pgvector. Caught on staging redeploy 2026-05-05:
#   pq: extension "vector" is not available
#
# Defaults (when sidecar IS spawned): MEMORY_PLUGIN_DATABASE_URL
# falls back to the tenant's DATABASE_URL.
#
# Phase A3 (#1792): MEMORY_V2_CUTOVER acceptance removed. The variable
# was deprecated by #1747 (binary stopped reading it) and only kept
# alive here as a synonym to bridge old CP user-data templates. With
# A3 dropping the entire v1 surface, the synonym is gone too. CP
# user-data sets MEMORY_PLUGIN_URL directly; if a stale template
# without that var ships, the sidecar simply doesn't start and the
# tenant boots without memory — loud but recoverable, same posture as
# any other required env missing.
MEMORY_PLUGIN_PID=""
memory_plugin_wanted=""
if [ -n "$MEMORY_PLUGIN_URL" ]; then
  memory_plugin_wanted=1
fi
if [ -z "$MEMORY_PLUGIN_DISABLE" ] && [ -n "$memory_plugin_wanted" ] && [ -n "$DATABASE_URL" ]; then
  # Schema isolation (issue #1733): when defaulting from the tenant
  # DATABASE_URL we co-locate the plugin's tables under a dedicated
  # `memory_plugin` schema so they never collide with platform-tenant
  # tables in `public`. The plugin's 000_schema_bootstrap migration
  # creates the schema; search_path here directs every subsequent CREATE
  # TABLE / SELECT to land in it.
  #
  # The search_path includes `public` as a fallback so the `vector` type
  # resolves regardless of which schema pgvector was installed into.
  # Fresh tenants (no prior `CREATE EXTENSION vector`) install the
  # extension into `memory_plugin` (first writable schema in the path),
  # keeping the SSOT intent. Tenants where pgvector was already
  # installed into `public` by a prior boot or operator action keep the
  # extension where it is and resolve `vector(1536)` via the public
  # fallback — without this fallback those tenants would crash the
  # plugin boot with "type vector does not exist" once the migrations
  # try to create memory_records (#1742 review finding).
  #
  # Operators who explicitly set MEMORY_PLUGIN_DATABASE_URL (separate DB
  # entirely) keep full control — search_path is only injected when we
  # default from DATABASE_URL.
  if [ -z "$MEMORY_PLUGIN_DATABASE_URL" ]; then
    case "$DATABASE_URL" in
      *\?*) MEMORY_PLUGIN_DATABASE_URL="${DATABASE_URL}&search_path=memory_plugin,public" ;;
      *)    MEMORY_PLUGIN_DATABASE_URL="${DATABASE_URL}?search_path=memory_plugin,public" ;;
    esac
  fi
  : "${MEMORY_PLUGIN_LISTEN_ADDR:=:9100}"
  export MEMORY_PLUGIN_DATABASE_URL MEMORY_PLUGIN_LISTEN_ADDR
  echo "memory-plugin: starting sidecar on $MEMORY_PLUGIN_LISTEN_ADDR" >&2
  /memory-plugin &
  MEMORY_PLUGIN_PID=$!
  # Wait up to 30s for /v1/health. Boot failure is fatal so a misconfigured
  # tenant crash-loops instead of silently serving cutover traffic against
  # a dead plugin.
  #
  # Probe via node: this image is node:20-alpine-based and node is
  # guaranteed on PATH, while busybox wget is unreliable on slim
  # alpine variants (some pin to the busybox applet which lacks
  # --timeout=2 and exits non-zero on a successful HTTP 200 with an
  # empty body, causing a false negative). node's stdlib http.get
  # is portable across all our runtime adapter templates.
  health_port=${MEMORY_PLUGIN_LISTEN_ADDR#:}
  ready=0
  for _ in $(seq 1 30); do
    if HEALTH_PORT="$health_port" node -e '
const http = require("http");
const port = process.env.HEALTH_PORT;
const req = http.get({ host: "localhost", port, path: "/v1/health", timeout: 2000 }, (res) => {
  process.exit(res.statusCode === 200 ? 0 : 1);
});
req.on("error", () => process.exit(1));
req.on("timeout", () => { req.destroy(); process.exit(1); });
' >/dev/null 2>&1; then
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
