# WebSocket Events

The canvas subscribes to the platform's WebSocket at `/ws` and receives real-time structure events as JSON messages.

## Message Format

Every WebSocket message has this structure:

```json
{
  "event": "EVENT_TYPE",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": { ... }
}
```

## Event Reference

### WORKSPACE_PROVISIONING

Workspace is being spun up. Canvas shows a spinner on the node.

```json
{
  "event": "WORKSPACE_PROVISIONING",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "name": "Vancouver SEO Agent",
    "tier": 1,
    "config": "seo-agent"
  }
}
```

### WORKSPACE_ONLINE

First heartbeat received, or workspace returned from offline.

```json
{
  "event": "WORKSPACE_ONLINE",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "url": "http://ws-abc-123:8000",
    "agent_card": {
      "name": "Vancouver SEO Agent",
      "version": "1.0.0",
      "skills": ["generate-seo-page", "audit-seo-page"],
      "capabilities": { "streaming": true }
    }
  }
}
```

### WORKSPACE_OFFLINE

Heartbeat TTL expired. Node turns gray.

```json
{
  "event": "WORKSPACE_OFFLINE",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:01:00Z",
  "payload": {}
}
```

### WORKSPACE_PROVISION_FAILED

Provisioning timed out or errored. Node turns red with retry button.

```json
{
  "event": "WORKSPACE_PROVISION_FAILED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:03:00Z",
  "payload": {
    "reason": "provisioning timeout -- no heartbeat received"
  }
}
```

### WORKSPACE_DEGRADED

Workspace is online but experiencing errors. Node shows warning indicator.

```json
{
  "event": "WORKSPACE_DEGRADED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:05:00Z",
  "payload": {
    "error_rate": 0.87,
    "sample_error": "anthropic API rate limit exceeded"
  }
}
```

### WORKSPACE_REMOVED

User deleted the workspace. Node removed from canvas.

```json
{
  "event": "WORKSPACE_REMOVED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:10:00Z",
  "payload": {
    "forwarded_to": null
  }
}
```

### AGENT_REPLACED

AI model swapped inside a workspace.

```json
{
  "event": "AGENT_REPLACED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "old_model": "anthropic:claude-sonnet-4-6",
    "new_model": "openai:gpt-4o"
  }
}
```

### AGENT_CARD_UPDATED

Workspace republished its Agent Card (new skill added, description changed, capabilities changed). The platform broadcasts this to all peer workspaces (siblings, children, parent) so they can rebuild their system prompts.

```json
{
  "event": "AGENT_CARD_UPDATED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "agent_card": {
      "name": "Vancouver SEO Agent",
      "version": "1.1.0",
      "skills": ["generate-seo-page", "audit-seo-page", "monitor-rankings"],
      "capabilities": { "streaming": true }
    }
  }
}
```

### WORKSPACE_EXPANDED

Workspace expanded into a team of sub-workspaces.

```json
{
  "event": "WORKSPACE_EXPANDED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "sub_workspace_ids": [
      "ws-frontend-001",
      "ws-backend-001",
      "ws-qa-001"
    ]
  }
}
```

### WORKSPACE_COLLAPSED

Team collapsed back to a single agent. Sub-workspaces are stopped and removed.

```json
{
  "event": "WORKSPACE_COLLAPSED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "removed_sub_workspace_ids": [
      "ws-frontend-001",
      "ws-backend-001",
      "ws-qa-001"
    ]
  }
}
```

### WORKSPACE_MOVED

Workspace moved to a new parent (dragged into a different team on canvas).

```json
{
  "event": "WORKSPACE_MOVED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "old_parent_id": "ws-team-a",
    "new_parent_id": "ws-team-b"
  }
}
```

### AGENT_ASSIGNED

A new AI agent assigned to a workspace (first time or after removal).

```json
{
  "event": "AGENT_ASSIGNED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "agent_id": "agent-xyz-789",
    "model": "anthropic:claude-sonnet-4-6"
  }
}
```

### AGENT_REMOVED

Agent removed from a workspace (workspace becomes empty).

```json
{
  "event": "AGENT_REMOVED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "agent_id": "agent-xyz-789",
    "reason": "user removed"
  }
}
```

### AGENT_MOVED

Agent moved from one workspace to another.

```json
{
  "event": "AGENT_MOVED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "agent_id": "agent-xyz-789",
    "from_workspace_id": "ws-abc-123",
    "to_workspace_id": "ws-def-456"
  }
}
```

### TASK_UPDATED

Agent's current task changed (via heartbeat). WebSocket-only — not persisted to `structure_events`.

```json
{
  "event": "TASK_UPDATED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "current_task": "Analyzing quarterly report",
    "active_tasks": 2
  }
}
```

Canvas shows the current task as an amber banner on the workspace node and side panel header. Only broadcast when the task actually changes (not on every heartbeat).

### ACTIVITY_LOGGED

New activity log entry created (A2A communication, webhook-triggered task ingress, agent log, error). WebSocket-only — not persisted to `structure_events` (stored in `activity_logs` table instead).

```json
{
  "event": "ACTIVITY_LOGGED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "activity_type": "a2a_receive",
    "method": "message/send",
    "summary": "message/send → ws-abc-123",
    "status": "ok",
    "source_id": "ws-def-456",
    "target_id": "ws-abc-123",
    "duration_ms": 1500
  }
}
```

Canvas ActivityTab uses this event as a refresh hint. The event is informational — the full activity details (request/response bodies) are fetched via `GET /workspaces/:id/activity`.

## Subscribers

Both canvas clients and workspace agents subscribe to the same WebSocket endpoint (`/ws`):

| Subscriber | Identifies via | Receives | Purpose |
|------------|---------------|----------|---------|
| Canvas client | No header (unrestricted) | All events | UI updates |
| Workspace agent | `X-Workspace-ID` header | Filtered — only events about reachable peers | System prompt rebuilds |

The platform filters server-side using `CanCommunicate()` — each workspace only receives events about workspaces it can talk to.

## Event Flow

```
Structure change occurs
      |
      v
Platform writes event to structure_events (Postgres)
      |
      v
Platform publishes to Redis pub/sub (events:broadcast)
      |
      v
WebSocket handler receives from Redis
      |
      v
WebSocket pushes JSON to subscribers (filtered per workspace)
      |
      +-> Canvas clients: update Zustand state -> React Flow re-renders
      +-> Workspace agents: rebuild system prompt if peer changed
```

## Related Docs

- [Canvas UI](../frontend/canvas.md) — How events drive the UI
- [Event Log](../architecture/event-log.md) — Persistent event storage
- [Registry & Heartbeat](./registry-and-heartbeat.md) — Events from registration
- [Provisioner](../architecture/provisioner.md) — Events from provisioning
- [Communication Rules](./communication-rules.md) — Hierarchy-based peer broadcasting
