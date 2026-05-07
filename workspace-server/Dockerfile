# Platform-only image (no canvas). Used by publish-workspace-server-image
# workflow for ECR. Tenant image uses Dockerfile.tenant instead.
#
# Templates + plugins are pre-cloned by scripts/clone-manifest.sh (in CI
# or on the operator host) into .tenant-bundle-deps/ — same pattern as
# Dockerfile.tenant. See that file's header for the full rationale; the
# short version is that post-2026-05-06 every workspace-template-* and
# org-template-* repo on Gitea is private, so an in-image `git clone`
# has no auth path that doesn't leak the Gitea token into a layer.
#
# Build context: repo root, with `.tenant-bundle-deps/` populated by the
# workflow's "Pre-clone manifest deps" step (Task #173).

FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY workspace-server/go.mod workspace-server/go.sum ./
# github-app-auth plugin removed 2026-05-07 (#157): per-agent Gitea
# identities replaced the GitHub-App-installation token flow after the
# 2026-05-06 suspension. Pre-removal this stage COPY'd the sibling
# plugin repo + injected a `replace` directive; both are gone.
RUN go mod download
COPY workspace-server/ .
# GIT_SHA mirror of Dockerfile.tenant — see that file for the rationale.
ARG GIT_SHA=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-X github.com/Molecule-AI/molecule-monorepo/platform/internal/buildinfo.GitSHA=${GIT_SHA}" \
    -o /platform ./cmd/server
# Bundle the built-in memory-plugin-postgres binary so an operator can
# activate Memory v2 by setting MEMORY_V2_CUTOVER=true + (default)
# MEMORY_PLUGIN_URL=http://localhost:9100. The entrypoint starts this
# binary in the background; main /platform talks to it over loopback.
# Stays inert until the operator flips the cutover env var.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-X github.com/Molecule-AI/molecule-monorepo/platform/internal/buildinfo.GitSHA=${GIT_SHA}" \
    -o /memory-plugin ./cmd/memory-plugin-postgres

FROM alpine:3.20
RUN apk add --no-cache ca-certificates git tzdata wget
COPY --from=builder /platform /platform
COPY --from=builder /memory-plugin /memory-plugin
COPY workspace-server/migrations /migrations
# Templates + plugins (pre-cloned by scripts/clone-manifest.sh in the
# trusted CI / operator-host context, .git already stripped). The Gitea
# token used to clone them never enters this image — same shape as
# Dockerfile.tenant.
COPY .tenant-bundle-deps/workspace-configs-templates /workspace-configs-templates
COPY .tenant-bundle-deps/org-templates /org-templates
COPY .tenant-bundle-deps/plugins /plugins
# Non-root runtime with Docker socket access for workspace provisioning.
RUN addgroup -g 1000 platform && adduser -u 1000 -G platform -s /bin/sh -D platform
EXPOSE 8080
COPY <<'ENTRY' /entrypoint.sh
#!/bin/sh
# Set up docker-socket group (unchanged from pre-sidecar entrypoint).
if [ -S /var/run/docker.sock ]; then
  SOCK_GID=$(stat -c '%g' /var/run/docker.sock 2>/dev/null || stat -f '%g' /var/run/docker.sock 2>/dev/null)
  if [ -n "$SOCK_GID" ] && [ "$SOCK_GID" != "0" ]; then
    addgroup -g "$SOCK_GID" docker 2>/dev/null || true
    addgroup platform docker 2>/dev/null || true
  else
    addgroup platform root 2>/dev/null || true
  fi
fi

# Memory v2 sidecar (built-in postgres plugin). Co-located with the
# main server so operators flipping MEMORY_V2_CUTOVER=true don't need
# to provision a separate service.
#
# Spawn-gating: only start the sidecar when the operator has indicated
# they want it — either MEMORY_V2_CUTOVER=true OR MEMORY_PLUGIN_URL set.
# Without that signal, the sidecar adds zero value (the platform's
# wiring.go skips building the client too) but pays a real cost: the
# plugin's first migration runs `CREATE EXTENSION vector`, which fails
# on tenant Postgres without pgvector preinstalled and aborts container
# boot via the 30s health gate. Caught on staging redeploy 2026-05-05.
#
# Env defaults (when sidecar IS spawned):
#   MEMORY_PLUGIN_DATABASE_URL = $DATABASE_URL  (share existing Postgres;
#       plugin's `memory_namespaces` / `memory_records` tables coexist
#       with `agent_memories` and the rest of the platform schema —
#       no conflicts. Operator can override with a separate URL.)
#   MEMORY_PLUGIN_LISTEN_ADDR  = 127.0.0.1:9100
#
# Set MEMORY_PLUGIN_DISABLE=1 to force-skip the sidecar even with
# cutover env set (e.g. running the plugin externally on a separate host).
memory_plugin_wanted=""
if [ "$MEMORY_V2_CUTOVER" = "true" ] || [ -n "$MEMORY_PLUGIN_URL" ]; then
  memory_plugin_wanted=1
fi
if [ -z "$MEMORY_PLUGIN_DISABLE" ] && [ -n "$memory_plugin_wanted" ] && [ -n "$DATABASE_URL" ]; then
  : "${MEMORY_PLUGIN_DATABASE_URL:=$DATABASE_URL}"
  : "${MEMORY_PLUGIN_LISTEN_ADDR:=:9100}"
  export MEMORY_PLUGIN_DATABASE_URL MEMORY_PLUGIN_LISTEN_ADDR
  echo "memory-plugin: starting sidecar on $MEMORY_PLUGIN_LISTEN_ADDR" >&2
  # Drop privs to the platform user — the plugin doesn't need root and
  # runs unprivileged elsewhere (tenant image already starts as canvas).
  su-exec platform /memory-plugin &
  MEMORY_PLUGIN_PID=$!
  # Wait up to 30s for the plugin's /v1/health to return 200. Boot
  # failure here is fatal — better to crash-loop than to silently
  # serve cutover traffic against a dead plugin.
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
    echo "memory-plugin: ❌ /v1/health never returned 200 after 30s — aborting boot. Check that DATABASE_URL is reachable, has the pgvector extension, and the plugin's migrations applied." >&2
    kill "$MEMORY_PLUGIN_PID" 2>/dev/null || true
    exit 1
  fi
  echo "memory-plugin: ✅ sidecar healthy on :$health_port" >&2
fi

exec su-exec platform /platform "$@"
ENTRY
RUN chmod +x /entrypoint.sh && apk add --no-cache su-exec
ENTRYPOINT ["/entrypoint.sh"]
