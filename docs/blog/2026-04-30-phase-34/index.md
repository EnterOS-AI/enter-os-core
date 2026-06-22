---
title: "What's New in Phase 34"
date: "2026-04-30"
slug: "whats-new-phase-34"
description: "Phase 34 ships Partner API Keys, SaaS Federation v2, Tool Trace observability, and Platform Instructions governance. Four production features for AI agent platforms."
tags: [phase-34, changelog, partner-api, federation, tool-trace, platform-instructions]
---

# What's New in Phase 34

**April 30, 2026**

Phase 34 ships four platform features: Partner API Keys (GA), SaaS Federation v2 (GA), Tool Trace observability (GA), and Platform Instructions governance (GA).

---

## Partner API Keys (GA)

Marketplace resellers, CI/CD tooling, and automation platforms can now manage EnterOS orgs programmatically using scoped, rate-limited, revocable `mol_pk_` API keys. Create orgs, provision workspaces, and revoke access — no browser session required.

Available scopes: `orgs:create`, `orgs:list`, `orgs:delete`, `workspaces:create`, `billing:read`. Pricing tiers [coming soon](https://molecule.ai/pricing). Available on Partner and Enterprise plans.

## SaaS Federation v2 (GA)

Organizations can now collaborate across org boundaries using a structured, auditable trust model. Grant named federation trusts to partner orgs — scoped to specific workspaces and operations — without sharing credentials. Trust relationships are revocable at any time.

Federation v2 uses A2A protocol with embedded federation claims, so cross-org agent handoffs are verifiable at the receiving end. Available on Enterprise plans.

## Tool Trace Observability (GA)

Every A2A response now includes a structured `tool_trace` array in `Message.metadata`. Each entry records the tool called, its inputs, an output preview, and a `run_id` that correctly groups concurrent parallel calls.

Tool Trace is available on all plans. No sidecar service. No sampling. See the [A2A Protocol Reference](/docs/api-protocol/a2a-protocol) for the full schema.

## Platform Instructions Governance (GA)

Policy rules can now be prepended to an agent's system prompt at workspace startup — at the source, before the first token is generated. Rules are scoped to `global` (all workspaces in the org) or `workspace` (a specific workspace only). The `wsAuth`-gated resolve endpoint ensures workspaces can only read their own instructions.

Platform Instructions are available on Enterprise plans. Each instruction is capped at 8KB. Resolved instruction sets are cached at startup to avoid per-turn latency.

---

For full details, see the [Phase 34 launch post](/blog/phase-34-launch).
