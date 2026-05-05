# Platform API (Go Backend)

The Go backend is Molecule AI's control plane. It does not execute agent reasoning itself. It manages the infrastructure and coordination around workspaces.

## Responsibilities

- workspace lifecycle
- registry and heartbeats
- hierarchy-aware discovery
- A2A proxying for browser-initiated calls
- approvals and activity logs
- memory APIs
- secrets and global secrets
- files, templates, bundles, terminal, and viewport state
- WebSocket fanout to canvas clients and workspaces

## Caller Identification

Workspace-scoped calls use the `X-Workspace-ID` header when the caller is another workspace. Browser/canvas calls do not send that header.

The platform uses the caller identity to enforce hierarchy-based access rules.


## Breaking Changes

### PR #701 — Input validation, route auth, UUID safety (2026-04-17)

**Affects:** `PATCH /workspaces/:id`, `GET /workspaces/:id`, `DELETE /workspaces/:id`, `GET /templates`, `GET /org/templates`

| Change | Before | After |
|---|---|---|
| `PATCH /workspaces/:id` auth | Open router — no token required for cosmetic fields | `wsAuth` group — workspace bearer token required unconditionally |
| `GET /templates` auth | No auth | AdminAuth |
| `GET /org/templates` auth | No auth | AdminAuth |
| `:id` path parameter validation | DB query with raw string; Postgres error on non-UUID | `uuid.Parse` check before DB access — 400 `"invalid workspace id"` on non-UUID |

**Field validation added to `POST /workspaces` and `PATCH /workspaces/:id`:**

| Field | Max length | Additional constraints |
|---|---|---|
| `name` | 255 chars | No `\n`, `\r`, or YAML-special chars (`{}[]|>*&!`) |
| `role` | 1,000 chars | No `\n`, `\r`, or YAML-special chars |
| `model` | 100 chars | No `\n`, `\r` |
| `runtime` | 100 chars | No `\n`, `\r` |

Violations return `400 Bad Request` with `{ "error": "<field> must be at most N characters" }` or `{ "error": "<field> must not contain newline characters" }`.

**Migration steps for callers:**
1. Add `Authorization: Bearer <workspace-token>` to all `PATCH /workspaces/:id` requests.
2. Add an admin bearer token to `GET /templates` and `GET /org/templates` requests.
3. Ensure `:id` values in E2E scripts and automation are valid UUIDs. Update any test fixtures that use non-UUID IDs (see `workspace-server/internal/handlers/*_test.go` for updated examples).

## Core Endpoints

### Health and metrics

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Health check |
| `GET` | `/metrics` | Prometheus metrics |

### Workspaces

| Method | Path | Description |
|---|---|---|
| `POST` | `/workspaces` | Create and provision a workspace |
| `GET` | `/workspaces` | List workspaces with inline canvas layout data |
| `GET` | `/workspaces/:id` | Get one workspace |
| `PATCH` | `/workspaces/:id` | Update workspace fields. **Requires workspace bearer token (WorkspaceAuth).** Validates `name` (≤255), `role` (≤1000), `model`/`runtime` (≤100 chars); `name` and `role` reject newlines and YAML-special chars (`{}[]|>*&!`). `:id` must be a valid UUID. See [Breaking Changes](#breaking-changes). |
| `DELETE` | `/workspaces/:id` | Remove workspace |
| `POST` | `/workspaces/:id/restart` | Restart workspace (reads runtime from container config.yaml before stop — detects runtime changes) |
| `POST` | `/workspaces/:id/pause` | Pause workspace |
| `POST` | `/workspaces/:id/resume` | Resume workspace |
| `POST` | `/workspaces/:id/a2a` | Proxy A2A request to the target workspace (synchronous, enforces hierarchy access control via `X-Workspace-ID`) |
| `POST` | `/workspaces/:id/delegate` | Async delegation — fire-and-forget, returns delegation_id |
| `GET` | `/workspaces/:id/delegations` | List delegation status (pending/completed/failed) |

### Async Delegation

`POST /workspaces/:id/delegate` sends a task to another workspace without blocking. The platform runs the A2A request in a background goroutine and returns immediately.

```json
POST /workspaces/:id/delegate
{"target_id": "<workspace-uuid>", "task": "Review the PLAN.md"}

→ 202 {"delegation_id": "...", "status": "delegated", "target_id": "..."}
```

Poll `GET /workspaces/:id/delegations` to check results. Each entry includes `delegation_id`, `status` (pending/completed/failed), and `response_preview`. WebSocket events `DELEGATION_COMPLETE` and `DELEGATION_FAILED` are broadcast on completion.

This is the recommended way for agents to delegate work — it works for all runtimes (Claude Code, LangGraph, etc.) since it operates at the platform level.

Workspace creation also assigns an `awareness_namespace` on the workspace row. That namespace is later injected into the provisioned runtime.

### Registry

| Method | Path | Description | Auth |
|---|---|---|---|
| `POST` | `/registry/register` | Workspace registration on startup. First register issues a per-workspace bearer token in the response body (`auth_token`); re-register is idempotent and omits the token. | — |
| `POST` | `/registry/heartbeat` | Liveness and task updates. | Phase 30.1 — `Authorization: Bearer <token>` required if the workspace has any live token on file; legacy workspaces grandfathered (fail-open). |
| `POST` | `/registry/update-card` | Push Agent Card updates after runtime/skill changes. | Phase 30.1 — same grandfather rule as `/heartbeat`. |
| `GET` | `/registry/discover/:id` | Resolve workspace URL for A2A calls. | Phase 30.6 — caller sends `X-Workspace-ID` + own bearer token; fail-open on DB hiccup (hierarchy check is primary gate). |
| `GET` | `/registry/:id/peers` | List reachable peers. | Phase 30.6 — same as `/discover/:id`. |
| `POST` | `/registry/check-access` | Validate reachability/access. | — |

**Why the auth callout matters:** remote (Phase 30) agents authenticate themselves with the bearer token returned by `POST /registry/register`. Local containers are transparent to this during the lazy-bootstrap grace window — the provisioner threads the token in as an env var on first register. See `docs/development/testing-e2e.md` for how E2E scripts handle token capture. If you change these routes, update `tests/e2e/test_api.sh` in the same PR.

### Activity and recall

| Method | Path | Description |
|---|---|---|
| `GET` | `/workspaces/:id/activity` | List activity rows (`?type=`, `?source=canvas\|agent`, `?limit=`) |
| `POST` | `/workspaces/:id/activity` | Report activity from a workspace |
| `POST` | `/workspaces/:id/notify` | Emit user-facing notifications/activity |
| `GET` | `/workspaces/:id/session-search` | Search recent activity + memory for recall |

### Memory

There are two distinct memory surfaces:

#### Scoped agent memory

| Method | Path | Description |
|---|---|---|
| `POST` | `/workspaces/:id/memories` | Commit a `LOCAL` / `TEAM` / `GLOBAL` memory |
| `GET` | `/workspaces/:id/memories` | Search scoped memories |
| `DELETE` | `/workspaces/:id/memories/:memoryId` | Delete an owned memory |

#### Key/value workspace memory

| Method | Path | Description |
|---|---|---|
| `GET` | `/workspaces/:id/memory` | List key/value memory entries |
| `GET` | `/workspaces/:id/memory/:key` | Get one key/value entry |
| `POST` | `/workspaces/:id/memory` | Upsert a key/value entry with optional TTL |
| `DELETE` | `/workspaces/:id/memory/:key` | Delete a key/value entry |

### Secrets

#### Workspace secrets

| Method | Path | Description |
|---|---|---|
| `GET` | `/workspaces/:id/secrets` | Return merged workspace + inherited global secret metadata |
| `POST` | `/workspaces/:id/secrets` | Upsert workspace secret |
| `PUT` | `/workspaces/:id/secrets` | Upsert workspace secret |
| `DELETE` | `/workspaces/:id/secrets/:key` | Delete workspace secret |
| `GET` | `/workspaces/:id/model` | Get workspace model override |

Important detail: `GET /workspaces/:id/secrets` does **not** return values. It returns key metadata plus a `scope` field so the frontend can distinguish inherited globals from workspace overrides.

#### Global secrets

| Method | Path | Description |
|---|---|---|
| `GET` | `/settings/secrets` | List global secret metadata |
| `POST` | `/settings/secrets` | Upsert global secret |
| `PUT` | `/settings/secrets` | Upsert global secret |
| `DELETE` | `/settings/secrets/:key` | Delete global secret |

Backward-compatible admin aliases also exist under `/admin/secrets`.

### Approvals

| Method | Path | Description |
|---|---|---|
| `GET` | `/approvals/pending` | List pending approvals |
| `POST` | `/workspaces/:id/approvals` | Create approval request |
| `GET` | `/workspaces/:id/approvals` | List approvals for a workspace |
| `POST` | `/workspaces/:id/approvals/:approvalId/decide` | Approve or deny |

### Team operations

| Method | Path | Description |
|---|---|---|
| `POST` | `/workspaces/:id/expand` | Expand workspace into a team |
| `POST` | `/workspaces/:id/collapse` | Collapse team back down |

### Plugins

| Method | Path | Description |
|---|---|---|
| `GET` | `/plugins` | List available plugins; accepts `?runtime=<name>` to filter to compatible plugins |
| `GET` | `/plugins/sources` | List registered install-source schemes (e.g. `{"schemes":["github","local"]}`) |
| `GET` | `/workspaces/:id/plugins` | List installed plugins (each includes `supported_on_runtime: bool`) |
| `GET` | `/workspaces/:id/plugins/available` | Plugins filtered to those compatible with the workspace runtime |
| `GET` | `/workspaces/:id/plugins/compatibility?runtime=X` | Preflight runtime change — which installed plugins would become inert |
| `POST` | `/workspaces/:id/plugins` | Install plugin `{"source":"<scheme>://<spec>"}` — e.g. `local://ecc`, `github://owner/repo#v1.0`. Auto-restarts workspace. |
| `DELETE` | `/workspaces/:id/plugins/:name` | Uninstall plugin — removes from container, auto-restarts |

Plugins are installed per-workspace into `/configs/plugins/<name>/`. Sources are pluggable via schemes (local + github shipped; clawhub/oci/https planned). See [`docs/plugins/sources.md`](../plugins/sources.md) for the two-axis source/shape model.

Install safeguards bound the cost of a single install (env-tunable via `PLUGIN_INSTALL_BODY_MAX_BYTES` / `PLUGIN_INSTALL_FETCH_TIMEOUT` / `PLUGIN_INSTALL_MAX_DIR_BYTES`).

### Files and templates

| Method | Path | Description |
|---|---|---|
| `GET` | `/templates` | List available templates. **Requires AdminAuth** (PR #701). |
| `GET` | `/org/templates` | List available org templates. **Requires AdminAuth** (PR #701). |
| `POST` | `/templates/import` | Import an agent folder as a new template |
| `GET` | `/workspaces/:id/files` | List files under an allowed root |
| `GET` | `/workspaces/:id/files/*path` | Read a file |
| `PUT` | `/workspaces/:id/files/*path` | Write a file |
| `PUT` | `/workspaces/:id/files` | Replace workspace file set |
| `DELETE` | `/workspaces/:id/files/*path` | Delete a file |

Query parameters for `GET /workspaces/:id/files`:

| Param | Default | Description |
|-------|---------|-------------|
| `root` | `/configs` | Base path — one of `/configs`, `/workspace`, `/home`, `/plugins` |
| `path` | `""` | Subdirectory relative to root (validated against path traversal) |
| `depth` | `1` | Max recursion depth (1–5). Use with `path` for lazy-loading subdirectories |

Invalid `depth` or traversal paths return 400.

### Terminal

| Protocol | Path | Description |
|---|---|---|
| `WS` | `/workspaces/:id/terminal` | Terminal session into the running container |

### Bundles

| Method | Path | Description |
|---|---|---|
| `GET` | `/bundles/export/:id` | Export workspace tree as a bundle |
| `POST` | `/bundles/import` | Import a bundle |

### Canvas viewport and events

| Method | Path | Description |
|---|---|---|
| `GET` | `/canvas/viewport` | Get saved canvas pan/zoom |
| `PUT` | `/canvas/viewport` | Save canvas pan/zoom |
| `GET` | `/events` | List structure events |
| `GET` | `/events/:workspaceId` | List workspace-scoped events |

### WebSocket

| Protocol | Path | Description |
|---|---|---|
| `WS` | `/ws` | Live events for canvas clients and workspaces |

Canvas clients receive the global event stream. Workspaces connect with `X-Workspace-ID` and receive filtered events based on communication rules.

## A2A Proxy Behavior

`POST /workspaces/:id/a2a` is more than a naive forwarder.

It currently:

- enforces access control via `CanCommunicate` for agent-to-agent calls (workspace caller IDs from `X-Workspace-ID`); canvas requests, self-calls, and system callers (`webhook:*`, `system:*`, `test:*`) bypass
- normalizes incoming JSON into JSON-RPC 2.0
- injects `messageId` when missing
- applies different timeout rules for browser-initiated vs workspace-initiated calls
- logs the resulting A2A activity
- broadcasts successful browser-initiated responses back to the canvas as `A2A_RESPONSE`
- triggers restart flow when the target container is confirmed dead

That is why the chat UX no longer depends on polling as the primary response path.

## Environment Variables

```bash
DATABASE_URL=postgres://dev:dev@postgres:5432/molecule?sslmode=prefer
REDIS_URL=redis://redis:6379
PORT=8080
SECRETS_ENCRYPTION_KEY=...
ACTIVITY_RETENTION_DAYS=7
ACTIVITY_CLEANUP_INTERVAL_HOURS=6
CORS_ORIGINS=http://localhost:3000,http://localhost:3001
RATE_LIMIT=600
```

## Related Docs

- [Registry & Heartbeat](./registry-and-heartbeat.md)
- [Communication Rules](./communication-rules.md)
- [Workspace Runtime](../agent-runtime/workspace-runtime.md)
- [Canvas UI](../frontend/canvas.md)
