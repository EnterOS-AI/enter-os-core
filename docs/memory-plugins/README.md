# Writing a Memory Plugin

This document is for operators and ecosystem authors who want to
replace the built-in postgres-backed memory plugin (the default
implementation that ships with workspace-server) with their own.

The contract was introduced by RFC #2728. The shipped binary is
`cmd/memory-plugin-postgres/`; reading its source is the fastest way
to see a complete reference implementation.

## What the contract is

The plugin is an HTTP server that workspace-server talks to via the
OpenAPI v1 spec at [`docs/api-protocol/memory-plugin-v1.yaml`](../api-protocol/memory-plugin-v1.yaml).

Six endpoints:

| Endpoint | Method | Purpose |
|---|---|---|
| `/v1/health` | GET | Liveness probe + capability list |
| `/v1/namespaces/{name}` | PUT | Idempotent upsert |
| `/v1/namespaces/{name}` | PATCH | Update TTL or metadata |
| `/v1/namespaces/{name}` | DELETE | Remove namespace and its memories |
| `/v1/namespaces/{name}/memories` | POST | Write a memory |
| `/v1/search` | POST | Multi-namespace search |
| `/v1/memories/{id}` | DELETE | Forget a memory |

The wire types are defined in
`workspace-server/internal/memory/contract/contract.go`. Run-time
validation is built into the Go bindings via `Validate()` methods —
your plugin SHOULD perform equivalent validation.

## What workspace-server takes care of

You do **not** implement these in the plugin; workspace-server is the
security perimeter:

- **Secret redaction** (SAFE-T1201). All `content` you receive is
  already scrubbed. Don't run additional redaction; it's pointless.
- **Namespace ACL**. workspace-server intersects the caller's
  readable namespaces against the requested list before sending you
  the search request. The list you receive is authoritative.
- **GLOBAL audit**. Org-namespace writes are recorded in
  `activity_logs` server-side; you don't see them.
- **Prompt-injection wrap**. Org memories returned to agents get a
  `[MEMORY id=... scope=ORG ns=...]:` prefix added at the
  workspace-server layer. Your `content` field is plain text.

## What you implement

- Storage of `memory_namespaces` and `memory_records` (or whatever
  shape you want — Pinecone vectors, an in-memory map, etc.)
- The 7 endpoints above with the request/response shapes the spec
  defines
- `/v1/health` reporting your supported capabilities (see below)
- Idempotency on namespace upsert (PUT semantics, not POST)
- Idempotency on memory commit when `MemoryWrite.id` is supplied
  (see "Memory idempotency" below)

## Memory idempotency

`MemoryWrite.id` is optional. Two contracts to honor:

| Caller passes | Plugin MUST |
|---|---|
| `id` omitted | Generate a fresh UUID, return it in the response |
| `id` set | Upsert keyed on this id — if a row with that id already exists, UPDATE it in place rather than inserting a duplicate |

The backfill CLI (`memory-backfill`) relies on the upsert behavior
so retries don't duplicate rows. Production agent commits leave `id`
empty and rely on the plugin's UUID generator — the hot path is
unchanged.

The built-in postgres plugin implements this with `INSERT ... ON
CONFLICT (id) DO UPDATE`. A vector-DB plugin (e.g., Pinecone) would
use the database's native upsert primitive on the same id.

## Capability negotiation

Your `/v1/health` response declares what features you support:

```json
{
  "status": "ok",
  "version": "1.0.0",
  "capabilities": ["embedding", "fts", "ttl", "pin", "propagation"]
}
```

| Capability | What it gates |
|---|---|
| `embedding` | Agents may ask for semantic search; you receive `embedding: [...]` in search bodies |
| `fts` | Agents may pass a query string; you decide how to match (FTS, ILIKE, regex) |
| `ttl` | Agents may set `expires_at`; you must not return expired rows |
| `pin` | Agents may set `pin: true`; you should rank pinned rows first |
| `propagation` | Agents may set `propagation: {...}`; you must store it as opaque JSON and return it on read |

A capability you DON'T list is fine — workspace-server adapts the MCP
tool surface to match. E.g., a Pinecone-only plugin that lists only
`embedding` will silently ignore agents' `query` strings.

## Deployment models

Three common shapes:

1. **Same machine, different process**: workspace-server boots, then
   `MEMORY_PLUGIN_URL=http://localhost:9100` points at your plugin
   running on a unix socket or localhost port. This is what the
   built-in postgres plugin does.

2. **Separate container**: deploy your plugin as its own service on
   the private network. Set `MEMORY_PLUGIN_URL` to its DNS name.

3. **Self-managed**: customer-owned plugin running on customer-owned
   infrastructure, accessed over a tunnel. Same env-var wiring.

Auth is **none** — the plugin must be reachable only on a private
network. workspace-server is the only sanctioned client.

## Replacing the built-in plugin

This is the canonical operator runbook for swapping the default
plugin out. The same sequence applies whether you're swapping for
another postgres plugin variant, Pinecone, Letta, or a custom
implementation.

1. **Stand up the new plugin.** Deploy the binary/container, confirm
   it boots, confirm `/v1/health` returns `ok` with the capability
   list you expect.

2. **Run the backfill in dry-run mode** to scope the migration:
   ```bash
   DATABASE_URL=postgres://... \
   MEMORY_PLUGIN_URL=http://your-plugin:9100 \
   memory-backfill -dry-run
   ```
   Reports row count + namespace mapping per workspace, no writes.

3. **Apply the backfill:**
   ```bash
   memory-backfill -apply
   ```
   Idempotent on retry — the backfill passes each `agent_memories.id`
   to `MemoryWrite.id`, so partial-then-full re-runs upsert in place.

4. **Verify parity** before flipping the cutover flag:
   ```bash
   memory-backfill -verify -verify-sample=200
   ```
   Random-samples N workspaces, diffs `agent_memories` direct query
   against plugin search via the workspace's readable namespaces.
   Reports mismatches and exits non-zero if any are found — wire
   into your CI to gate the cutover.

5. **Flip the cutover flag.** Set `MEMORY_V2_CUTOVER=true` on
   workspace-server and restart. Admin export/import now route
   through the plugin; legacy `agent_memories` becomes read-only.

6. **Existing data in the old plugin's tables is NOT auto-dropped.**
   Deliberate safety property — operator drops manually after the
   ~60-day grace window. If you switch back later, old data comes
   back into use (no loss).

If `-verify` reports mismatches, do NOT set `MEMORY_V2_CUTOVER` —
inspect the output, re-run `-apply` to backfill missing rows (it
upserts, so this is safe), and re-verify.

## Worked examples

- [`pinecone-example/`](pinecone-example/) — full Pinecone-backed plugin
- [`testing-your-plugin.md`](testing-your-plugin.md) — running the
  contract test harness against your implementation

## When to write one vs. fork the default

Fork the default postgres plugin if:
- You want different SQL (Materialized views? Different vector index?)
- You want extra auth on top
- You want server-side metrics emission

Write a fresh plugin if:
- The storage backend is fundamentally different (vector DB, KV store,
  in-memory, file-based)
- You're integrating an existing memory service (Letta, Mem0, etc.)

## See also

- [`CHANGELOG.md`](CHANGELOG.md) — contract revisions and fixup waves
- RFC #2728 — design rationale
- [`cmd/memory-plugin-postgres/`](../../workspace-server/cmd/memory-plugin-postgres/) — reference implementation
- [`docs/api-protocol/memory-plugin-v1.yaml`](../api-protocol/memory-plugin-v1.yaml) — full OpenAPI spec
