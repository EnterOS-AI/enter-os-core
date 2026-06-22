# Workspace Runtime Package

`molecule-ai-workspace-runtime` is the shared Python runtime consumed by
workspace template images and by external MCP integrations.

## Source Of Truth

The source of truth is the standalone Gitea repo:

```text
https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-runtime
```

Do not add runtime source back under `enter-os-core/workspace/`. The core repo
owns the platform server, canvas, provisioning, and tests around the installed
runtime package.

## Package Registry

The runtime package is published to the EnterOS Gitea package registry:

```text
https://git.moleculesai.app/api/packages/molecule-ai/pypi/simple/
```

PyPI is intentionally not part of the critical path. Template Dockerfiles,
external-runtime snippets, and CI install checks should use the Gitea registry.

## Release Flow

1. Land a reviewed PR in `molecule-ai-workspace-runtime`.
2. Bump `version =` in that repo's `pyproject.toml`.
3. Tag `runtime-vX.Y.Z` on the runtime repo.
4. The runtime repo's `publish-runtime` workflow builds the wheel and sdist,
   publishes to the Gitea registry, verifies install from that registry, then
   cascades `.runtime-version` pins to workspace template repos.

## Core Repo Contract

`enter-os-core` must not ship editable runtime code. Its responsibilities are:

- Test platform behavior against the installed runtime contract.
- Keep MCP/registry/TenantGuard behavior compatible with the runtime package.
- Fail CI if `workspace/` or legacy build-from-workspace scripts are restored.
