---
title: "Container or Remote? How to Choose Your Agent Runtime in EnterOS"
date: 2026-04-20
slug: container-vs-remote
description: "Phase 30 ships remote workspaces. Phase 31 ships container workspaces. Here's how to choose between them — and when to use both in the same org."
tags: [platform, runtime, deployment, remote-agents, containers, decision-guide]
---

# Container or Remote? How to Choose Your Agent Runtime in EnterOS

One of the first decisions when you add an agent to a EnterOS org is: **where does it run?**

Before Phase 30, that question had one answer — a Docker container on the platform. Now it has two. And for most teams, that turns out to be a feature, not a complication. Here's how to think through it.

---

## The Two Runtimes

**Container (Docker)** — the agent runs inside a Docker container that the EnterOS platform provisions and manages. The platform controls the lifecycle: start, stop, restart, pause, resource limits, secrets injection.

**Remote (external)** — the agent runs wherever you want — your laptop, a cloud VM, an on-premises server, a third-party endpoint. The platform doesn't provision or manage the container. It registers the workspace, issues an auth token, and communicates via A2A over HTTPS.

The platform's canvas, registry, A2A proxy, audit trail, and lifecycle controls are identical for both. The difference is who manages the process.

---

## When to Use a Container

Container runtime is the right default when:

- **You want zero-infrastructure agent management.** The platform handles provisioning, boot, resource limits, health checks, and restarts. You write the agent; EnterOS handles the ops.
- **You need predictable resource allocation.** Tiers T1–T4 map to CPU/memory limits on the container. You control what the agent has access to.
- **You're running in a trusted environment.** All agents are on the same Docker network as the control plane. No external access required.
- **You want the simplest setup.** `runtime: langgraph` → platform provisions → agent is online. No tunnel, no public endpoint, no external networking.

Best for: production workloads, managed platforms, self-hosted deployments where Docker is already part of the infrastructure story.

---

## When to Use a Remote Agent

Remote runtime is the right choice when:

- **The agent is already running somewhere.** Your developer has an agent on their laptop. Your data pipeline is an existing Python process in AWS. Your enterprise has a legacy agent on an on-premises server. You don't want to containerize and redeploy — you want it on the canvas as-is.
- **You need agents across multiple networks or clouds.** PM on GCP, researcher on AWS, pipeline on an on-prem datacenter. Remote runtime means they all connect to the same platform without a shared network.
- **You need local filesystem access.** Container agents run in an isolated filesystem. A remote agent on your laptop can access local files, write to local directories, and integrate with local tools without Docker volume mounts.
- **You're debugging an agent in development.** Run the agent in your IDE with your full toolchain, point it at the platform, and see it on the canvas. No Docker layering between you and the agent's stdout.

Best for: cross-cloud orgs, developer laptops, on-premises deployments with data residency requirements, existing agent infrastructure you don't want to migrate.

---

## The Mixed-Fleet Pattern

The strongest use case for remote runtime isn't "all agents are remote." It's "some agents are remote, most are containers, all are on the same canvas."

```
Canvas
  ├── pm-agent      [CONTAINER — managed, GCP]      ← standard pill
  ├── researcher    [REMOTE — laptop]             ← purple badge, your MacBook
  ├── data-pipeline  [CONTAINER — managed, AWS]     ← standard pill
  └── legacy-agent  [REMOTE — on-prem]             ← purple badge, existing infra
```

The PM talks to the researcher and the data pipeline via A2A. The canvas shows all four as online workspaces with the same status indicators, activity logs, and chat tabs. The only difference is the badge.

This is the pattern Phase 30 enables: **one org, mixed fleet, single governance surface.**

---

## How to Decide

| Factor | Choose Container | Choose Remote |
|---|---|---|
| Infrastructure control | Platform-managed | Self-managed |
| Network | Platform Docker network | Public HTTPS |
| Lifecycle | Platform controls (start/stop/restart) | Agent controls (heartbeat loop) |
| Resource limits | Tier-based (T1–T4) | External to EnterOS |
| Setup complexity | One API call | ngrok / tunnel + registration |
| Best for | Production workloads | Cross-cloud, laptops, existing infra |

---

## One More Thing: You Can Change Your Mind

The `runtime` field is a deployment property, not a permanent identity. An agent that starts as a container can be replaced by a remote agent with the same workspace ID. An agent that starts as remote can be containerized later.

The canvas, the org hierarchy, the A2A relationships, and the audit trail all survive the transition. Where the process lives is a runtime concern — it doesn't change the workspace's role in the org.

→ [Remote Workspaces Guide →](/docs/guides/remote-workspaces.md)
→ [External Agent Registration →](/docs/guides/external-agent-registration.md)
→ [Phase 30 Announcement →](/docs/blog/2026-04-20-remote-workspaces)