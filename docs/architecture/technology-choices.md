# Technology Choices

This document explains why each technology was chosen for EnterOS.

## Next.js 15 (Canvas Frontend)

App router, React Server Components, and easy API proxying to the Go backend. React Flow is the industry standard for node-based canvas UIs and is what Sim.ai, Flowise, and most serious canvas tools use. Zustand is the state manager — lighter than Redux, works cleanly with React Flow's controlled flow pattern.

## Go + Gin (Platform Backend)

The platform API handles high-concurrency concerns: hundreds of workspace heartbeats every 30 seconds, WebSocket connections from multiple canvas clients, Redis pub/sub event broadcasting. Go's goroutines handle this with minimal resource usage. The `gin` framework is minimal and well-documented.

Go is **not** used for agent logic — just infrastructure coordination.

## Workspace Runtime Adapters

The workspace runtime is adapter-based. LangGraph and DeepAgents are Python-native, while Claude Code and OpenClaw are CLI-driven runtimes. The platform standardizes them behind the same A2A surface, heartbeat lifecycle, memory tools, and workspace contract instead of forcing one execution backend for every team.

## Deep Agents + LangGraph

Deep Agents is an "agent harness" built on LangGraph, inspired by Claude Code's architecture. It provides four things out of the box:

1. A planning tool (TODO list)
2. Sub-agent spawning
3. Filesystem-backed memory
4. A detailed system prompt structure

LangGraph is the underlying runtime — it manages the agent loop, state persistence, retries, streaming, and human-in-the-loop.

Neither Deep Agents nor LangGraph provide AI models — they call whatever provider you configure.

## a2a-sdk (A2A Wrapper)

`a2a-sdk` is the server package that exposes a workspace over HTTP with a standard A2A interface. It handles JSON-RPC 2.0 request parsing, Agent Card serving at `/.well-known/agent-card.json`, SSE streaming, and task management. LangGraph-based runtimes and CLI-based adapters both terminate into the same A2A contract.

## A2A Protocol

The Agent-to-Agent protocol (Google, Linux Foundation) is the standard for agent-to-agent communication. Key properties:

- Every workspace is an A2A server and can be an A2A client
- Workspaces communicate **directly** without routing through the platform
- Uses JSON-RPC 2.0 over HTTP — any language can implement it
- Any A2A-compliant agent from any framework can plug in

The platform only handles discovery (resolving workspace URLs) and registry (knowing what workspaces exist).

## Docker

Workspace instances are Docker containers. The generic `workspace-template` image is built once. Each instance gets different environment variables injecting the workspace ID, config path, model provider, and tier.

Docker gives process isolation, easy cleanup, portability, and a clear way to express tiered privilege. Tier 4 uses a full-host Docker configuration (privileged + host PID + host network + Docker socket) rather than a separate VM provisioner.

## Postgres

Source of truth for workspace registry, workspace hierarchy, agent assignments, and the immutable structure event log. The event log is append-only — rows are never updated or deleted. The `workspaces` table is a projection of that log (current state).

Using `wal_level=logical` from the start enables future streaming of change events without a schema migration.

## Redis

Handles everything ephemeral:

- **Liveness detection** via TTL keys — if a workspace stops sending heartbeats, its key expires automatically
- **URL caching** for fast workspace resolution
- **Pub/sub** for broadcasting structure events to canvas WebSocket clients

Redis does not need to be durable — if it restarts, workspaces re-register on next heartbeat.

## Langfuse (Observability)

Self-hosted, fully open source, runs in Docker. LangGraph has native Langfuse integration — setting `LANGFUSE_HOST` is all that's needed. Captures every LLM call across all workspaces in one unified view.

LangSmith (the managed version) requires an enterprise license for self-hosting and was ruled out for this reason.

## Version Requirements

```
Go              1.25+ (go.mod)
Python          3.11+
Node.js         22+
Next.js         15
React Flow      12   (@xyflow/react)
a2a-sdk         0.3+ (A2A server SDK)
langfuse        3.x  (self-hosted Docker)
Postgres        16
Redis           7
Docker Compose  2.x
```
