# API Reference

This document describes the REST API exposed by the Molecule AI workspace server (Go/Gin, default port `:8080`). Clients include the Canvas frontend, workspace agents communicating over A2A, and external tooling such as the MCP server and CLI.

**Base URL:** `http://localhost:8080` (development default)
**Rate limit:** 600 req/min (configurable via `RATE_LIMIT`)
**CORS origins:** `http://localhost:3000,http://localhost:3001` by default (configurable via `CORS_ORIGINS`)

---

## Authentication

Three middleware classes gate server-side routes:

- **`AdminAuth`** — strict bearer-only. Required for any route that can leak prompts/memory, create/mutate workspaces, or expose ops intel. Lazy-bootstrap fail-open when no live tokens exist globally.
- **`WorkspaceAuth`** — binds a bearer token to a specific workspace `:id`. A token for workspace A cannot be used against workspace B's sub-routes.
- **`CanvasOrBearer`** — accepts a bearer token OR a request Origin matching `CORS_ORIGINS`. Used only for cosmetic routes with zero data/security impact (currently `PUT /canvas/viewport` only). Do not extend to routes that leak data or create resources.

Full contract: `docs/runbooks/admin-auth.md`.

---

## Routes

| Method | Path | Handler |
|--------|------|---------|
| GET | /health | inline |
| GET | /metrics | metrics.Handler() — Prometheus text format; no auth, scrape-safe |
| POST/GET/PATCH/DELETE | /workspaces[/:id] | workspace.go — `GET /workspaces`, `POST /workspaces`, and `DELETE /workspaces/:id` require `AdminAuth`. `PATCH /workspaces/:id` enforces field-level authz: cosmetic fields (name, role, x, y, canvas) pass through; sensitive fields (tier, parent_id, runtime, workspace_dir) require a valid bearer token when any live token exists. |
| GET/PATCH | /workspaces/:id/config | workspace.go |
| GET/POST | /workspaces/:id/memory | workspace.go |
| DELETE | /workspaces/:id/memory/:key | workspace.go |
| POST/PATCH/DELETE | /workspaces/:id/agent | agent.go |
| POST | /workspaces/:id/agent/move | agent.go |
| GET/POST/PUT | /workspaces/:id/secrets | secrets.go (POST/PUT auto-restarts workspace) |
| DELETE | /workspaces/:id/secrets/:key | secrets.go (DELETE auto-restarts workspace) |
| GET | /workspaces/:id/model | secrets.go |
| GET | /settings/secrets | secrets.go — list global secrets (keys only, values masked) |
| PUT/POST | /settings/secrets | secrets.go — set a global secret `{key, value}`; auto-restarts every non-paused/non-removed/non-external workspace that does not shadow the key with a workspace-level override |
| DELETE | /settings/secrets/:key | secrets.go — delete a global secret; same auto-restart fan-out as PUT/POST |
| GET | /admin/workspaces/:id/test-token | admin_test_token.go — mint a fresh bearer token for E2E scripts; returns 404 unless `MOLECULE_ENV != production` or `MOLECULE_ENABLE_TEST_TOKENS=1` |
| GET/POST/DELETE | /admin/secrets[/:key] | secrets.go — legacy aliases for /settings/secrets |
| WS | /workspaces/:id/terminal | terminal.go |
| POST/GET | /workspaces/:id/approvals | approvals.go |
| POST | /workspaces/:id/approvals/:id/decide | approvals.go |
| GET | /approvals/pending | approvals.go |
| POST/GET | /workspaces/:id/memories | memories.go |
| DELETE | /workspaces/:id/memories/:id | memories.go |
| GET | /workspaces/:id/traces | traces.go |
| GET/POST | /workspaces/:id/activity | activity.go |
| POST | /workspaces/:id/notify | activity.go (agent→user push message via WebSocket) |
| POST | /workspaces/:id/restart | workspace.go |
| POST | /workspaces/:id/pause | workspace.go (stops container, status→paused) |
| POST | /workspaces/:id/resume | workspace.go (re-provisions paused workspace) |
| POST | /workspaces/:id/a2a | workspace.go |
| POST | /workspaces/:id/delegate | delegation.go (async fire-and-forget) |
| GET | /workspaces/:id/delegations | delegation.go (list delegation status) |
| GET/POST | /workspaces/:id/schedules | schedules.go (cron CRUD) |
| PATCH/DELETE | /workspaces/:id/schedules/:scheduleId | schedules.go |
| POST | /workspaces/:id/schedules/:scheduleId/run | schedules.go (manual trigger) |
| GET | /workspaces/:id/schedules/:scheduleId/history | schedules.go (past runs) |
| GET/POST | /workspaces/:id/channels | channels.go (social channel CRUD) |
| PATCH/DELETE | /workspaces/:id/channels/:channelId | channels.go |
| POST | /workspaces/:id/channels/:channelId/send | channels.go (outbound message) |
| POST | /workspaces/:id/channels/:channelId/test | channels.go (test connection) |
| GET | /channels/adapters | channels.go (list available platforms) |
| POST | /channels/discover | channels.go (auto-detect chats for a bot token) |
| POST | /webhooks/:type | channels.go (incoming social webhook) |
| GET/PUT/DELETE | /workspaces/:id/files[/*path] | templates.go |
| GET | /canvas/viewport | viewport.go — open, no auth required (cosmetic, bootstrap-friendly) |
| PUT | /canvas/viewport | viewport.go — `CanvasOrBearer` middleware; accepts bearer OR Origin matching `CORS_ORIGINS`. Cosmetic-only route — worst case viewport corruption, recovered by page refresh. |
| GET | /templates | templates.go |
| POST | /templates/import | templates.go — `AdminAuth` required |
| POST | /registry/register | registry.go |
| POST | /registry/heartbeat | registry.go — requires `Authorization: Bearer <token>` once a workspace has any live token on file (legacy workspaces grandfathered) |
| POST | /registry/update-card | registry.go — requires `Authorization: Bearer <token>` once a workspace has any live token on file |
| GET | /registry/discover/:id | discovery.go — requires `X-Workspace-ID` + bearer token on the caller side |
| GET | /registry/:id/peers | discovery.go — requires `X-Workspace-ID` + bearer token on the caller side |
| POST | /registry/check-access | discovery.go |
| GET | /plugins | plugins.go (list registry; supports `?runtime=` filter) |
| GET | /plugins/sources | plugins.go (list registered install-source schemes) |
| GET/POST/DELETE | /workspaces/:id/plugins[/:name] | plugins.go — list, install (`{"source":"scheme://spec"}`), uninstall per-workspace |
| GET | /workspaces/:id/plugins/available | plugins.go (filtered by workspace runtime) |
| GET | /workspaces/:id/plugins/compatibility?runtime=X | plugins.go (preflight runtime-change check) |
| GET/POST | /workspaces/:id/tokens | tokens.go — list active tokens (prefix + metadata), create new token (plaintext returned once). Max 50 per workspace. |
| DELETE | /workspaces/:id/tokens/:tokenId | tokens.go — revoke specific token by ID |
| GET | /bundles/export/:id | bundle.go — `AdminAuth` required |
| POST | /bundles/import | bundle.go — `AdminAuth` required |
| GET | /org/templates | org.go (list available org templates) |
| POST | /org/import | org.go — `AdminAuth` required; applies `resolveInsideRoot` path sanitiser on template paths |
| GET | /events | events.go — `AdminAuth` required |
| GET | /events/:workspaceId | events.go — `AdminAuth` required |
| GET | /admin/liveness | inline — `AdminAuth` required. Returns per-subsystem `supervised.Snapshot()` ages; use to check health of scheduler/heartbeat goroutines |
| GET | /ws | socket.go |

---

## Database

Migration files live in `workspace-server/migrations/` (latest: `022_workspace_schedules_source`). Each migration ships as a `.up.sql`/`.down.sql` pair. The migration runner globs `*.sql`, filters out `.down.sql` files, sorts alphabetically, and executes each file on boot. All `.up.sql` files must be idempotent (`CREATE TABLE IF NOT EXISTS`, `ALTER TABLE ... IF NOT EXISTS`) because the runner re-applies every migration on every boot.

### Key Tables

| Table | Description |
|-------|-------------|
| `workspaces` | Core entity — status, runtime, `agent_card` JSONB, heartbeat columns, `current_task`, `awareness_namespace`, `workspace_dir` |
| `canvas_layouts` | Per-workspace x/y canvas position |
| `structure_events` | Append-only event log (workspace lifecycle, agent, approval events) |
| `activity_logs` | A2A communications, task updates, agent logs, errors. `error_detail` is populated by the scheduler so cron run history can surface failure reasons. |
| `workspace_schedules` | Cron tasks — expression, timezone, prompt, run history, `source` (`'template'` for org/import-seeded, `'runtime'` for Canvas/API-created), `last_status` (includes `'skipped'` when the scheduler concurrency-skips a busy workspace) |
| `workspace_channels` | Social channel integrations (Telegram, Slack, etc.) with JSONB config and allowlist |
| `agents` | Agent records |
| `workspace_secrets` | Per-workspace encrypted secrets |
| `global_secrets` | Platform-wide encrypted secrets |
| `workspace_auth_tokens` | Bearer tokens; auto-revoked on workspace delete |
| `agent_memories` | HMA scoped memory (LOCAL / TEAM / GLOBAL) |
| `approvals` | Human-in-the-loop approval requests |
