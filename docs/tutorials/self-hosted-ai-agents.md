---
title: "Self-Hosted AI Agents: EnterOS on Docker, Fly Machines, or Bare Metal"
date: 2026-04-21
slug: self-hosted-ai-agents-molecule-ai
description: "EnterOS runs anywhere — Docker containers, Fly Machines, or bare metal. This guide covers all three deployment models, when to use each, and how to choose for your infra constraints."
tags: [self-hosted, deployment, Docker, Fly Machines, tutorial, infrastructure]
---

# Self-Hosted AI Agents: EnterOS on Docker, Fly Machines, or Bare Metal

EnterOS is designed to run wherever your agents need to run. Whether you're deploying on a single VPS, distributing agents across cloud VMs, or running on hardware that can't be containerized, EnterOS has a path that fits.

This guide covers the three deployment models — Docker containers, Fly Machines, and bare metal — with concrete use cases and configuration for each.

## Choosing a Deployment Model

| Model | Best for | Provisioning | Cold start | Isolation |
|---|---|---|---|---|
| **Docker** | Single-host, dev/test, one-box production | Manual (`docker run`) or Docker Compose | ~15–30s | Shared kernel |
| **Fly Machines** | Multi-region, auto-scaling, per-tenant isolation | Platform API (`POST /workspaces`) | <1s | Firecracker microVM |
| **Bare metal / remote** | On-prem, laptops, CI/CD, air-gapped | Manual registration | N/A | None (your infra) |

All three models use the same agent runtime and A2A protocol. The differences are in how agents are provisioned, how secrets are delivered, and how liveness is tracked.

## Model 1: Docker Containers

The default deployment. The platform manages container lifecycle — you get workspace provisioning, secret injection, and platform heartbeat handling out of the box.

**How it works:**

```
POST /workspaces → platform runs `docker run ghcr.io/molecule-ai/workspace-<runtime>`
```

The platform injects `WORKSPACE_ID`, `PLATFORM_URL`, and workspace secrets as environment variables before the container starts. The agent inside registers itself via `POST /registry/register` on boot, and the platform sends health checks through Docker's health subsystem.

**Configuration:**

```bash
# Your platform's .env
CONTAINER_BACKEND=docker        # default
PLATFORM_URL=https://your-host   # reachable from containers
WORKSPACE_IMAGE_PREFIX=ghcr.io/molecule-ai/workspace-

# Optional: restrict which runtimes are allowed
ALLOWED_RUNTIMES=hermes,claude-code,langgraph

# For CI on the same host:
WORKSPACE_NETWORK=host          # use host network for zero-config networking
```

**When to choose Docker:**
- Single-host deployments (VPS, single EC2)
- Dev/test environments where isolation is less critical
- Teams that already have Docker infra
- You want the platform to handle provisioning automatically

## Model 2: Fly Machines

Fly Machines are Firecracker microVMs managed by the Fly.io API. They offer sub-second cold starts, multi-region placement, and hardware-level isolation between workspaces — without the shared kernel risk of Docker.

**How it works:**

```
POST /workspaces → platform calls Fly API → Fly Machine boots workspace image
```

The platform talks to Fly Machines API directly, passing workspace config and secrets as environment variables. The same agent runtime runs inside the Machine.

**Configuration:**

```bash
# Your platform's .env
CONTAINER_BACKEND=flyio
FLY_API_TOKEN=<fly-deploy-token>          # flyctl tokens create deploy
FLY_WORKSPACE_APP=my-molecule-workspaces   # Fly app for workspace Machines
FLY_REGION=ord                             # default region (or leave for auto)
```

**Resource tiers** (configured per workspace via `"tier": 2|3|4`):

| Tier | RAM | CPUs | Use case |
|---|---|---|---|
| T2 | 512 MB | 1 | Light workers, eval agents |
| T3 | 2 GB | 2 | General-purpose orchestrators |
| T4 | 4 GB | 4 | Heavy inference, long-context tasks |

**Setting tier on creation:**

```bash
curl -X POST https://platform.moleculesai.app/workspaces \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -d '{
    "name": "eu-worker",
    "runtime": "hermes",
    "tier": 3,
    "metadata": { "region": "ams" }
  }'
```

Fly picks the closest region to the `region` metadata field, or defaults to `FLY_REGION`.

**When to choose Fly Machines:**
- Multi-tenant SaaS where workspace isolation matters (Firecracker = no shared kernel)
- Sub-second cold starts matter (queue workers, on-demand workers)
- You want multi-region agent distribution without managing your own fleet
- You want pay-per-second billing instead of always-on VMs

**See:** [Provision Workspaces on Fly Machines](/docs/tutorials/fly-machines-provisioner) — full walkthrough with `flyctl` commands.

## Model 3: Bare Metal / Remote Agents

For agents that can't be containerized — on-prem hardware, laptops, CI/CD runners — EnterOS ships a registration API. Your agent registers with the platform, receives a bearer token, and maintains canvas visibility via a heartbeat loop.

This is the most flexible model. The platform doesn't manage the agent's lifecycle — it just provides a coordination layer (fleet visibility, secret management, A2A routing).

**How it works:**

1. Create an external workspace via the API
2. Register the agent and receive a one-time bearer token
3. The agent starts a 30-second heartbeat loop
4. The canvas shows the agent with a **REMOTE** badge

**Step-by-step registration:**

```bash
ADMIN_TOKEN="your-admin-token"
PLATFORM_URL="https://platform.moleculesai.app"
AGENT_URL="https://your-agent.example.com"  # must be HTTPS and reachable

# 1. Create external workspace
WORKSPACE=$(curl -s -X POST "${PLATFORM_URL}/workspaces" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "{
    \"name\": \"CI Agent\",
    \"runtime\": \"external\",
    \"external\": true,
    \"url\": \"${AGENT_URL}\"
  }")
WORKSPACE_ID=$(echo $WORKSPACE | jq -r '.id')

# 2. Register and receive bearer token
REG=$(curl -s -X POST "${PLATFORM_URL}/registry/register" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "{
    \"id\": \"${WORKSPACE_ID}\",
    \"url\": \"${AGENT_URL}\",
    \"agent_card\": {\"name\": \"CI Agent\", \"runtime\": \"external\"}
  }")
AUTH_TOKEN=$(echo $REG | jq -r '.auth_token')

# 3. Heartbeat every 30s
curl -s -X POST "${PLATFORM_URL}/registry/heartbeat" \
  -H "Authorization: Bearer ${AUTH_TOKEN}" \
  -d "{\"workspace_id\": \"${WORKSPACE_ID}\"}"
```

**Agent-side heartbeat (Python):**

```python
import requests, time, threading

AUTH_TOKEN = "<from registration>"
WORKSPACE_ID = "<from registration>"
PLATFORM_URL = "https://platform.moleculesai.app"

def heartbeat_loop():
    while True:
        requests.post(
            f"{PLATFORM_URL}/registry/heartbeat",
            headers={"Authorization": f"Bearer {AUTH_TOKEN}"},
            json={"workspace_id": WORKSPACE_ID},
        )
        time.sleep(30)

threading.Thread(target=heartbeat_loop, daemon=True).start()
```

**For agents behind NAT or firewall:**

The platform needs to reach `AGENT_URL` for inbound A2A messages. Expose your agent with a tunnel:

```bash
# Cloudflare Tunnel (recommended for production)
cloudflared tunnel --url http://localhost:8080

# Or ngrok (quick dev/test)
ngrok http 8080
```

Copy the public URL and use it as `AGENT_URL` in the registration call.

**When to choose bare metal / remote:**
- On-prem hardware that can't be containerized
- Laptops or workstations where Docker isn't practical
- CI/CD runners (GitHub Actions, Jenkins) that spin up per job
- Air-gapped networks
- Any scenario where the platform shouldn't own the agent's lifecycle

**See:** [Register a Remote Agent on EnterOS](/docs/tutorials/register-remote-agent) — full tutorial with CI/CD examples and minimal Python agent.

## Comparing the Three Models

| | Docker | Fly Machines | Bare Metal / Remote |
|---|---|---|---|
| Provisioning | Platform (`docker run`) | Platform (Fly API) | Manual via API |
| Secrets | Injected as env vars at boot | Injected as env vars at boot | Pulled on demand via API |
| Heartbeat | Platform (Docker health) | Platform (health check) | Agent sends every 30s |
| Canvas badge | None (standard) | None (standard) | Purple REMOTE |
| Cold start | ~15–30s | <1s | N/A |
| Isolation | Shared kernel | Hardware (Firecracker) | None (your infra) |
| Lifecycle managed | ✅ Yes | ✅ Yes | ❌ No (your code) |
| Works with existing infra | ❌ No | ❌ No | ✅ Yes |
| Best for | Single-host, dev/test | Multi-region, SaaS | On-prem, CI/CD, laptops |

## Mixing Deployment Models

You can combine models in the same organization. A typical production setup might look like:

- **CI/CD agents** → bare metal / remote (register per pipeline run)
- **Queue workers** → Fly Machines (auto-scale, sub-second spin-up)
- **Staging / dev** → Docker on a single VPS
- **Long-running services** → Fly Machines in the region closest to your users

All of these show up on the same canvas, visible to the same orchestrator, reachable via A2A. The deployment model is an implementation detail — the coordination layer is uniform.

## Which Model Should You Use?

**Start with Docker** if you're evaluating EnterOS or running on a single host. It's the lowest friction path.

**Move to Fly Machines** when you need multi-region, per-tenant isolation, or sub-second scaling. The platform handles Fly provisioning automatically — just set env vars and `POST /workspaces`.

**Add remote / bare metal** when you have agents that can't live in either container model — on-prem hardware, CI/CD runners, or air-gapped networks. Register them via API and they join the fleet alongside container-provisioned agents.

→ [Register a Remote Agent](/docs/tutorials/register-remote-agent) — bare metal tutorial
→ [Provision Workspaces on Fly Machines](/docs/tutorials/fly-machines-provisioner) — Fly Machines walkthrough
→ [Platform API Reference](/docs/api-reference) — full endpoint documentation

---
*EnterOS is open source. All three deployment models are documented in `docs/tutorials/` on `main`.*