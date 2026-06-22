---
title: "Govern Your AI Fleet at the System Prompt Level"
date: 2026-04-23
slug: govern-ai-fleet-system-prompt-level
description: "Platform Instructions lets enterprise IT teams enforce org-wide policy rules at the system prompt level — before the first agent turn executes. No code deploys. No SDK integration."
og_title: "Govern Your AI Fleet at the System Prompt Level"
og_description: "Platform Instructions: global and workspace-scoped rules prepended to the system prompt. Governance before the first turn, not after."
tags: [governance, platform-instructions, enterprise, security, it-governance, system-prompt, policy, a2a]
keywords: [AI fleet governance, enterprise AI policy, system prompt governance, AI agent compliance, platform instructions, workspace policy enforcement, enterprise AI security, AI agent ACL]
canonical: https://docs.molecule.ai/blog/govern-ai-fleet-system-prompt-level
og_image: ""
---

<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "Article",
  "headline": "Govern Your AI Fleet at the System Prompt Level",
  "description": "Platform Instructions lets enterprise IT teams enforce org-wide policy rules at the system prompt level — before the first agent turn executes. No code deploys.",
  "author": { "@type": "Organization", "name": "EnterOS" },
  "datePublished": "2026-04-23",
  "publisher": {
    "@type": "Organization",
    "name": "EnterOS",
    "logo": { "@type": "ImageObject", "url": "https://molecule.ai/logo.png" }
  }
}
</script>

# Govern Your AI Fleet at the System Prompt Level

The moment an AI agent goes into production, the governance question stops being theoretical. Which tools can it call? What data can it write to? Are there constraints that apply to every turn, not just the ones where someone remembered to add a guardrail?

Most platforms answer these questions with post-hoc filtering — a rule that checks outputs after the agent has already decided what to do. Platform Instructions takes a different approach: governance at the source, before the first token is generated. Rules are prepended to the system prompt at workspace startup, shaping what the agent is instructed to do from the very first turn.

## Two Scopes, One Governance Plane

Platform Instructions supports two scoping levels:

- **Global** — applied to every workspace in your organization. One rule, enforced everywhere.
- **Workspace** — applied to a specific workspace only. Fine-grained control without global impact.

When a workspace starts, EnterOS resolves all applicable instructions — combining global rules with workspace-specific ones — and prepends them to the agent's system prompt. The agent doesn't receive these rules as a filter; it receives them as part of its core instruction set. That distinction matters: a filter can be worked around; a system prompt instruction shapes the agent's reasoning from the ground up.

## The CRUD API

Platform Instructions are managed via a REST API:

```bash
# Create a global instruction
POST /instructions
{
  "scope": "global",
  "content": "Before invoking any tool that writes to external storage, confirm the target path is within the org-approved sandbox directory. Reject and report if not."
}

# Create a workspace-scoped instruction
POST /instructions
{
  "scope": "workspace",
  "workspace_id": "ws_01HXKM3T8PRQN4ZW7XYVD2EJ5A",
  "content": "This workspace handles customer PII. Redact all PII fields in tool outputs before writing to external systems."
}

# Retrieve resolved instructions for a workspace
GET /workspaces/ws_01HXKM3T8PRQN4ZW7XYVD2EJ5A/instructions/resolve
Authorization: Bearer <workspace_token>
```

The resolve endpoint is gated by `wsAuth` — the calling workspace's own token. Workspaces cannot enumerate or retrieve instructions belonging to other workspaces. There is no cross-workspace read-back. Global instructions are org-scoped and visible to org admins.

Each instruction is capped at **8KB** of content. A workspace's total resolved instruction set (global + workspace-scoped) is fetched once at startup and cached — so governance is enforced without per-turn latency overhead.

## Enforcement Before Execution

The key architectural difference between Platform Instructions and post-hoc policy enforcement is timing. A post-hoc filter evaluates after the agent decides what to do. Platform Instructions are in the system prompt before the agent decides anything.

This matters in regulated environments where the requirement isn't "flag bad behavior" but "prevent bad behavior from being possible." A compliance team that requires PII redaction doesn't want the agent to write raw PII and then redact it on the way out — they want the agent to reason about redaction as part of its core task framing.

With Platform Instructions, the rule isn't a gate. It's a context.

## Enterprise Security: ACLs and Access Control

Platform Instructions are enterprise-only because enterprise governance requires enterprise-grade access control. The security model reflects this:

- **Global instructions** are managed by org admins — not workspace owners
- **Workspace instructions** are managed by workspace admins within their own scope
- **Resolve endpoint** requires `wsAuth` — a workspace cannot retrieve another workspace's resolved instructions
- **No cross-workspace enumeration** — the API does not expose instruction lists to callers outside the owning scope

For security and IT governance teams evaluating AI agent platforms, this is the access control surface they need: policy lives at the org level, is enforced at the workspace level, and cannot be read or modified by the agents or workspaces it governs.

## Get Started

Platform Instructions are available on **Enterprise plans**. To get started:

- Contact your account team or visit your workspace settings
- Define your first global instruction via `POST /instructions`
- Assign workspace-scoped instructions to specific workspaces via `POST /instructions` with `workspace_id`
- Verify resolved instructions via `GET /workspaces/{id}/instructions/resolve`

For a complete governance picture, combine Platform Instructions with [Tool Trace](/blog/ai-agent-observability-without-overhead/) — see exactly which tools were called and what inputs were passed, alongside the policy that governed them.
- See both features in one post: [Tool Trace + Platform Instructions Overview](/blog/tool-trace-platform-instructions-overview/)

---

*EnterOS is open source. Platform Instructions shipped in Phase 34 (2026-04-23). Enterprise plans include org-scoped governance, wsAuth-gated resolve endpoints, and full instruction audit logs.*
