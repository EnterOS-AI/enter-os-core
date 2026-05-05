# MCP Server Setup Guide

The Molecule AI MCP server lets any MCP-compatible AI agent manage workspaces, agents, secrets, memory, schedules, channels, and more through the platform API. It works with Claude Code, Cursor, and any other tool that speaks the MCP protocol.

## Quick Start

### 1. Install

The MCP server is published as `@molecule-ai/mcp-server` on npm.

```bash
npx @molecule-ai/mcp-server
```

### 2. Configure in `.mcp.json`

Add to your project's `.mcp.json`:

```json
{
  "mcpServers": {
    "molecule": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@molecule-ai/mcp-server"],
      "env": {
        "MOLECULE_URL": "http://localhost:8080"
      }
    }
  }
}
```

For production/SaaS deployments, set `MOLECULE_URL` to your tenant URL:
```json
"MOLECULE_URL": "https://your-org.moleculesai.app"
```

### 3. Verify

Once configured, your MCP client should show 87 Molecule AI tools. Test with:
```
list_workspaces
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MOLECULE_URL` | `http://localhost:8080` | Platform API URL |

## Tool Reference

### Workspace Management

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `list_workspaces` | `/workspaces` | GET | List all workspaces |
| `get_workspace` | `/workspaces/:id` | GET | Get workspace details |
| `create_workspace` | `/workspaces` | POST | Create a new workspace |
| `update_workspace` | `/workspaces/:id` | PATCH | Update workspace fields |
| `delete_workspace` | `/workspaces/:id` | DELETE | Delete a workspace |
| `restart_workspace` | `/workspaces/:id/restart` | POST | Restart workspace container |
| `pause_workspace` | `/workspaces/:id/pause` | POST | Pause workspace |
| `resume_workspace` | `/workspaces/:id/resume` | POST | Resume paused workspace |
| `discover_workspace` | `/registry/discover/:id` | GET | Get workspace URL + agent card |

### Agent Management

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `assign_agent` | `/workspaces/:id/agent` | POST | Assign agent to workspace |
| `remove_agent` | `/workspaces/:id/agent` | DELETE | Remove agent |
| `replace_agent` | `/workspaces/:id/agent` | PATCH | Replace agent config |
| `move_agent` | `/workspaces/:id/agent/move` | POST | Move agent to different workspace |

### Communication

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `chat_with_agent` | `/workspaces/:id/a2a` | POST | Send A2A message to agent |
| `async_delegate` | `/workspaces/:id/delegate` | POST | Fire-and-forget delegation |
| `check_delegations` | `/workspaces/:id/delegations` | GET | Check delegation status |
| `send_channel_message` | `/workspaces/:id/channels/:channelId/send` | POST | Send to social channel |
| `notify_user` | `/workspaces/:id/notify` | POST | Push notification to canvas |
| `list_peers` | `/registry/:id/peers` | GET | Find sibling/parent workspaces |
| `check_access` | `/registry/check-access` | POST | Check if two workspaces can communicate |

### Configuration

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `get_config` | `/workspaces/:id/config` | GET | Get workspace config.yaml |
| `update_config` | `/workspaces/:id/config` | PATCH | Update config fields |
| `get_model` | `/workspaces/:id/model` | GET | Get configured LLM model |

### Secrets

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `list_secrets` | `/workspaces/:id/secrets` | GET | List workspace secret keys |
| `set_secret` | `/workspaces/:id/secrets` | POST | Set a workspace secret |
| `delete_secret` | `/workspaces/:id/secrets/:key` | DELETE | Delete a secret |
| `list_global_secrets` | `/settings/secrets` | GET | List global secrets |
| `set_global_secret` | `/settings/secrets` | PUT | Set a global secret |
| `delete_global_secret` | `/settings/secrets/:key` | DELETE | Delete global secret |

### Memory

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `memory_list` | `/workspaces/:id/memory` | GET | List memory keys |
| `memory_get` | `/workspaces/:id/memory/:key` | GET | Get memory value |
| `memory_set` | `/workspaces/:id/memory` | POST | Set memory key-value |
| `memory_delete_kv` | `/workspaces/:id/memory/:key` | DELETE | Delete memory key |
| `search_memory` | `/workspaces/:id/memories` | GET | Full-text search memories |
| `commit_memory` | `/workspaces/:id/memories` | POST | Commit HMA memory |
| `delete_memory` | `/workspaces/:id/memories/:id` | DELETE | Delete HMA memory |

### Files

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `list_files` | `/workspaces/:id/files` | GET | List workspace files |
| `read_file` | `/workspaces/:id/files/*path` | GET | Read file content |
| `write_file` | `/workspaces/:id/files/*path` | PUT | Write/overwrite file |
| `delete_file` | `/workspaces/:id/files/*path` | DELETE | Delete file |
| `replace_all_files` | `/workspaces/:id/files` | PUT | Replace all files atomically |

### Schedules

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `list_schedules` | `/workspaces/:id/schedules` | GET | List cron schedules |
| `create_schedule` | `/workspaces/:id/schedules` | POST | Create cron schedule |
| `update_schedule` | `/workspaces/:id/schedules/:id` | PATCH | Update schedule |
| `delete_schedule` | `/workspaces/:id/schedules/:id` | DELETE | Delete schedule |
| `run_schedule` | `/workspaces/:id/schedules/:id/run` | POST | Trigger schedule now |
| `get_schedule_history` | `/workspaces/:id/schedules/:id/history` | GET | Past run history |

### Channels (Social Integrations)

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `list_channels` | `/workspaces/:id/channels` | GET | List configured channels |
| `add_channel` | `/workspaces/:id/channels` | POST | Add Telegram/Slack/Lark channel |
| `update_channel` | `/workspaces/:id/channels/:id` | PATCH | Update channel config |
| `remove_channel` | `/workspaces/:id/channels/:id` | DELETE | Remove channel |
| `test_channel` | `/workspaces/:id/channels/:id/test` | POST | Test channel connectivity |
| `list_channel_adapters` | `/channels/adapters` | GET | Available platforms |
| `discover_channel_chats` | `/channels/discover` | POST | Auto-detect chats for bot token |

### Plugins

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `list_installed_plugins` | `/workspaces/:id/plugins` | GET | List installed plugins |
| `install_plugin` | `/workspaces/:id/plugins` | POST | Install plugin from source |
| `uninstall_plugin` | `/workspaces/:id/plugins/:name` | DELETE | Uninstall plugin |
| `list_available_plugins` | `/workspaces/:id/plugins/available` | GET | Plugins matching runtime |
| `list_plugin_registry` | `/plugins` | GET | Full plugin registry |
| `list_plugin_sources` | `/plugins/sources` | GET | Registered source schemes |
| `check_plugin_compatibility` | `/workspaces/:id/plugins/compatibility` | GET | Preflight check |

### Teams / Hierarchy

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|

### Templates & Bundles

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `list_templates` | `/templates` | GET | Available templates |
| `import_template` | `/templates/import` | POST | Import template |
| `list_org_templates` | `/org/templates` | GET | Org template list |
| `import_org` | `/org/import` | POST | Import org template |
| `export_bundle` | `/bundles/export/:id` | GET | Export workspace bundle |
| `import_bundle` | `/bundles/import` | POST | Import workspace bundle |

### Tokens

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `list_tokens` | `/workspaces/:id/tokens` | GET | List workspace tokens |
| `create_token` | `/workspaces/:id/tokens` | POST | Create new bearer token |
| `revoke_token` | `/workspaces/:id/tokens/:id` | DELETE | Revoke specific token |

### Monitoring

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `list_activity` | `/workspaces/:id/activity` | GET | Activity log |
| `report_activity` | `/workspaces/:id/activity` | POST | Report agent activity |
| `list_events` | `/events` | GET | Platform event stream |
| `list_traces` | `/workspaces/:id/traces` | GET | LLM traces (Langfuse) |
| `session_search` | `/workspaces/:id/session-search` | GET | Search chat sessions |

### Approvals

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `create_approval` | `/workspaces/:id/approvals` | POST | Request approval |
| `get_workspace_approvals` | `/workspaces/:id/approvals` | GET | List approvals |
| `decide_approval` | `/workspaces/:id/approvals/:id/decide` | POST | Approve/reject |
| `list_pending_approvals` | `/approvals/pending` | GET | All pending approvals |

### Canvas

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `get_canvas_viewport` | `/canvas/viewport` | GET | Current viewport |
| `set_canvas_viewport` | `/canvas/viewport` | PUT | Set viewport position |

### Remote Agents

| MCP Tool | API Route | Method | Description |
|----------|-----------|--------|-------------|
| `list_remote_agents` | `/workspaces?runtime=external` | GET | List remote agents |
| `get_remote_agent_state` | `/registry/discover/:id` | GET | Remote agent status |
| `check_remote_agent_freshness` | `/registry/heartbeat` | POST | Check if agent is alive |
| `get_remote_agent_setup_command` | (local) | — | Get setup instructions |

## Authentication

Most routes require a bearer token:

```bash
curl -H "Authorization: Bearer <token>" http://localhost:8080/workspaces
```

Tokens are issued on workspace registration (`POST /registry/register`) or via the token management API (`POST /workspaces/:id/tokens`).

The MCP server handles auth automatically when configured with the correct `MOLECULE_URL`.

## Troubleshooting

| Issue | Fix |
|-------|-----|
| "Connection refused" | Check `MOLECULE_URL` points to running platform |
| "401 Unauthorized" | Token expired or revoked — create a new one |
| Tools not showing | Run `npx @molecule-ai/mcp-server` standalone to check for errors |
| Stale data | MCP server doesn't cache — check platform directly |
