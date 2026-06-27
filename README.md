<div align="center">

<p>
  <img src="./docs/assets/branding/enteros-logo.svg#gh-light-mode-only" alt="EnterOS" width="420" />
  <img src="./docs/assets/branding/enteros-logo-white.svg#gh-dark-mode-only" alt="EnterOS" width="420" />
</p>

<h3>The first true operating system for AI agents</h3>

<p>
  EnterOS is building the agent-native workforce layer: through a unified runtime, organizational
  topology, and a production-grade control plane, it gives agents real job roles, org structure
  (seniority), scoped memory, and governance — evolving them into production-grade automated labor
  with enormous upside.
</p>

<p>
  <b>Frameworks build agents. EnterOS builds the organization they work in.</b>
</p>

<p>
  <a href="https://www.enteros.ai"><b>Website</b></a> ·
  <a href="./docs/quickstart.md"><b>Quick Start</b></a> ·
  <a href="./docs/architecture/architecture.md"><b>Architecture</b></a> ·
  <a href="./docs/api-protocol/platform-api.md"><b>API</b></a> ·
  <a href="./README.zh-CN.md"><b>中文</b></a>
</p>

<p>
  <a href="LICENSE"><img alt="License: BSL 1.1" src="https://img.shields.io/badge/License-BSL%201.1-orange.svg"></a>
  <a href="https://golang.org/"><img alt="Go 1.25+" src="https://img.shields.io/badge/go-1.25+-00ADD8?logo=go"></a>
  <a href="https://www.python.org/"><img alt="Python 3.11+" src="https://img.shields.io/badge/python-3.11+-3776AB?logo=python"></a>
  <a href="https://nextjs.org/"><img alt="Next.js 15" src="https://img.shields.io/badge/Next.js-15-black?logo=next.js"></a>
  <a href="https://github.com/EnterOS-AI/enter-os-core/stargazers"><img alt="GitHub stars" src="https://img.shields.io/github/stars/EnterOS-AI/enter-os-core?style=social"></a>
</p>

</div>

<!-- TODO: drop a Canvas screenshot or product GIF here — it's the single highest-impact addition to this README.
<p align="center">
  <img src="./docs/assets/screenshots/canvas.png" alt="EnterOS Canvas" width="900" />
</p>
-->

---

## Why EnterOS

Agents are becoming labor. But today's tools still treat them like prompts, chats, scripts, or workflow nodes — isolated demos that can't be staffed, governed, or trusted in production.

**EnterOS is the first true operating system for AI agents** — the missing operating layer for this shift. It gives AI agents everything a real workforce needs: job roles, reporting lines, memory boundaries, runtime contracts, approvals, and production controls. Anyone can build a workflow, a strong single agent, or a custom multi-agent graph. Only EnterOS runs all of it as a *governed organization* — with clear structure, durable memory boundaries, and real operations.

That is the gap EnterOS closes — and we're the first to close it.

|  | Workflow builders | Agent frameworks | Coding agents | **EnterOS** |
|---|:---:|:---:|:---:|:---:|
| Visual automation | ✅ | — | — | ✅ |
| Strong runtime semantics | — | ✅ | ✅ | ✅ (any runtime) |
| Org-native roles & hierarchy | — | — | — | ✅ |
| Unified control plane & ops | — | — | — | ✅ |
| Scoped, compounding memory | — | — | — | ✅ |
| Multi-runtime, one operating model | — | — | — | ✅ |

## Features

- **🧩 Roles, not tasks** — A workspace is an organizational role with a stable identity, lifecycle, and memory boundary. It can start as one agent and grow into a managed sub-team without breaking upstream integrations.
- **🏢 The org chart is the topology** — Delegation, visibility, approvals, and memory follow your hierarchy automatically. No hand-wired collaboration graphs.
- **🔌 Bring any runtime** — Claude Code, Codex, Hermes, and OpenClaw run side by side behind one workspace contract. Standardize governance without forcing every team onto one runtime.
- **🧠 Memory as infrastructure** — `LOCAL` / `TEAM` / `GLOBAL` scopes backed by Memory v2 and pgvector semantic recall. Sharing follows hierarchy instead of leaking across the system.
- **♻️ Self-improving teams** — Durable wins are promoted from memory into reusable, hot-reloadable skills — visible at the platform level, not hidden inside an agent loop.
- **🎛️ A real control plane** — Registry, heartbeats, approvals, terminal, files, traces, pause/resume/restart, and WebSocket-driven live updates are first-class, not afterthoughts.

## Quick Start

```bash
git clone https://github.com/EnterOS-AI/enter-os-core.git
cd enter-os-core

cp .env.example .env
# Defaults boot the full stack locally out of the box.
# See .env.example for production hardening (ADMIN_TOKEN, SECRETS_ENCRYPTION_KEY, …).

# Boots Postgres, Redis, Langfuse, and Temporal on a shared Docker network,
# and populates the template/plugin registry. Requires jq (`brew install jq`).
./infra/scripts/setup.sh

# Start the control plane (applies migrations on first boot)
cd workspace-server && go run ./cmd/server

# In a second shell, start the Canvas UI
cd canvas && npm install && npm run dev
```

Then open **[http://localhost:3000](http://localhost:3000)**:

1. Deploy a template or create a blank workspace from the empty state.
2. Follow the onboarding wizard into **Config**.
3. Add a provider key under **Secrets & API Keys**.
4. Open **Chat** and send your first task.

> See the full [Quickstart Guide](./docs/quickstart.md) for prerequisites, manual setup, and troubleshooting.

## Architecture

```text
Canvas (Next.js 15, :3000)  <--HTTP / WS-->  Platform (Go 1.25, :8080)  <--->  Postgres + Redis
        │                                              │
        │                                              ├─ Provisioner: Docker (local) / cloud VMs (prod)
        │                                              └─ bundles · templates · secrets · KMS
        │
        └──────────── shows ────────────> workspaces, teams, tasks, traces, events

Workspace Runtime (Python ≥3.11, adapter image)
  • 4 adapters: Claude Code / Codex / Hermes / OpenClaw
  • Agent Card + A2A server (typed response path, fuzz-tested)
  • heartbeat · activity · Memory v2 (pgvector semantic recall)
  • skills · plugins · hot reload

EnterOS Cloud (managed SaaS, separate offering)
  • per-tenant cloud VM + Postgres branch + private tunnel
  • WorkOS · Stripe · KMS · managed secret store
  • tenant_resources audit + 30-min reconciler
```

Read the full [System Architecture](./docs/architecture/architecture.md) and [Memory Architecture](./docs/architecture/memory.md) for design details.

## Runtime Compatibility

EnterOS doesn't replace these runtimes — it's the system that makes them easy to run *together*.

| Runtime | Status | Native strength | What EnterOS adds |
|---|---|---|---|
| **Claude Code** | ✅ Shipping on `main` | Real coding workflows, CLI-native continuity | Secure workspace abstraction, A2A delegation, org boundaries, shared control plane |
| **Codex** | ✅ Shipping on `main` | OpenAI Codex CLI workflows | Same workspace contract, delegation, and ops surface |
| **Hermes 4** | ✅ Shipping on `main` | Hybrid reasoning, native tools, structured output | A2A bridge to OpenAI-compatible APIs, multi-provider derivation |
| **OpenClaw** | ✅ Shipping on `main` | CLI-native runtime with its own session model | Workspace lifecycle, templates, activity logs, topology-aware collaboration |
| **NemoClaw** | 🚧 Branch (`feat/nemoclaw-t4-docker`) | NVIDIA-oriented runtime path | Joins the same abstraction once merged — not yet on `main` |

**Many agent runtimes, one organizational operating system.**

## Why the Memory Architecture Compounds

Most projects stop at "we added memory." EnterOS treats memory as scoped, governed infrastructure — and turns it into a flywheel:

```text
Task execution
  → durable insight captured in scoped memory
  → repeated success becomes a signal
  → workflow promoted into a reusable skill
  → skill hot-reloads into the live runtime
  → future work gets faster and more reliable
```

| Conventional memory | EnterOS |
|---|---|
| Flat store or weak namespaces | Hierarchy-aligned `LOCAL` / `TEAM` / `GLOBAL` scopes |
| Sharing easily over-exposes | Sharing is explicit and structure-aware |
| Memory and procedure get mixed | Memory stores durable facts; skills store repeatable procedure |
| Every agent risks over-privilege | Per-workspace namespaces shrink the blast radius |

The result isn't "an agent that learns" — it's *an organization that gets more capable as its workspaces accumulate durable memory and reusable procedure*.

## What Ships in `main`

<table>
<tr>
<td valign="top" width="50%">

**Canvas (v4)**
- Next.js 15 · React Flow · Zustand
- Warm-paper theme system (light / dark / system)
- Drag-to-nest team building & onboarding wizard
- Template palette + bundle import/export
- 10-tab side panel: chat, activity, details, skills, terminal, config, files, memory, traces, events

**Platform**
- Go 1.25 / Gin control plane (80+ endpoints + WebSocket fanout)
- Workspace CRUD & provisioning (Docker local / cloud VM prod)
- Typed, fuzz-tested A2A response path
- Registry, heartbeats, browser-safe A2A proxy
- Activity logs, approvals, secrets, files, terminal, templates

</td>
<td valign="top" width="50%">

**Runtime**
- Adapter-driven execution across 4 maintained runtimes
- Agent Card registration
- Memory v2 backed by pgvector (FTS + semantic recall)
- Plugin-mounted shared rules/skills, hot-reloadable skills

**Ops**
- Langfuse traces & current-task reporting
- Pause / resume / restart flows
- Activity streaming, runtime tiers
- Direct workspace inspection via terminal & files

**EnterOS Cloud**
- Multi-tenant cloud VMs + per-tenant Postgres + private tunnels
- WorkOS AuthKit · Stripe · KMS envelope encryption
- `tenant_resources` audit table + 30-min reconciler

</td>
</tr>
</table>

## Built for Teams That Need More Than a Demo

EnterOS is strongest when you need to run:

- AI engineering teams with PM / Dev Lead / QA / Research / Ops roles
- Mixed-runtime organizations — one team on Hermes, another on Claude Code
- Long-lived agent organizations that need memory boundaries and reusable procedures
- Internal platforms that expose agent teams as structured infrastructure, not ad-hoc scripts

## Documentation

| | |
|---|---|
| [Docs Home](./docs/index.md) | [Quick Start](./docs/quickstart.md) |
| [Product Overview](./docs/product/overview.md) | [System Architecture](./docs/architecture/architecture.md) |
| [Memory Architecture](./docs/architecture/memory.md) | [Platform API](./docs/api-protocol/platform-api.md) |
| [Workspace Runtime](./docs/agent-runtime/workspace-runtime.md) | [Canvas UI](./docs/frontend/canvas.md) |
| [Local Development](./docs/development/local-development.md) | [Testing Strategy](./docs/engineering/testing-strategy.md) |
| [Backend Parity Matrix](./docs/architecture/backends.md) | [Glossary](./docs/glossary.md) |

## Contributing

EnterOS is built in the open and we welcome contributors. Start with [Local Development](./docs/development/local-development.md), browse [open issues](https://github.com/EnterOS-AI/enter-os-core/issues), and open a PR. For larger changes, please open an issue first to discuss direction.

## Community

- 🌐 Website — [enteros.ai](https://www.enteros.ai)
- 🐛 Issues & feature requests — [GitHub Issues](https://github.com/EnterOS-AI/enter-os-core/issues)
- 💬 Discussions — [GitHub Discussions](https://github.com/EnterOS-AI/enter-os-core/discussions)

If EnterOS resonates with you, **a ⭐ helps more than you'd think** — it's how this category gets noticed.

## License

[Business Source License 1.1](LICENSE) — copyright © 2025 EnterOS.

Personal, internal, and non-commercial use is permitted without restriction. You may not use the Licensed Work to offer a competing product or service. On **January 1, 2029**, the license converts to **Apache 2.0**.

---

## Founder

**Maverick** — Founder, EnterOS

- 💬 WeChat: `-MrCui-`
- ✉️ Email: [saitannrinn@gmail.com](mailto:saitannrinn@gmail.com)

<div align="center">
<sub>Building the operating system for the agent workforce. Frameworks build agents — EnterOS builds the organization they work in.</sub>
</div>
