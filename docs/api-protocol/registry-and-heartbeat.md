# Registry & Heartbeat

Every workspace registers with the platform on startup and sends a heartbeat every 30 seconds. This is how the platform knows which workspaces are alive and where to find them.

## Registration Flow

When a workspace container starts:

```
POST /registry/register
Body: { id, url, agent_card }
```

The platform:
1. Writes the Agent Card to Postgres (`workspaces` table)
2. Sets Redis key `ws:{id}` = `"online"` with TTL 60 seconds
3. Appends a `WORKSPACE_ONLINE` event to `structure_events`
4. Broadcasts event via WebSocket — canvas node turns green

## Heartbeat Flow

Every 30 seconds:

```python
# workspace/heartbeat.py

await platform.post("/registry/heartbeat", json={
    "workspace_id": WORKSPACE_ID,

    # used by platform to make status decisions
    "error_rate": error_tracker.error_rate,      # triggers degraded
    "sample_error": error_tracker.sample_error,  # shown on canvas

    # informational — shown on canvas node tooltip
    "active_tasks": task_counter.current,         # how many tasks running now
    "uptime_seconds": time.time() - START_TIME,   # how long since container start
    "current_task": current_task_description,      # what the agent is working on
})
```

Five fields. Memory usage, CPU, queue depth — those are infrastructure metrics for Prometheus/Grafana or CloudWatch. The platform registry is a service discovery layer, not a metrics store.

`active_tasks` is included because the canvas uses it for a busy indicator on the node, and it sets up backpressure for Phase 2 without a schema change.

`current_task` is a human-readable description of what the agent is currently working on. The platform stores it in `workspaces.current_task` and broadcasts a `TASK_UPDATED` WebSocket event only when the value changes (not on every heartbeat). The canvas shows it as an amber banner on the workspace node and side panel header.

The platform:
1. Overwrites heartbeat columns in Postgres (latest snapshot only — no history)
2. Refreshes Redis TTL to 60 seconds
3. Checks error rate for status transitions (see Health Monitoring below)

```go
// workspace-server/internal/registry/heartbeat.go

func HandleHeartbeat(workspaceID string, stats HeartbeatStats) {
    db.Exec(`
        UPDATE workspaces SET
            last_heartbeat_at = now(),
            last_error_rate   = $2,
            last_sample_error = $3,
            active_tasks      = $4,
            uptime_seconds    = $5,
            current_task      = $6
        WHERE id = $1
    `, workspaceID,
       stats.ErrorRate, stats.SampleError,
       stats.ActiveTasks, stats.UptimeSeconds,
       stats.CurrentTask,
    )
    redis.Refresh(workspaceID, 60*time.Second)
    evaluateStatusTransition(workspaceID, stats)
}
```

No heartbeat history table. Heartbeats arrive every 30 seconds — storing history would be 2880 rows per workspace per day with no practical use. If you need health trends, Langfuse traces capture that at the task level with far more detail.

## Health Monitoring

The workspace self-reports its health via the heartbeat payload. The platform decides status transitions based on error rate thresholds:

| Condition | Transition | Event |
|-----------|-----------|-------|
| `error_rate >= 0.5` and status is `online` | `online` -> `degraded` | `WORKSPACE_DEGRADED` |
| `error_rate < 0.1` and status is `degraded` | `degraded` -> `online` | `WORKSPACE_ONLINE` |

**What counts as an error:** Only things that indicate the workspace itself is broken — 5xx responses, timeouts, connection errors. Client errors (400, 403) are the caller's fault and are not counted.

The workspace tracks errors locally using a rolling 60-second window and includes the stats in every heartbeat. The platform doesn't sit in the A2A message path, so it can't monitor response codes directly — self-reporting via heartbeat is the mechanism.

## Liveness Detection (No Polling)

Redis keyspace notifications are enabled (`notify-keyspace-events = KEA`). When `ws:{id}` TTL expires (workspace missed 2 heartbeats), Redis fires an expiry event automatically.

```
Workspace starts
      |
      v
POST /registry/register  ->  Platform writes Agent Card to Postgres
                          ->  Platform sets Redis key: ws:{id} = "online" TTL 60s

Every 30s:
POST /registry/heartbeat ->  Platform refreshes Redis TTL

Workspace crashes / goes dark:
      |
      v
Redis TTL expires (60s)
      |
      v
Redis keyspace event fires
      |
      v
Platform marks workspace offline in Postgres
      |
      v
WebSocket broadcast -> canvas node turns gray
```

On expiry, the platform:
1. Receives Redis keyspace expiry event
2. Writes `WORKSPACE_OFFLINE` event to Postgres
3. Updates `workspaces.status = 'offline'`
4. Broadcasts via WebSocket — canvas node turns gray

## Heartbeat Delegation Checking

In addition to alive signals, the heartbeat loop checks for completed delegations every 30 seconds:

```
Every 30s:
1. POST /registry/heartbeat        → alive signal
2. GET /workspaces/:id/delegations → check results
3. If new completions found:
   a. Write to /tmp/delegation_results.jsonl (for context injection)
   b. Send A2A self-message to wake the agent (5-min cooldown)
   c. Push notification to user via POST /notify
```

**Self-message trigger:** When the heartbeat detects a completed delegation, it sends an A2A message to its own workspace: "Delegation results are ready. Review them and take appropriate action." This wakes the agent so it can report results back up the chain.

**Cooldown:** At most one self-message per 5 minutes to prevent infinite loops (agent delegates → completes → self-message → agent delegates again → ...).

**Context injection:** When the agent receives any message (including the self-message), the CLI executor reads `/tmp/delegation_results.jsonl` and injects results into the prompt as `[Delegation results received while you were idle]`.

**Delegation status lifecycle:**
```
pending → dispatched → received → in_progress → completed | failed
```

Each transition broadcasts a `DELEGATION_STATUS` WebSocket event. `DELEGATION_COMPLETE` and `DELEGATION_FAILED` are broadcast on terminal states.

## Workspace Moves to a New Machine

When a workspace starts on a new machine (e.g. new EC2 instance):

1. Workspace sends `POST /registry/register` with new URL
2. Platform updates `workspaces.url` in Postgres
3. Platform busts Redis URL cache key `ws:{id}:url`
4. All subsequent inter-workspace calls use the new URL automatically

## Workspace Forwarding

The `forwarded_to` column is set when a workspace is retired but a replacement exists. Three scenarios:

**1. Workspace replaced by a better version:** User deploys a new SEO agent with improved skills and retires the old one. Old workspace gets `forwarded_to = new_workspace_id`. Any workspace that still has the old URL cached gets a `410 Gone` + redirect pointer and updates its reference automatically.

**2. Workspace expanded into a team:** A single Developer Agent expands into a Developer Team. The original single-agent workspace is retired. `forwarded_to = developer_pm_id` (the team lead). Anything that was talking to the old Developer Agent now gets redirected to Developer PM.

**3. Workspace moved to a different parent:** A workspace is reorganized in the hierarchy. Old workspace entry kept for redirect, new one created under the new parent.

In all cases, the forwarding is transparent — callers follow the redirect and update their cached URL.

## Querying a Deleted Workspace

When a workspace is queried after being deleted or retired:

1. Platform checks `workspaces` table — status is `removed`
2. Platform queries `structure_events` for the last event on that ID
3. If `workspaces.forwarded_to` is set, returns `301` with the new workspace URL
4. If no forwarding, returns `410 Gone` with the last event payload
5. Canvas removes the node; parent workspace is notified

## Related Docs

- [Platform API](./platform-api.md) — Full endpoint reference
- [Event Log](../architecture/event-log.md) — How events are recorded
- [Database Schema](../architecture/database-schema.md) — Redis key patterns
