<div align="center">

<p>
  <img src="./docs/assets/branding/molecule-icon.png" alt="Molecule AI Icon Logo" width="160" />
</p>

<p>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="./docs/assets/branding/molecule-text-white.png">
    <img src="./docs/assets/branding/molecule-text-black.png" alt="Molecule AI Text Logo" width="420" />
  </picture>
</p>

<p>
  <a href="./README.md">English</a> | <a href="./README.zh-CN.md">中文</a>
</p>

<h3>The Org-Native Control Plane For Heterogeneous AI Agent Teams</h3>

<p>
  The world's most powerful governance platform for AI agent teams.
</p>

[![License: BSL 1.1](https://img.shields.io/badge/License-BSL%201.1-orange.svg)](LICENSE)

[![Go Version](https://img.shields.io/badge/go-1.25+-00ADD8?logo=go)](https://golang.org/)
[![Python Version](https://img.shields.io/badge/python-3.11+-3776AB?logo=python)](https://www.python.org/)
[![Next.js](https://img.shields.io/badge/Next.js-15-black?logo=next.js)](https://nextjs.org/)

<p>
  Visual Canvas • Runtime Compatibility • Hierarchical Memory • Skill Evolution • Operational Guardrails
</p>

<p>
  <a href="./docs/index.md"><strong>Docs Home</strong></a> •
  <a href="./docs/quickstart.md"><strong>Quick Start</strong></a> •
  <a href="./docs/architecture/architecture.md"><strong>Architecture</strong></a> •
  <a href="./docs/api-protocol/platform-api.md"><strong>Platform API</strong></a> •
  <a href="./docs/agent-runtime/workspace-runtime.md"><strong>Workspace Runtime</strong></a>
</p>

[![Deploy on Railway](https://railway.app/button.svg)](https://railway.app/new/template?template=https://github.com/Molecule-AI/molecule-monorepo)
[![Deploy to Render](https://render.com/images/deploy-to-render-button.svg)](https://render.com/deploy?repo=https://github.com/Molecule-AI/molecule-monorepo)

</div>

---

## The Pitch

Molecule AI is the most powerful way to govern an AI agent organization in production.

It combines the parts that are usually scattered across demos, internal glue code, and framework-specific tooling into one product:

- one org-native control plane for teams, roles, hierarchy, and lifecycle
- one runtime layer that lets LangGraph, DeepAgents, Claude Code, CrewAI, AutoGen, and OpenClaw run side by side
- one memory model that keeps recall, sharing, and skill evolution aligned with organizational boundaries
- one operational surface for observing, pausing, restarting, inspecting, and improving live workspaces

Most teams can build a workflow, a strong single agent, a coding agent, or a custom multi-agent graph.

Very few teams can run all of that as a governed organization with clear structure, durable memory boundaries, and production operations.

That is the gap Molecule AI closes.

## Why Molecule AI Feels Different

### 1. The node is a role, not a task

In Molecule AI, a workspace is an organizational role. That role can begin as one agent, later expand into a sub-team, and still keep the same external identity, hierarchy position, memory boundary, and A2A interface.

### 2. The org chart is the topology

You do not wire collaboration paths by hand. Hierarchy defines the default communication surface. The structure is not decorative UI. It is part of the operating model.

### 3. Runtime choice stops being a dead-end decision

LangGraph, DeepAgents, Claude Code, CrewAI, AutoGen, and OpenClaw can all plug into the same workspace abstraction. Teams can standardize governance without forcing every group onto one runtime.

### 4. Memory is treated like infrastructure

Molecule AI's HMA approach is designed around organizational boundaries, not just “store more context somewhere.” Durable recall, scoped sharing, awareness namespaces, and skill promotion are all part of one coherent system.

### 5. It comes with a real control plane

Registry, heartbeats, restart, pause/resume, activity logs, approvals, terminal access, files, traces, bundles, templates, and WebSocket fanout are not afterthoughts. They are first-class parts of the platform.

## The Category Gap Molecule AI Fills

| Category | What it does well | Where it breaks | What Molecule AI adds |
|---|---|---|---|
| Workflow builders | Visual task automation | Nodes are tasks, not durable organizational roles | Role-native workspaces, hierarchy, long-lived teams |
| Agent frameworks | Strong runtime semantics | Weak control plane and weak org-level operations | Unified lifecycle, canvas, registry, policies, observability |
| Coding agents | Excellent local execution | Usually not designed as team infrastructure | Workspace abstraction, A2A collaboration, platform ops |
| Custom multi-agent graphs | Full flexibility | Brittle topology and governance sprawl | Standardized operating model without losing runtime freedom |

## What Makes Molecule AI Defensible

| Advantage | Why it matters in practice |
|---|---|
| **Role-native workspace abstraction** | Your org structure survives model swaps, framework changes, and team expansion |
| **Fractal team expansion** | A single specialist can become a managed department without breaking upstream integrations |
| **Heterogeneous runtime compatibility** | Different teams can keep their preferred agent architecture while sharing one control plane |
| **HMA + awareness namespaces** | Memory sharing follows hierarchy instead of leaking across the whole system |
| **Skill evolution loop** | Durable successful workflows can graduate from memory into reusable, hot-reloadable skills |
| **WebSocket-first operational UX** | The canvas reflects task state, structure changes, and A2A responses in near real time |
| **Global secrets with local override** | Centralize provider access, then override only where a workspace needs specialized credentials |

## Runtime Compatibility, Compared

Molecule AI is not trying to replace the frameworks below. It is the system that makes them easier to run together.

| Runtime / architecture | Status in current repo | Native strength | What Molecule AI adds |
|---|---|---|---|
| **LangGraph** | Shipping on `main` | Graph control, tool use, Python extensibility | Canvas orchestration, hierarchy routing, A2A, memory scopes, operational lifecycle |
| **DeepAgents** | Shipping on `main` | Deeper planning and decomposition | Same workspace contract, team topology, activity stream, restart behavior |
| **Claude Code** | Shipping on `main` | Real coding workflows, CLI-native continuity | Secure workspace abstraction, A2A delegation, org boundaries, shared control plane |
| **CrewAI** | Shipping on `main` | Role-based crews | Persistent workspace identity, policy consistency, shared canvas and registry |
| **AutoGen** | Shipping on `main` | Assistant/tool orchestration | Standardized deployment, hierarchy-aware collaboration, shared ops plane |
| **OpenClaw** | Shipping on `main` | CLI-native runtime with its own session model | Workspace lifecycle, templates, activity logs, topology-aware collaboration |
| **NemoClaw** | WIP on `feat/nemoclaw-t4-docker` | NVIDIA-oriented runtime path | Planned to join the same abstraction once merged; not yet part of `main` |

This is the key idea: **many agent runtimes, one organizational operating system**.

## Why The Memory Architecture Compounds

Most projects stop at “we added memory.” Molecule AI pushes further:

| Conventional memory setup | Molecule AI |
|---|---|
| Flat store or weak namespaces | Hierarchy-aligned `LOCAL`, `TEAM`, `GLOBAL` scopes |
| Sharing is easy to overexpose | Sharing is explicit and structure-aware |
| Memory and procedure get mixed together | Memory stores durable facts; skills store repeatable procedure |
| Every agent can become over-privileged | Workspace awareness namespaces reduce blast radius |
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

This is one of Molecule AI's strongest long-term advantages: the system can get more operationally capable without turning into one giant hidden prompt.

## Self-Improving Agent Teams, Built Into Molecule AI

Most agent systems stop at "a smart runtime." Molecule AI pushes further: it gives teams a way to **capture what worked, promote repeatable procedure into skills, reload those improvements into live workspaces, and keep the whole loop visible at the platform level**.

| Positioning lens | Conventional self-improving agent pattern | Molecule AI |
|---|---|---|
| **Unit of improvement** | A single agent session or runtime | A workspace, a team, and eventually the whole org graph |
| **Operational surface** | Mostly hidden inside the agent loop | Visible in the platform, Canvas, activity stream, memory surfaces, and runtime controls |
| **Strategic outcome** | A smarter agent | A compounding organization with durable knowledge and governed reusable skills |

### Where that shows up in Molecule AI

| Core mechanism | Molecule AI module(s) | Why it matters |
|---|---|---|
| **Durable memory that survives sessions** | `workspace/builtin_tools/memory.py`, `workspace/builtin_tools/awareness_client.py`, `workspace-server/internal/handlers/memories.go` | Memory is not just durable, it is **workspace-scoped** and can route into awareness namespaces tied to the org structure |
| **Cross-session recall** | `workspace-server/internal/handlers/activity.go` (`/workspaces/:id/session-search`) | Recall spans both activity history and memory rows, so the system can search what happened and what was learned without inventing a separate hidden store |
| **Skills built from experience** | `workspace/builtin_tools/memory.py` (`_maybe_log_skill_promotion`) | Promotion from memory into a skill candidate is surfaced as an explicit platform activity, not a silent internal side effect |
| **Skill improvement during use** | `workspace/skill_loader/watcher.py`, `workspace/skill_loader/loader.py`, `workspace/main.py` | Skills hot-reload into the live runtime, so improvements become available on the next A2A task without restarting the workspace |
| **Persistent skill lifecycle** | `workspace-server/cmd/cli/cmd_agent_skill.go`, `workspace/plugins.py` | Skills are not just generated once; they can be audited, installed, published, shared, mounted by plugins, and governed as reusable operational assets |

### Why this matters in Molecule AI

1. **The learning loop is org-aware, not just session-aware.**
   Memory can live at `LOCAL`, `TEAM`, or `GLOBAL` scope, and awareness namespaces give each workspace a durable identity boundary.

2. **The learning loop is visible to operators.**
   Promotion events, activity logs, current-task updates, traces, and WebSocket fanout mean self-improvement is part of the control plane, not a hidden black box.

3. **The learning loop compounds across teams, not just one agent.**
   A workflow learned by one workspace can become a governed skill, reload into the runtime, appear in the Agent Card, and become usable inside a larger organizational hierarchy.

The result is not just “an agent that learns.” It is **an organization that gets more capable as its workspaces accumulate durable memory and reusable procedure**.

## What Ships In `main`

### Canvas

- Next.js 15 + React Flow + Zustand
- drag-to-nest team building
- empty-state deployment + onboarding wizard
- template palette
- bundle import/export
- 10-tab side panel for chat, activity, details, skills, terminal, config, files, memory, traces, and events

### Platform

- Go/Gin control plane
- workspace CRUD and provisioning
- registry and heartbeats
- browser-safe A2A proxy
- team expansion/collapse
- activity logs and approvals
- secrets and global secrets
- files API, terminal, bundles, templates, viewport persistence

### Runtime

- unified `workspace/` image
- adapter-driven execution
- Agent Card registration
- awareness-backed memory integration
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

## Built For Teams That Need More Than A Demo

Molecule AI is especially strong when you need to run:

- AI engineering teams with PM / Dev Lead / QA / Research / Ops roles
- mixed runtime organizations where one team prefers LangGraph and another prefers Claude Code
- long-lived agent organizations that need memory boundaries and reusable procedures
- internal platforms that want to expose agent teams as structured infrastructure, not ad hoc scripts

## Architecture

```text
Canvas (Next.js :3000)  <--HTTP / WS-->  Platform (Go :8080)  <---> Postgres + Redis
         |                                          |
         |                                          +--> Docker provisioner / bundles / templates / secrets
         |
         +-------------------- shows --------------------> workspaces, teams, tasks, traces, events

Workspace Runtime (Python image with adapters)
  - LangGraph / DeepAgents / Claude Code / CrewAI / AutoGen / OpenClaw
  - Agent Card + A2A server
  - heartbeat + activity + awareness-backed memory
  - skills + plugins + hot reload
```

## Quick Start

```bash
git clone https://github.com/Molecule-AI/molecule-monorepo.git
cd molecule-monorepo

cp .env.example .env
# Defaults boot the stack locally out of the box. See .env.example for
# production hardening knobs (ADMIN_TOKEN, SECRETS_ENCRYPTION_KEY, etc.).

./infra/scripts/setup.sh
# Boots Postgres (:5432), Redis (:6379), Langfuse (:3001),
# and Temporal (:7233 gRPC, :8233 UI) on the shared
# `molecule-monorepo-net` Docker network. Temporal runs with
# no auth on localhost — dev-only; production must gate it.
#
# Also populates the template/plugin registry by cloning every repo
# listed in manifest.json into workspace-configs-templates/,
# org-templates/, and plugins/. Requires jq — install via
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
- [Backend Parity Matrix](./docs/architecture/backends.md) — Docker vs EC2 feature parity tracker
- [Testing Strategy](./docs/engineering/testing-strategy.md) — tiered coverage floors, not blanket 100%
- [PR Hygiene](./docs/engineering/pr-hygiene.md) — small PRs, clean branches, cherry-pick on drift
- [Engineering Postmortems](./docs/engineering/) — architecture + testing lessons from real incidents
- [Ecosystem Watch](./docs/ecosystem-watch.md) — adjacent projects we track (Holaboss, Hermes, gstack, …)
- [Glossary](./docs/glossary.md) — how we use "harness", "workspace", "plugin", "flow" vs. ecosystem neighbors

## Current Scope

The current `main` branch already includes the core platform, canvas, memory model, six production adapters, skill lifecycle, and operational surfaces. Adjacent runtime work such as **NemoClaw** remains branch-level until merged, and this README keeps that distinction explicit on purpose.

## License

[Business Source License 1.1](LICENSE) — copyright © 2025 Molecule AI.

Personal, internal, and non-commercial use is permitted without restriction. You may not use the Licensed Work to offer a competing product or service. On January 1, 2029, the license converts to Apache 2.0.
