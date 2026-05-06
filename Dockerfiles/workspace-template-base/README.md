# workspace-template-base

Shared base Docker image for `Molecule-AI/molecule-ai-workspace-template-*`
repos. Inherited via `FROM ghcr.io/molecule-ai/workspace-template-base:vX.Y.Z`.

## Why this exists

Pre-base, 4 of 9 template Dockerfiles were byte-identical (sha
`075683edb742…`) and the other 5 layered adapter-specific concerns on
top of the same boilerplate. Every system-package update, every cache-
trap fix, every base-image security patch had to land in 9 PRs. RFC
#3018 / issue #2277 captures the full investigation; this image is
Stage 1 of that rollout.

## What's in the base

- `python:3.11-slim`
- `curl gosu ca-certificates`
- agent user (uid 1000)
- `WORKDIR /app`
- `ARG RUNTIME_VERSION` (cascade cache-trap fix)
- `pip install molecule-ai-workspace-runtime`
- `ENV ADAPTER_MODULE=adapter`
- `ENTRYPOINT ["molecule-runtime"]`

## What's NOT in the base (intentionally)

Adapter-specific concerns stay in each template:

- nodejs / npm — layered by claude-code, codex, gemini-cli, hermes, openclaw
- gh CLI — layered by claude-code (agent autonomy)
- drop-priv entrypoint — layered by claude-code (claude-code refuses --dangerously-skip-permissions as root)
- adapter-specific Python deps — each template's `requirements.txt`

These are deliberately not in the base because they vary per template.
A "kitchen-sink" base would re-introduce drift at the adapter layer.

## Versioning

SemVer per release:

| Tag | Use case |
|-----|----------|
| `:v1` | major-only pin; auto-picks up minor fixes (recommended for templates) |
| `:v1.0.0` | exact pin (opt out of minor updates) |
| `:latest` | NOT recommended for production templates — moves with each main push |

Adding a new system package = minor (`:v1.1.0`).
Removing a package, changing ENTRYPOINT, or any breaking adapter contract = major (`:v2.0.0`).

## Consuming from a template

Minimal template Dockerfile:

```dockerfile
FROM ghcr.io/molecule-ai/workspace-template-base:v1

COPY adapter.py .
COPY __init__.py .
ENV ADAPTER_MODULE=adapter
# ENTRYPOINT inherited
```

Customized template Dockerfile (claude-code, hermes, etc.):

```dockerfile
FROM ghcr.io/molecule-ai/workspace-template-base:v1

# Adapter-specific deltas only
RUN apt-get update && apt-get install -y --no-install-recommends \
    nodejs npm gh \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g @anthropic-ai/claude-code

COPY adapter.py claude_sdk_executor.py __init__.py ./
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
```

## Drift gate

`internal/templatedrift/` (Stage 1b, follow-up) AST-walks each template
repo's Dockerfile and asserts:

- The first `FROM` line points at this base image (org-pinned).
- Templates do NOT re-implement the apt-get / useradd / ARG-RUNTIME_VERSION boilerplate locally.

The gate runs in WARN mode for one deploy cycle before going BLOCK.

## Rollout (per RFC #3018)

- Stage 1 (this PR): publish base image + WARN-mode drift gate
- Stage 2: pilot 1 template (langgraph)
- Stage 3: fan out to remaining 8
- Stage 4: drift gate flips BLOCK
