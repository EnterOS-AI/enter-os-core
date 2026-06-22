# Memory Architecture (HMA)

EnterOS's memory model is built around one principle:

> memory boundaries should follow organizational boundaries.

That is the purpose of **HMA: Hierarchical Memory Architecture**.

The organizational boundary is enforced **physically**, not at the application layer: each org runs as its own tenant on its own EC2, with its own memory plugin sidecar and its own Postgres. Memory writes are loopback-only — never cross-tenant. See [`workspace-placement.md`](workspace-placement.md) for the architecture contract that makes HMA tenant-isolated by construction.

## The Three Scopes

| Scope | Meaning | Intended use |
|---|---|---|
| `LOCAL` | visible only to the current workspace | private scratch facts and local recall |
| `TEAM` | visible to the local team boundary | handoffs between a parent and its direct children, or siblings under the same parent |
| `GLOBAL` | readable across the tree; writable only from the root side | org-wide guidance, standards, shared institutional knowledge |

These are the scopes exposed through the runtime memory tools:

- `commit_memory(content, scope)`
- `search_memory(query, scope)`

## What Exists In The Current Implementation

There are **multiple memory surfaces**, and the distinction matters.

### 1. Scoped agent memory (`agent_memories`)

This is the HMA-facing storage used by:

- `POST /workspaces/:id/memories`
- `GET /workspaces/:id/memories`
- runtime tools `commit_memory` / `search_memory`

It stores durable facts with a `LOCAL`, `TEAM`, or `GLOBAL` scope.

### 2. Workspace key/value memory (`workspace_memory`)

This is the simpler key/value surface used by the canvas `Memory` tab:

- `GET /workspaces/:id/memory`
- `POST /workspaces/:id/memory`
- `DELETE /workspaces/:id/memory/:key`

It is useful for structured per-workspace state and optional TTL entries. It is not the same thing as scoped HMA memories.

### 3. Activity recall (`session-search`)

`GET /workspaces/:id/session-search` provides a thin recall surface over recent activity rows and memory rows. It is for “what just happened in this workspace?” rather than long-term semantic storage.

### 4. Memory v2 plugin (`memory_records` / `memory_namespaces`)

This is the production-direction backend, behind the RFC #2728 HTTP
contract. The plugin runs as a sidecar on each tenant EC2 (auto-spawned
by `entrypoint-tenant.sh` when `MEMORY_PLUGIN_URL` is set), owns its
own tables under the `memory_plugin` schema, and serves:

- `POST /workspaces/:id/v2/memories` (canvas `MemoryInspectorPanel`)
- `GET /workspaces/:id/v2/memories`
- `DELETE /workspaces/:id/v2/memories/:id`
- runtime tools `commit_memory_v2`, `search_memory`, `commit_summary`,
  `forget_memory`
- legacy MCP tool names `commit_memory` / `recall_memory` via the
  scope→namespace shim in `mcp_tools_memory_legacy_shim.go`

Capability negotiation (FTS, embedding, TTL, pin, propagation) is
declared by the plugin via `GET /v1/health`; workspace-server adapts
the tool surface to what the plugin actually supports. See
[`memory-plugin-v1.yaml`](../api-protocol/memory-plugin-v1.yaml) for
the full wire contract.

## Access Model

EnterOS's memory rules follow the same hierarchy logic as communication rules:

- `LOCAL` belongs to one workspace
- `TEAM` follows the immediate team boundary
- `GLOBAL` is readable widely but writable only from the root side

The platform-side memory handlers still apply reachability checks for shared/team reads instead of trusting callers blindly.

## Current Schema Reality

The current `agent_memories` migration is intentionally simple:

```sql
CREATE TABLE IF NOT EXISTS agent_memories (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID REFERENCES workspaces(id),
    content      TEXT NOT NULL,
    scope        VARCHAR(10) NOT NULL CHECK (scope IN ('LOCAL', 'TEAM', 'GLOBAL')),
    created_at   TIMESTAMPTZ DEFAULT now(),
    updated_at   TIMESTAMPTZ DEFAULT now()
);
```

`pgvector` is **not enabled by default in the shipped migration**. The repo keeps vector support as an optional future extension, not as a current hard dependency. The docs should reflect that explicitly.

## Why This Architecture Matters

| Flat shared memory model | EnterOS HMA |
|---|---|
| easy to over-share | scopes align to hierarchy |
| unclear ownership | each memory belongs to a workspace and a scope |
| recall and procedure blur together | memory stores facts, skills store repeatable procedure |
| hard to govern | org structure and memory rules reinforce each other |

## Memory To Skill Promotion

EnterOS intentionally separates:

- **durable fact storage**
- **repeatable operational procedure**

The documented promotion path is:

1. a durable workflow is captured in memory
2. repeated success becomes a signal
3. the workflow is promoted into a skill package
4. the runtime hot-reloads that skill

This is why memory and skills are presented as adjacent systems, not one merged blob.

## Practical Summary

If you need:

- **private agent recall**: use `LOCAL`
- **shared team handoff knowledge**: use `TEAM`
- **org-wide guidance**: use `GLOBAL`
- **simple UI-visible structured state**: use `workspace_memory`
- **recent decision/task recall**: use `session-search`
- **semantic / FTS search across memories**: use the v2 plugin endpoints (`/v2/memories?q=…`); they go through the plugin's pgvector + tsvector indexes when the plugin declares the capability

## Related Docs

- [Workspace Runtime](../agent-runtime/workspace-runtime.md)
- [Skills](../agent-runtime/skills.md)
- [Communication Rules](../api-protocol/communication-rules.md)
- [Platform API](../api-protocol/platform-api.md)
