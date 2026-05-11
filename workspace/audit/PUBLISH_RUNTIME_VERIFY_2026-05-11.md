# Publish-runtime pipeline verification — 2026-05-11

Marker file for the canonical end-to-end pipeline verification after
`publish-runtime-bot` provisioning (internal#327) + stale-tag drift
resolution (`runtime-v0.1.131` deleted from main).

## Purpose

Triggers `workspace/**` path filter on `publish-runtime-autobump.yml`,
exercising the full pipeline:

1. `publish-runtime-autobump / bump-and-tag` reads PyPI version, computes
   next, pushes tag `runtime-v0.1.131` (or higher) using new bot scope.
2. `publish-runtime.yml` fires on tag, builds + publishes to PyPI.
3. Cascade autobump: 9 template repos get their `.runtime-version`
   pinned to the new version.

## Acceptance criteria

- [ ] autobump bump-and-tag context green on merged commit
- [ ] tag `runtime-v0.1.131` (or computed next) exists on molecule-core
- [ ] publish-runtime.yml run green
- [ ] PyPI molecule-ai-workspace-runtime updated from 0.1.130
- [ ] 9 template repos updated their pinned runtime version

## Rollback

This file is informational only — no code dependency. Safe to delete
in any future PR once pipeline is proven stable.

— core-devops (per Hongming "long-term proper robust" directive 2026-05-11 19:48-19:50Z)
