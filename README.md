<div align="center">

<p>
  <img src="./docs/assets/branding/enteros-logo.svg" alt="EnterOS" width="420" />
</p>

<p>
  <a href="./README.md">English</a> | <a href="./README.zh-CN.md">中文</a>
</p>

<h3>The world's first operating system for AI Agent teams</h3>

<p>
  EnterOS creates the agent-native workforce layer: agents with roles, org charts, memory, governance, and production control, running at scale as one organized system instead of disappearing after a chat.
</p>

<p>
  <a href="https://www.enteros.ai"><img alt="Website: enteros.ai" src="./docs/assets/branding/cta-website.svg" height="40"></a>
  <a href="https://github.com/EnterOS-AI/enter-os-core"><img alt="GitHub: star repo" src="./docs/assets/branding/cta-star.svg" height="40"></a>
  <a href="#quick-start"><img alt="Quick Start: run locally" src="./docs/assets/branding/cta-quick-start.svg" height="40"></a>
  <a href="./docs/architecture/architecture.md"><img alt="Architecture: read design" src="./docs/assets/branding/cta-architecture.svg" height="40"></a>
</p>

[![License: BSL 1.1](https://img.shields.io/badge/License-BSL%201.1-orange.svg)](LICENSE)

[![Go Version](https://img.shields.io/badge/go-1.25+-00ADD8?logo=go)](https://golang.org/)
[![Python Version](https://img.shields.io/badge/python-3.11+-3776AB?logo=python)](https://www.python.org/)
[![Next.js](https://img.shields.io/badge/Next.js-15-black?logo=next.js)](https://nextjs.org/)

<p>
  Frameworks build agents • EnterOS builds the world they work in
</p>

<p>
  <a href="./docs/index.md"><strong>Docs Home</strong></a> •
  <a href="./docs/quickstart.md"><strong>Quick Start</strong></a> •
  <a href="./docs/architecture/architecture.md"><strong>Architecture</strong></a> •
  <a href="./docs/api-protocol/platform-api.md"><strong>Platform API</strong></a> •
  <a href="./docs/agent-runtime/workspace-runtime.md"><strong>Workspace Runtime</strong></a>
</p>

</div>

---

## The Big Bet

Agents are becoming labor, but today's tools still treat them like prompts, chats, scripts, or workflow nodes.

EnterOS is the missing operating layer for that shift: a world where AI agents have job roles, reporting lines, memory boundaries, runtime contracts, approvals, and production controls. It is how agents stop being isolated demos and start becoming an organized digital workforce.

> Frameworks build agents. EnterOS builds the workforce they run in.

## Why This Repo Is Worth A Star

| If you believe... | EnterOS is building... |
|---|---|
| **AI agents will become a real workforce** | A role-native workspace model where every agent has identity, ownership, lifecycle, and responsibility. |
| **Agent teams need org structure, not spaghetti graphs** | Org-chart topology where delegation, visibility, approvals, and memory naturally follow hierarchy. |
| **Memory must compound without becoming a security leak** | `LOCAL`, `TEAM`, and `GLOBAL` memory scopes backed by Memory v2 and pgvector semantic recall. |
| **No single runtime will own the future** | A shared operating layer for Claude Code, Codex, Hermes, OpenClaw, and future runtimes. |
| **Production agents need operations, not vibes** | Canvas, WebSockets, traces, terminal, files, restart flows, runtime tiers, and live workspace control. |
| **Agent systems should improve with use** | A memory-to-skill loop where durable wins become reusable, hot-reloadable procedure. |

## Quick Start

```bash
git clone https://github.com/EnterOS-AI/enter-os-core.git
cd enter-os-core
./scripts/dev-start.sh
```

Then open [http://localhost:3000](http://localhost:3000), add your model API key in **Config → Secrets & API Keys → Global**, and create a workspace from a template.

See the full [Quickstart Guide](./docs/quickstart.md) for prerequisites, manual setup, and troubleshooting.

## The Pitch

EnterOS is the world's first operating system for AI Agent teams: the org-native layer that turns fragmented agents into a governed, memory-driven, production-ready workforce.

It combines the parts that are usually scattered across demos, internal glue code, and framework-specific tooling into one product:

- one org-native operating layer for teams, roles, hierarchy, permissions, and lifecycle
- one runtime layer that lets **four** maintained agent runtimes — Claude Code, Codex, **Hermes**, and OpenClaw — run side by side behind one workspace contract
- one memory model that keeps recall, sharing, and skill evolution aligned with organizational boundaries (Memory v2 backed by pgvector for semantic recall)
- one operational surface for observing, pausing, restarting, inspecting, and improving live workspaces

Most teams can build a workflow, a strong single agent, a coding agent, or a custom multi-agent graph.

Very few teams can run all of that as a governed organization with clear structure, durable memory boundaries, and production operations.

That is the gap EnterOS closes.

## Why EnterOS Feels Different

### 1. The node is a role, not a task

In EnterOS, a workspace is an organizational role. That role can begin as one agent, later expand into a sub-team, and still keep the same external identity, hierarchy position, memory boundary, and A2A interface.

### 2. The org chart is the topology

You do not wire collaboration paths by hand. Hierarchy defines the default communication surface. The structure is not decorative UI. It is part of the operating model.

### 3. Runtime choice stops being a dead-end decision

Claude Code, Codex, Hermes, and OpenClaw can all plug into the same workspace abstraction. Teams can standardize governance without forcing every group onto one runtime.

### 4. Memory is treated like infrastructure

EnterOS's HMA approach is designed around organizational boundaries, not just "store more context somewhere." Durable recall, scoped sharing through the v2 memory plugin, and skill promotion are all part of one coherent system.

### 5. It comes with a real control plane

Registry, heartbeats, restart, pause/resume, activity logs, approvals, terminal access, files, traces, bundles, templates, and WebSocket fanout are not afterthoughts. They are first-class parts of the platform.

## The Category Gap EnterOS Fills

| Category | What it does well | Where it breaks | What EnterOS adds |
|---|---|---|---|
| Workflow builders | Visual task automation | Nodes are tasks, not durable organizational roles | Role-native workspaces, hierarchy, long-lived teams |
| Agent frameworks | Strong runtime semantics | Weak control plane and weak org-level operations | Unified lifecycle, canvas, registry, policies, observability |
| Coding agents | Excellent local execution | Usually not designed as team infrastructure | Workspace abstraction, A2A collaboration, platform ops |
| Custom multi-agent graphs | Full flexibility | Brittle topology and governance sprawl | Standardized operating model without losing runtime freedom |

## What Makes EnterOS Defensible

| Advantage | Why it matters in practice |
|---|---|
| **Role-native workspace abstraction** | Your org structure survives model swaps, framework changes, and team expansion |
| **Fractal team expansion** | A single specialist can become a managed department without breaking upstream integrations |
| **Heterogeneous runtime compatibility** | Different teams can keep their preferred agent architecture while sharing one control plane |
| **HMA + v2 memory plugin** | Memory sharing follows hierarchy instead of leaking across the whole system; one plugin per tenant, namespace-scoped per workspace |
| **Skill evolution loop** | Durable successful workflows can graduate from memory into reusable, hot-reloadable skills |
| **WebSocket-first operational UX** | The canvas reflects task state, structure changes, and A2A responses in near real time |
| **Global secrets with local override** | Centralize provider access, then override only where a workspace needs specialized credentials |

## Runtime Compatibility, Compared

EnterOS is not trying to replace the frameworks below. It is the system that makes them easier to run together.

| Runtime / architecture | Status in current repo | Native strength | What EnterOS adds |
|---|---|---|---|
| **Claude Code** | Shipping on `main` | Real coding workflows, CLI-native continuity | Secure workspace abstraction, A2A delegation, org boundaries, shared control plane |
| **Codex** | Shipping on `main` | OpenAI Codex CLI workflows | Secure workspace abstraction, A2A delegation, org boundaries, shared control plane |
| **Hermes 4** | Shipping on `main` | Hybrid reasoning, native tools, json_schema (NousResearch/hermes-agent) | Option B upstream hook, A2A bridge to OpenAI-compat API, multi-provider provider derivation |
| **OpenClaw** | Shipping on `main` | CLI-native runtime with its own session model | Workspace lifecycle, templates, activity logs, topology-aware collaboration |
| **NemoClaw** | WIP on `feat/nemoclaw-t4-docker` | NVIDIA-oriented runtime path | Planned to join the same abstraction once merged; not yet part of `main` |

This is the key idea: **many agent runtimes, one organizational operating system**.

## Why The Memory Architecture Compounds

Most projects stop at "we added memory." EnterOS pushes further:

| Conventional memory setup | EnterOS |
|---|---|
| Flat store or weak namespaces | Hierarchy-aligned `LOCAL`, `TEAM`, `GLOBAL` scopes |
| Sharing is easy to overexpose | Sharing is explicit and structure-aware |
| Memory and procedure get mixed together | Memory stores durable facts; skills store repeatable procedure |
| Every agent can become over-privileged | Per-workspace namespaces in the v2 memory plugin reduce blast radius |
| UI memory and runtime memory blur together | Separate surfaces for scoped agent memory, key/value workspace memory, and recall |

### The flywheel

```text
Task execution
   -> durable insight captured in memory
   -> repeated success becomes a signal
   -> workflow promoted into a reusable skill
   -> skill hot-reloads into the runtime
   -> future work gets faster and more reliable
```

This is one of EnterOS's strongest long-term advantages: the system can get more operationally capable without turning into one giant hidden prompt.

## Self-Improving Agent Teams, Built Into EnterOS

Most agent systems stop at "a smart runtime." EnterOS pushes further: it gives teams a way to **capture what worked, promote repeatable procedure into skills, reload those improvements into live workspaces, and keep the whole loop visible at the platform level**.

| Positioning lens | Conventional self-improving agent pattern | EnterOS |
|---|---|---|
| **Unit of improvement** | A single agent session or runtime | A workspace, a team, and eventually the whole org graph |
| **Operational surface** | Mostly hidden inside the agent loop | Visible in the platform, Canvas, activity stream, memory surfaces, and runtime controls |
| **Strategic outcome** | A smarter agent | A compounding organization with durable knowledge and governed reusable skills |

### Where that shows up

| Core mechanism | Why it matters |
|---|---|
| **Durable memory that survives sessions** | Memory is not just durable, it is **workspace-scoped** — every write lands in the workspace's own `workspace:<id>` namespace, with `team:<root>` and `org:<root>` available for cross-workspace shares via the platform's namespace ACL when an agent explicitly promotes a memory |
| **Cross-session recall** | Recall spans both activity history and memory rows, so the system can search what happened and what was learned without inventing a separate hidden store |
| **Skills built from experience** | Promotion from memory into a skill candidate is surfaced as an explicit platform activity, not a silent internal side effect |
| **Skill improvement during use** | Skills hot-reload into the live runtime, so improvements become available on the next A2A task without restarting the workspace |
| **Persistent skill lifecycle** | Skills are not just generated once; they can be audited, installed, published, shared, mounted by plugins, and governed as reusable operational assets |

### Why this matters

1. **The learning loop is org-aware, not just session-aware.**
   Memory can live at `LOCAL`, `TEAM`, or `GLOBAL` scope, and the v2 plugin's namespace ACL gives each workspace a durable identity boundary.

2. **The learning loop is visible to operators.**
   Promotion events, activity logs, current-task updates, traces, and WebSocket fanout mean self-improvement is part of the control plane, not a hidden black box.

3. **The learning loop compounds across teams, not just one agent.**
   A workflow learned by one workspace can become a governed skill, reload into the runtime, appear in the Agent Card, and become usable inside a larger organizational hierarchy.

The result is not just "an agent that learns." It is **an organization that gets more capable as its workspaces accumulate durable memory and reusable procedure**.

## What Ships In `main`

### Canvas (v4)

- Next.js 15 + React Flow + Zustand
- **warm-paper theme system** — light / dark / follow-system, SSR cookie + nonce'd boot script + ThemeProvider; terminal + code surfaces stay dark unconditionally
- drag-to-nest team building
- empty-state deployment + onboarding wizard
- template palette
- bundle import/export
- 10-tab side panel for chat, activity, details, skills, terminal, config, files, memory, traces, and events

### Platform

- Go 1.25 / Gin control plane (80+ HTTP endpoints + Gorilla WebSocket fanout)
- workspace CRUD and provisioning (pluggable Provisioner — Docker locally, cloud VMs in production)
- **A2A response path is a typed discriminated union** — frozen dataclasses + total parser; 100% unit + adversarial fuzz coverage
- registry and heartbeats
- browser-safe A2A proxy
- team expansion/collapse
- activity logs and approvals
- secrets and global secrets
- files API, terminal, bundles, templates, viewport persistence

### Runtime

- standalone workspace-template images that install the workspace runtime from a package registry; thin VM image in production
- adapter-driven execution across **4 maintained runtimes** (Claude Code, Codex, Hermes, OpenClaw)
- Agent Card registration
- **Memory v2 backed by pgvector** — per-tenant plugin sidecar serving HMA namespaces with FTS + semantic recall
- plugin-mounted shared rules/skills
- hot-reloadable local skills
- coordinator-only delegation path

### Ops

- Langfuse traces
- current-task reporting
- pause/resume/restart flows
- activity streaming
- runtime tiers
- direct workspace inspection through terminal and files

### SaaS (EnterOS Cloud)

- multi-tenant on cloud VMs + per-tenant Postgres branch + per-tenant tunnels (no public ports)
- WorkOS AuthKit + Stripe Checkout + Customer Portal
- KMS envelope encryption (DB / Redis connection strings); managed secret store for tenant bootstrap
- `tenant_resources` audit table + 30-min boot-event-aware reconciler — every infrastructure lifecycle event recorded, claim vs live state diffed

## Built For Teams That Need More Than A Demo

EnterOS is especially strong when you need to run:

- AI engineering teams with PM / Dev Lead / QA / Research / Ops roles
- mixed runtime organizations where one team prefers Hermes and another prefers Claude Code
- long-lived agent organizations that need memory boundaries and reusable procedures
- internal platforms that want to expose agent teams as structured infrastructure, not ad hoc scripts

## Architecture

```text
Canvas (Next.js 15, warm-paper :3000)  <--HTTP / WS-->  Platform (Go 1.25 :8080)  <---> Postgres + Redis
         |                                                           |
         |                                                           +--> Provisioner: Docker (local) / cloud VMs (prod)
         |                                                           +--> bundles · templates · secrets · KMS
         |
         +------------------------- shows ------------------------> workspaces, teams, tasks, traces, events

Workspace Runtime (Python ≥3.11, image with adapters)
  - 4 adapters: Claude Code / Codex / Hermes / OpenClaw
  - Agent Card + A2A server (typed-SSOT response path)
  - heartbeat + activity + Memory v2 (pgvector semantic recall via per-tenant plugin sidecar)
  - skills + plugins + hot reload

EnterOS Cloud (managed SaaS, separate offering)
  - per-tenant cloud VM + Postgres branch + tunnel
  - WorkOS · Stripe · KMS · managed secret store
  - tenant_resources audit + 30-min reconciler
```

## Quick Start

```bash
git clone https://github.com/EnterOS-AI/enter-os-core.git
cd enter-os-core

cp .env.example .env
# Defaults boot the stack locally out of the box. See .env.example for
# production hardening knobs (ADMIN_TOKEN, SECRETS_ENCRYPTION_KEY, etc.).

./infra/scripts/setup.sh
# Boots Postgres (:5432), Redis (:6379), Langfuse (:3001),
# and Temporal (:7233 gRPC, :8233 UI) on the shared
# Docker network. Temporal runs with no auth on localhost —
# dev-only; production must gate it.
#
# Also populates the template/plugin registry by cloning every repo
# listed in manifest.json. Requires jq — install via
# `brew install jq` (macOS) or `apt install jq` (Debian). Idempotent:
# re-runs skip any target dir that's already populated.

cd workspace-server
go run ./cmd/server   # applies pending migrations on first boot

cd ../canvas
npm install
npm run dev
```

Then open `http://localhost:3000`:

1. Deploy a template or create a blank workspace from the empty state.
2. Follow the onboarding guide into `Config`.
3. Add a provider key in `Secrets & API Keys`.
4. Open `Chat` and send the first task.

## Documentation Map

- [Docs Home](./docs/index.md)
- [Quick Start](./docs/quickstart.md)
- [Product Overview](./docs/product/overview.md)
- [System Architecture](./docs/architecture/architecture.md)
- [Memory Architecture](./docs/architecture/memory.md)
- [Platform API](./docs/api-protocol/platform-api.md)
- [Workspace Runtime](./docs/agent-runtime/workspace-runtime.md)
- [Canvas UI](./docs/frontend/canvas.md)
- [Local Development](./docs/development/local-development.md)
- [Backend Parity Matrix](./docs/architecture/backends.md) — Docker vs cloud-VM feature parity tracker
- [Testing Strategy](./docs/engineering/testing-strategy.md) — tiered coverage floors, not blanket 100%
- [Glossary](./docs/glossary.md) — how we use "harness", "workspace", "plugin", "flow" vs. ecosystem neighbors

## Current Scope

The current `main` branch ships the core platform, Canvas v4 (warm-paper themed), Memory v2 (pgvector semantic recall), the typed-SSOT A2A response path, **four maintained production adapters** (Claude Code, Codex, Hermes, OpenClaw), skill lifecycle, and operational surfaces.

**EnterOS Cloud** provides the managed SaaS surface — multi-tenant orchestration on cloud VMs + per-tenant Postgres + tunnels, KMS envelope encryption, WorkOS auth, Stripe billing, and a `tenant_resources` audit table with a 30-min reconciler.

Adjacent runtime work such as **NemoClaw** remains branch-level until merged, and this README keeps that distinction explicit on purpose.

## License

[Business Source License 1.1](LICENSE) — copyright © 2025 EnterOS.

Personal, internal, and non-commercial use is permitted without restriction. You may not use the Licensed Work to offer a competing product or service. On January 1, 2029, the license converts to Apache 2.0.
