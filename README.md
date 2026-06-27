<div align="center">

<p>
  <img src="./docs/assets/branding/enteros-logo.svg" alt="EnterOS" width="420" />
</p>

<p>
  <a href="./README.md">English</a> | <a href="./README.zh-CN.md">中文</a>
</p>

<h3>The first agent-native operating system for AI Agent teams</h3>

<p>
  EnterOS turns autonomous agents into an organized, production-grade workforce: roles, hierarchy, scoped memory, governance, runtime choice, and live operations in one control plane.
</p>

<p>
  <a href="https://www.enteros.ai"><img alt="Website: enteros.ai" src="./docs/assets/branding/cta-website.svg" height="40"></a>
  <a href="#quick-start"><img alt="Quick Start: run locally" src="./docs/assets/branding/cta-quick-start.svg" height="40"></a>
  <a href="./docs/architecture/architecture.md"><img alt="Architecture: read design" src="./docs/assets/branding/cta-architecture.svg" height="40"></a>
</p>

[![License: BSL 1.1](https://img.shields.io/badge/License-BSL%201.1-orange.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.25+-00ADD8?logo=go)](https://golang.org/)
[![Python Version](https://img.shields.io/badge/python-3.11+-3776AB?logo=python)](https://www.python.org/)
[![Next.js](https://img.shields.io/badge/Next.js-15-black?logo=next.js)](https://nextjs.org/)

<p>
  <strong>Frameworks build agents. EnterOS builds the organization they work in.</strong>
</p>

<p>
  <a href="./docs/index.md"><strong>Docs</strong></a> ·
  <a href="./docs/quickstart.md"><strong>Quick Start</strong></a> ·
  <a href="./docs/architecture/architecture.md"><strong>Architecture</strong></a> ·
  <a href="./docs/api-protocol/platform-api.md"><strong>Platform API</strong></a> ·
  <a href="#founder--early-tester-group"><strong>Founder & Testers</strong></a>
</p>

</div>

---

## Why EnterOS

AI agents are moving from tools into labor. But most stacks still manage them as isolated chats, scripts, workflow nodes, or framework-specific demos.

EnterOS is the operating layer for the next step: a system where agents have durable roles, reporting lines, memory boundaries, credentials, approvals, runtime contracts, traces, and restartable production workflows.

If you are building one agent, a framework may be enough. If you are building an AI organization, you need an operating system.

## What EnterOS Gives You

| Layer | What it means in practice |
|---|---|
| **Agent-native organization** | Workspaces are durable team roles, not disposable task nodes. A role can start as one agent and later expand into a managed sub-team without breaking its identity. |
| **Org chart as runtime topology** | Delegation, visibility, approvals, and memory sharing follow hierarchy instead of a hand-wired graph. |
| **Heterogeneous runtime control** | Claude Code, Codex, Hermes, and OpenClaw can run behind one workspace contract and one operational surface. |
| **Scoped memory that can compound safely** | `LOCAL`, `TEAM`, and `GLOBAL` memory scopes align durable knowledge with organizational boundaries. |
| **Production operations for agent teams** | Canvas, registry, heartbeats, activity logs, traces, terminal, files, pause/resume, restart, and WebSocket updates are first-class platform primitives. |
| **Memory-to-skill evolution** | Successful work can move from durable memory into reusable, hot-reloadable skills that make future work faster and more reliable. |

## Built For

EnterOS is designed for teams that are already past the "single impressive agent" stage:

- AI engineering teams building PM, Dev Lead, QA, Research, Ops, and Support agent roles
- platform teams that need agents to behave like managed internal infrastructure
- product teams turning agent workflows into repeatable, governed production systems
- enterprises that need memory, credentials, approvals, and execution boundaries before agents can scale
- builders comparing multiple runtimes and avoiding lock-in to one agent framework

## What Ships In `main`

| Area | Current capability |
|---|---|
| **Canvas** | Next.js 15 + React Flow canvas, drag-to-nest teams, template deployment, onboarding, bundle import/export, and a 10-tab workspace side panel for chat, activity, details, skills, terminal, config, files, memory, traces, and events. |
| **Control plane** | Go 1.25 / Gin backend with workspace CRUD, registry, A2A proxy, approvals, secrets, global secrets, files API, terminal access, viewport persistence, and Gorilla WebSocket fanout. |
| **Runtime layer** | Python workspace runtime with maintained adapters for Claude Code, Codex, Hermes, and OpenClaw under a shared workspace contract. |
| **Memory** | Memory v2 backed by pgvector semantic recall, full-text search, per-workspace namespaces, and hierarchy-aware `LOCAL` / `TEAM` / `GLOBAL` scopes. |
| **Skills** | Local `SKILL.md` packages, plugin-mounted shared skills/rules, audit/install/publish helpers, and hot reload into live workspaces. |
| **Operations** | Langfuse traces, current-task reporting, pause/resume/restart flows, runtime tiers, activity streaming, and direct workspace inspection. |
| **EnterOS Cloud** | Managed SaaS surface with cloud VM orchestration, per-tenant Postgres, tunnels, WorkOS AuthKit, Stripe billing, KMS envelope encryption, and tenant resource reconciliation. |

## Runtime Compatibility

EnterOS is not trying to replace agent frameworks. It gives them a shared organizational operating model.

| Runtime / architecture | Status in this repo | Native strength | What EnterOS adds |
|---|---|---|---|
| **Claude Code** | Shipping on `main` | CLI-native coding workflows | Workspace boundaries, A2A delegation, shared secrets, traces, and team operations. |
| **Codex** | Shipping on `main` | OpenAI Codex CLI workflows | Same control plane, memory model, and collaboration contract as the rest of the org. |
| **Hermes** | Shipping on `main` | Hybrid reasoning, tools, OpenAI-compatible API paths | A2A bridge, provider derivation, workspace lifecycle, and team-level visibility. |
| **OpenClaw** | Shipping on `main` | CLI-native runtime and session model | Templates, activity logs, topology-aware collaboration, and platform operations. |
| **NemoClaw** | Branch-level work | NVIDIA-oriented runtime path | Planned to join the same abstraction when merged; not claimed as `main` functionality. |

## Architecture

```text
Canvas (Next.js 15, React Flow, Zustand)
  |
  | HTTP / WebSocket
  v
Platform Control Plane (Go 1.25, Gin, Postgres, Redis)
  |
  | provisioning, registry, secrets, files, events, A2A proxy
  v
Workspace Runtime (Python 3.11+)
  |
  | adapters: Claude Code / Codex / Hermes / OpenClaw
  v
Agent Team
  |
  | memory, skills, traces, terminal, files, activity
  v
Operational Feedback Loop
```

The core idea is simple: **many agent runtimes, one agent-native organization.**

## Quick Start

```bash
git clone https://github.com/EnterOS-AI/enter-os-core.git
cd enter-os-core
./scripts/dev-start.sh
```

The script boots the local stack, including Postgres, Redis, Langfuse, ClickHouse, Temporal, the Go control plane, and the Next.js canvas.

Then open [http://localhost:3000](http://localhost:3000):

1. Add your model key in **Config -> Secrets & API Keys -> Global**.
2. Deploy a template or create a blank workspace.
3. Open **Chat** and send the first task.

For prerequisites, manual setup, and remote-agent paths, read the [Quickstart Guide](./docs/quickstart.md).

## Tech Stack

| Layer | Stack |
|---|---|
| **Frontend** | Next.js 15, React 19, React Flow, Zustand, Tailwind CSS, Radix UI, xterm.js |
| **Backend** | Go 1.25, Gin, Gorilla WebSocket, Postgres, Redis |
| **Runtime** | Python 3.11+, workspace adapters, A2A server, plugin-mounted skills |
| **Memory** | pgvector, full-text search, per-workspace namespaces, scoped recall |
| **Ops** | Docker Compose, Langfuse, Temporal, traces, runtime tiers, restart flows |
| **Cloud** | Cloud VM provisioning, per-tenant Postgres, tunnels, WorkOS, Stripe, KMS |

## Founder & Early Tester Group

EnterOS is led by founder **Mr. Cui** and is actively onboarding early testers who are building real agent teams, internal automation platforms, or production AI workforce systems.

If you want to test EnterOS, give product feedback, or explore the managed cloud offering, reach out:

- WeChat: `-MrCui-`
- Email: [saitannrinn@gmail.com](mailto:saitannrinn@gmail.com)

## Documentation

- [Docs Home](./docs/index.md)
- [Quick Start](./docs/quickstart.md)
- [Architecture Overview](./docs/architecture/overview.md)
- [System Architecture](./docs/architecture/architecture.md)
- [Memory Architecture](./docs/architecture/memory.md)
- [Platform API](./docs/api-protocol/platform-api.md)
- [Workspace Runtime](./docs/agent-runtime/workspace-runtime.md)
- [Canvas UI](./docs/frontend/canvas.md)
- [Local Development](./docs/development/local-development.md)
- [Testing Strategy](./docs/engineering/testing-strategy.md)
- [Glossary](./docs/glossary.md)

## Current Scope

The current `main` branch ships the core platform, Canvas, Memory v2, the typed A2A response path, four maintained runtime adapters, skill lifecycle, and the main operational surfaces for running agent teams locally.

EnterOS Cloud is the managed SaaS surface for teams that want hosted orchestration, tenant isolation, billing, authentication, encrypted secrets, and managed infrastructure lifecycle.

Adjacent runtime work such as NemoClaw remains branch-level until merged, and this README keeps that distinction explicit.

## License

[Business Source License 1.1](LICENSE) — copyright © 2025 EnterOS.

Personal, internal, and non-commercial use is permitted without restriction. You may not use the Licensed Work to offer a competing product or service. On January 1, 2029, the license converts to Apache 2.0.
