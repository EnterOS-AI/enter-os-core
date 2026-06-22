---
title: "The MCP Server List: Which Servers Work With EnterOS?"
date: 2026-04-20
slug: mcp-server-list
description: "A practical guide to the Model Context Protocol ecosystem — finding the right MCP server for your use case, which ones integrate with EnterOS, and how to evaluate servers before you commit."
tags: [mcp, model-context-protocol, ai-agents, integrations]
author: EnterOS
og_title: "The MCP Server List: Which Servers Work With EnterOS?"
og_description: "Find the right MCP server for your AI agent workflow. Full list of reference servers, official integrations, server frameworks, and community registries — with EnterOS compatibility notes."
og_image: /assets/blog/2026-04-20-mcp-server-list/og.png
twitter_card: summary_large_image
canonical: https://molecule.ai/blog/mcp-server-list
keywords:
  - MCP server list
  - MCP servers
  - Model Context Protocol
  - MCP server
  - MCP integration
---

<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "Article",
  "headline": "The MCP Server List: Which Servers Work With EnterOS?",
  "datePublished": "2026-04-20",
  "dateModified": "2026-04-21",
  "author": { "@type": "Organization", "name": "EnterOS" },
  "publisher": {
    "@type": "Organization",
    "name": "EnterOS",
    "logo": { "@type": "ImageObject", "url": "https://molecule.ai/logo.png" }
  },
  "description": "A practical guide to the Model Context Protocol ecosystem — finding the right MCP server for your use case, which ones integrate with EnterOS, and how to evaluate servers before you commit.",
  "keywords": "MCP server list, MCP servers, Model Context Protocol, MCP server, MCP integration",
  "url": "https://molecule.ai/blog/mcp-server-list"
}
</script>

# The MCP Server List: Which Servers Work With EnterOS?

The [Model Context Protocol](/docs/guides/mcp-server-setup) (MCP) is an open standard that lets AI agents connect to external tools and data sources through a unified interface. Rather than writing custom code for every tool integration, MCP servers expose resources and tools that any compatible AI agent can discover and call. This MCP server list covers everything you need to find the right integration for your workflow.

EnterOS supports MCP out of the box. This means any MCP server in the ecosystem — from lightweight reference implementations to enterprise-grade integrations — can be added to a EnterOS agent with a server configuration. No forks, no wrappers, no compatibility layers required. This page is your practical MCP server list for real-world AI agent workflows.

This guide covers the full MCP server list that matters: reference servers from the MCP spec authors, official integrations from major vendors, server frameworks for building your own, and community-maintained registries where the broader MCP ecosystem publishes new MCP servers every week. Whether you need one MCP server or a stack of them, this MCP server list gives you the starting points for every major category.

---

## What Is an MCP Server?

An MCP server is a process that implements the Model Context Protocol. It runs separately from your AI agent and communicates over stdio or HTTP+SSE. When a compatible AI agent connects, it receives a manifest of available **tools**, **resources**, and **prompts** — no code changes on the agent side.

The MCP specification defines the transport layer and message shapes. The server implementer decides what capabilities to expose. This separation is what makes the MCP ecosystem portable: an MCP server written for one MCP-compatible platform works on any other, including EnterOS.

The key MCP concepts that every server implements:

- **Tools** — functions the agent can call (e.g., `search_code`, `read_file`)
- **Resources** — data the agent can read (e.g., repository contents, database schemas)
- **Prompts** — reusable prompt templates the agent can load

Every MCP server in this list exposes at least one of these three primitives. Most expose tools; a well-designed MCP server also exposes resources. The Model Context Protocol makes all of this possible by providing a shared vocabulary and transport — so MCP servers and the agents that call them don't need to coordinate on anything beyond the protocol itself.

---

## MCP Reference Servers

The [modelcontextprotocol GitHub organization](https://github.com/modelcontextprotocol) maintains a set of reference server implementations. These are canonical examples maintained by the MCP spec authors and are often the best starting point for common integrations. This reference MCP server list is kept up to date with each protocol release.

### Filesystem MCP Server

Provides local file system access. Useful for AI agents that need to read project files, write output, or navigate a codebase.

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/path/to/allowed/directory"]
    }
  }
}
```

**EnterOS note:** Configure `allowedDirectories` in the server args to scope filesystem access. Use separate server configs per workspace if you need per-project isolation.

### Git MCP Server

Exposes Git operations — commit history, diffs, branch listings, file contents at any ref. Useful for AI agents doing code review or changelog generation.

```json
{
  "mcpServers": {
    "git": {
      "command": "uvx",
      "args": ["mcp-server-git", "--repository", "/path/to/repo"]
    }
  }
}
```

**EnterOS note:** Pass the `--repository` flag to scope the server to a specific project. Without it, the server operates on whatever directory the process runs in.

### Memory MCP Server

A vector-backed memory server that persists facts across agent sessions using embeddings. The agent can store key-value facts and retrieve them semantically later.

```json
{
  "mcpServers": {
    "memory": {
      "command": "node",
      "args": ["/path/to/memory-server/dist/index.js"]
    }
  }
}
```

**EnterOS note:** Combine with EnterOS's built-in session context for hybrid short-term + long-term memory strategies.

### Brave Search MCP Server

Web search via the Brave Search API. Gives the agent real-time internet access for research tasks.

```json
{
  "mcpServers": {
    "brave-search": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-brave-search"],
      "env": {
        "BRAVE_API_KEY": "your-api-key"
      }
    }
  }
}
```

**EnterOS note:** Set the `BRAVE_API_KEY` as an environment variable in your EnterOS workspace secrets, not in the server config file.

---

## Official MCP Integrations

Beyond the reference servers, several established products ship MCP-compatible servers. These are production-grade implementations maintained by the vendors. Each MCP integration in this section ships with vendor support and backward-compatibility guarantees.

### Slack MCP Integration

The official Slack SDK includes an MCP server that exposes channels, messages, and thread replies as tools and resources. An agent can post updates, read channel history, or monitor for specific events.

**Use cases:** Team status updates, incident channel posting, cross-team workflow automation.

### GitHub MCP Integration

The GitHub MCP server surfaces repositories, issues, pull requests, and discussions as structured resources. Agents can create issues, comment on PRs, or query code search.

**Use cases:** Automated code review summaries, issue triaging, release note generation.

### AWS KB Retrieval MCP Integration

Amazon's Bedrock Knowledge Bases can be accessed via MCP. Gives agents read access to indexed enterprise documents.

**Use cases:** Internal knowledge base queries, policy document retrieval, compliance checking.

### Google Drive MCP Integration

Read access to Google Drive files and folders. Agents can search documents, read sheet data, or pull slide content.

**Use cases:** Research synthesis from Drive documents, automated reporting from Sheets.

---

## MCP Server Frameworks

If you need a custom MCP integration not covered by existing servers, MCP server frameworks let you build one without implementing the Model Context Protocol from scratch. These frameworks handle the protocol boilerplate so you can focus on your tool's logic. Building your own MCP server is the right call when you have a proprietary data source, an internal API, or a domain-specific tool that isn't covered by the available MCP servers in the ecosystem.

### Python MCP SDK

The official Python implementation. Ideal for data-heavy or ML-adjacent integrations.

```python
from mcp.server import Server
from mcp.types import Tool, TextContent

server = Server("my-analytics-server")

@server.list_tools()
async def list_tools():
    return [
        Tool(
            name="query_analytics",
            description="Run a query against the analytics database",
            inputSchema={
                "type": "object",
                "properties": {
                    "sql": {"type": "string", "description": "SQL query to execute"}
                },
                "required": ["sql"]
            }
        )
    ]

@server.call_tool()
async def call_tool(name: str, arguments: dict):
    if name == "query_analytics":
        result = run_query(arguments["sql"])
        return [TextContent(type="text", text=str(result))]
```

**EnterOS note:** Package your server as a Docker image and reference it by image URL in your EnterOS workspace server config for one-command deployment.

### TypeScript MCP SDK

The official Node.js/TypeScript implementation. Best for web-service integrations, API wrappers, and real-time data sources.

### Go MCP Server Framework

A lightweight Go implementation for high-performance or infrastructure-level integrations.

---

## Community MCP Registries

The MCP ecosystem grows through community contributions. These registries index servers by category and are the best places to discover new MCP servers without searching GitHub manually. Bookmark these — the community publishes new MCP servers every week, and these registries stay current.

### awesome-mcp

The canonical community MCP server list. Maintained on GitHub with categorized entries for tools, resources, and prompt servers. Covers everything from production-grade MCP servers to experimental community projects. Start here when you know the category you need but not the specific MCP server.

### Model Context Protocol Registry (registry.mcp.so)

A structured registry that categorizes servers by domain: development, productivity, data, infrastructure. Each entry links to the implementation and documents supported MCP features.

### MCP Hub

A community-curated directory with install commands for each server. Particularly useful for quickly spinning up a new MCP server via `npx` or `uvx`.

---

## How to Install an MCP Server

The exact install steps depend on the server, but most MCP servers follow the same startup patterns. Most servers can be started with a single command:

```bash
# Via npx (Node.js servers)
npx -y @modelcontextprotocol/server-filesystem /allowed/path

# Via uvx (Python servers)
uvx mcp-server-git --repository /path/to/repo

# Via Docker (any server, in an isolated container)
docker run -v /data:/data my-registry/my-mcp-server --allowed-path /data
```

Once started, add the MCP server to your EnterOS workspace configuration:

```json
{
  "mcpServers": {
    "my-server": {
      "command": "docker",
      "args": ["run", "--rm", "-v", "/data:/data", "my-registry/my-mcp-server", "--allowed-path", "/data"]
    }
  }
}
```

**EnterOS note:** Use Docker-based servers for any MCP integration that requires credentials or filesystem access you don't want to co-locate with the agent process. EnterOS's workspace isolation handles the container lifecycle automatically.

---

## Choosing the Right MCP Server: A Decision Guide

Not every MCP server belongs in every project. Here's how to evaluate which MCP servers to add to your EnterOS workspace. The best MCP server is the one that does exactly what your agent needs — nothing more. Extra MCP servers add latency, credential surface, and maintenance burden without adding value.

| Need | Recommended MCP servers |
|------|------------------------|
| Read project files | filesystem |
| Git operations | git |
| Web search | brave-search |
| Slack/Teams integration | slack, teams |
| Cloud infrastructure queries | aws-kb, google-drive |
| Long-term memory | memory |
| Custom data source | Build with Python/TypeScript SDK |

**Start narrow.** Add MCP servers as your agent's tasks require them. Each MCP server is a new attack surface and a new failure mode. The Model Context Protocol gives you a consistent interface to manage them all — but you still need to evaluate each MCP server's security posture before adding it to a workspace. EnterOS's workspace-level server configuration makes it easy to add servers incrementally and revoke access at the workspace boundary.

---

## MCP Server Governance With EnterOS

Every MCP server your agent can access is a decision about what the agent is permitted to do. EnterOS gives you controls at the workspace level so you can govern your MCP servers in production:

- **Server allowlisting** — configure exactly which servers can run in a workspace
- **Environment variable scoping** — API keys used by MCP servers stay in workspace secrets, not in config files
- **Audit logging** — every tool call made through an MCP server is recorded in the workspace activity log
- **Workspace isolation** — each workspace runs its server config independently, so one team's servers don't affect another's

This is the governance layer that makes running MCP servers practical in production. A list of MCP servers is only as useful as the controls around them. EnterOS provides those controls built in.

Get started with MCP on EnterOS in the [MCP Server Setup Guide](/docs/guides/mcp-server-setup).

<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "FAQPage",
  "mainEntity": [
    {
      "@type": "Question",
      "name": "What is an MCP server?",
      "acceptedAnswer": {
        "@type": "Answer",
        "text": "An MCP server is a process that implements the Model Context Protocol (MCP). It runs separately from your AI agent and exposes tools, resources, and prompts that any MCP-compatible AI agent can discover and call — without custom code on the agent side."
      }
    },
    {
      "@type": "Question",
      "name": "How do I add an MCP server to EnterOS?",
      "acceptedAnswer": {
        "@type": "Answer",
        "text": "Add the server configuration to your EnterOS workspace config under the mcpServers key. Most servers can be started with a single command (npx, uvx, or Docker) and then referenced in your workspace configuration. EnterOS's workspace isolation handles the container lifecycle automatically."
      }
    },
    {
      "@type": "Question",
      "name": "Which MCP servers are officially supported?",
      "acceptedAnswer": {
        "@type": "Answer",
        "text": "EnterOS supports the full MCP ecosystem. Reference servers (filesystem, git, memory, brave-search) are maintained by the modelcontextprotocol GitHub organization. Official integrations from Slack, GitHub, AWS, and Google are also available. Any MCP server in the ecosystem is compatible with EnterOS."
      }
    },
    {
      "@type": "Question",
      "name": "How do I evaluate an MCP server before adding it to my agent?",
      "acceptedAnswer": {
        "@type": "Answer",
        "text": "Start narrow — add MCP servers only when your agent's tasks require them. Evaluate each server's security posture, credential requirements, and failure modes before adding it. EnterOS's workspace-level server configuration makes it easy to add servers incrementally and revoke access at the workspace boundary."
      }
    },
    {
      "@type": "Question",
      "name": "Can I build a custom MCP server?",
      "acceptedAnswer": {
        "@type": "Answer",
        "text": "Yes. MCP server frameworks in Python (official), TypeScript, and Go let you build custom integrations without implementing the protocol from scratch. Package your server as a Docker image and reference it by image URL in your EnterOS workspace server config for one-command deployment."
      }
    }
  ]
}
</script>

---

*To stay current with the MCP ecosystem, watch the [modelcontextprotocol GitHub organization](https://github.com/modelcontextprotocol) for new server releases and protocol updates. This MCP server list is updated as the ecosystem evolves.*
