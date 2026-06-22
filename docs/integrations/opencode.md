# EnterOS + opencode Integration

> **opencode** is an AI coding agent ([opencode.ai](https://opencode.ai)) that supports remote MCP servers via `opencode.json`. This guide shows how to wire it to your EnterOS workspace.

## Prerequisites

- A running EnterOS platform (`MOLECULE_MCP_URL` — e.g. `https://api.molecule.ai`)
- A workspace-scoped bearer token (`MOLECULE_MCP_TOKEN`) issued via the platform API

## 1. Declare EnterOS as a remote MCP server

Create (or extend) `opencode.json` in your project root:

```json
{
  "mcpServers": {
    "molecule": {
      "type": "remote",
      "url": "${MOLECULE_MCP_URL}/workspaces/${WORKSPACE_ID}/mcp",
      "headers": { "Authorization": "Bearer ${MOLECULE_MCP_TOKEN}" },
      "description": "EnterOS A2A orchestration — delegate_task, list_peers, check_task_status"
    }
  }
}
```

> ⚠️ **Never embed the token in the URL** (e.g. `?token=...`). Always use the `Authorization: Bearer` header. URL-embedded tokens appear in server logs, browser history, and Git history if the file is committed.

A pre-configured template is available at `org-templates/molecule-dev/opencode.json`.

## 2. Obtain a workspace-scoped token

```bash
curl -X POST https://$MOLECULE_MCP_URL/workspaces/$WORKSPACE_ID/tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "opencode-agent", "scopes": ["mcp:read", "mcp:delegate"]}'
```

Store the returned token as `MOLECULE_MCP_TOKEN` in your `.env` (see `.env.example`).

## 3. Available tools

When opencode connects to the EnterOS MCP endpoint, the agent gains access to:

| Tool | Description |
|------|-------------|
| `list_peers` | Discover available workspaces in your org |
| `delegate_task` | Send a task to a peer workspace and wait for the result |
| `delegate_task_async` | Fire-and-forget task delegation; returns a `task_id` |
| `check_task_status` | Poll an async delegation by `task_id` |
| `commit_memory` | Persist information to LOCAL or TEAM memory scope |
| `recall_memory` | Search previously committed memories |

### Restricted tools

- **`send_message_to_user`** — disabled for remote MCP callers by default; requires explicit opt-in via `MOLECULE_MCP_ALLOW_SEND_MESSAGE=true`
- **GLOBAL memory scope** — `commit_memory` with `scope: GLOBAL` is blocked for external agents; LOCAL and TEAM scopes are available

## 4. Example: delegate a research task

```json
{
  "tool": "delegate_task",
  "arguments": {
    "target": "research-lead",
    "task": "Summarise the last 7 days of commits in Molecule-AI/enter-os-core"
  }
}
```

opencode sends this tool call to the EnterOS MCP endpoint. The platform routes it to your `research-lead` workspace and streams the response back.

## 5. Security notes

### SAFE-T1401 — org topology exposure
`list_peers` returns the full set of workspace names and roles visible to your workspace. This is intentional: provisioned agents need to know their peers to delegate effectively. Be aware that any opencode agent with a valid `MOLECULE_MCP_TOKEN` can enumerate your org topology.

### SAFE-T1201 — tool surface audit pending
The full `@molecule-ai/mcp-server` npm package exposes additional tools beyond those listed above. These are pending a SAFE-T1201 security audit (tracked in #747 follow-on) and **must not be exposed to external agents in production** until that audit completes.

### Token scoping
Issue tokens with the minimum required scopes (`mcp:read`, `mcp:delegate`). Rotate tokens regularly. Revoke via `DELETE /workspaces/:id/tokens/:token_id`.

## 6. Environment variables

Add to your `.env`:

```bash
MOLECULE_MCP_URL=https://api.molecule.ai   # or http://localhost:8080 for local dev
MOLECULE_MCP_TOKEN=                         # workspace-scoped bearer token from step 2
WORKSPACE_ID=                               # UUID of the agent workspace opencode acts as
                                            # find it in Canvas sidebar or GET /workspaces
```

See `.env.example` for the canonical reference.
