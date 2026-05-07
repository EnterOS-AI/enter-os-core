---
title: "Introducing Remote Workspaces: Your Agent Fleet, Everywhere It Runs"
date: 2026-04-22
slug: remote-workspaces
description: "Molecule AI Phase 30 ships today. Connect any AI agent — wherever it runs — to your fleet canvas with full A2A collaboration and enterprise-grade auth, without moving a single agent."
tags: [platform, phase-30, external-agents, fleet-management, a2a, mcp]
canonicalUrl: "https://docs.molecule.ai/blog/remote-workspaces"
---

<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "TechArticle",
  "headline": "Introducing Remote Workspaces: Your Agent Fleet, Everywhere It Runs",
  "description": "Molecule AI Phase 30 ships Remote Workspaces — connect any AI agent to your fleet canvas with full A2A collaboration and enterprise-grade per-workspace bearer tokens, without moving a single agent.",
  "datePublished": "2026-04-22",
  "author": {
    "@type": "Organization",
    "name": "Molecule AI",
    "url": "https://molecule.ai"
  },
  "publisher": {
    "@type": "Organization",
    "name": "Molecule AI",
    "logo": {
      "@type": "ImageObject",
      "url": "https://molecule.ai/logo.png"
    }
  },
  "about": {
    "@type": "Thing",
    "name": "AI Agent Fleet Management",
    "description": "Managing AI agents running across multiple cloud providers, on-premises infrastructure, and SaaS platforms through a unified canvas interface with A2A protocol support."
  },
  "keywords": [
    "remote workspaces AI",
    "heterogeneous fleet visibility",
    "per-workspace bearer tokens",
    "AI agent fleet management",
    "multi-tenant AI agents",
    "A2A protocol external agents",
    "external AI agent registration",
    "AI agent orchestration across clouds"
  ],
  " proficiencyLevel": "Expert",
  "genre": ["technical documentation", "product announcement"],
  "sameAs": [
    "https://git.moleculesai.app/molecule-ai/molecule-core",
    "https://molecule.ai"
  ]
}
</script>

# Introducing Remote Workspaces: Your Agent Fleet, Everywhere It Runs

Your AI agents are scattered across AWS, GCP, a data center in Virginia, and a SaaS tool you integrate with via webhook. They're all doing real work. They need to talk to each other.

But right now, they're invisible to each other — and invisible to you.

Most agent platforms would ask you to move everything into their runtime. Re-architect your infrastructure. Change your deployment. Accept a migration tax before you've even evaluated whether the product works.

**Molecule AI Phase 30 changes that.** Today we're shipping external agent registration — a way for any AI agent, running anywhere, to join your Molecule AI fleet with full feature parity: the canvas, the A2A protocol, and per-workspace auth isolation.

No re-deploy. No VPN. No separate dashboard.

---

## The Buyer's Problem, in Their Own Words

> "Our agents need to talk to each other even when they're in different clouds. And they need to be visible in the same place. That's the product we can't find today."

This is the quote we kept coming back to as we designed Phase 30 — because it's not a technical complaint. It's an operational one. The platform you're using today doesn't have a real answer for it.

Two specific failure modes emerge from this:

**Visibility failure.** Agents running outside the platform's Docker network don't appear on your canvas. You lose the ability to see fleet-wide status, hierarchy, and active tasks in one view — let alone achieve **heterogeneous fleet visibility** across AWS, GCP, on-prem, and SaaS tools simultaneously. Instead you get a spreadsheet, a custom dashboard, or just mental models.

**Communication failure.** Agents on different clouds or on-prem can't send each other messages through the platform without VPN tunnels, manual API stitching, or custom proxies. The "federation" problem is real and unsolved in most stacks.

Phase 30 addresses both directly.

---

## What Phase 30 Ships

### External Agent Registration

An **external agent** is any AI agent that runs outside the Molecule AI platform's Docker network — on your own servers, a different cloud account, on-prem hardware, or as a SaaS bot — but participates in the canvas, A2A protocol, and auth model as a first-class workspace.

The registration flow is intentionally minimal. Register, heartbeat, respond to A2A messages. The agent logic stays where it is.

**Step 1 — Create the workspace:**

```bash
curl -X POST http://localhost:8080/workspaces \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <admin-token>" \
  -d '{
    "name": "On-prem Research Agent",
    "role": "researcher",
    "runtime": "external",
    "external": true,
    "url": "https://research.internal.example.com",
    "tier": 2
  }'
```

**Step 2 — Register with the platform:**

```bash
curl -X POST http://localhost:8080/registry/register \
  -H "Content-Type: application/json" \
  -d '{
    "id": "<workspace-id>",
    "url": "https://research.internal.example.com",
    "agent_card": {
      "name": "On-prem Research Agent",
      "description": "Handles research tasks and summarization",
      "skills": ["research", "summarization", "analysis"],
      "runtime": "external"
    }
  }'
```

The response includes your `auth_token` — shown once, store it in your secrets manager. Every subsequent call requires this token plus the `X-Workspace-ID` header.

**Step 3 — Heartbeat every 30 seconds:**

```bash
curl -X POST http://localhost:8080/registry/heartbeat \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth_token>" \
  -d '{
    "workspace_id": "<workspace-id>",
    "error_rate": 0.0,
    "active_tasks": 1,
    "current_task": "Summarizing Q1 deployment metrics",
    "uptime_seconds": 3600
  }'
```

The full Python and Node.js reference implementations — both under 100 lines — are in [the external agent registration guide](/docs/guides/external-agent-registration).

---

### One Canvas for the Entire Fleet

External agents appear on the canvas with a purple **REMOTE** badge — same real-time status, same hierarchy, same chat panel as Docker-provisioned agents. There is no separate view.

Your entire fleet, one canvas:

```
┌─────────────────────────────────────────────────────┐
│  TEAM: Deployment Orchestrator          [T3 badge]  │
│                                                     │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────┐ │
│  │ LANGGRAPH    │  │ CLAUDE-CODE │  │ ● REMOTE  │ │
│  │ [online]     │  │ [degraded]  │  │ [online]  │ │
│  │ 2 tasks      │  │ 1 task      │  │ 1 task    │ │
│  └──────────────┘  └──────────────┘  └───────────┘ │
│                                                     │
└─────────────────────────────────────────────────────┘
```

The REMOTE badge is a first-class citizen, not an afterthought. It shows active tasks, current task description, uptime, and error rate — identical information to Docker-provisioned agents.

---

### Cross-Cloud A2A Without VPN

The platform's A2A proxy handles message routing between agents regardless of where they run. Agents only need two things:

1. A publicly reachable HTTPS endpoint for incoming A2A messages (no inbound ports opened on your network)
2. Outbound HTTPS access to the platform API

An agent on AWS can send a task to an agent on GCP via the platform proxy — neither agent needs to know the other's cloud environment. The `CanCommunicate` rules (siblings, parent-child) are enforced at the proxy layer, so the same access control applies as if both agents ran in Docker.

```bash
curl -X POST http://localhost:8080/workspaces/<target-id>/a2a \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth_token>" \
  -H "X-Workspace-ID: <your-workspace-id>" \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "parts": [{"type": "text", "text": "Get the latest deployment status"}]
      },
      "metadata": {"source": "agent"}
    },
    "id": "req-456"
  }'
```

No VPN. No VPC peering. No firewall rules between clouds.

---

## The Security Model: Auth Isolation as Protocol

Security is the question every enterprise buyer asks first. We built Phase 30.1 (per-workspace bearer tokens) and Phase 30.6 (`X-Workspace-ID` validation) specifically to answer it structurally, not as a policy checkbox — because per-workspace bearer tokens are only as strong as the enforcement layer on every authenticated route.

**How auth works:**

Every authenticated route requires two things simultaneously:
1. A valid 256-bit bearer token issued at first registration
2. An `X-Workspace-ID` header matching the token's bound workspace

Workspace A's token cannot hit Workspace B's routes — not because of a policy enforcement check, but because the `X-Workspace-ID` must match at every authenticated endpoint. The protocol enforces it, not a rule that could be misconfigured.

**Token security:**

The platform stores only the SHA-256 hash of each token. The raw token is returned once, at first registration, and cannot be recovered. If lost, the workspace must be deleted and re-created.

**For multi-tenant platforms:**

Per-workspace tokens mean each tenant's agents are isolated from each other — structurally, not by policy. This is the architecture SaaS builders need for multi-tenant agent products without distributing cloud credentials to tenant instances.

---

## Use Cases

### Hybrid Cloud

Agents running on AWS (your data science team), GCP (your infrastructure team), and Azure (a partner integration) all need to collaborate on a shared deployment pipeline. Phase 30's A2A proxy routes messages between them without VPC peering or VPN tunnels. The canvas shows the full deployment team — all three clouds, one canvas.

### On-Prem Agents

Your security team runs agents on on-prem hardware that cannot be containerized by the platform. Those agents register externally, appear on the canvas alongside your cloud agents, and can receive tasks from and send results to the rest of the fleet — without exposing any on-prem ports to the internet.

### SaaS Integrations

A third-party service exposes an A2A-compatible HTTP endpoint. That SaaS agent registers with your Molecule AI org, appears in the canvas as a REMOTE agent, and participates in your agent workflows — without a custom webhook per vendor.

---

## What's the Same

Switching to Phase 30 external registration changes **where** workspaces register, not **how** they work:

- Agent registration and boot sequence — unchanged
- Model routing and provider dispatch — unchanged
- A2A message format and protocol — unchanged (open JSON-RPC A2A)
- Workspace hierarchy and communication rules (`CanCommunicate`) — unchanged
- Canvas feature set — unchanged; remote agents get identical treatment

Your agent's code, model choices, tool definitions, and orchestration logic all stay exactly the same.

---

## Extend the Fleet: Browser Automation with MCP

One natural extension of a heterogeneous agent fleet is giving those agents tool access — browser automation, API integrations, codebase browsing — without moving them into the platform's runtime.

Molecule AI's MCP server (`@molecule-ai/mcp-server`) exposes platform tools for workspace management, file access, secrets, browser automation via the Chrome DevTools protocol, and more. Install it in one line:

```bash
npx @molecule-ai/mcp-server
```

Configure it in your project's `.mcp.json` and any AI agent (Claude Code, Cursor, etc.) can manage workspaces, send A2A messages, and run browser automation tasks through the platform — inside the same fleet context that Phase 30 makes possible.

→ [MCP Server Setup Guide](/docs/guides/mcp-server-setup) — full tool reference and configuration

---

## Get Started

→ [External Agent Registration Guide](/docs/guides/external-agent-registration) — full step-by-step with Python and Node.js reference implementations

→ [GitHub: molecule-core](https://git.moleculesai.app/molecule-ai/molecule-core) — source and issues

→ [Phase 30 Launch Thread on X](https://x.com) — follow for updates

---

*Phase 30 external agent registration is available today. Molecule AI is open source — contributions welcome.*
