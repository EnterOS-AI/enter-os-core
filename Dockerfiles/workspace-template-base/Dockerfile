# workspace-template-base — shared base image for the 9 workspace
# template repos.
#
# RFC #3018 / closes #2277. Eliminates the byte-identical Dockerfile
# boilerplate that propagates across templates today (4 of 9 repos
# share sha 075683edb742… for their entire Dockerfile; the rest layer
# adapter-specific concerns on top of the same boilerplate).
#
# Inherits from:
#   FROM ghcr.io/molecule-ai/workspace-template-base:vX.Y.Z
#
# The 4 minimal templates (autogen, crewai, deepagents, langgraph)
# need only:
#   FROM ghcr.io/molecule-ai/workspace-template-base:v1
#   COPY adapter.py .
#   COPY __init__.py .
#   ENV ADAPTER_MODULE=adapter
#   # ENTRYPOINT inherited from base ("molecule-runtime")
#
# The 5 customized templates (claude-code, codex, gemini-cli, hermes,
# openclaw) layer adapter-specific concerns (npm CLIs, drop-priv
# entrypoints, GH credentials helpers) on top of the same base.
#
# Versioning: SemVer-tagged via runtime-vX.Y.Z pattern. Templates pin
# `:v1` (major-only) to auto-pick up minor fixes; `:v1.0.0` if they
# want to opt out of minor updates.

FROM python:3.11-slim

# System deps: curl/gosu for runtime, ca-certs for TLS. Adapter-specific
# CLIs (nodejs, npm, gh, etc.) layer in each template's own Dockerfile.
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl gosu ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Agent user: uid 1000 stable for volume permission expectations across
# tenant EC2 hosts. Templates that drop-priv (claude-code) chown via
# their own entrypoint.
RUN useradd -u 1000 -m -s /bin/bash agent

WORKDIR /app

# RUNTIME_VERSION is forwarded from molecule-ci's reusable publish
# workflow as a docker build-arg. Cascade-triggered builds set it to
# the exact runtime version PyPI just published. Including it as an
# ARG changes the cache key for the pip install layer below — the
# fix for the cascade cache trap that bit the team 5x on 2026-04-27.
#
# Empty default = falls back to whatever requirements.txt resolves to.
ARG RUNTIME_VERSION=

# Install Python deps. The RUNTIME_VERSION ARG is a no-op argument to
# the RUN command itself but its presence as a declared ARG above
# means buildx hashes it into the cache key.
#
# requirements.txt is OPTIONAL on the base — templates that need only
# molecule-ai-workspace-runtime can skip COPYing one. The base
# pre-installs the runtime so the minimal-template Dockerfile stays
# tiny.
RUN pip install --no-cache-dir molecule-ai-workspace-runtime && \
    if [ -n "${RUNTIME_VERSION}" ]; then \
      pip install --no-cache-dir --upgrade "molecule-ai-workspace-runtime==${RUNTIME_VERSION}"; \
    fi

# ADAPTER_MODULE is the canonical env var the runtime resolver reads
# at boot to import the adapter class. Each template overrides via
# ENV ADAPTER_MODULE=adapter (or whatever name they use). Default
# "adapter" matches the convention across all 9 current templates.
ENV ADAPTER_MODULE=adapter

# molecule-runtime is the entrypoint binary shipped by the runtime
# wheel. Templates that need a custom entrypoint (claude-code's
# drop-priv entrypoint.sh) override ENTRYPOINT explicitly.
ENTRYPOINT ["molecule-runtime"]
