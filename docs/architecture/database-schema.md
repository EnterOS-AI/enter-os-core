# Database Schema

## Postgres Tables

### workspaces — Workspace Registry (Current State)

The mutable projection of `structure_events`. Represents the current state of all workspaces.

```sql
CREATE TABLE workspaces (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name          TEXT NOT NULL,
  role          TEXT,
  tier          INTEGER DEFAULT 1,
  status        TEXT DEFAULT 'provisioning',
  source_bundle_id TEXT,
  agent_card    JSONB,
  url           TEXT,
  parent_id     UUID REFERENCES workspaces(id),
  forwarded_to  UUID REFERENCES workspaces(id),
  last_heartbeat_at  TIMESTAMPTZ,
  last_error_rate    FLOAT DEFAULT 0,
  last_sample_error  TEXT,
  active_tasks       INTEGER DEFAULT 0,
  uptime_seconds     INTEGER DEFAULT 0,
  created_at    TIMESTAMPTZ DEFAULT now(),
  updated_at    TIMESTAMPTZ DEFAULT now()
);
```

| Column | Purpose |
|--------|---------|
| `id` | Unique workspace identifier |
| `name` | Display name |
| `role` | The org chart role (e.g. "Marketing", "QA") |
| `tier` | 1-4, determines deployment method |
| `status` | `provisioning`, `online`, `degraded`, `offline`, `failed`, or `removed` |
| `agent_card` | Full A2A Agent Card as JSONB |
| `url` | Current endpoint URL |
| `parent_id` | Parent workspace (defines hierarchy AND communication topology) |
| `source_bundle_id` | Original bundle ID this workspace was created from |
| `forwarded_to` | Redirect pointer when workspace is replaced, expanded, or moved (see [Registry — Workspace Forwarding](../api-protocol/registry-and-heartbeat.md#workspace-forwarding)) |
| `last_heartbeat_at` | Timestamp of last heartbeat received |
| `last_error_rate` | Latest self-reported error rate (triggers `degraded` at >= 0.5) |
| `last_sample_error` | Latest sample error message (shown on canvas tooltip) |
| `active_tasks` | Number of tasks currently running (shown as busy indicator on canvas) |
| `uptime_seconds` | Seconds since container start |
| `current_task` | Human-readable description of what the agent is currently working on (set via heartbeat) |

### agents — Agent Assignments

```sql
CREATE TABLE agents (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id    UUID REFERENCES workspaces(id),
  model           TEXT,
  status          TEXT DEFAULT 'active',
  removed_at      TIMESTAMPTZ,
  removal_reason  TEXT,
  created_at      TIMESTAMPTZ DEFAULT now()
);
```

Tracks which AI model is assigned to which workspace, and the history of assignments.

### workspace_secrets — Encrypted Credentials

```sql
CREATE TABLE workspace_secrets (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id  UUID REFERENCES workspaces(id),
  key           TEXT NOT NULL,
  encrypted_value BYTEA NOT NULL,
  created_at    TIMESTAMPTZ DEFAULT now(),
  updated_at    TIMESTAMPTZ DEFAULT now(),
  UNIQUE(workspace_id, key)
);
```

Stores API keys, credentials, and other secrets needed by workspace agents. Values are encrypted with AES-256-GCM at the application layer. The encryption key comes from the tenant's `SECRETS_ENCRYPTION_KEY` environment variable, provisioned at tenant boot by the control plane (which holds the master key in AWS KMS — see [secrets-key-custody.md](./secrets-key-custody.md)). The key is never stored in the database.

The provisioner reads secrets from this table, decrypts them, and injects them as environment variables when spinning up workspace containers. Secrets are never included in bundles (see [Constraints — Rule 5](../development/constraints-and-rules.md)).

### canvas_layouts — Node Layout

```sql
CREATE TABLE canvas_layouts (
  workspace_id  UUID REFERENCES workspaces(id) ON DELETE CASCADE,
  x             FLOAT NOT NULL DEFAULT 0,
  y             FLOAT NOT NULL DEFAULT 0,
  collapsed     BOOLEAN DEFAULT false,
  PRIMARY KEY (workspace_id)
);
```

Stores the visual position and UI state of each workspace node on the canvas. One row per workspace. Updated via `PATCH /workspaces/:id` when the user drags a node.

### canvas_viewport — Viewport State

```sql
CREATE TABLE canvas_viewport (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  x          FLOAT NOT NULL DEFAULT 0,
  y          FLOAT NOT NULL DEFAULT 0,
  zoom       FLOAT NOT NULL DEFAULT 1,
  saved_at   TIMESTAMPTZ DEFAULT now()
);
```

Single row — upserted on viewport change. Stores the canvas pan and zoom position so the user returns to the same view. Separate from `canvas_layouts` to avoid bloating the per-node table.

### structure_events — Immutable Event Log

```sql
CREATE TABLE structure_events (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  event_type    TEXT NOT NULL,
  workspace_id  UUID,
  agent_id      UUID,
  target_id     UUID,
  payload       JSONB,
  created_at    TIMESTAMPTZ DEFAULT now()
);
```

**Append-only.** Never UPDATE or DELETE rows. See [Event Log](./event-log.md).

### activity_logs — Operational Activity Log

```sql
CREATE TABLE activity_logs (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  activity_type TEXT NOT NULL,
  source_id     UUID,
  target_id     UUID,
  method        TEXT,
  summary       TEXT,
  request_body  JSONB,
  response_body JSONB,
  duration_ms   INTEGER,
  status        TEXT DEFAULT 'ok',
  error_detail  TEXT,
  created_at    TIMESTAMPTZ DEFAULT now()
);
```

| Column | Purpose |
|--------|---------|
| `activity_type` | `a2a_send`, `a2a_receive`, `task_update`, `agent_log`, or `error` |
| `source_id` | Workspace that initiated the action (nullable for canvas-originated) |
| `target_id` | Workspace that received the action (for A2A communications) |
| `method` | A2A method name (e.g. `message/send`) or task action |
| `request_body` | Full request payload as JSONB (for A2A) |
| `response_body` | Full response payload as JSONB (for A2A) |
| `duration_ms` | Operation duration in milliseconds |
| `status` | `ok`, `error`, or `timeout` |
| `error_detail` | Error message when status is not `ok` |

Separate from `structure_events` by design: structure_events tracks lifecycle/structural changes (append-only, never deleted), while activity_logs tracks operational activity (A2A communications, task updates, agent logs) and has a 7-day retention policy with automatic cleanup.

Indexes: composite `(workspace_id, activity_type, created_at DESC)` for filtered queries, standalone `created_at` for retention cleanup.

## Redis Keys

| Key Pattern | Value | TTL | Purpose |
|-------------|-------|-----|---------|
| `ws:{id}` | `"online"` | 60s | Liveness detection |
| `ws:{id}:url` | `"https://..."` | 5min | URL cache for fast resolution |
| `events:broadcast` | pub/sub channel | -- | Push events to canvas/workspace WebSocket |

### Redis Configuration

Keyspace notifications must be enabled for liveness detection:

```
notify-keyspace-events = KEA
```

This allows the platform to subscribe to key expiry events without polling.

## Design Decisions

- **Postgres is source of truth. Redis is ephemeral.** If Redis is wiped, workspaces re-register on next heartbeat and state is restored. Nothing critical lives only in Redis.
- **`org_id` is omitted from MVP schema.** Added later in the SaaS migration for multi-tenancy.
- **`wal_level=logical`** is set from the start to enable future streaming of change events without a schema migration.

## Related Docs

- [Event Log](./event-log.md) — Event sourcing pattern
- [Registry & Heartbeat](../api-protocol/registry-and-heartbeat.md) — How Redis keys are managed
- [Platform API](../api-protocol/platform-api.md) — API that reads/writes these tables
- [Provisioner](./provisioner.md) — Workspace lifecycle states
- [Workspace Tiers](./workspace-tiers.md) — What the `tier` column means
- [Communication Rules](../api-protocol/communication-rules.md) — How `parent_id` drives access control
