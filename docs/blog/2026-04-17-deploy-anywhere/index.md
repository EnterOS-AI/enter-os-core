---
title: "Deploy AI Agents on Fly.io — or Any Cloud — with One Config Change"
date: 2026-04-17
slug: deploy-anywhere
description: "Molecule AI supports fly.io agent deployment and control-plane provisioning. Switch backends with one env var — no agent code changes required."
tags: [platform, fly.io, deployment, infrastructure]
---

# Deploy AI Agents on Fly.io — or Any Cloud — with One Config Change

Your infrastructure choice just got decoupled from your agent platform choice. Molecule AI now ships three production-ready workspace backends — `docker`, `flyio`, and `controlplane` — and switching between them takes a single environment variable. Your agent code, model choices, and workspace topology stay exactly the same.

This post covers what shipped in [PR #501](https://git.moleculesai.app/molecule-ai/molecule-core/pull/501) (Fly Machines provisioner) and [PR #503](https://git.moleculesai.app/molecule-ai/molecule-core/pull/503) (control plane provisioner), and which backend fits your situation.

## Before: One Deployment Model for Every Use Case

Until this week, Molecule AI workspaces ran on one backend: Docker. That was the right default for self-hosters — no external dependencies, full control, works anywhere a Docker daemon runs. But it left two groups making a compromise they shouldn't have to:

- **Indie developers and small teams** wanted Fly.io's economics: pay-per-use compute, fast cold starts, scale to zero when nobody's working.
- **SaaS builders** needed structural credential isolation. A Fly API token sitting in the tenant layer is one misconfiguration away from a security incident — not a policy problem, an architecture problem.

Both groups were choosing between "use the platform" and "get the deployment model I need." That trade-off is gone.

## Run AI Agents on Fly: The Indie Dev Path

You're already on Fly. You have an account, a Fly app, and you're comfortable with Machines. You want Molecule AI workspaces to provision as Fly Machines — no separate Docker host, no idle infrastructure, just workspaces that appear when needed and disappear when they don't.

Set three environment variables on your tenant platform instance:

```bash
CONTAINER_BACKEND=flyio
FLY_API_TOKEN=<your-fly-deploy-token>
FLY_WORKSPACE_APP=<your-fly-app-name>

# Optional — defaults to ord
FLY_REGION=ord
```

When a workspace is created, the Fly provisioner:

1. Spins up a Fly Machine inside your `FLY_WORKSPACE_APP`
2. Injects workspace secrets and the platform registration URL as machine env vars
3. Selects the right GHCR image for the runtime (`hermes` → `ghcr.io/molecule-ai/workspace-hermes:latest`, and so on)
4. Applies tier-based resource limits — T2 at 512 MB / 1 vCPU, T3 at 2 GB / 2 vCPU, T4 at 4 GB / 4 vCPU
5. Issues a boot-time auth token so the workspace agent can register with the platform immediately

Your workspaces run as first-class Fly Machines. When they're idle, Fly handles the scale-down. Your bill reflects actual usage, not reserved capacity.

## Multi-Tenant Agent Provisioning Without Credential Sprawl

You're building a SaaS product on top of Molecule AI. Each customer gets a Molecule workspace. The problem: if every tenant platform instance carries a `FLY_API_TOKEN`, you've distributed cloud credentials across your tenants — structurally. Policy controls help, but they don't remove the credential from the attack surface.

`CONTAINER_BACKEND=controlplane` removes it entirely.

```
Canvas → Tenant Platform → Control Plane API → Fly Machines API
```

The tenant platform never holds a Fly token. It calls the Molecule control plane at `https://api.moleculesai.app` (overridable via `CP_PROVISION_URL` for staging environments), which holds Fly credentials and orchestrates workspace provisioning centrally.

For standard SaaS deployments, you don't configure this manually — the platform auto-detects the right backend:

- `MOLECULE_ORG_ID` set → SaaS tenant → **control plane provisioner activates automatically**
- `MOLECULE_ORG_ID` empty → self-hosted → **Docker provisioner, no change needed**

The right backend is the default for your context. For most SaaS builders: set `MOLECULE_ORG_ID` at tenant launch, and credential isolation is structural from day one.

## Self-Hosted vs Cloud AI Agents: Backend Comparison

| Backend | `CONTAINER_BACKEND` | Best for | Who holds cloud credentials |
|---|---|---|---|
| **Docker** | *(empty / default)* | Self-hosted, local dev | No external credentials needed |
| **Fly Machines** | `flyio` | Indie devs / small teams on Fly | `FLY_API_TOKEN` lives on the tenant |
| **Control Plane** | `controlplane` | SaaS builders, multi-tenant products | Fly token held by control plane only — never on tenant |

**Fly backend env vars** (for `CONTAINER_BACKEND=flyio`):

| Variable | Required | Default | What it does |
|---|---|---|---|
| `CONTAINER_BACKEND` | Yes | — | Activates the Fly provisioner |
| `FLY_API_TOKEN` | Yes | — | Fly deploy token |
| `FLY_WORKSPACE_APP` | Yes | — | Fly app that hosts workspace machines |
| `FLY_REGION` | No | `ord` | Region for new machines |

## Agent Orchestration in the Cloud: What Doesn't Change

Switching backends changes where workspaces run, not how they work. From any agent runtime's perspective — Hermes, Letta, or whatever you're orchestrating — the workspace is the workspace. Unchanged across all three backends:

- Agent registration and boot sequence
- Model routing and provider dispatch
- Workspace secrets injection
- The full platform API surface

No changes to agent code, tool definitions, or orchestration logic. Swap `CONTAINER_BACKEND`, redeploy, done.

## Multi-Agent Cloud Deployment: Choose Your Path

- **Self-hosting?** Leave `CONTAINER_BACKEND` unset. Docker is the default — nothing to configure.
- **On Fly, small team?** Set `CONTAINER_BACKEND=flyio` with `FLY_API_TOKEN` and `FLY_WORKSPACE_APP`. Workspaces become Fly Machines in your own Fly account.
- **Building a SaaS product on Molecule AI?** Set `MOLECULE_ORG_ID` at tenant launch. The control plane provisioner activates automatically. No Fly credentials on the tenant, ever.

**Pick your backend. Deploy your agents.**

→ [Quickstart: choose your deployment backend](/docs/quickstart)

**See also:** [Give Your AI Agent a Real Browser: MCP + Chrome DevTools](/blog/browser-automation-ai-agents-mcp) — wire Chrome DevTools Protocol into your Molecule AI workspace via MCP for production-grade browser automation.

---

*[PR #501](https://git.moleculesai.app/molecule-ai/molecule-core/pull/501) (Fly Machines provisioner) and [PR #503](https://git.moleculesai.app/molecule-ai/molecule-core/pull/503) (control plane provisioner) are both merged to `main`. Molecule AI is open source — contributions welcome.*
