---
title: "Tool Trace + Platform Instructions: Full Visibility and Policy-Level Governance"
date: 2026-04-23
slug: tool-trace-platform-instructions-overview
description: "See every tool your agent called — inputs, outputs, timing — in real-time. And enforce org-wide governance policy at the system prompt level with Platform Instructions."
og_title: "Tool Trace + Platform Instructions: Full Visibility and Policy-Level Governance"
og_description: "Tool-level observability in every A2A response meets system-prompt governance. Two enterprise-grade features, shipped together."
tags: [tool-trace, observability, platform-instructions, governance, enterprise, debugging, a2a]
keywords: [AI agent debugging, tool trace observability, agent governance, platform instructions, enterprise AI audit, system prompt governance, Claude tool call visibility, agent observability]
canonical: https://docs.molecule.ai/blog/tool-trace-platform-instructions-overview
og_image: ""
---

<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "Article",
  "headline": "Tool Trace + Platform Instructions: Full Visibility and Policy-Level Governance",
  "description": "See every tool your agent called — inputs, outputs, timing — in real-time. And enforce org-wide governance policy at the system prompt level.",
  "author": { "@type": "Organization", "name": "EnterOS" },
  "datePublished": "2026-04-23",
  "publisher": {
    "@type": "Organization",
    "name": "EnterOS",
    "logo": { "@type": "ImageObject", "url": "https://molecule.ai/logo.png" }
  }
}
</script>

# Tool Trace + Platform Instructions: Full Visibility and Policy-Level Governance

When an agent makes a tool call in production, most platforms show you the result. They don't show you *which* tool was called, with *what inputs*, what it *returned*, and whether a parallel call happened at the same time. That gap is where debugging ends and guessing begins.

Today we're shipping two features that close it: **Tool Trace** and **Platform Instructions**.

Tool Trace gives every A2A response a structured, chronological record of every tool the agent called — including inputs, output previews, and timing metadata — so you can step through an agent's reasoning the same way you'd step through a debugger. Platform Instructions gives your platform team a governance layer that sits at the system prompt level: configurable rules, scoped globally or per-workspace, enforced before every agent turn.

Together, they address the two questions enterprise teams ask first when evaluating an AI agent platform: *what did the agent actually do?* and *can we enforce policy at the platform level?*

## Tool Trace: Every Tool Call, Captured

Tool Trace is now embedded in every A2A response via `Message.metadata.tool_trace`. Each entry records the tool name, the input passed to it, and an `output_preview` (the first ~200 characters of the result, so the trace stays readable at scale). Entries are stored in the `activity_logs.tool_trace` JSONB column, making them queryable.

Parallel tool calls — multiple tools invoked simultaneously in a single agent turn — are handled correctly via `run_id` pairing of start and end events. This means you can trace two concurrent tool calls independently rather than seeing them collapsed into a single ambiguous log line.

The trace is capped at 200 entries per response to keep payload sizes manageable.

```json
{
  "tool_trace": [
    {
      "tool": "Write",
      "input": {
        "file_path": "/workspace/src/auth.go",
        "content": "package auth\n\nimport \"crypto/rand\"\n\nfunc GenerateToken() (string, error) { ..."
      },
      "output_preview": "Wrote 847 bytes to /workspace/src/auth.go"
    },
    {
      "tool": "Bash",
      "input": {
        "command": "go build ./...",
        "description": "Verify Go package compiles"
      },
      "output_preview": "Build succeeded. 0 errors, 0 warnings."
    },
    {
      "tool": "Grep",
      "input": {
        "pattern": "TODO.*governance",
        "glob": "**/*.go",
        "output_mode": "content"
      },
      "output_preview": "auth.go:14 // TODO: governance layer check\nauth_test.go:8 // TODO: add governance test"
    }
  ],
  "run_id": "01HXKM3...7TQZN"
}
```

## Platform Instructions: Governance at the System Prompt Level

Tool Trace tells you what the agent *did*. Platform Instructions let you define what it *should* do before it does it.

Platform Instructions are configurable rules with two scopes:

- **Global** — applied to every workspace in your organization
- **Workspace** — applied to a specific workspace only

They are fetched at workspace startup and prepended directly to the system prompt. This means they govern agent behavior *before* the first turn executes, not as a post-hoc filter — they shape what the agent is instructed to do in the first place.

A CRUD API manages instruction lifecycle:

```
GET    /instructions              # list (global only, org-scoped)
POST   /instructions              # create (global or workspace)
PUT    /instructions/{id}
DELETE /instructions/{id}
GET    /workspaces/{id}/instructions/resolve  # fetch for a workspace (wsAuth-gated)
```

The resolve endpoint is gated by `wsAuth` — the calling workspace's own token. There is no cross-workspace enumeration: a workspace can only retrieve its own resolved instructions. The content cap is 8KB per instruction.

## Enterprise Governance: Policy in Production

The combination of Tool Trace and Platform Instructions creates a complete governance loop.

**Write the policy once. Enforce it everywhere.**

For regulated industries, Platform Instructions means your security team defines data handling rules — say, a global instruction that every agent in the `customer-data` workspace must redact PII before writing to any external tool — and it applies at the system prompt level, automatically, on every agent turn, without a code deploy.

When something goes wrong — an agent calls an unexpected tool, or behavior drifts from the system prompt — Tool Trace gives you the forensic record to understand exactly what happened. Paired with the org API key attribution from Phase 30's audit trail, you can reconstruct the complete chain: *which org key, which workspace, which agent, which tool calls, in what order.*

**Platform Instructions are enterprise-only.** Tool Trace is available on all plans.

## Get Started

- Tool Trace is enabled by default on all workspaces. Check `Message.metadata.tool_trace` in your A2A responses.
- Platform Instructions are available on Enterprise plans. Visit your workspace settings or contact your account team.
- Explore the [A2A protocol documentation](/docs/api-protocol/a2a-protocol.md)
- Deep-dive: [Tool Trace — every tool call captured](/blog/ai-agent-observability-without-overhead/)
- Deep-dive: [Platform Instructions — governance at the system prompt level](/blog/govern-ai-fleet-system-prompt-level/)

---

*EnterOS is open source. Tool Trace and Platform Instructions shipped in Phase 34 (2026-04-23).*
