---
title: "Org-Scoped API Keys: Enterprise Key Management for Multi-Agent Teams"
date: 2026-04-21
slug: org-scoped-api-keys
description: "Named, revocable, audit-trail-enabled tokens for every integration in your organization. Replace shared ADMIN_TOKEN with org-level keys that rotate without downtime and trace every call back to the key that made it."
tags: [security, enterprise, API-keys, multi-agent, audit]
---

# Org-Scoped API Keys: Enterprise Key Management for Multi-Agent Teams

When your engineering team scales from two agents to twenty, the last thing you want is a single `ADMIN_TOKEN` hardcoded in your environment. It's a single point of failure, impossible to rotate without downtime, and impossible to audit. Today's launch changes that.

EnterOS is rolling out **org-scoped API keys** — named, revocable, audit-trail-enabled tokens that live at the organization level and reach every workspace in your org without breaking the security model.

## What Are Org-Scoped API Keys?

Org-scoped API keys are long-lived credentials minted at the organization level via the Canvas UI or the REST API. Each key has:

- A **display name** you choose at creation time (e.g., `ci-deploy-bot`, `devops-rev-proxy`)
- A **sha256 hash** stored server-side — the plaintext is shown once and never again
- A **prefix** (first 8 chars) visible in listings so you can identify keys without exposing secrets
- A **created-by** field that tracks provenance in the audit trail
- **Immediate revocation** — drop a key and it stops being accepted on the very next request

The keys work across all workspaces in your org — not just admin-surface endpoints, but also per-workspace sub-routes like `/workspaces/:id/channels` and `/workspaces/:id/secrets`.

## The `ADMIN_TOKEN` Problem

A single env-var token works for prototypes. For production multi-agent systems it creates three compounding risks:

1. **Rotation requires downtime.** You can't rotate a token used by ten agents simultaneously. You rotate, or you don't — and both choices are bad.
2. **No attribution.** When something calls your API, you have no idea which agent or integration is responsible.
3. **No compartmentalization.** One compromised token compromises everything.

Org-scoped keys give each integration its own credential with its own identity. The table below summarizes the difference:

| Capability | Shared `ADMIN_TOKEN` | Org-Scoped Keys |
|---|---|---|
| Rotate without downtime | ❌ | ✅ |
| Identify caller per request | ❌ | ✅ |
| Revoke a single integration | ❌ | ✅ |
| Use on workspace sub-routes | ❌ | ✅ |
| Full audit trail with attribution | Partial | ✅ |

## How to Create and Revoke Keys

### Mint a key via API

```bash
curl -X POST https://platform.moleculesai.app/org/tokens \
  -H "Authorization: Bearer <your-session-token>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "ci-deploy-bot"
  }'
```

The response contains the plaintext token — shown exactly once. Store it immediately; the platform never stores or retrieves the plaintext, only the SHA-256 hash:

```json
{
  "id": "tok_01HXYZ...",
  "name": "ci-deploy-bot",
  "prefix": "mole_a1b2",
  "auth_token": "eXzKpL9...",
  "warning": "copy this token now; it will not be shown again"
}
```

### List active keys

```bash
curl https://platform.moleculesai.app/org/tokens \
  -H "Authorization: Bearer <your-session-token>"
```

Returns key IDs, names, prefixes, and creation timestamps — no plaintext.

### Revoke a key immediately

```bash
curl -X DELETE https://platform.moleculesai.app/org/tokens/tok_01HXYZ... \
  -H "Authorization: Bearer <your-session-token>"
```

The key stops being accepted on the very next request. No grace period, no cooldown.

## Audit Trail and Attribution

Every request authenticated with an org API key carries the key's prefix in the audit log:

```
org-token:mole_a1b2 POST /workspaces/ws_abc123/channels 200 12ms
org-token:mole_a1b2 GET /workspaces/ws_abc123/secrets 200 3ms
```

When combined with the `created_by` field stored at mint time, you get full provenance: which admin created this key, when, and what it has been calling. If a CI pipeline key is compromised, revoke it in one API call and know exactly which calls it made.

## Key Use Cases

### Team API keys

Give each team its own named key. The `devops-rev-proxy` key only talks to the observability stack; the `data-pipeline` key only accesses workspaces running the data pipeline agent. If one key is compromised, revoke it without touching the others.

### Service accounts

Long-running integrations — CI pipelines, external monitoring tools, backup scripts — get their own credential scoped to exactly the endpoints they need. Rotate on a schedule without coordinating downtime with other integrations.

### Key rotation without downtime

When you need to rotate a key, mint a new one, update your integration, and revoke the old one. Both keys are valid simultaneously during the window when you're updating the integration — zero downtime, full audit trail.

## Authentication Tier Reference

Org API keys sit in a defined priority hierarchy:

| Tier | Auth method | Use case |
|---|---|---|
| 0 | Lazy bootstrap | Only active when no org tokens and no `ADMIN_TOKEN` exist |
| 1 | WorkOS session | Human users authenticated via the Canvas |
| 2a | Org API token | New org-scoped keys — primary path for service integrations |
| 2b | `ADMIN_TOKEN` env var | Break-glass for operators and CLI tooling |
| 3 | Workspace tokens | Deprecated — use org tokens instead |

When a request arrives, the platform checks tiers in priority order. An org API key bypasses workspace-auth middleware and reaches any workspace in the org.

## Get Started

Navigate to **Settings → Org API Keys** in the Canvas to mint your first key, or use the REST API directly. Store the plaintext when it is returned — it will not be shown again. Use the key prefix in your observability pipeline to trace calls back to the key that made them.

Revoke and rotate at any time from the same screen.

→ [Canvas → Settings → Org API Keys](https://canvas.moleculesai.app)
→ [Platform API Reference](/docs/api-protocol/platform-api)

---

*EnterOS is open source. Org-scoped API keys shipped in PRs #1105, #1107, #1109, and #1110.*