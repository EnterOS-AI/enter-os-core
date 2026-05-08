---
title: "Phase 30: Run AI Agents Anywhere — Remote Workspaces is Now GA"
date: 2026-04-20
slug: remote-workspaces-ga
description: "Molecule AI's Phase 30 ships today. Agents can now run on your laptop, a different cloud, or an on-premises server — and appear on the canvas as first-class workspaces, side by side with your Docker agents."
tags: [launch, platform, remote-agents, federation, phase-30]
---

# Phase 30: Run AI Agents Anywhere — Remote Workspaces is Now GA

Your laptop is now a valid Molecule AI runtime.

Starting today, any Python agent — running on your machine, a cloud instance, an on-premises server, or a third-party endpoint — can register with a Molecule AI org, appear on the canvas, receive tasks from parent agents, and report status. The canvas doesn't care where the agent's process lives.

This is Phase 30: Remote Workspaces. It's generally available as of today.

---

## Before Phase 30: All Agents on One Network

Molecule AI has always let you run agents in Docker containers on the platform. That's great for self-hosting — fully managed, no external dependencies. But it meant every agent had to be on the same Docker network as the control plane.

That ruled out three real-world scenarios:

- **Developers running agents locally** — you want to debug an agent on your laptop, with your IDE, using your local filesystem, while it participates in the org
- **Cross-cloud deployments** — your PM runs on GCP, your researcher runs on AWS, your data pipeline runs on an on-premises server
- **Existing infrastructure** — you already have an agent. You don't want to containerize it and redeploy it. You just want it in the canvas

Phase 30 removes all three constraints.

---

## What Ships Today

Phase 30 is eight bounded improvements stacked into one coherent feature:

| | What it means for you |
|---|---|
| **Workspace auth tokens** | Every remote agent gets a cryptographic identity — a 256-bit bearer token minted at registration. No shared secrets, no guessing workspace IDs. |
| **Token-gated secrets pull** | Agents pull their API keys from the platform at boot via `GET /workspaces/:id/secrets/values`. No credentials baked into container images. Rotate a key in the UI, the agent picks it up on next pull. |
| **Plugin tarball download** | Remote agents install plugins by downloading a tarball from the platform, unpacking it, and loading it at runtime. No Docker exec required. |
| **State polling** | No WebSocket required from the agent side. Agents poll `GET /workspaces/:id/state` every 30 seconds to detect pause, resume, or delete — and react accordingly. |
| **A2A proxy with caller auth** | The platform proxies task dispatches to the agent's registered URL. Agents call back via the proxy too. Mutual bearer auth throughout. |
| **Sibling discovery + URL caching** | Agents discover peer workspaces via `GET /registry/:id/peers` and cache those URLs. They call siblings directly when reachable. |
| **Poll-based liveness** | Redis TTL with 90-second timeout. If the agent stops polling, the canvas shows it as offline. No Docker health check needed. |
| **Python SDK** | `molecule-sdk-python` ships `RemoteAgentClient` — a dependency-light Python client (only `requests`) that wraps all eight endpoints above. |

---

## How It Works

The registration flow has three steps. After that, the agent stays alive by heartbeat and reacts to platform commands.

**Step 1 — Create a workspace (admin side)**

```bash
curl -s -X POST https://acme.moleculesai.app/workspaces \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"researcher","runtime":"external","tier":2}'
# → {"id":"ws-abc123","status":"online","external":true}
```

`runtime: "external"` tells the platform not to provision a Docker container. The workspace row is created immediately.

**Step 2 — Register and authenticate**

```python
from molecule_agent import RemoteAgentClient

client = RemoteAgentClient(
    workspace_id="ws-abc123",
    platform_url="https://acme.moleculesai.app",
    agent_card={"name": "researcher", "skills": ["web-search"]},
)
client.register()   # receives + caches auth token
```

The `register()` call hits `POST /registry/register` with an admin token (one-time setup) and receives a workspace-scoped bearer token back. That token is cached to disk and used for all subsequent calls.

**Step 3 — Pull secrets, start the loop**

```python
secrets = client.pull_secrets()
# {"OPENAI_API_KEY": "sk-...", "MODEL_NAME": "gpt-4o"}

client.run_heartbeat_loop(
    task_supplier=lambda: {
        "current_task": "idle",
        "active_tasks": 0,
    }
)
```

The `run_heartbeat_loop()` method runs a concurrent heartbeat + state-polling loop in the background. It exits cleanly when the platform reports the workspace paused or deleted. In between, the agent can receive A2A task dispatches routed by the platform.

---

## The Canvas Doesn't Know the Difference

Here's what you see on the canvas once the remote agent is registered:

- A workspace node with the agent's name and skills list
- A **purple REMOTE badge** — the only visual signal that this agent isn't a Docker container
- Status: online, degraded, or offline — same indicators as any other workspace
- Current task, active task count, error rate — all surfaced in real time
- A chat tab, an activity log, a terminal tab — identical to the Docker workspaces

The deployment location is a badge. Everything else is the same.

---

## One Org, Multiple Clouds

The scenario Phase 30 enables:

```
Canvas (your browser)
    │
    ├── pm-agent     [DOCKER — GCP]       ← standard runtime pill
    ├── researcher   [REMOTE — laptop]     ← purple badge, your MacBook
    ├── pipeline    [REMOTE — AWS EC2]   ← purple badge, your data team
    └── on-prem     [REMOTE — datacenter] ← purple badge, your legacy system
```

All four agents receive tasks from the PM via A2A. All four appear on the same canvas. The platform A2A proxy handles the routing — no VPN, no shared Docker network, no special firewall rules on the platform.

---

## What's Not in Phase 30

Phase 30 handles the single-hop case: agents behind NAT need the platform proxy to reach them, but the proxy can only initiate calls in one direction. Two agents both behind NAT can't call each other directly without a relay. That's Phase 31.

Also out of scope: mutual TLS from the agent side — agents trust the platform URL in their environment. A future iteration will add platform-identity verification for deployments where that matters.

---

## Try It

The fastest path:

```bash
pip install molecule-ai-sdk
```

Then follow the [quick-start guide](/docs/guides/remote-workspaces.md).

Or run the annotated example directly:

```bash
git clone https://git.moleculesai.app/molecule-ai/molecule-sdk-python
cd molecule-sdk-python/examples/remote-agent
# Create workspace with runtime:external, grab the ID, then:
WORKSPACE_ID=<your-id> PLATFORM_URL=https://acme.moleculesai.app python3 run.py
```

The agent appears on the canvas within seconds.

---

→ [Remote Workspaces Guide →](/docs/guides/remote-workspaces.md)
→ [External Agent Registration Reference →](/docs/guides/external-agent-registration.md)
→ [molecule-sdk-python →](https://git.moleculesai.app/molecule-ai/molecule-sdk-python)

*Phase 30 shipped in PRs #1075–#1083 and #1085–#1100 on `molecule-core`.*
