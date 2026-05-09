# Provisioner

The provisioner is the platform component that deploys workspace containers and VMs. It is triggered when a workspace is created, imported from a bundle, or expanded into a team.

## How It Works

1. Platform receives a workspace creation request (API call or bundle import)
2. Platform writes a `WORKSPACE_PROVISIONING` event and broadcasts it (canvas shows spinner)
3. Provisioner reads the workspace config (tier, model, env requirements)
4. Provisioner reads secrets from `workspace_secrets` table, decrypts them, prepares as env vars
5. Provisioner deploys based on tier (via `ApplyTierConfig()`):
   - **T1 (Sandboxed):** Docker container, readonly rootfs, tmpfs /tmp, no `/workspace` mount
   - **T2 (Standard):** Docker container + `/workspace` mount + resource limits (512 MiB, 1 CPU)
   - **T3 (Privileged):** Docker container, `--privileged` + host PID (Docker network, not host)
   - **T4 (Full Access):** Docker container, privileged + host PID + host network + Docker socket
5. Provisioner waits for first heartbeat (workspace is live)
6. On first heartbeat: status transitions to `online`
7. On timeout (3 minutes) or immediate error: status transitions to `failed`

## Docker Networking (Tier 1-3, Tier 4 uses host)

All workspace containers join the `molecule-core-net` Docker network. Containers are named `ws-{id[:12]}` (first 12 chars of workspace UUID). Two exported helpers in `provisioner` package provide the canonical naming:

- `provisioner.ContainerName(workspaceID)` → `ws-{id[:12]}`
- `provisioner.InternalURL(workspaceID)` → `http://ws-{id[:12]}:8000`

These are used by discovery, workspace provisioning, and terminal handlers — always use them instead of constructing names inline.

Containers are also given an ephemeral host port binding (`127.0.0.1:0→8000/tcp`) so the platform can reach them from the host.

After `ContainerStart`, the provisioner inspects the container to resolve the actual mapped port and stores the host-accessible URL:

```
http://127.0.0.1:{ephemeral_port}
```

This URL is pre-stored in both Postgres and Redis before the agent registers. When the agent calls `POST /registry/register`, the register endpoint preserves the provisioner URL (any URL starting with `http://127.0.0.1`) instead of overwriting it with the agent's Docker-internal hostname.

**Why not use Docker-internal URLs?** In local dev, the platform runs on the host (not in Docker), so it cannot resolve Docker container hostnames. The ephemeral port mapping lets the A2A proxy reach agents via localhost. In production (platform in Docker), the Docker-internal URL (`http://ws-{id}:8000`) would work directly.

**Workspace-to-workspace discovery:** When a workspace discovers another workspace (via `X-Workspace-ID` header on `GET /registry/discover/:id`), the platform returns the Docker-internal URL (`http://ws-{first12chars}:8000`) so containers can reach each other directly on `molecule-core-net`. The internal URL is cached in Redis at provision time and also synthesized as a fallback if the cache misses (only for online/degraded workspaces).

For external HTTPS access (multi-host mode), Nginx on the host handles TLS termination and proxies to the container.

## Tier-Based Container Flags

| Tier | Flags |
|------|-------|
| T1 (Sandboxed) | Config volume only, readonly rootfs, tmpfs /tmp, no `/workspace` mount |
| T2 (Standard) | Config + workspace volume, 512 MiB memory, 1 CPU |
| T3 (Privileged) | Config + workspace + `--privileged` + `--pid=host` (Docker network) |
| T4 (Full Access) | Config + workspace + `--privileged` + `--pid=host` + `--network=host` + Docker socket |

Tier configuration is applied via the exported `ApplyTierConfig()` function in `provisioner.go`. Unknown or zero tier values default to T2 (safe resource-limited container).

## Workspace Lifecycle States

```
provisioning -> online <-----> degraded
     |              |              |
     v              v              v
   failed        offline        offline
     |              |              |
     v              v              v
   removed        removed        removed
     ^              ^
     |              |
  (retry)     (re-register)
```

- `provisioning -> online`: first heartbeat received
- `online -> degraded`: error_rate >= 50% (via heartbeat self-report)
- `degraded -> online`: error_rate < 10% (recovered)
- `online/degraded -> offline`: heartbeat TTL expired OR proactive health sweep detects dead container
- `offline -> provisioning`: auto-restart triggered by liveness monitor or health sweep
- `provisioning -> failed`: 3min timeout or immediate Docker error
- `failed -> provisioning`: user clicks Retry on canvas
- `offline -> online`: workspace re-registers (after auto-restart or manual restart)
- `any -> paused`: user pauses workspace (container stopped, config preserved)
- `paused -> provisioning`: user resumes workspace
- `any -> removed`: user deletes workspace

| Status | Meaning | Canvas Display |
|--------|---------|----------------|
| `provisioning` | Container/VM is being spun up, waiting for first heartbeat | Spinner on node |
| `online` | Heartbeat received, reachable, accepting A2A messages | Green node |
| `degraded` | Online but error rate above 50%, self-reported via heartbeat | Yellow node with warning |
| `offline` | Heartbeat TTL expired, unreachable but not deleted | Gray node |
| `paused` | User paused — container stopped, config preserved, no auto-restart | Indigo node |
| `failed` | Provisioning timed out or immediate launch error | Red node + retry button |
| `removed` | User deleted it, kept in DB for event log + 410 responses | Node removed from canvas |

## Restart & Runtime Detection

When a workspace is restarted (`POST /workspaces/:id/restart`):

1. **Read runtime** from the `workspaces.runtime` column in Postgres
2. **Stop** the existing container
3. **Resolve template** — checks request body, name-based match, then runtime-default template (e.g. `claude-code-default/`)
4. **Re-provision** with the same config volume (configs persist across restarts)

**Runtime stored in DB:** The `runtime` column is set at creation time and persists across restarts. No need to read from the container.

**Template resolution at creation:** When a workspace specifies a template that doesn't exist (e.g. `org-marketing-lead`), the Create handler falls back in order: (1) `{runtime}-default` template (e.g. `claude-code-default/`), (2) `ensureDefaultConfig` (generates minimal config + copies `.auth-token` from `claude-code-default/`).

## Container Health Detection

Three layers detect dead containers:

1. **Passive (Redis TTL):** Each heartbeat refreshes a 60s Redis key (`ws:{id}`). When the key expires, the liveness monitor marks the workspace offline and triggers auto-restart. Gap: up to 60s of false "online" state.

2. **Proactive (Health Sweep):** A goroutine checks all online/degraded workspaces against Docker API (`ContainerInspect`) every 15 seconds. If a container is gone, it immediately marks the workspace offline, clears Redis caches, and triggers auto-restart. Catches bulk container death (e.g. Docker Desktop crash) within 15s.

3. **Reactive (A2A Proxy):** When the A2A proxy (`POST /workspaces/:id/a2a`) gets a connection error, it checks `provisioner.IsRunning()`. If the container is dead, it marks offline, clears caches, triggers restart, and returns 503 with `"restarting": true`. If the container is running but unresponsive, returns 502.

All three layers use the same `onWorkspaceOffline` callback: broadcast `WORKSPACE_OFFLINE` + `go wh.RestartByID(workspaceID)`. `RestartByID` has a per-workspace mutex (`TryLock`) that deduplicates concurrent restart attempts.

When a workspace goes offline and is auto-restarted, Redis keys are cleaned up via `db.ClearWorkspaceKeys()` which removes `ws:{id}`, `ws:{id}:url`, and `ws:{id}:internal_url`.

## Failure Handling

When provisioning fails:
1. Status set to `failed`
2. `WORKSPACE_PROVISION_FAILED` event written with reason
3. Canvas shows a red node with the error message
4. User can click **Retry** — resets status to `provisioning` and re-runs the provisioner

## Docker Volume Mounts

By default, each workspace gets an isolated named Docker volume:

```
docker volume: ws-{id}-workspace
  -> mounted at /workspace inside the container
  -> persists across: container restart, re-provision, image update
  -> destroyed only when: user deletes workspace or runs nuke.sh
```

The volume is named after the workspace ID, not the container name. So even when a container is destroyed and re-provisioned, the new container mounts the same volume. Tier 1 workspaces skip the workspace volume for read-only isolation.

### Per-Workspace Directory (`workspace_dir`)

Each workspace can optionally specify a host directory to bind-mount as `/workspace`. The priority chain is:

1. **Per-workspace `workspace_dir`** (DB column, set via API or org template) — highest priority
2. **Global `WORKSPACE_DIR` env var** — fallback for all workspaces without a per-workspace value
3. **Isolated Docker named volume** — default when neither is set

```yaml
# org-templates/molecule-dev/org.yaml
workspaces:
  - name: PM
    workspace_dir: /Users/you/project  # bind-mounts repo
  - name: Backend Engineer
    # no workspace_dir → isolated Docker volume
```

API support:
- `POST /workspaces {"workspace_dir": "/path"}` — set on create
- `PATCH /workspaces/:id {"workspace_dir": "/path"}` — update (returns `needs_restart: true`)
- `PATCH /workspaces/:id {"workspace_dir": null}` — clear (reverts to isolated volume)

Path validation: must be absolute, no `..` traversal, rejects system paths (`/etc`, `/var`, `/proc`, `/sys`, `/dev`, `/boot`, `/sbin`, `/bin`, `/lib`, `/usr`).

See [Memory](./memory.md) for full memory backend details.

## Container Cleanup

When a workspace is deleted:
1. Docker container is stopped and removed
2. Memory cleaned up (DB rows deleted, Redis keys cleared)
3. Workspace status set to `removed` in Postgres
4. `WORKSPACE_REMOVED` event written

Structure events and agent card history are **never** deleted — only the conversational memory is cleaned.

## Related Docs

- [Memory](./memory.md) — Memory backends and persistence
- [Workspace Tiers](./workspace-tiers.md) — What each tier provides
- [Workspace Runtime](../agent-runtime/workspace-runtime.md) — What runs inside the container
- [Registry & Heartbeat](../api-protocol/registry-and-heartbeat.md) — How provisioning transitions to online
- [Team Expansion](../agent-runtime/team-expansion.md) — Provisioning triggered by team expansion
