---
title: "Give Your AI Agents Exactly One Key: Org-Scoped API Keys for Agentic Workflows"
date: 2026-04-22
slug: ai-agents-org-scoped-keys
description: "Org-scoped API keys solve the AI agent credential problem: full admin tokens are too powerful, workspace tokens are too narrow. Here's the model that works."
tags: [security, ai-agents, platform, api, enterprise]
---

# Give Your AI Agents Exactly One Key: Org-Scoped API Keys for Agentic Workflows

The credential problem for AI agents isn't unique — it's the same problem every service integration faces. But AI agents make it worse, because agents are dynamic in a way Zapier integrations and CI pipelines aren't.

An agent can spawn workspaces. It can dispatch tasks. It can modify secrets. It can read org-wide configuration. When you hand an agent an `ADMIN_TOKEN`, you're giving it all of that simultaneously, and you're giving it a credential that has no name, no revocation granularity, and no audit trail back to the agent that used it.

Org-scoped API keys fix this for agents the same way they fix it for every other integration — but with some agent-specific wrinkles worth calling out.

## The agent credential problem

The default path to making an agent productive looks like this:

```bash
ADMIN_TOKEN=sk-...
```

That one variable gives the agent everything. Create workspaces? Yes. Read all secrets across every workspace? Yes. Mint more tokens? Yes. Delete the org? In theory yes — in practice the platform probably guards that call, but nothing in the credential model stops it.

The three failure modes are specific to agents:

**Agents are dynamic.** A Zapier integration calls a fixed set of endpoints. An AI agent can call anything the tool interface exposes — which grows over time. A credential scoped to "what the agent needs today" stays correct for longer than one that gives everything.

**Agent behavior is emergent.** You tested the agent in dev. In production it hits an edge case and starts creating workspaces it shouldn't. With `ADMIN_TOKEN` you have no way to contain that — revoke the token and you take down everything. With org-scoped keys you revoke the one key the agent holds.

**Agents persist.** A CI pipeline runs for minutes. An agent runs for weeks or months. The longer a credential lives, the higher the probability it gets compromised, leaked in a log file, or copied into a repo that shouldn't have it.

## The right model: one key, named, scoped to the agent

The mental model for agent credentials:

```
1. Create a named org-scoped key for each agent
2. Give the agent only that key
3. Monitor what the key calls
4. Revoke if anything looks wrong
```

"Named" is the operational anchor. When you look at the audit log and see `org:keyId=ci-agent-prod_abc123` calling `/secrets/ws_prod_001`, you know exactly which agent made that call. When you look at the key listing in Canvas and see that same name, you know which agent to investigate if something goes wrong.

## The delegation chain

Here's something staging's enterprise-key-management post covers less directly: org-scoped keys can mint other org-scoped keys.

This matters for multi-agent architectures. If you have a supervisor agent that orchestrates sub-agents:

1. Supervisor gets `orchestrator-prod`
2. Sub-agents each get their own named key (`data-agent-prod`, `code-agent-prod`)
3. Supervisor can mint, monitor, and revoke sub-agent keys programmatically
4. The audit trail goes `orchestrator-prod` → `data-agent-prod` → individual API calls

If the supervisor is compromised, revoke one key. If a sub-agent is behaving unexpectedly, revoke its key independently. Neither action requires rotating the supervisor.

## Least privilege by default

Today, org-scoped keys are full-admin — they can do everything an `ADMIN_TOKEN` can do. The roadmap includes role scoping (admin / editor / read-only) and per-workspace bindings.

The goal: an agent gets exactly the access surface it needs. For a read-only monitoring agent, that's list and read on specific resources. For a workspace-provisioning agent, that's write on workspaces and nothing else.

Until role scoping ships: name your keys well, monitor their usage, and treat them as you would any other long-lived secret — with rotation schedules and revocation plans.

## Monitoring what your agents call

Once an agent is running on an org-scoped key, you monitor it the same way you'd monitor any long-lived service credential:

**In Canvas:** Settings → Org API Keys → [key name] → Activity Log shows recent calls for that key.

**Per-token activity logs via API** (planned): a structured API endpoint for querying an org-scoped key's call history — timestamp, endpoint, response code, duration — is on the roadmap. Until it ships, the Canvas Activity Log is the primary monitoring interface.

If a monitoring agent's call pattern changes — it suddenly starts calling `/workspaces POST` instead of read-only endpoints — that's a signal. Revoke the key, investigate, and re-issue with tighter scope if needed.

## The security properties that survive agent compromise

If an agent is compromised and an attacker gains access to its org-scoped key:

- The key is sha256-hashed server-side — the attacker gets a hash, not a usable token
- Revocation is immediate — one API call and the key stops working before the next heartbeat
- The attacker's calls are attributable — every request is labeled with the compromised key's prefix in the audit log
- No other integration is affected — Zapier's key, the CI pipeline's key, and the monitoring agent's key all continue working

Compare that to `ADMIN_TOKEN` compromise: everything is exposed, nothing is attributable, rotation requires coordinating downtime across every integration simultaneously.

## Get started

The org-scoped key system is live. Create your first key:

**In Canvas:** Settings → Org API Keys → New Key → name it after the agent it powers

**By API:**

```bash
curl -X POST https://acme.moleculesai.app/org/tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"name": "ci-agent-prod"}'
```

Store the returned plaintext token in your secret manager. Hand it to the agent. Monitor the key's usage in Settings → Org API Keys → [key name] → Activity Log.

*Org-scoped API keys shipped in PRs #1105, #1107, #1109, and #1110. Role scoping, per-workspace bindings, and per-token activity logs via API are on the roadmap.*
