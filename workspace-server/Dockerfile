# Platform-only image (no canvas). Used by publish-platform-image workflow
# for GHCR + Fly registry. Tenant image uses Dockerfile.tenant instead.
#
# Build context: repo root.

FROM golang:1.25-alpine AS builder
WORKDIR /app
# Plugin source for replace directive in go.mod
COPY molecule-ai-plugin-github-app-auth/ /plugin/
COPY workspace-server/go.mod workspace-server/go.sum ./
# Add replace directives for Docker builds:
# 1. Platform → plugin (plugin source at /plugin/)
# 2. Plugin → platform (plugin's go.mod has a relative replace that doesn't
#    work in Docker; fix it to point at /app where the platform source lives)
RUN echo 'replace github.com/Molecule-AI/molecule-ai-plugin-github-app-auth => /plugin' >> go.mod
RUN sed -i 's|replace github.com/Molecule-AI/molecule-monorepo/platform => .*|replace github.com/Molecule-AI/molecule-monorepo/platform => /app|' /plugin/go.mod
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

# Clone templates + plugins at build time from manifest.json
FROM alpine:3.20 AS templates
RUN apk add --no-cache git jq
COPY manifest.json /manifest.json
COPY scripts/clone-manifest.sh /scripts/clone-manifest.sh
RUN chmod +x /scripts/clone-manifest.sh && /scripts/clone-manifest.sh /manifest.json /workspace-configs-templates /org-templates /plugins

FROM alpine:3.20
RUN apk add --no-cache ca-certificates git tzdata wget
COPY --from=builder /platform /platform
COPY --from=builder /memory-plugin /memory-plugin
COPY workspace-server/migrations /migrations
COPY --from=templates /workspace-configs-templates /workspace-configs-templates
COPY --from=templates /org-templates /org-templates
COPY --from=templates /plugins /plugins
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
