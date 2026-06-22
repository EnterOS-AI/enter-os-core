# Observability (Langfuse)

## Overview

Self-hosted Langfuse runs in Docker alongside the rest of the stack. Every workspace agent sends traces automatically via LangGraph's built-in Langfuse integration.

## What Gets Traced Automatically

LangGraph detects Langfuse env vars at import time and patches the tracer automatically. No callback handlers, no explicit instrumentation — just env vars:

```
LANGFUSE_HOST=http://langfuse-web:3000
LANGFUSE_PUBLIC_KEY=pk-...
LANGFUSE_SECRET_KEY=sk-...
LANGSMITH_TRACING=true    # LangGraph reads this to enable tracing
```

Automatic traces include:

| Traced | What's captured |
|--------|----------------|
| LLM calls | Input prompt, output, tokens, latency, cost |
| Tool calls | Function name, arguments, return value |
| Agent steps | Planning steps, TODO list updates |
| Sub-agent spawns | Nested trace under parent task |
| Errors | Full stack traces |

## What Requires Manual Instrumentation

A2A delegations are HTTP calls — LangGraph doesn't know about them. The delegation tool creates a manual span:

```python
# workspace/builtin_tools/delegation.py

from langfuse import Langfuse
langfuse = Langfuse()

@tool
async def delegate_to_workspace(workspace_id: str, task: str) -> dict:
    span = langfuse.span(
        name="a2a_delegation",
        metadata={"from": WORKSPACE_ID, "to": workspace_id},
    )
    result = await send_a2a_task(workspace_id, task)
    span.end(output=result)
    return result
```

The `parent_task_id` in the A2A message metadata links the child workspace's trace back to the parent — Langfuse renders the full call tree across workspaces.

All workspaces report to a single unified Langfuse instance, giving you a cross-workspace view of all agent activity.

## Activity Logs (Platform-Level)

In addition to Langfuse traces (LLM-level), the platform maintains its own `activity_logs` table for operational observability:

| Activity Type | What's Captured |
|---------------|----------------|
| `a2a_receive` | Every A2A proxy request/response — method, duration, request/response bodies, status |
| `a2a_send` | Agent-reported outbound A2A calls |
| `task_update` | Agent task lifecycle events (start, complete, fail) |
| `agent_log` | Generic agent log entries with optional metadata |
| `error` | Agent errors with detail messages |

Activity logs are accessible via:
- **Canvas UI**: ActivityTab in the side panel with type filters and auto-refresh
- **API**: `GET /workspaces/:id/activity?type=&limit=`
- **MCP**: `list_activity` tool

Activity logs have a configurable retention policy (default 7 days, cleanup every 6 hours). Configure via env vars: `ACTIVITY_RETENTION_DAYS` (default `7`), `ACTIVITY_CLEANUP_INTERVAL_HOURS` (default `6`). This is separate from `structure_events` (which is append-only and never deleted).

The current task description (`current_task` field in heartbeat) is displayed as an amber banner on workspace nodes and the side panel header, giving immediate visibility into what each agent is doing.

## Prometheus Metrics

The platform exposes a `GET /metrics` endpoint in Prometheus text exposition format (v0.0.4). No external dependencies — implemented in `workspace-server/internal/metrics/metrics.go`.

| Metric | Type | Description |
|--------|------|-------------|
| `molecule_http_requests_total{method,path,status}` | counter | Total HTTP requests by route |
| `molecule_http_request_duration_seconds_total{method,path}` | counter | Cumulative request latency |
| `molecule_websocket_connections_active` | gauge | Current WebSocket connections |
| `go_goroutines` | gauge | Go runtime goroutine count |
| `go_memstats_alloc_bytes` | gauge | Heap allocated bytes |
| `go_memstats_sys_bytes` | gauge | Total OS memory |
| `go_gc_duration_seconds_total` | counter | Cumulative GC pause time |

Scrape with: `curl http://localhost:8080/metrics`

Uses matched route patterns (e.g. `/workspaces/:id`) to avoid high-cardinality label explosion from workspace UUIDs.

## Why This Matters

Debugging distributed agents is hard. Without centralized observability, you cannot see what happened across multiple machines. Langfuse provides the unified trace view at the LLM call level. Activity logs provide the operational view at the inter-agent communication level. Together they give complete visibility into what happened, why, and how long it took.

## Local Stack

Langfuse runs as part of the Docker Compose stack:

- **Langfuse web** on `:3001` (host-mapped; internal container port is `:3000`)
- **Langfuse worker** (background processing)
- **ClickHouse** (Langfuse dependency for analytics)

## Why Not LangSmith?

LangSmith (the managed version from LangChain) requires an enterprise license for self-hosting. Langfuse is fully open source and self-hostable.

## Related Docs

- [Architecture](../architecture/architecture.md) — Where Langfuse fits in the system
- [Local Development](./local-development.md) — Running the full stack
- [Workspace Runtime](../agent-runtime/workspace-runtime.md) — Where `LANGFUSE_HOST` is configured
- [Technology Choices](../architecture/technology-choices.md) — Why Langfuse was chosen over LangSmith
