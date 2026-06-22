# Tests

This repo uses the standard monorepo testing convention: **unit tests live with their package, cross-component E2E tests live here.**

## Where to find tests

| Scope | Location |
|---|---|
| Go unit + integration (platform, CLI, handlers) | `workspace-server/**/*_test.go` — run with `cd workspace-server && go test -race ./...` |
| TypeScript unit (canvas components, hooks, store) | `canvas/src/**/__tests__/` — run with `cd canvas && npm test -- --run` |
| TypeScript unit (MCP server handlers) | `mcp-server/src/__tests__/` — run with `cd mcp-server && npx jest` |
| Python unit (workspace runtime, adapters) | `molecule-ai-workspace-runtime/tests/` in the standalone runtime repo |
| Python unit (SDK: plugin + remote agent) | `sdk/python/tests/` — run with `cd sdk/python && python3 -m pytest` |
| **Cross-component E2E** (spans platform + runtime + HTTP) | `tests/e2e/` ← **you are here** |

## Why split this way

- **Go** requires co-located `_test.go` files to access unexported symbols.
- **Per-package test commands** keep the inner loop fast — changing canvas doesn't re-run Go tests.
- **`tests/e2e/`** covers scenarios that no single package owns: a full workspace lifecycle, A2A across two provisioned agents, delegation chains, bundle round-trips.

## Running E2E

Every E2E script here assumes the platform is running at `localhost:8080` and (where noted) provisioned agents are online. See the header comment of each `.sh` for specifics.

## Cleaning up rogue test workspaces

If an E2E run aborts before its teardown runs (Ctrl-C, crash, CI timeout),
the platform can be left with workspaces whose config volume is stale or
empty — Docker's `unless-stopped` restart policy then spins those
containers in a FileNotFoundError loop. The platform's pre-flight check
(#17) marks such workspaces `failed` on the next restart, but a manual
cleanup is useful:

```bash
bash scripts/cleanup-rogue-workspaces.sh               # deletes ws with id/name starting aaaaaaaa-, bbbbbbbb-, cccccccc-, test-ws-
MOLECULE_URL=http://host:8080 bash scripts/cleanup-rogue-workspaces.sh
```

The script DELETEs each matching workspace via the API and
force-removes the `ws-<id[:12]>` container as a belt-and-suspenders
fallback.
