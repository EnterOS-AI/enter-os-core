---
layout: home

hero:
  name: EnterOS
  text: The Org-Native OS For AI Agent Teams
  tagline: Visual canvas, Go control plane, pluggable runtimes, scoped memory, and operational guardrails for heterogeneous agent organizations.
  image:
    src: /assets/branding/enteros-icon.png
    alt: EnterOS
  actions:
    - theme: brand
      text: Quickstart
      link: /quickstart
    - theme: alt
      text: Architecture
      link: /architecture/architecture
    - theme: alt
      text: Platform API
      link: /api-protocol/platform-api

features:
  - title: Visual Org Canvas
    details: Build agent organizations as nested workspaces on a live React Flow canvas with drag-to-nest hierarchy, template deployment, bundles, and real-time updates.
    icon: "🗺️"
  - title: Runtime Compatibility
    details: Current main ships adapters for Claude Code, Codex, Hermes, and OpenClaw under one workspace contract and A2A surface.
    icon: "⚙️"
  - title: Hierarchical Memory
    details: HMA-style LOCAL, TEAM, and GLOBAL scopes backed by the v2 memory plugin (per-tenant pgvector sidecar with FTS + semantic recall).
    icon: "🧠"
  - title: Skill Evolution
    details: Local SKILL.md packages, tool loading, plugin-mounted shared capabilities, hot reload, and a documented memory-to-skill promotion path.
    icon: "🧩"
  - title: Operational Control Plane
    details: Registry, heartbeats, pause/resume/restart, approvals, activity logs, traces, terminal access, and runtime tiered provisioning.
    icon: "🛡️"
  - title: Remote Agent Support
    details: Register agents on any infrastructure — Docker, Fly Machines, bare metal, or laptops — and manage the full fleet from one canvas with bearer token auth and 30s heartbeat visibility.
    icon: "🌐"
  - title: Global Secrets
    details: Platform-wide API keys can be inherited by every workspace, with workspace-level overrides when a role needs custom credentials.
    icon: "🔐"
---

## What Current `main` Includes

| Area | Current capability |
|---|---|
| **Canvas** | Empty-state deployment, onboarding guide, 10-tab side panel, template palette, bundle import/export, drag-to-nest teams, search, activity and trace views |
| **Platform** | Workspace CRUD, registry, A2A proxy, team expansion, approvals, secrets, global secrets, memory APIs, files API, terminal, viewport persistence, WebSocket fanout |
| **Runtime** | One workspace image with four shipping adapters on `main`: Claude Code, Codex, Hermes, OpenClaw |
| **Memory** | v2 plugin (pgvector + FTS) serving scoped agent memories under per-workspace namespaces; key/value workspace memory; session-search recall |
| **Skills** | Local skill packages, plugin-mounted shared skills/rules, audit/install/publish CLI helpers, hot reload |

## Compatibility Note

`main` currently ships four runtime adapters: Claude Code, Codex, Hermes, and OpenClaw. `NemoClaw` appears in branch-level work (`feat/nemoclaw-t4-docker`) and is not documented here as merged `main` functionality.

## Recommended Reading

- [Quickstart](/quickstart)
- [Product Overview](/product/overview)
- [Product Narrative](/product/molecule-product-doc)
- [System Architecture](/architecture/architecture)
- [Comprehensive Technical Documentation](/architecture/molecule-technical-doc)
- [Memory Architecture](/architecture/memory)
- [Workspace Runtime](/agent-runtime/workspace-runtime)
- [Canvas UI](/frontend/canvas)
- [Platform API](/api-protocol/platform-api)

## Blog

- [Deploy AI Agents on Fly.io — or Any Cloud — with One Config Change](/blog/deploy-anywhere) *(2026-04-17)*
- [Give Your AI Agent a Real Browser: MCP + Chrome DevTools](/blog/browser-automation-ai-agents-mcp) *(2026-04-20)*
- [Give Your AI Agent a Git Repository: EnterOS + Cloudflare Artifacts](/blog/cloudflare-artifacts-molecule-ai) *(2026-04-21)*
- [One Canvas, Every Agent: Remote AI Agents and Fleet Visibility](/blog/remote-workspaces) *(2026-04-20)*
- [Skills Over Bundled Tools: Why Composable AI Beats Platform Primitives](/blog/skills-vs-bundled-tools-ai-agent-platforms) *(2026-04-21)*
