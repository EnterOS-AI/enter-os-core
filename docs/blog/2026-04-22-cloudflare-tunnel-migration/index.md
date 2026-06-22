---
title: "Phase 33: From Cloudflare Tunnel to Direct Connect — How EnterOS Agent Workspaces Get Their Own IP"
date: 2026-04-22
slug: cloudflare-tunnel-migration
description: "Phase 33 replaces Cloudflare Tunnel with direct-connect agent workspaces that get their own public IPs. Here's what changed, why, and what it means for your deployment."
tags: [platform, infrastructure, cloud, deployment]
---

# Phase 33: From Cloudflare Tunnel to Direct Connect — How EnterOS Agent Workspaces Get Their Own IP

In Phase 33, EnterOS changes how cloud-hosted agent workspaces connect to the platform. Previously, every workspace connected outbound through a Cloudflare Tunnel — a lightweight daemon that maintained a persistent connection to Cloudflare's edge, routing traffic through their network. Starting today, workspaces provisioned in your cloud account get their own public IP addresses and connect directly, with no tunnel in the path.

This post covers what changed architecturally, why we made the change, and what operators and developers need to know.

## What was there before: the Cloudflare Tunnel model

Cloudflare Tunnel (formerly `cloudflared`) worked like this:

1. A lightweight daemon ran inside each agent workspace container
2. It maintained an outbound-only WebSocket connection to a Cloudflare edge node
3. External traffic (your browser, API calls, CLI commands) hit a Cloudflare-assigned hostname (`*.trydirect.io` or a custom domain via Cloudflare)
4. Cloudflare routed that traffic through the tunnel WebSocket to the workspace

This was elegant for one specific constraint: **no inbound firewall rules required**. The workspace container opened only an outbound connection. Everything else was handled at Cloudflare's edge. For development environments and scenarios where you can't modify network security groups, this was a valid tradeoff.

The tradeoff became less acceptable at scale:

- **Latency**: every request from the platform to the workspace traveled through Cloudflare's network — extra hops, extra latency
- **Bandwidth costs**: Cloudflare metered tunnel egress; at agent-fleet scale this compounded
- **Single dependency**: if Cloudflare had an outage, every agent workspace lost its connection path simultaneously
- **No direct diagnostics**: you couldn't `curl` a workspace's IP directly or run network checks without the tunnel path

For teams running production agent fleets, these weren't hypothetical concerns.

## What's different now: public IP per workspace

Phase 33 provisions each workspace with its own public IP address from the VPC's public subnet. The connection model:

```
Your browser / API client
        │
        ▼
   Platform API (api.moleculesai.app)
        │  platform knows workspace IP from provisioning
        ▼
   AWS security group: platform-controlled inbound rules
        │  port 443 (WebSocket), authenticated by platform JWT
        ▼
   Agent workspace — public IP, direct WebSocket
```

The platform still handles auth and routing. But the data path no longer goes through Cloudflare's tunnel network — it's a direct TCP connection from client to workspace.

What changes for you:

| | Cloudflare Tunnel (before) | Direct Connect (now) |
|---|---|---|
| Workspace gets | Cloudflare-assigned hostname | Public IP from your VPC |
| Inbound connection | Outbound tunnel WebSocket only | Direct WebSocket on :443 |
| Firewall config | None required | Security group rules managed by platform |
| Latency | Extra Cloudflare hop | Direct — ~20–40ms reduction depending on region |
| Platform dependency | Cloudflare required for connectivity | Platform API still required for auth/routing; workspace IP works for direct curl |
| Debugging | Must go through tunnel | `curl https://<workspace-ip>` works directly |

## What operators need to do

If you already have a CP-managed workspace in your AWS account (provisioned via the `controlplane` backend with `MOLECULE_ORG_ID` set), Phase 33 transitions automatically. The platform manages the security group rules, so no manual changes are required.

**New provisioners:** when you create a CP-managed workspace, the platform now assigns a public IP from the workspace subnet. This is automatic — the provisioning flow is the same, just with a different network configuration on the backend.

**Existing self-hosted or Fly.io workspaces:** no change. Those backends don't use the CP provisioner path and were never on Cloudflare Tunnel in the same way.

**If you have a custom VPC configuration:** the platform expects a workspace subnet with outbound internet access (for `pip install`, model API calls, etc.) and a security group that the platform can manage. If you've locked down your security groups to deny all inbound from the platform's IP ranges, you may need to allow port 443 from the platform CIDR. Check `docs.molecule.ai/infra/network-requirements` for the current allowlist.

## What developers need to know

From an agent runtime perspective — nothing changes. Your code talks to the platform API, registers workspaces, receives task dispatch, and runs tools. The transport layer is different but the API contract is identical.

Specific things that do change:

- **Direct workspace access**: if your code or tooling needs to reach a running workspace directly (for monitoring, log scraping, port-forwarding), you can now use its public IP instead of going through the platform proxy
- **WebSocket path**: the workspace still opens a WebSocket to the platform on boot. That connection is now outbound from the workspace's public IP to the platform — same direction as before, different path
- **CI/CD and health checks**: scripts that hit workspace health endpoints can use the public IP directly; no tunnel hostname required

## Security model

The security group rules are managed by the platform, not operator-configured. This is intentional — it means the platform can enforce:

- Port 443 only (no other inbound ports)
- TLS required on all connections
- JWT validation before any workspace data is served

What it doesn't do: the platform doesn't manage your VPC-level security groups beyond the workspace-specific one. If your VPC has overly restrictive route tables or NAT-only egress for the workspace subnet, model API calls from the agent may fail. Ensure your workspace subnet has both inbound 443 from the platform and outbound 443/443 to model provider endpoints.

## When this ships

Phase 33 is rolling out to all new CP-managed workspace provisions starting 2026-04-22. Existing workspaces will migrate on their next restart cycle — the platform handles this automatically during normal workspace rotation.

If you have questions or hit issues during migration, the runbook is at `docs.molecule.ai/infra/cloudflare-tunnel-migration`.

---

*Phase 33 is part of the EnterOS infrastructure hardening track. For the full roadmap, see `docs.molecule.ai/roadmap`.*