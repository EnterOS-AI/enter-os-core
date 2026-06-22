# Architecture Overview

EnterOS is a platform for orchestrating AI agent workspaces that form an organizational hierarchy. Workspaces register with a central platform, communicate via A2A protocol, and are visualized on a drag-and-drop canvas.

## System Diagram

```
Canvas (Next.js :3000) ←WebSocket→ Platform (Go :8080) ←HTTP→ Postgres + Redis
                                                                  ↑
                                   Workspace A ←──A2A──→ Workspace B
                                   (Python agents)
                                        ↑ register/heartbeat ↑
                                        └───── Platform ─────┘
```

## Main Components

- **Workspace Server** (`workspace-server/`): Go/Gin control plane — workspace CRUD, registry, discovery, WebSocket hub, liveness monitoring.
- **Canvas** (`canvas/`): Next.js 15 + React Flow (@xyflow/react v12) + Zustand + Tailwind — visual workspace graph.
- **Workspace Runtime**: Shared runtime published from [`molecule-ai-workspace-runtime`](https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-runtime) to the EnterOS Gitea package registry. Supports LangGraph, Claude Code, OpenClaw, Hermes, Codex, and AutoGen. Each adapter lives in its own standalone template repo (e.g. `molecule-ai-workspace-template-claude-code`). See `docs/workspace-runtime-package.md` for the full picture.
- **molecli** (`workspace-server/cmd/cli/`): Go TUI dashboard (Bubbletea + Lipgloss) — real-time workspace monitoring, event log, health overview, delete/filter operations.

## Key Architectural Patterns

### Import Cycle Prevention

The platform uses function injection to avoid Go import cycles between `ws`, `registry`, and `events` packages:

- `ws.NewHub(canCommunicate AccessChecker)` — Hub accepts `registry.CanCommunicate` as a function parameter.
- `registry.StartLivenessMonitor(ctx, onOffline OfflineHandler)` — Liveness accepts a broadcaster callback.
- `registry.StartHealthSweep(ctx, checker ContainerChecker, interval, onOffline)` — Health sweep accepts a Docker checker interface.

Wiring happens in `workspace-server/cmd/server/main.go` — init order: `wh → onWorkspaceOffline → liveness/healthSweep → router`.

### Container Health Detection

Three layers detect dead containers (e.g. Docker Desktop crash):

1. **Passive (Redis TTL):** 60s heartbeat key expires → liveness monitor → auto-restart.
2. **Proactive (Health Sweep):** `registry.StartHealthSweep` polls Docker API every 15s — catches dead containers faster than TTL expiry.
3. **Reactive (A2A Proxy):** On connection error, checks `provisioner.IsRunning()` → immediate offline + restart.

All three call `onWorkspaceOffline`, which broadcasts `WORKSPACE_OFFLINE` and calls `go wh.RestartByID()`. Redis cleanup uses the shared `db.ClearWorkspaceKeys()` helper.

### Template Resolution (Workspace Create)

Runtime detection happens **before** the DB insert: if `payload.Runtime` is empty and a template is specified, the handler reads `runtime:` from `configsDir/template/config.yaml` first. If still empty, it defaults to `"claude-code"`. This ensures the correct runtime is persisted in the DB and used for container image selection.

When the requested template does not exist, the Create handler falls back in order:

1. Check `os.Stat(configsDir/template)` — use if exists.
2. Try `{runtime}-default` template (e.g. `claude-code-default/`).
3. Generate a default config via `ensureDefaultConfig()` (includes `.auth-token` copy for CLI runtimes).

### Communication Rules (`registry/access.go`)

`CanCommunicate(callerID, targetID)` determines whether two workspaces may communicate:

- Same workspace → allowed
- Siblings (same `parent_id`) → allowed
- Root-level siblings (both `parent_id IS NULL`) → allowed
- Parent ↔ child → allowed
- Everything else → denied

The A2A proxy (`POST /workspaces/:id/a2a`) enforces this for agent-to-agent calls. Canvas requests (no `X-Workspace-ID` header), self-calls, and system callers (`webhook:*`, `system:*`, `test:*` prefixes via `isSystemCaller()` in `a2a_proxy.go`) bypass the check.

### Handler Decomposition

Large handler functions are split into focused private helpers to keep individual functions under ~60 lines. The decomposition pattern used across the codebase:

- `a2a_proxy.go::proxyA2ARequest` — helpers: `resolveAgentURL`, `normalizeA2APayload`, `dispatchA2A`, `handleA2ADispatchError`, `maybeMarkContainerDead`, `logA2AFailure`, `logA2ASuccess`; sentinel `proxyDispatchBuildError`.
- `delegation.go::Delegate` — helpers: `bindDelegateRequest`, `lookupIdempotentDelegation`, `insertDelegationRow`; typed `insertDelegationOutcome` enum replaces a `(bool, bool)` positional return.
- `discovery.go::Discover` — helpers: `discoverWorkspacePeer`, `writeExternalWorkspaceURL`, `discoverHostPeer`.
- `activity.go::SessionSearch` — helpers: `parseSessionSearchParams`, `buildSessionSearchQuery`, `scanSessionSearchRows`.

When modifying any of these handlers, prefer extending the helper rather than inlining logic back into the top-level function.

### JSONB Gotcha

When inserting Go `[]byte` (from `json.Marshal`) into Postgres JSONB columns, you must:

1. Convert to `string()` first.
2. Use a `::jsonb` cast in the SQL statement.

`lib/pq` treats `[]byte` as `bytea`, not JSONB, so skipping either step silently stores binary data instead of a JSON value.

### WebSocket Events Flow

1. An action occurs (register, heartbeat, config change, etc.).
2. `broadcaster.RecordAndBroadcast()` inserts a row into the `structure_events` table and publishes to Redis pub/sub.
3. The Redis subscriber relays the message to the WebSocket hub.
4. The hub broadcasts to canvas clients (all events) and workspace clients (filtered by `CanCommunicate`).

### Canvas State Management

- **Initial load:** HTTP fetch from `GET /workspaces` → Zustand hydrate.
- **Real-time updates:** WebSocket events → `applyEvent()` in the Zustand store.
- **Position persistence:** `onNodeDragStop` → `PATCH /workspaces/:id` with `{x, y}`.
- **Embedded sub-workspaces:** `nestNode` sets `hidden: !!targetId` on child nodes; children render as recursive `TeamMemberChip` components inside the parent (up to 3 levels), not as separate canvas nodes. Use `n.data.parentId` (not React Flow's `n.parentId`) for hierarchy lookups.
- **Chat:** two sub-tabs — "My Chat" (user↔agent, `source=canvas`) and "Agent Comms" (agent↔agent A2A traffic, `source=agent`). History loaded from `GET /activity` with source filter. Real-time via `A2A_RESPONSE` + `AGENT_MESSAGE` WebSocket events. Conversation history (last 20 messages) sent via `params.metadata.history` in A2A `message/send` requests.
- **Config save:** "Save & Restart" writes `config.yaml` and auto-restarts the workspace. "Save" writes only (shows a restart banner). Secrets POST/DELETE auto-restart on the platform side.

### Initial Prompt

Agents can auto-execute a prompt on startup before any user interaction. Configure via `initial_prompt` (inline string) or `initial_prompt_file` (path relative to config dir) in `config.yaml`. After the A2A server is ready, `main.py` sends the prompt as a `message/send` to self. A `.initial_prompt_done` marker file prevents re-execution on restart. Org templates support `initial_prompt` on both `defaults` (applies to all agents) and per-workspace (overrides the default).

**Important:** Initial prompts must not send A2A messages (`delegate_task`, `send_message_to_user`) because other agents may not yet be ready. Keep them local: clone repos, read docs, save to memory, wait for tasks.

### Idle Loop

Opt-in pattern: when `idle_prompt` is non-empty in `config.yaml`, the workspace self-sends it every `idle_interval_seconds` (default 600) **while `heartbeat.active_tasks == 0`**. The idle check is local (no LLM call) and the prompt only fires when there is genuinely nothing to do. Set per-workspace or as a per-org default in `org.yaml`. The fire timeout clamps to `max(60, min(300, idle_interval_seconds))`. Both the idle loop and `initial_prompt` self-posts include `auth_headers()` so they work in multi-tenant mode.

### Admin Auth Middleware Variants

Three Gin middleware classes gate server-side routes. Full contract in `docs/runbooks/admin-auth.md`.

- **`middleware.AdminAuth(db.DB)`** — strict bearer-only and **fail-closed in every environment** (harden/no-fail-open-auth). Used for any route where a forged request could leak prompts/memory, create/mutate workspaces, or leak ops intel. The former lazy-bootstrap fail-open (pass when `HasAnyLiveTokenGlobal` returns 0) and the dev-mode escape hatch have both been removed — a fresh install must provision `ADMIN_TOKEN` to reach admin routes.
- **`middleware.CanvasOrBearer(db.DB)`** — accepts a bearer token OR an Origin matching `CORS_ORIGINS`. Used **only** for cosmetic routes where a forged request has zero data/security impact. Currently only on `PUT /canvas/viewport`. Do not extend this to any route that leaks data or creates resources — see the runbook.
- **`middleware.WorkspaceAuth(db.DB)`** — binds a bearer token to `:id`. Workspace A's token cannot hit workspace B's sub-routes. Used for the entire `/workspaces/:id/*` group except the A2A proxy (which has its own `CanCommunicate` layer).

### Migration Runner (`workspace-server/internal/db/postgres.go`)

`RunMigrations` globs `*.sql` in `migrationsDir`, filters out `.down.sql` files, sorts alphabetically, then `DB.Exec()`s each file on boot. The filter is load-bearing: without it, alphabetical sort places `.down.sql` before `.up.sql` (since "d" sorts before "u"), which would wipe tables like `workspace_auth_tokens` on every boot. All `.up.sql` files must be **idempotent** (`CREATE TABLE IF NOT EXISTS`, `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`) because the runner re-applies every migration on every startup.

### Workspace Lifecycle

```
provisioning → online → degraded → online → offline → (auto-restart) → provisioning → ... → removed
     ↑                                                                                         ↑
     └──────────────────────────── paused ◄──────── any state ──────────────────────────────┘
                                      │
                                      └── (user resumes) → provisioning
```

State transitions:

- `provisioning` → `online`: workspace registers via `/registry/register`.
- `online` → `degraded`: error rate exceeds 0.5.
- `degraded` → `online`: error rate recovers.
- `online`/`degraded` → `offline`: Redis TTL expires OR the health sweep detects a dead container.
- `offline` → `provisioning`: auto-restart fires.
- Any state → `paused`: user pauses the workspace (container is stopped).
- `paused` → `provisioning`: user resumes.
- Any state → `removed`: workspace is deleted.

Paused workspaces are excluded from the health sweep, liveness monitor, and auto-restart.

**Restart context message:** After any restart and successful re-registration, the platform sends a synthetic A2A `message/send` to the workspace with `metadata.kind=restart_context`. The body contains the restart timestamp, previous session end time + duration, and the env-var keys (keys only, never values) now available in the container. The sender uses the `system:restart-context` caller prefix, which bypasses `CanCommunicate` via `isSystemCaller()`. If the workspace does not re-register within 30 seconds, the message is dropped (logged). Handler: `workspace-server/internal/handlers/restart_context.go`.
