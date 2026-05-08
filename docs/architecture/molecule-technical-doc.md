# Molecule AI — Comprehensive Technical Documentation

> Definitive technical reference for the Molecule AI Agent Team platform.
> Based on a full non-invasive scan of the [molecule-monorepo](https://git.moleculesai.app/molecule-ai/molecule-monorepo) repository.

---

## Table of Contents

1. [Executive Summary](#1-executive-summary)
2. [Product Positioning](#2-product-positioning)
3. [System Architecture](#3-system-architecture)
4. [Database Schema](#4-database-schema)
5. [Workspace Lifecycle](#5-workspace-lifecycle)
6. [Communication Rules](#6-communication-rules)
7. [Platform API Routes](#7-platform-api-routes)
8. [A2A Protocol](#8-a2a-protocol)
9. [Hierarchical Memory Architecture](#9-hierarchical-memory-architecture)
10. [Runtime Tier System](#10-runtime-tier-system)
11. [Provisioning & Container Lifecycle](#11-provisioning--container-lifecycle)
12. [Workspace Runtime](#12-workspace-runtime)
13. [Skills System](#13-skills-system)
14. [Bundle System](#14-bundle-system)
15. [Canvas UI](#15-canvas-ui)
16. [Tools & Capabilities](#16-tools--capabilities)
17. [Coordinator Pattern](#17-coordinator-pattern)
18. [Codebase Structure](#18-codebase-structure)
19. [Key Design Patterns](#19-key-design-patterns)
20. [Docker Compose Orchestration](#20-docker-compose-orchestration)
21. [Environment Variables](#21-environment-variables)
22. [Recent Feature Highlights](#22-recent-feature-highlights)
23. [Known Gaps & Backlog](#23-known-gaps--backlog)
24. [Licensing & Commercialization Path](#24-licensing--commercialization-path)
25. [OSS Growth Research](#25-oss-growth-research)
26. [Technical Debt & Constraints](#26-technical-debt--constraints)
27. [Production Deployment](#27-production-deployment)
28. [MCP Server & Integrations](#28-mcp-server--integrations)
29. [Summary Statistics](#29-summary-statistics)
30. [Vision: From Agent Teams to Robot Teams](#30-vision-from-agent-teams-to-robot-teams)

---

## 1. Executive Summary

Molecule AI is an **org-native control plane for heterogeneous AI agent teams**. It is not a workflow builder or a replacement for agent frameworks; rather, it is the operational and organizational layer that sits above multiple runtime frameworks and provides:

- **Workspace-as-role abstraction** (not task nodes)
- **Hierarchy-driven topology** (org chart = communication paths)
- **Hierarchical Memory Architecture (HMA)** with LOCAL/TEAM/GLOBAL scopes
- **A2A (Agent-to-Agent)** direct inter-workspace communication via JSON-RPC 2.0
- **Canvas-based visual team building** with drag-to-nest hierarchy
- **Comprehensive control plane operations** — registry, heartbeats, lifecycle, approvals, secrets, traces, bundles

Six runtime adapters ship production-ready on `main`: LangGraph, DeepAgents, Claude Code, CrewAI, AutoGen, OpenClaw.

---

## 2. Product Positioning

### Core Narrative

**One-liner**: *"Molecule AI is the org-native control plane for heterogeneous AI agent teams."*

### Five Key Differentiators

| # | Principle | Implication |
|---|-----------|-------------|
| 1 | **Workspace = role, not task** | Internal AI model can swap, but organizational identity persists across model/framework changes |
| 2 | **Org chart = topology** | Hierarchy determines communication boundaries — no manual edge wiring needed |
| 3 | **Heterogeneous runtime support** | 6 adapters shipped; teams choose freely without forced standardization |
| 4 | **Memory follows org boundaries** | HMA prevents over-sharing, aligns data isolation with organizational structure |
| 5 | **Skill evolution loop** | memory → signal → skill → hot-reload → operational improvement (self-improving flywheel) |

### What Molecule AI Is NOT

| Category | Examples | How Molecule AI Differs |
|----------|---------|---------------------|
| Workflow builders | n8n, Windmill, Temporal | Molecule AI models **roles**, not tasks |
| Agent frameworks | LangGraph, CrewAI, AutoGen | Molecule AI sits **above** frameworks as runtime adapters |
| Coding agents | Claude Code, Cursor, Codex | Molecule AI runs coding agents as **workspace roles** alongside other types |
| Chat UIs | ChatGPT, Claude.ai | Molecule AI is **operational infrastructure**, not a conversation interface |

---

## 3. System Architecture

### System Boundary Diagram

```
┌─────────────────────────────────────────────────────────────┐
│ Canvas (Next.js 15 · port 3000)                             │
│ React Flow + Zustand + WebSocket                            │
│ Visual drag-to-nest org chart · 10-tab ops panel            │
└──────────────────┬──────────────────────────────────────────┘
                   │ HTTP + WebSocket
┌──────────────────▼──────────────────────────────────────────┐
│ Platform (Go / Gin · port 8080)                             │
│ Control plane: workspace CRUD, registry, discovery,         │
│ A2A proxy, activity, memory APIs, secrets, approvals        │
└─────────┬────────────────────────────────────┬──────────────┘
          │                                    │
    Postgres 16                             Redis 7
    (internal: 5432)                        (internal: 6379)

┌─────────────────────────────────────────────────────────────┐
│ Workspace Runtime (Python 3.11+ Docker image)               │
│ Pluggable adapters: LangGraph, DeepAgents, Claude Code,     │
│ CrewAI, AutoGen, OpenClaw                                   │
│ A2A protocol server · heartbeat · skills · HMA memory       │
└─────────────────────────────────────────────────────────────┘
          │
┌─────────────────────────────────────────────────────────────┐
│ Langfuse (self-hosted · ClickHouse + Postgres backend)      │
│ OpenTelemetry traces for every LLM call                     │
└─────────────────────────────────────────────────────────────┘
```

### Network Model

| Path | Protocol | Purpose |
|------|----------|---------|
| Canvas ↔ Platform | HTTP REST + WebSocket | UI operations + real-time event fanout |
| Platform ↔ Postgres | TCP | Source of truth for all durable state |
| Platform ↔ Redis | TCP | Ephemeral state (liveness TTL), caching, pub/sub |
| Workspace ↔ Workspace | HTTP (A2A JSON-RPC 2.0) | Direct peer-to-peer, **platform not in data path** |
| Workspace → Langfuse | HTTP | Automatic OpenTelemetry tracing |
| Docker Network | `molecule-monorepo-net` | Internal-only by default, no exposed DB/Redis ports |

### Core Components

**1. Canvas (Next.js 15)**
- React Flow for visual workspace graph
- Zustand for state management
- WebSocket for real-time updates
- 10-tab side panel: Chat, Activity, Details, Skills, Terminal, Config, Files, Memory, Traces, Events
- Drag-to-nest team building
- Bundle import/export via drag-and-drop
- Empty state with template palette + onboarding wizard

**2. Platform (Go 1.25+ / Gin)**
- Gin-based REST API + WebSocket hub
- Workspace lifecycle management (CRUD + pause/resume/restart)
- Registry and heartbeat system (30s default)
- Hierarchy-aware access control (`CanCommunicate()`)
- A2A proxy for browser-safe inter-workspace communication
- Event broadcasting (Redis pub/sub → WebSocket fanout)
- Docker provisioner with T1–T4 tier enforcement
- Activity logging with configurable retention (default 7 days)
- Secrets management (AES-256-GCM encryption)
- File, terminal, bundle, template, traces APIs
- Langfuse integration
- Prometheus metrics endpoint

**3. Workspace Runtime (Python 3.11+)**
- Unified `workspace/` Docker image
- Adapter-driven execution (6 adapters)
- A2A server via Uvicorn
- Heartbeat loop (30s default)
- Skill hot-reload system (~3 second propagation)
- Memory tools with HMA scope support
- Approval/human-in-the-loop integration
- Activity reporting
- Awareness namespace integration (optional)
- Plugin-mounted shared rules and skills

**4. Infrastructure**
- **Postgres 16**: Source of truth (workspaces, events, activity, secrets, memories)
- **Redis 7**: Ephemeral state (liveness TTL 60s), URL caching, pub/sub
- **Langfuse 2.x**: LLM tracing and observability (self-hosted, ClickHouse backend)
- **Docker**: Workspace provisioning with T1–T4 tier system
- **LiteLLM proxy** (optional): Unified API for multiple model providers
- **Ollama** (optional): Local LLM models

---

## 4. Database Schema

11 migration files in `workspace-server/migrations/`.

### Core Tables

| Table | Purpose | Key Columns |
|-------|---------|-------------|
| `workspaces` | Current state registry | `id`, `name`, `role`, `tier` (1-4), `status`, `parent_id`, `agent_card` (JSONB), `url`, `forwarded_to`, `last_heartbeat_at`, `last_error_rate`, `active_tasks`, `uptime_seconds`, `current_task`, `runtime` |
| `agents` | Agent assignment history | `workspace_id`, `model`, `status`, `removed_at`, `removal_reason` |
| `workspace_secrets` | Encrypted credentials | `workspace_id`, `key`, `encrypted_value` (BYTEA, AES-256-GCM) |
| `agent_memories` | HMA-scoped memory | `workspace_id`, `content`, `scope` (LOCAL/TEAM/GLOBAL) |
| `structure_events` | **Immutable** event log (APPEND-ONLY, never UPDATE/DELETE) | `event_type`, `workspace_id`, `agent_id`, `target_id`, `payload` (JSONB) |
| `activity_logs` | Operational activity with retention | `workspace_id`, `activity_type`, `source_id`, `target_id`, `method`, `request_body`, `response_body`, `duration_ms`, `status`, `error_detail` |
| `canvas_layouts` | Node visual positions | `workspace_id`, `x`, `y`, `collapsed` |
| `canvas_viewport` | Canvas pan/zoom state | Single row, upserted |

### Redis Key Patterns

| Key Pattern | Value | TTL | Purpose |
|-------------|-------|-----|---------|
| `ws:{id}` | `"online"` | 60s | Liveness detection (heartbeat refreshes) |
| `ws:{id}:url` | Host-mapped URL | 5min | URL cache for external discovery |
| `ws:{id}:internal_url` | Docker-internal URL | — | Container-to-container discovery |
| `events:broadcast` | pub/sub channel | — | Event fanout to WebSocket hub |

---

## 5. Workspace Lifecycle

### State Machine

```
provisioning → online ↔ degraded
   ↓             ↓         ↓
 failed       offline    offline
   ↓             ↓
 retry      (auto-restart)

↓ (any state)
paused → (user resumes) → provisioning

↓ (any state)
removed
```

### Status Definitions

| Status | Meaning | Canvas Indicator |
|--------|---------|-----------------|
| `provisioning` | Waiting for first heartbeat | Spinner |
| `online` | Heartbeat received, reachable | Green dot |
| `degraded` | Online but `error_rate ≥ 0.5` | Yellow node with warning |
| `offline` | Heartbeat TTL expired, unreachable | Gray node |
| `paused` | User paused, container stopped, config preserved | Indigo badge |
| `failed` | Provisioning timeout or launch error | Red node + retry button |
| `removed` | Deleted, kept for event log history | Node removed from Canvas |

### Health Detection (Three Layers)

| Layer | Mechanism | Interval | Trigger |
|-------|-----------|----------|---------|
| **Passive** | Redis TTL expiry | 60s heartbeat key | Liveness monitor callback |
| **Proactive** | Docker API poll | Every 15s | Health sweep goroutine |
| **Reactive** | A2A proxy connection error | On-demand | `provisioner.IsRunning()` check |

All three layers call `onWorkspaceOffline()` → broadcast `WORKSPACE_OFFLINE` + auto-restart.

### Cascade Behavior

- **Pause**: Pausing a parent cascades to all children. Children of a paused parent cannot be individually resumed.
- **Delete**: Removes container, cleans memory (DB rows, Redis keys). Structure events and Agent Card history are **never** deleted.

---

## 6. Communication Rules

### Hierarchy = Topology

The `CanCommunicate()` function is the single source of truth for all access control.

| Direction | Allowed | Example |
|-----------|---------|---------|
| Sibling ↔ Sibling | **YES** | Marketing Agent ↔ Developer PM |
| Parent → Child | **YES** | Developer PM → Frontend Agent |
| Child → Parent | **YES** | Frontend Agent → Developer PM |
| Skip levels | **NO** | Frontend Agent → Business Core (403) |
| Cross-team | **NO** | Frontend Agent → Operations Team (403) |

### Access Check Algorithm

```
IF caller.parent_id == target.parent_id → ALLOW (siblings)
ELIF both parent_id IS NULL → ALLOW (root-level siblings)
ELIF caller.ID == target.parent_id → ALLOW (parent→child)
ELIF target.ID == caller.parent_id → ALLOW (child→parent)
ELSE → DENY (403 Forbidden)
```

This same logic governs: A2A delegation, memory scope enforcement, activity visibility, approval routing, and WebSocket event fanout.

---

## 7. Platform API Routes

### Workspace Lifecycle (8 endpoints)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `POST` | `/workspaces` | Create and provision new workspace |
| `GET` | `/workspaces` | List all with canvas layout data |
| `GET` | `/workspaces/:id` | Get single workspace |
| `PATCH` | `/workspaces/:id` | Update name, role, tier, runtime, parent |
| `DELETE` | `/workspaces/:id` | Remove workspace |
| `POST` | `/workspaces/:id/restart` | Restart container |
| `POST` | `/workspaces/:id/pause` | Pause with cascade to children |
| `POST` | `/workspaces/:id/resume` | Resume from pause |

### Registry & Discovery (5 endpoints)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `POST` | `/registry/register` | Workspace self-registration on startup |
| `POST` | `/registry/heartbeat` | Liveness + current task + error rate (30s interval) |
| `POST` | `/registry/update-card` | Push Agent Card updates (skills, capabilities) |
| `GET` | `/registry/discover/:id` | Resolve workspace URL (hierarchy-validated) |
| `GET` | `/registry/:id/peers` | List reachable peers for workspace |

### Memory (6 endpoints)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `POST` | `/workspaces/:id/memories` | Commit HMA-scoped memory (LOCAL/TEAM/GLOBAL) |
| `GET` | `/workspaces/:id/memories` | Search scoped memories with query |
| `DELETE` | `/workspaces/:id/memories/:memoryId` | Delete specific memory entry |
| `GET` | `/workspaces/:id/memory` | List key/value workspace memory |
| `POST` | `/workspaces/:id/memory` | Upsert key/value pair (optional TTL) |
| `DELETE` | `/workspaces/:id/memory/:key` | Delete key/value entry |

### Secrets & Config (5 endpoints)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/workspaces/:id/secrets` | Get merged workspace + global secrets |
| `POST` | `/workspaces/:id/secrets` | Upsert workspace secret (**triggers auto-restart**) |
| `DELETE` | `/workspaces/:id/secrets/:key` | Delete secret (**triggers auto-restart**) |
| `GET` | `/workspaces/:id/config` | Get workspace config.yaml |
| `PATCH` | `/workspaces/:id/config` | Update workspace config |

### A2A & Activity (5 endpoints)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `POST` | `/workspaces/:id/a2a` | Proxy A2A request to target workspace |
| `GET` | `/workspaces/:id/activity` | List activity rows (filterable) |
| `POST` | `/workspaces/:id/activity` | Report activity from workspace |
| `POST` | `/workspaces/:id/notify` | Emit user-facing notification |
| `GET` | `/workspaces/:id/session-search` | Search recent activity + memory for contextual recall |

### Team & Hierarchy (2 endpoints)

| Method | Endpoint | Purpose |
|--------|----------|---------|

### Files, Terminal, Templates, Bundles (8 endpoints)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/workspaces/:id/files[/*path]` | List or read files |
| `PUT` | `/workspaces/:id/files/*path` | Write file |
| `DELETE` | `/workspaces/:id/files/*path` | Delete file |
| `WS` | `/workspaces/:id/terminal` | WebSocket terminal session into container |
| `GET` | `/bundles/export/:id` | Export workspace as `.bundle.json` |
| `POST` | `/bundles/import` | Import workspace bundle (recursive) |
| `GET` | `/templates` | List available workspace templates |
| `POST` | `/templates/import` | Import custom template folder |

### Observability & Real-Time (5 endpoints)

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/health` | Health check |
| `GET` | `/metrics` | Prometheus metrics (v0.0.4 format) |
| `GET` | `/workspaces/:id/traces` | Langfuse trace links |
| `GET` | `/events[/:workspaceId]` | Event stream (SSE) |
| `WS` | `/ws` | WebSocket hub for real-time event fanout |

---

## 8. A2A Protocol

### Message Format (JSON-RPC 2.0 over HTTP)

```json
{
  "jsonrpc": "2.0",
  "id": "task-123",
  "method": "message/send",
  "params": {
    "message": {
      "role": "user",
      "parts": [{"kind": "text", "text": "Build the login feature"}],
      "messageId": "msg-456"
    }
  }
}
```

### Two Call Modes

| Mode | Method | Use Case |
|------|--------|----------|
| Synchronous | `message/send` | Short tasks, immediate response |
| Streaming | `message/sendSubscribe` | Long tasks, SSE progress updates |

### Discovery Flow

1. Caller queries `GET /registry/discover/:targetId` with `X-Workspace-ID` header
2. Platform validates `CanCommunicate(caller, target)` — returns 403 if denied
3. Returns Docker-internal URL (workspace caller) or host-mapped URL (Canvas/external caller)
4. Caller sends A2A message **directly** to target (peer-to-peer)
5. Target processes task and responds

### Task State Machine

```
submitted → working → completed
                   → failed
                   → canceled
         → input-required → working (caller provides input)
```

### Authentication

- **MVP (current)**: Discovery-time validation only. Direct A2A calls are unauthenticated (acceptable for self-hosted Docker network isolation).
- **Post-MVP**: Platform-issued short-lived signed tokens scoped to caller/target pair.

---

## 9. Hierarchical Memory Architecture

### Three Scopes

| Scope | Visibility | Write Access | Use Case |
|-------|-----------|-------------|----------|
| **LOCAL** | This workspace only | Self | Private scratch facts, reasoning, working state |
| **TEAM** | Parent + children + siblings | Self | Handoffs, coordination, team-level knowledge |
| **GLOBAL** | Readable by all workspaces | Root only | Org-wide policies, standards, institutional knowledge |

### Four Memory Surfaces

| Surface | Storage | Endpoint | Purpose |
|---------|---------|----------|---------|
| **Scoped agent memory** | `agent_memories` table | `POST /workspaces/:id/memories` | HMA-backed distributed memory with scope enforcement |
| **Key/value workspace memory** | `workspace_memory` table | `POST /workspaces/:id/memory` | Simple structured state, UI-visible, optional TTL |
| **Activity recall** | `activity_logs` + `agent_memories` | `GET /workspaces/:id/session-search` | "What just happened?" contextual recall |
| **Awareness-backed** | External service | Same tool interface | When `AWARENESS_URL` + `AWARENESS_NAMESPACE` configured |

### Memory → Skill Compounding Flywheel

```
Task execution
  → Durable insight captured in LOCAL/TEAM memory
  → Repeated success patterns detected (repetition signal)
  → Memory row promoted → SKILL.md package created
  → Hot-reload (~3 seconds) → skill injected into live runtime
  → Agent Card updated → broadcast to peers via WebSocket
  → Future tasks use promoted skill → faster + more reliable
  → Organization becomes more capable over time
```

Key property: promotion events are **visible** in activity logs. Skills are **inspectable** in Canvas Skills tab. This is not hidden prompt inflation.

---

## 10. Runtime Tier System

| Tier | Name | Container Flags | Use Case |
|------|------|-----------------|----------|
| **T1** | Sandboxed | Read-only rootfs, tmpfs /tmp, 512 MiB, no `/workspace` mount | Untrusted code, text-only analysis |
| **T2** | Standard (default) | Read-write, 512 MiB, 1 CPU, `/workspace` mount | Most agent workloads |
| **T3** | Privileged | `--privileged`, `--pid=host`, Docker network access | Internal tooling, elevated operations |
| **T4** | Full Access | T3 + `--network=host` + Docker socket mount | System-level orchestration, DevOps |

Unknown tier values default to T2 for safety. Applied via `provisioner.ApplyTierConfig()` during container creation.

---

## 11. Provisioning & Container Lifecycle

### Docker Networking

- All containers join `molecule-monorepo-net` private network
- Container naming: `ws-{workspace_id[:12]}`
- Ephemeral host port binding: `127.0.0.1:0→8000/tcp`

### URL Resolution

| Caller | URL Type | Example |
|--------|----------|---------|
| Workspace (container) | Docker-internal | `http://ws-{id}:8000` |
| Canvas (browser) | Host-mapped | `http://127.0.0.1:{ephemeral_port}` |

### Container Cleanup on Delete

1. Docker container stopped and removed
2. Memory cleaned (DB rows, Redis keys)
3. Status set to `removed`
4. `WORKSPACE_REMOVED` event written to structure_events
5. Structure events and Agent Card history **never** deleted (audit trail)

---

## 12. Workspace Runtime

### Entry Point: `workspace/main.py`

**Startup Sequence** (10 steps):

1. Initialize telemetry (OpenTelemetry, no-op if packages absent)
2. Load `config.yaml` into `WorkspaceConfig` dataclass
3. Run preflight validation (model availability, skills, configs)
4. Create `HeartbeatLoop` for background task tracking
5. Resolve adapter from `runtime` field in config
6. Run adapter `setup()` and `create_executor()`
7. Build Agent Card from loaded skills + runtime capabilities
8. Register: `POST /registry/register` with workspace ID + Agent Card
9. Start heartbeat loop (30s interval) + skill hot-reload watcher
10. Serve A2A over Uvicorn on configured port

### Runtime Configuration Schema (`config.yaml`)

```yaml
name: "Workspace Name"
description: ""
version: "1.0.0"
tier: 2                                    # 1=sandboxed, 2=standard, 3=privileged, 4=full-host
model: "anthropic:claude-sonnet-4-6"       # provider:model syntax
runtime: "langgraph"                       # langgraph | deepagents | claude-code | crewai | autogen | openclaw
runtime_config:                            # Runtime-specific settings
  command: "claude"                        # For CLI runtimes
  args: []
  auth_token_file: ".auth-token"
  timeout: 0
  model: ""                                # Override model just for this runtime
skills: ["skill1", "skill2"]               # Folder names under skills/
tools: ["web_search", "filesystem"]        # Built-in tool names
prompt_files: ["system-prompt.md"]         # Additional prompt text files
# `shared_context` was removed; team-shared knowledge now lives in memory v2's
# team:<id> namespace (recall_memory MCP tool). See RFC #2789 for shared files.

a2a:
  port: 8000
  streaming: true
  push_notifications: true

delegation:
  retry_attempts: 3
  retry_delay: 5.0
  timeout: 120.0
  escalate: true

sandbox:
  backend: "subprocess"                    # subprocess | docker
  memory_limit: "256m"
  timeout: 30

rbac:
  roles: ["operator"]
  allowed_actions: {}

hitl:
  channels:
    - type: "dashboard"
  default_timeout: 300
  bypass_roles: []

governance:
  enabled: false
  policy_mode: "audit"                     # audit | permissive | strict
  policy_file: ""

security_scan:
  mode: "warn"                             # warn | block | off

compliance:
  mode: "owasp_agentic"
  prompt_injection: "detect"               # detect | block
  max_tool_calls_per_task: 50
  max_task_duration_seconds: 300
```

### Six Runtime Adapters

| Adapter | Core Strength | Image Tag |
|---------|--------------|-----------|
| **LangGraph** | Graph-based state machine, tool use, streaming | `workspace-template:langgraph` |
| **DeepAgents** | Deep planning, multi-step task decomposition | `workspace-template:deepagents` |
| **Claude Code** | Native coding workflows, CLI continuity, OAuth auth | `workspace-template:claude-code` |
| **CrewAI** | Role-based crews, structured task orchestration | `workspace-template:crewai` |
| **AutoGen** | Multi-agent conversations, explicit strategies | `workspace-template:autogen` |
| **OpenClaw** | CLI-native runtime, own session model | `workspace-template:openclaw` |

**Branch-level WIP**: NemoClaw (NVIDIA T4 + Docker socket) on `feat/nemoclaw-t4-docker`.

Each adapter implements `setup()` + `create_executor()`. The base adapter provides shared infrastructure: system prompt assembly, skill loading, tool registration, coordinator detection, plugin injection.

---

## 13. Skills System

### Three Capability Sources

1. **Workspace-local skills**: `skills/<skill-name>/SKILL.md` + `tools/` directory
2. **Plugin-mounted rules**: `/plugins` volume (read-only), shared across all workspaces
3. **Built-in tools**: delegation, approval, memory, sandbox, telemetry, audit, compliance, governance

### Skill Directory Structure

```
skills/generate-seo-page/
├── SKILL.md              # YAML frontmatter + natural language instructions
├── tools/
│   ├── write_page.py     # @tool-decorated Python functions
│   └── check_gsc.py
├── examples/             # Few-shot examples
├── templates/            # Reference files (HTML, etc.)
└── links.yaml            # External resource URLs
```

### SKILL.md Frontmatter

```yaml
---
name: "Generate SEO Landing Page"
description: "Create SEO-optimized bilingual landing pages"
version: "1.0.0"
tags: ["seo", "content", "bilingual"]
examples: ["Create a Vancouver renovation page in EN/ZH"]
requires:
  env: ["GSC_CREDENTIALS"]
  bins: ["jq"]
---

# Agent instructions follow in natural language...
```

### Hot-Reload Pipeline

| Step | Action | Timing |
|------|--------|--------|
| 1 | File watcher detects change in `skills/` | 2s debounce |
| 2 | Reload skill metadata + tool Python modules | Immediate |
| 3 | Rebuild agent tools and Agent Card | ~100ms |
| 4 | Broadcast updated card via WebSocket | ~50ms |
| 5 | Peer system prompts rebuilt with new awareness | ~500ms |
| **Total** | End-to-end propagation | **~3 seconds** |

---

## 14. Bundle System

### Bundle Format (`.bundle.json`)

```json
{
  "schema": "1.0",
  "id": "seo-agent-vancouver",
  "name": "Vancouver SEO Agent",
  "tier": 1,
  "model": "anthropic:claude-sonnet-4-6",
  "system_prompt": "...full prompt text...",
  "skills": [{
    "id": "generate-seo-page",
    "name": "Generate SEO Landing Page",
    "files": {
      "SKILL.md": "---\nname: ...",
      "tools/write_page.py": "def write_page(...):\n    ..."
    }
  }],
  "tools": [{"id": "web_search", "config": {}}],
  "prompts": {"prompts/page-generation.md": "..."},
  "sub_workspaces": [],
  "agent_card": {"...": "A2A card snapshot"},
  "author": "molecule",
  "version": "1.2.0"
}
```

### Inclusion/Exclusion Rules

| Included | Excluded |
|----------|----------|
| Full system prompt text | API keys / secrets |
| All skill files (inlined) | Memory / conversation history |
| Prompt templates + assets | Database data |
| Tool configurations | Runtime state |
| Sub-workspace bundles (recursive) | |
| Agent Card snapshot | |

### Workflow

- **Export**: Right-click workspace → "Export as bundle" → `.bundle.json` download
- **Import**: Drag `.bundle.json` onto Canvas → `POST /bundles/import` → recursive provisioning → new IDs → `source_bundle_id` traces lineage

---

## 15. Canvas UI

### Tech Stack

| Layer | Technology |
|-------|-----------|
| Framework | Next.js 15 (App Router) |
| Graph | React Flow v12 (`@xyflow/react`) |
| State | Zustand |
| Styling | TailwindCSS v4 |
| Real-time | Native WebSocket API |

### Core Interactions

- **Drag-to-Nest**: Drag workspace over another → overlap detection → highlight → drop → update `parent_id`
- **Right-Click Menu**: Open Details/Chat/Terminal, Restart, Duplicate, Export Bundle, Expand/Collapse Team, Extract from Team, Delete
- **Template Palette**: Empty state shows up to 6 templates + "Create blank workspace"
- **Onboarding Wizard**: 4-step guided setup tracked in localStorage

### 10-Tab Operations Panel

| # | Tab | Function |
|---|-----|----------|
| 1 | **Chat** | A2A conversational interface with session history (last 20 messages) |
| 2 | **Activity** | Rich operation log — A2A messages, task updates, logs, skill promotions (filterable) |
| 3 | **Details** | Workspace metadata, runtime summary, status, Agent Card, restart/pause controls, peer list |
| 4 | **Skills** | Live skill display from Agent Card — metadata, tags, examples |
| 5 | **Terminal** | WebSocket shell into workspace container |
| 6 | **Config** | Structured YAML editor — runtime, skills, tools, A2A, delegation, sandbox settings |
| 7 | **Files** | File browser + editor for /configs, /workspace, /home, /plugins |
| 8 | **Memory** | Scoped memory view (LOCAL/TEAM/GLOBAL) + key/value workspace memory with TTL |
| 9 | **Traces** | Langfuse trace viewer — every LLM call with input/output/tokens/cost |
| 10 | **Events** | Structure event stream — real-time workspace change log |

### Real-Time Architecture

| Phase | Mechanism |
|-------|-----------|
| Initial load | `GET /workspaces` → Zustand store hydration |
| Live updates | WebSocket events → `applyEvent()` → instant Canvas re-render |
| Position persistence | `onNodeDragStop` → `PATCH /workspaces/:id` with x, y |
| Error recovery | Error boundary with reload button + hydration retry banner |

---

## 16. Tools & Capabilities

### Workspace Tools (`workspace/builtin_tools/`)

| Tool File | Purpose | RBAC |
|-----------|---------|------|
| `memory.py` | HMA memory `commit_memory()` / `search_memory()` | memory.write, memory.read |
| `delegation.py` | A2A delegation to peer workspaces with retry + tracing | delegate permission |
| `approval.py` | Human-in-the-loop approval flow with polling/WebSocket | approve permission |
| `audit.py` | RBAC enforcement + audit trail logging | audit enforcement |
| `compliance.py` | OWASP Agentic compliance checks | compliance check |
| `governance.py` | Microsoft Agent Governance Toolkit integration | policy evaluation |
| `hitl.py` | Multi-channel HITL (dashboard, Slack, email) | hitl.bypass_roles |
| `sandbox.py` | Code execution (subprocess or Docker backend) | sandbox access |
| `telemetry.py` | OpenTelemetry span creation and tracing | trace emission |
| `awareness_client.py` | Awareness namespace memory wrapper | memory scope |
| `security_scan.py` | CVE and security scanning (pip-audit/Snyk) | security audit |
| `temporal_workflow.py` | Temporal.io workflow integration | workflow engine |
| `a2a_tools.py` | A2A delegation helpers and route resolution | delegate/receive |

### Built-In MCP Tools (from `.mcp.json`)

| Server | Purpose |
|--------|---------|
| `molecule` | 20+ platform management tools (workspace CRUD, chat, memory, teams, secrets, files, approvals) |
| `awareness-memory` | Persistent cross-session memory via Awareness SDK |

---

## 17. Coordinator Pattern

### How Team Expansion Works

1. Workspace "expands into team" → becomes **coordinator** (team lead)
2. Coordinator fetches children's Agent Cards to understand capabilities
3. For each incoming task: analyzes → selects best-suited child → delegates via A2A
4. Aggregates responses when tasks span multiple children
5. Falls back to self-handling only if no child suitable

### Key Properties

- **Enforcement**: Coordinators **cannot** do direct work — all execution delegated to children
- **Recursive**: Child workspaces can themselves expand into teams (unlimited depth)
- **Transparent**: Upstream parent doesn't need to know if child is single agent or team of fifty
- **Detectable**: `coordinator.py` checks `get_children()` — if children exist, coordinator mode activates

---

## 18. Codebase Structure

### Python Runtime (95 files)

```
workspace/
├── main.py                    # Entry point (startup sequence)
├── config.py                  # Config parsing → dataclasses (120+ lines)
├── heartbeat.py               # 30s heartbeat loop
├── preflight.py               # Startup validation
├── plugins.py                 # Plugin rule/skill injection
├── coordinator.py             # Team lead routing
├── prompt.py                  # System prompt builder
├── adapters/
│   ├── __init__.py            # Adapter registry
│   ├── base.py                # BaseAdapter interface
│   ├── shared_runtime.py      # Shared execution logic
│   ├── langgraph/adapter.py
│   ├── deepagents/adapter.py
│   ├── claude_code/adapter.py
│   ├── crewai/adapter.py
│   ├── autogen/adapter.py
│   └── openclaw/adapter.py
├── tools/                     # 14 tool files
├── skills/
│   ├── loader.py              # SKILL.md parser + tool loader
│   └── watcher.py             # Hot-reload file watcher
└── tests/                     # 148 pytest tests
```

### Go Platform (94 files)

```
workspace-server/
├── cmd/
│   ├── server/main.go         # Entry point + dependency injection
│   └── cli/                   # molecli TUI dashboard
├── internal/
│   ├── handlers/              # 26 HTTP handler files (26k+ lines)
│   ├── registry/              # 6 files — workspace registry + access control
│   ├── provisioner/           # 8 files — Docker provisioning + tier enforcement
│   ├── ws/                    # 4 files — WebSocket hub + fanout
│   ├── events/                # 3 files — event broadcasting + Postgres persistence
│   ├── router/                # 2 files — route definitions + middleware
│   ├── db/                    # 6 files — Postgres + Redis drivers, migrations
│   └── crypto/                # 2 files — AES-256-GCM secrets encryption
└── migrations/                # 11 SQL migration files
```

### Canvas Frontend (62 TypeScript files)

```
canvas/
├── src/
│   ├── store/                 # Zustand store (workspaces, viewport, chat, activity)
│   ├── components/            # React Flow nodes, side panel tabs, context menus, modals
│   ├── hooks/                 # Custom hooks (WebSocket, resize, etc.)
│   └── lib/                   # API client, utilities
└── tests/                     # 188 Vitest tests
```

---

## 19. Key Design Patterns

### 1. Import Cycle Prevention (Go)

Function injection avoids circular imports between packages:
```go
hub := ws.NewHub(registry.CanCommunicate)
broadcaster := events.NewBroadcaster(hub)
registry.StartLivenessMonitor(ctx, onWorkspaceOffline)
```

### 2. JSONB Handling

Go `[]byte` must convert to `string()` before JSONB insert with `::jsonb` cast. `lib/pq` treats `[]byte` as bytea, not JSONB.

### 3. Event Sourcing

`structure_events` table is append-only — **never UPDATE, never DELETE**. Provides complete audit trail and event replay capability.

### 4. Template Resolution

On workspace create: (1) check template folder → (2) try `{runtime}-default` → (3) generate minimal config via `ensureDefaultConfig()`.

### 5. Hierarchy-Driven Everything

`CanCommunicate()` is the single source of truth. All operational concerns (communication, memory, access, approvals, event visibility) derive from the same hierarchy.

---

## 20. Docker Compose Orchestration

### Full Stack (`docker-compose.yml`)

| Service | Image | Port | Purpose |
|---------|-------|------|---------|
| `postgres` | postgres:16 | 5432 (internal) | Primary database (`wal_level=logical`) |
| `redis` | redis:7 | 6379 (internal) | Cache + pub/sub (`notify-keyspace-events=KEA`) |
| `langfuse-clickhouse` | clickhouse/clickhouse-server | internal | Analytics backend |
| `langfuse-web` | langfuse/langfuse | 3100 | Observability UI |
| `platform` | Built from Go | 8080 | Control plane |
| `canvas` | Built from Next.js | 3000 | Frontend |

### Optional Profiles

| Profile | Service | Purpose |
|---------|---------|---------|
| `multi-provider` | LiteLLM proxy | Unified API for OpenAI, Anthropic, Google, etc. |
| `local-models` | Ollama | Local LLM inference |

### Infrastructure-Only (`docker-compose.infra.yml`)

Postgres + Redis + Langfuse only (for local development without containerized workspace-server/canvas).

---

## 21. Environment Variables

### Platform (Go)

| Variable | Default | Purpose |
|----------|---------|---------|
| `DATABASE_URL` | `postgres://dev:dev@localhost:5432/molecule?sslmode=prefer` | Postgres connection |
| `REDIS_URL` | `redis://localhost:6379` | Redis connection |
| `PORT` | `8080` | Platform listen port |
| `PLATFORM_URL` | `http://host.docker.internal:8080` | Injected to workspace containers |
| `SECRETS_ENCRYPTION_KEY` | Optional | AES-256-GCM key (32 bytes) for tenant secret encryption. Provisioned at tenant boot by the control plane, which holds the master key in AWS KMS — see [secrets-key-custody.md](./secrets-key-custody.md). |
| `CONFIGS_DIR` | `/configs` | Workspace config template directory |
| `PLUGINS_DIR` | `/plugins` | Shared plugin directory |
| `ACTIVITY_RETENTION_DAYS` | `7` | Activity log retention |
| `ACTIVITY_CLEANUP_INTERVAL_HOURS` | `6` | Cleanup frequency |
| `CORS_ORIGINS` | `http://localhost:3000,...` | CORS whitelist |
| `RATE_LIMIT` | `600` | Requests per minute |
| `WORKSPACE_DIR` | Optional | Shared workspace volume |
| `AWARENESS_URL` | Optional | Awareness service URL |

### Canvas (Next.js)

| Variable | Default | Purpose |
|----------|---------|---------|
| `NEXT_PUBLIC_PLATFORM_URL` | `http://localhost:8080` | Platform backend URL |
| `NEXT_PUBLIC_WS_URL` | `ws://localhost:8080/ws` | WebSocket URL |
| `PORT` | `3000` | Canvas listen port |

### Workspace Runtime (Python)

| Variable | Default | Purpose |
|----------|---------|---------|
| `WORKSPACE_ID` | `workspace-default` | Unique workspace identifier |
| `WORKSPACE_CONFIG_PATH` | `/configs` | Config directory mount |
| `PLATFORM_URL` | `http://platform:8080` | Platform connection |
| `PARENT_ID` | Empty | Parent workspace ID (set if nested) |
| `AWARENESS_URL` | Optional | Awareness service |
| `AWARENESS_NAMESPACE` | Optional | Scoped namespace for awareness memory |
| `LANGFUSE_HOST` | `http://langfuse-web:3000` | Langfuse endpoint |
| `LANGFUSE_PUBLIC_KEY` | Optional | Langfuse auth |
| `LANGFUSE_SECRET_KEY` | Optional | Langfuse auth |
| `DEPLOYMENT_RETRY_ATTEMPTS` | `3` | Delegation retry count |
| `DELEGATION_TIMEOUT` | `120` | Delegation timeout (seconds) |
| `APPROVAL_TIMEOUT` | `300` | Approval wait timeout (seconds) |
| `AUDIT_LOG_PATH` | `/var/log/molecule/audit.jsonl` | Audit log file path |

---

## 22. Recent Feature Highlights

| Feature | Description |
|---------|-------------|
| **A2A streaming response** | Real-time task result delivery via SSE (`message/sendSubscribe`) |
| **Onboarding wizard** | 4-step guided first-run experience in Canvas |
| **Global API keys** | Platform-wide secrets with per-workspace override + AES-256 encryption |
| **Coordinator enforcement** | Team leads cannot do work, only route and aggregate |
| **Cascade pause/resume** | Pausing a parent cascades to all children; paused children can't be individually resumed |
| **Graceful A2A errors** | `[A2A_ERROR]` sentinel + retry with exponential backoff + fallback |
| **Canvas error boundary** | React class component catches render errors, shows retry button |
| **Hydration retry** | Banner with "Retry" button + `PLATFORM_URL` hint on WebSocket stale state |
| **Activity log retention** | Configurable cleanup (default 7 days, `ACTIVITY_RETENTION_DAYS`) |
| **Security hardening** | Hub double-close race fix (`sync.Once`), A2A proxy timeout (5min canvas, ∞ workspace), Python JSON decode guards |

---

## 23. Known Gaps & Backlog

### Test Coverage

18 of 26 Go handler files have **zero unit tests**: a2a_proxy, workspace, templates, registry, discovery, secrets, etc. Current: 278 tests with 25% baseline enforced.

### Silent Failures

6+ locations with fire-and-forget `ExecContext` DB writes need proper error handling (activity log inserts, event broadcasts).

### Python Tool Error Handling

Tools call `resp.json()` without catching JSON decode errors. Should wrap in try/except for malformed responses.

### Branch-Level Work

| Branch | Feature | Status |
|--------|---------|--------|
| `feat/nemoclaw-t4-docker` | NemoClaw adapter (NVIDIA T4 support) | WIP |
| Backlog | Firecracker backend (faster cold starts) | Planned |
| Backlog | E2B backend (cloud-hosted code sandbox) | Planned |
| Backlog | pgvector semantic memory search | Planned |
| Backlog | Canvas search, batch operations, keyboard shortcuts | Planned |

---

## 24. Licensing & Commercialization Path

### Open Source (Current)

- **License**: MIT
- **Strategy**: Maximize adoption, zero friction
- **Model**: Follows n8n Community Edition approach

### SaaS Path (Future `molecule-cloud` repo)

| Feature | Technology |
|---------|-----------|
| Authentication | Clerk or Auth.js |
| Multi-tenancy | `org_id` column added to schema |
| Billing | Stripe integration |
| Managed infrastructure | ECS + Neon + Upstash |
| White-labeling | Custom Canvas branding |

**Key principle**: No changes to core open-source repo. SaaS layer is purely additive.

---

## 25. OSS Growth Research

Analysis of 8 OSS agent projects (from `oss-agent-growth-research.md`):

### Winning Launch Formula

```
[Viral Demo] + [HN Front Page] + [One Major Amplifier] + [Zero-Friction Install]
     ↓              ↓                   ↓                         ↓
  60s video     400+ upvotes      Karpathy / Altman /       docker compose up
  screen rec    top comment       Major YouTuber             3 commands max
```

Every Tier 1 launch (Open Interpreter, CrewAI) had all four elements.

### Documentation Best Practice (Diataxis Model)

| Type | Purpose | Example |
|------|---------|---------|
| Tutorials | Learning-oriented | "Build your first agent team in 5 minutes" |
| How-to guides | Task-oriented | "How to configure RBAC for production" |
| Explanation | Understanding-oriented | "Why memory follows org boundaries" |
| Reference | Information-oriented | API route tables, config schema |

---

## 26. Technical Debt & Constraints

### Hard Design Constraints

1. **Platform never routes agent messages** — A2A is strictly peer-to-peer
2. **Postgres is fact source, Redis is cache** — Redis loss is fully recoverable
3. **`structure_events` is append-only** — Never UPDATE, never DELETE
4. **`workspace-template` has no business logic** — Logic lives in `workspace-configs-templates/`
5. **Bundles never include secrets** — API keys forbidden from serialization
6. **Hierarchy = topology** — No manual edge wiring; all communication derived from `parent_id`

---

## 27. Production Deployment

### Multi-Host Configuration

- Docker-internal URLs (`http://ws-{id}:8000`) work directly between containers
- Nginx on host handles TLS termination
- For external HTTPS: proxy requests to host-mapped URLs

### Volume Management

| Mode | Configuration | Behavior |
|------|--------------|----------|
| **Default** | No `WORKSPACE_DIR` | Each workspace gets isolated Docker volume `ws-{id}-workspace` |
| **Shared** | `WORKSPACE_DIR=/path` | All agents mount same host directory (read/write) |

---

## 28. MCP Server & Integrations

### Molecule AI MCP Server (`mcp-server/`)

20+ tools for Claude Code, Cursor, Codex, or any MCP client:

- Workspace CRUD (list, create, get, delete, restart)
- Agent communication (`chat_with_agent`)
- Memory operations (`commit_memory`, `search_memory`)
- Team management (`expand_team`, `collapse_team`)
- Secrets management (`set_secret`, `list_secrets`)
- File operations (`read_file`, `write_file`, `delete_file`)
- Approvals (`list_pending_approvals`, `decide_approval`)
- Config updates (`update_workspace`)
- Templates (`list_templates`)

**Transport**: stdio (local CLI integration)

```json
{
  "mcpServers": {
    "molecule": {
      "type": "stdio",
      "command": "node",
      "args": ["./mcp-server/dist/index.js"],
      "env": {"MOLECULE_URL": "http://localhost:8080"}
    }
  }
}
```

### Awareness MCP Server

For persistent cross-session memory:

```json
{
  "awareness-memory": {
    "type": "stdio",
    "command": "npx",
    "args": ["-y", "@awareness-sdk/local", "mcp"]
  }
}
```

---

## 29. Summary Statistics

| Metric | Value |
|--------|-------|
| Python runtime files | 95 |
| Go platform files | 94 |
| TypeScript/JS canvas files | 62 |
| Runtime adapter implementations | 6 |
| Go handler files | 26 |
| Postgres migrations | 11 |
| Core workspace tools | 14 |
| Platform API endpoints | 40+ |
| MCP tools | 20+ |
| Go tests | 278 (with `-race` flag) |
| Canvas Vitest tests | 188 |
| Python pytest tests | 148 |
| **Total tests** | **614** |
| Activity retention | 7 days (configurable) |
| Heartbeat interval | 30s (default) |
| Redis liveness TTL | 60s |
| Health sweep interval | 15s (proactive) |
| Skill hot-reload propagation | ~3 seconds |
| Coverage baseline (Go) | 25% enforced in CI |

---

## 30. Vision: From Agent Teams to Robot Teams

Molecule AI's workspace abstraction is **runtime-agnostic by design**. A workspace is a role with an A2A interface — not an LLM with a prompt. The same hierarchy, memory boundaries, approval chains, and governance that organize AI agents in containers today can organize any autonomous system that speaks A2A.

| Phase | Era | Systems | Status |
|-------|-----|---------|--------|
| **NOW** | Software Agent Teams | LLM agents in Docker, 6 adapters, HMA, Langfuse, A2A | **LIVE on main** |
| **NEXT** | Terminal + Device Agents | Terminal bots, browser agents, IoT controllers, CI/CD agents | **BUILDING** |
| **HORIZON** | Embodied Robot Teams | Warehouse robots, autonomous vehicles, manufacturing cells, field inspection | **HORIZON** |

> *The workspace is the role. The protocol is A2A. The boundary between digital and physical disappears — the organizational layer remains.*

---

## Links

- **GitHub**: https://git.moleculesai.app/molecule-ai/molecule-monorepo
- **Architecture Docs**: https://git.moleculesai.app/molecule-ai/molecule-monorepo/src/branch/main/docs/architecture
- **API Protocol**: https://git.moleculesai.app/molecule-ai/molecule-monorepo/src/branch/main/docs/api-protocol
- **Agent Runtime**: https://git.moleculesai.app/molecule-ai/molecule-monorepo/src/branch/main/docs/agent-runtime
- **Product Docs**: https://git.moleculesai.app/molecule-ai/molecule-monorepo/src/branch/main/docs/product

---

*© 2026 Molecule AI Technologies, Inc.*
