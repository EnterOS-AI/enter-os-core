# A2A Protocol (Inter-Workspace Communication)

Workspaces talk to each other **directly** via A2A (Agent-to-Agent protocol) — the platform is not in the message path.

## How It Works

Every workspace is an A2A server. The platform is an A2A client when it needs to communicate with workspaces. Workspaces communicate with each other directly — the platform only handles discovery.

```
Business Core (A2A client)  ->  Developer PM (A2A server)
                                  (opaque to Business Core
                                   what's inside)
```

## Discovery Flow

How Business Core finds Developer PM's URL:

1. Business Core asks platform: `GET /registry/discover/developer-pm-id` with `X-Workspace-ID` header
2. Platform checks `CanCommunicate()` for the caller/target pair
3. Platform resolves the URL:
   - **Workspace caller** (has `X-Workspace-ID`): returns Docker-internal URL from `ws:{id}:internal_url` Redis key — containers can reach each other by hostname on the Docker network
   - **Canvas/external** (no header): returns host-mapped URL from `ws:{id}:url` Redis key — the ephemeral `127.0.0.1:PORT` bound by the provisioner
4. If cache miss, platform reads from Postgres, refreshes cache
5. Business Core sends A2A JSON-RPC message **directly** to Developer PM
6. Developer PM processes the task and responds

The platform is **only** involved in URL resolution. The actual task messages go workspace-to-workspace.

## Message Format

A2A uses JSON-RPC 2.0 over HTTP:

```json
{
  "jsonrpc": "2.0",
  "id": "task-123",
  "method": "message/send",
  "params": {
    "message": {
      "role": "user",
      "parts": [{ "kind": "text", "text": "Build the login feature" }],
      "messageId": "msg-456"
    }
  }
}
```

The receiving workspace:
1. Processes this as a task
2. Streams progress updates via SSE
3. Returns artifacts (files, structured data, text) when done

## On-Demand Discovery (Not Pushed)

Topology is **not** pushed to workspaces at startup. A workspace only queries the platform for another workspace's URL at the moment it decides to delegate to it.

**Why not push at startup:** The topology changes while the workspace is running — sub-workspaces get added, removed, come online and go offline. If you push at startup you'd need to also push every topology change to every affected workspace and keep them in sync. That's complex and fragile.

On-demand fits naturally with how agents work — an agent only needs to know about another workspace at the moment it decides to delegate, not before.

**Note:** While URL resolution is on-demand, the workspace does fetch peer Agent Cards on startup to build its system prompt (see [System Prompt Structure](../agent-runtime/system-prompt-structure.md)). The system prompt is rebuilt reactively when `AGENT_CARD_UPDATED` events arrive — but the actual A2A URL for sending messages is resolved on-demand at delegation time.

## Authentication Between Workspaces

**MVP: discovery-time validation only.** The platform validates `CanCommunicate()` when workspace A calls `GET /registry/discover/:id` (using `X-Workspace-ID` header). Once A has B's URL, direct A2A calls are unauthenticated.

This is acceptable for MVP because:
- All workspaces are provisioned by the same platform on trusted infrastructure
- Docker network isolation (`molecule-core-net`) limits who can reach workspace endpoints
- The tool is self-hosted — the operator controls the network

**Known gap:** Once workspace A caches workspace B's URL, nothing stops A from calling B directly even after the hierarchy changes and A is no longer supposed to reach B. The cached URL remains valid until the container is restarted or the URL changes.

**Post-MVP fix — platform-issued tokens:** On discovery, the platform issues a short-lived signed token scoped to the specific caller/target pair. The target workspace validates the token on every A2A request. When the hierarchy changes, old tokens expire and new discovery attempts are blocked by `CanCommunicate()`.

## Task Lifecycle

Every A2A message creates a task with a defined lifecycle:

```
submitted → working → completed
                    → failed
                    → canceled
           → input-required → working (caller provides input)
```

### Full Flow

```
Caller sends message/send or message/sendSubscribe
      │
      ▼
Task created: status = submitted
      │
      ▼
Workspace starts processing: status = working
      │
      ├── needs clarification?
      │         │
      │         ▼
      │   status = input-required
      │   SSE event fires to caller
      │   caller sends follow-up message
      │         │
      │         ▼
      │   status = working (resumes)
      │
      ├── success
      │         │
      │         ▼
      │   status = completed
      │   SSE terminal event fires
      │   artifacts returned
      │
      └── error
                │
                ▼
          status = failed
          SSE terminal event fires
          error details returned
```

### Calling Patterns

Two patterns — synchronous for short tasks, streaming for long ones:

```python
# pattern 1 — synchronous (short tasks)
# caller blocks until terminal state
result = await a2a.send({
    "method": "message/send",
    "params": { "message": { ... } }
})
# returns when completed/failed — no streaming

# pattern 2 — streaming (long tasks)
# caller subscribes to SSE stream
async for event in a2a.subscribe({
    "method": "message/sendSubscribe",
    "params": { "message": { ... } }
}):
    if event["status"] == "working":
        # intermediate progress update
        print(event["message"])

    if event["status"] in ("completed", "failed", "canceled"):
        # terminal event — stream ends here
        result = event["artifacts"]
        break
```

No polling needed. The SSE stream includes a terminal event — the caller knows the task is done when it receives `completed`, `failed`, or `canceled`.

### Task ID

Every task gets an ID on creation, returned in the first SSE event or synchronous response:

```python
task_id = response["id"]

# caller can check status explicitly if needed
status = await a2a.get(f"/tasks/{task_id}")
```

### Cancellation

```python
# cancel an in-flight task
await a2a.send({
    "method": "tasks/cancel",
    "params": { "id": task_id }
})
# workspace receives cancel signal
# status → canceled
# SSE terminal event fires to all subscribers
```

The workspace handles cancellation via the `LangGraphA2AExecutor.cancel()` method, which uses LangGraph's interrupt mechanism:

```python
# workspace/a2a_executor.py
async def cancel(self, context: RequestContext, queue: EventQueue):
    await self.agent.ainterrupt(context.context_id)
    # status → canceled, SSE terminal event fires automatically
```

See [Workspace Runtime — A2A Server Wrapping](../agent-runtime/workspace-runtime.md#a2a-server-wrapping) for the full executor implementation.

### Artifacts

On completion, the task returns artifacts:

```json
{
  "status": "completed",
  "artifacts": [
    {
      "type": "text/plain",
      "content": "Page generated successfully"
    },
    {
      "type": "application/json",
      "content": { "page_path": "/kitchen-renovation-vancouver" }
    }
  ]
}
```

## Platform A2A Proxy

The canvas (browser) cannot reach Docker-internal agent URLs directly. The platform provides `POST /workspaces/:id/a2a` as a proxy:

1. Canvas sends JSON-RPC to the platform proxy
2. Proxy resolves the agent's host-accessible URL from Redis cache (falls back to DB)
3. If the request lacks a `jsonrpc` field, the proxy wraps it in a JSON-RPC 2.0 envelope with a generated UUID
4. If `params.message.messageId` is missing, the proxy injects one (required by a2a-sdk)
5. Proxy forwards the request to the agent (120s timeout, 10MB response limit)
6. Agent response is returned to the caller

This proxy is the **only** way the canvas communicates with agents. Workspace-to-workspace communication is direct (no proxy).

## Key Properties

- **Transport:** JSON-RPC 2.0 over HTTP — any language can implement it
- **Discovery:** Agent Cards at `/.well-known/agent-card.json`
- **On-demand:** Workspaces discover peers when needed, not at startup
- **Opaque execution:** The caller doesn't know (or care) what's inside the callee
- **Interoperable:** Any A2A-compliant agent from any framework can plug in
- **Direct:** Workspace-to-workspace messages go direct; canvas uses platform proxy
- **MVP auth:** Discovery-time only; post-MVP adds signed tokens

## Related Docs

- [Agent Card](../agent-runtime/agent-card.md) — The identity document used for discovery
- [Communication Rules](./communication-rules.md) — Who can communicate with whom
- [System Prompt Structure](../agent-runtime/system-prompt-structure.md) — How peer Agent Cards are used in prompts
- [Registry & Heartbeat](./registry-and-heartbeat.md) — How workspaces register URLs
- [Platform API](./platform-api.md) — The discovery endpoint
