# System Architecture

## Overview

The platform is fully distributed — workspaces can run on different machines and communicate via the A2A (Agent-to-Agent) protocol.

## System Boundaries

The platform consists of four distinct systems:

```
+-----------------------------------------------------------+
|  canvas/              Next.js 15 frontend                  |
|  React Flow visual canvas, Zustand state, WebSocket        |
+-----------------------------+-----------------------------+
                              | HTTP + WebSocket
+-----------------------------v-----------------------------+
|  workspace-server/            Go (gin) backend                     |
|  Registry, hierarchy, event log, provisioner, bundles       |
+------+---------------------------------------+------------+
       | Postgres                               | Redis
+------v------+                       +---------v---------+
|  Postgres   |                       |     Redis         |
|  (Docker)   |                       |    (Docker)       |
+-------------+                       +-------------------+

       A2A HTTP (JSON-RPC 2.0) — direct workspace-to-workspace
+-----------------------------------------------------------+
|  workspace/  pluggable workspace runtime          |
|  LangGraph / DeepAgents / Claude Code / CrewAI / AutoGen   |
|  / OpenClaw + a2a-sdk                                      |
+-----------------------------------------------------------+
       |
+------v----------------------------------------------------+
|  Langfuse             Observability (Docker)               |
|  Traces every LLM call across all workspaces               |
+-----------------------------------------------------------+
```

### Plugins

Shared skill/rule collections mounted into every workspace container at `/plugins/`:

```
plugins/
├── ecc/                    # Everything Claude Code
│   ├── rules/              # Always-on guidelines
│   ├── skills/             # Shared skills (coding-standards, tdd-workflow, etc.)
│   └── AGENTS.md           # Prompt fragment
└── superpowers/            # obra/superpowers
    └── skills/             # Shared skills (debugging, verification, etc.)
```

Plugins inject rules and skills into all agents automatically. Workspace-specific skills take priority over plugin skills (deduplicated by ID).

### Data Flow Summary

- **Canvas <-> Platform:** HTTP REST + WebSocket for real-time events
- **Platform <-> Postgres:** Source of truth for registry, hierarchy, events
- **Platform <-> Redis:** Ephemeral state — liveness, caching, pub/sub
- **Platform -> Workspace:** Provisioning (tiered Docker deployment), discovery
- **Workspace <-> Workspace:** Direct A2A (JSON-RPC 2.0) — platform not in path
- **Workspace -> Langfuse:** Automatic LLM tracing

## Folder Structure

```
molecule/
|
+-- docker-compose.yml               # full local dev stack
+-- docker-compose.infra.yml         # postgres, redis, langfuse only
+-- .env.example
+-- README.md
|
+-- canvas/                          # Next.js 15 frontend
+-- workspace-server/                        # Go backend
+-- workspace/              # Python agent runtime (generic image)
+-- workspace-configs-templates/     # workspace personality definitions
+-- infra/                           # scripts + langfuse compose
+-- docs/                            # documentation
```

## Related Docs

- [Platform API](../api-protocol/platform-api.md) — Go backend details
- [Workspace Runtime](../agent-runtime/workspace-runtime.md) — Runtime layer details
- [Canvas UI](../frontend/canvas.md) — Next.js frontend details
- [Technology Choices](./technology-choices.md) — Why each piece was chosen
