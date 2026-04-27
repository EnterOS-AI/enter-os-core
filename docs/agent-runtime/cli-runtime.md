# Agent Runtime Adapters

## Overview

The workspace runtime uses a **pluggable adapter architecture** — each agent infrastructure (Claude Code, OpenClaw, LangGraph, CrewAI, AutoGen, etc.) has its own adapter that bridges the A2A protocol to the infra's native interface.

Adapters live in `workspace/adapters/<runtime>/` and are auto-discovered at startup. Each adapter implements `BaseAdapter` (from `adapters/base.py`) with `setup()` and `create_executor()` methods.

The runtime is selected via `config.yaml`:

```yaml
runtime: claude-code    # or: langgraph, openclaw, deepagents, crewai, autogen
runtime_config:
  model: sonnet
  auth_token_file: .auth-token
  timeout: 0
```

## How It Works

The unified `workspace-template` Docker image includes both Python (LangGraph) and Node.js (CLI runtimes). At startup, `main.py` checks the `runtime` field in `config.yaml`, discovers the matching adapter in `adapters/<runtime>/`, calls `adapter.setup(config)` then `adapter.create_executor(config)` to get an `AgentExecutor` that handles A2A requests.

```
A2A request arrives
      |
      v
AgentExecutor.execute(context, event_queue)
      |  - extracts user message from A2A parts
      |  - extracts conversation history from params.metadata.history
      |  - sets current_task on heartbeat (shows on canvas card)
      |  - invokes the runtime (LangGraph graph, CLI subprocess, etc.)
      v
Response → A2A event queue → JSON-RPC response
```

### Conversation History

Chat sessions in the Canvas UI send prior messages (up to 20) via `params.metadata.history` in each A2A `message/send` request. Executors extract this history:

- **LangGraph/DeepAgents**: Prepends history as `("human", text)` / `("ai", text)` tuples to the LangGraph message list
- **CrewAI/AutoGen**: Prepends history as a text prefix in the task description (`"Conversation so far:\n..."`)
- **Claude Code**: Uses `--resume <session_id>` for native session continuity (history not needed)
- **OpenClaw**: Uses `--session-id` for native session continuity

### Current Task Reporting

All executors update the workspace's `current_task` via the heartbeat during execution. This shows an amber banner on the canvas card. The shared `set_current_task(heartbeat, task)` function in `a2a_executor.py` handles this for all runtimes.

## Built-in Adapters

### LangGraph (`runtime: langgraph`) — Default

Full Python agent with LangGraph ReAct pattern. Supports skills, tools, plugins, peer coordination, and team routing.

### Claude Code (`runtime: claude-code`)

```yaml
runtime: claude-code
runtime_config:
  model: sonnet          # or opus, haiku
  auth_token_file: .auth-token   # OAuth token file in /configs/
```

Uses the **Claude Agent SDK** (`claude-agent-sdk` Python package) to invoke the Claude Code engine programmatically via `ClaudeSDKExecutor`. This replaced the earlier subprocess-based approach (`claude --print ...`) to eliminate stdout buffering, zombie processes, session-ID parsing fragility, and ~500ms per-message startup overhead.

The SDK uses the same Claude Code engine under the hood — plugins, CLAUDE.md discovery, hooks, auto-memory, and skills all work identically. The `@anthropic-ai/claude-code` npm package is still installed in the image because the SDK wraps it internally.

**Auth:** Uses the `CLAUDE_CODE_OAUTH_TOKEN` env var — the OAuth token is read from `/configs/.auth-token` and picked up by the SDK automatically.

**Concurrency:** Turns are serialized per-executor via an `asyncio.Lock` so session state stays race-free. Cooperative cancel support via `aclose()` on the SDK's async generator.

**Important:** Claude Code refuses to run as root with `--dangerously-skip-permissions`. The Dockerfile creates a non-root `agent` user.

### CrewAI (`runtime: crewai`)

Role-based multi-agent framework. Creates a CrewAI Agent + Task + Crew per request with A2A delegation tools (`delegate_to_peer`, `list_available_peers`).

```yaml
runtime: crewai
model: openrouter:google/gemini-2.5-flash
```

**Auth:** Uses `OPENROUTER_API_KEY` or `OPENAI_API_KEY` env var.

### AutoGen (`runtime: autogen`)

Microsoft AutoGen AssistantAgent with tool use. Creates an `AssistantAgent` per request with A2A delegation tools.

```yaml
runtime: autogen
model: openai:gpt-4.1-mini
```

**Auth:** Uses `OPENAI_API_KEY` env var.

### DeepAgents (`runtime: deepagents`)

LangGraph-based agent with deep planning capabilities. Uses the same `LangGraphA2AExecutor` as the default runtime but with a specialized agent setup including delegation, memory, and search tools.

```yaml
runtime: deepagents
model: openrouter:google/gemini-2.5-flash
```

### OpenClaw (`runtime: openclaw`)

Proxies A2A messages to OpenClaw via `openclaw agent` CLI subprocess. Handles its own session continuity via `--session-id`.

```yaml
runtime: openclaw
```

**Auth:** Uses OpenClaw's own authentication (configured during `openclaw setup`).

## Session Continuity (Claude Code)

Claude Code workspaces maintain conversation state across messages using the SDK's `resume` option:

1. **First message**: the SDK's `ResultMessage` returns a `session_id`
2. **Subsequent messages**: the SDK is called with `resume=<session_id>` to continue the same conversation
3. **System prompt**: only injected on the first message — resumed sessions already have it
4. **Memories**: recalled from the platform API on the first turn only; subsequent turns already have context

Session state is stored inside the container at `~/.claude/` and persists across messages but resets on container restart.

## System Prompt

All runtimes load `system-prompt.md` from the workspace's config directory (`/configs/system-prompt.md`). For Claude Code (SDK executor) and other CLI runtimes, the prompt is re-read on each message (supports hot-reload without restart). A2A delegation instructions are appended automatically.

For LangGraph runtimes, the system prompt is built from multiple sources (config, skills, plugins, peer capabilities) at startup.

## Auth Token Resolution

The CLI executor resolves auth tokens in this order:

1. **Environment variable** — `CLAUDE_AUTH_TOKEN`, `OPENAI_API_KEY`, etc.
2. **Token file** — `/configs/.auth-token` (relative to config dir)

For Claude Code specifically:
- Extract your OAuth access token from the macOS keychain: `security find-generic-password -s "Claude Code-credentials" -a "<username>" -w`
- Write it to `workspace-configs-templates/claude-code-default/.auth-token`
- The provisioner copies this file to each new workspace's config dir

## Auto-Provisioning Without Templates

Workspaces can be created without specifying a `template`. The platform automatically:

1. Creates a config directory (`ws-<id>`) under `workspace-configs-templates/`
2. Generates a minimal `config.yaml` with the workspace's name, role, runtime, and model
3. Copies `.auth-token` from the `claude-code-default` template (if it exists)
4. Merges any files previously uploaded via the Files API
5. Starts the container

This means you can create a workspace with just:
```bash
curl -X POST http://localhost:8080/workspaces \
  -H "Content-Type: application/json" \
  -d '{"name": "My Agent", "role": "Does things", "runtime": "claude-code"}'
```

And it provisions, registers, and comes online automatically.

## Dockerfile

The unified `workspace/Dockerfile` includes both Python and Node.js:

```dockerfile
FROM python:3.11-slim

# Node.js for CLI runtimes (claude-code, codex)
RUN apt-get update && apt-get install -y nodejs
RUN npm install -g @anthropic-ai/claude-code

# Non-root user (claude --dangerously-skip-permissions refuses root)
RUN useradd -m -s /bin/bash agent

# Python deps for LangGraph runtime
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY *.py ./
USER agent
CMD ["python", "main.py"]
```

## Inter-Agent Communication (A2A Delegation)

CLI-based workspaces can communicate with other workspaces via two mechanisms:

### MCP Tools (Claude Code and other MCP-compatible runtimes)

For MCP-compatible runtimes, an A2A MCP server (`a2a_mcp_server.py`) is automatically injected via `--mcp-config`. This gives the agent three MCP tools:

| Tool | Description |
|------|-------------|
| `list_peers` | Discover sibling/parent/child workspaces (name, ID, status, role) |
| `delegate_task` | Send a task to a peer and get their response via A2A |
| `delegate_task_async` | Send a task and return immediately with a task_id (for long tasks) |
| `check_task_status` | Poll an async task's status and get results when done |
| `get_workspace_info` | Get this workspace's own metadata |

The agent uses these tools naturally — no special instructions needed. Access control is enforced by the platform registry.

Example flow: Marketing uses `delegate_task(seo_id, "What is your status?")` → A2A message to SEO → SEO responds → result returned to Marketing.

### Delegation Error Handling

When `delegate_task` receives an error from a child (auth failure, timeout, offline), the MCP server wraps it as a `DELEGATION FAILED` message with instructions for the calling agent to: (1) try a different peer, (2) handle the task itself, or (3) inform the user which peer is unavailable and provide its own best answer. Errors are tagged with a `[A2A_ERROR]` sentinel prefix so they can be reliably distinguished from normal response text. Coordinator prompts and A2A instructions reinforce that agents must never forward raw error messages to the user.

### CLI Commands (Custom runtimes)

For non-MCP runtimes, A2A instructions are injected into the system prompt. The agent uses bash commands:

```bash
a2a peers                          # List available peers
a2a delegate <workspace_id> <task>  # Send task to a peer
a2a info                           # Show workspace info
```

Both approaches use the same backend: platform registry for discovery, A2A protocol for messaging, and access control enforcement (parent↔child, siblings only).

## Workspace Awareness

CLI runtimes keep the same memory tool surface as the Python runtime. When `AWARENESS_URL` and `AWARENESS_NAMESPACE` are injected into the workspace, `commit_memory` and `search_memory` route to the workspace's own awareness namespace instead of the fallback platform memory API. This keeps the agent contract stable while giving each workspace an isolated memory scope.

## Task Status Reporting

Any process inside a workspace container (cron jobs, scripts, background tasks) can update the canvas card display:

```bash
python3 -m molecule_runtime.molecule_ai_status "Running weekly SEO audit..."  # show on canvas
python3 -m molecule_runtime.molecule_ai_status ""                              # clear when done
```

From Python:
```python
from molecule_runtime.molecule_ai_status import set_status
set_status("Analyzing competitor data...")
```

This pushes an immediate heartbeat with `current_task` to the platform, which broadcasts via WebSocket to the canvas. The task banner appears instantly on the workspace card.

## Key Files

| File | Role |
|------|------|
| `main.py` | Runtime selector — discovers adapter, calls setup/create_executor |
| `claude_sdk_executor.py` | `ClaudeSDKExecutor` for Claude Code runtime (SDK-based, replaces subprocess) |
| `executor_helpers.py` | Shared helpers: memory recall/commit, delegation results, heartbeat, system prompt, error sanitization |
| `cli_executor.py` | `CLIAgentExecutor` for Codex, Ollama, custom runtimes (subprocess-based) |
| `a2a_executor.py` | `LangGraphA2AExecutor`, shared `set_current_task()`, `_extract_history()` |
| `adapters/base.py` | `BaseAdapter` interface + `AdapterConfig` dataclass |
| `adapters/__init__.py` | Auto-discovers adapters from subdirectories |
| `molecule_ai_status.py` | CLI tool + module for updating canvas task display from any process |
| `a2a_mcp_server.py` | MCP server exposing A2A delegation tools (list_peers, delegate_task) |
| `a2a_cli.py` | CLI tool for A2A delegation (all runtimes) |
| `config.py` | `RuntimeConfig` dataclass, `runtime` field in `WorkspaceConfig` |

## Rate Limit Handling

Both executors include built-in retry logic with exponential backoff:
- Empty responses (common rate limit signal) → retry up to 3 times (5s, 10s, 20s)
- Rate limit errors (429, "overloaded") → retry with same backoff
- Auth errors (OAuth token transient failures) → retry with backoff
- Timeouts → kill subprocess (CLI) or close stream (SDK) and report (no retry)
- All error messages are sanitized via `sanitize_agent_error()` — no raw stderr or exception details leak to the user chat

The A2A CLI (`a2a_cli.py`) also retries delegation calls on rate limits.

For production with many concurrent agents, consider:
- Using different auth tokens per workspace (separate subscriptions)
- Staggering agent invocations
- Using `delegate_task_async` for long-running tasks

## Known Limitations

- **Tier 1 (sandboxed)**: Read-only root filesystem is disabled for CLI runtimes because Claude Code needs writable directories (`.claude/`, `.npm/`, `/tmp`). Tier 1 still restricts the `/workspace` volume.
- **Rate limits**: All workspaces share the same Claude subscription. Retry logic handles transient rate limits, but sustained high volume needs separate tokens.
- **Auth token lifecycle**: OAuth tokens expire and need refreshing. Use `claude setup-token` for long-lived tokens in production.

## Extending with New Runtimes

To add a new adapter:

1. Create `workspace/adapters/<name>/` with:
   - `adapter.py` — class extending `BaseAdapter` with `setup()` and `create_executor()` methods
   - `requirements.txt` — runtime-specific Python dependencies (installed at container startup)
   - `__init__.py` — exports adapter class as `Adapter`

2. The `create_executor()` method returns an `AgentExecutor` (from `a2a.server.agent_execution`) whose `execute(context, event_queue)` method handles A2A requests.

3. Use `set_current_task()` from `a2a_executor.py` for heartbeat/canvas integration.

4. Use it in config.yaml: `runtime: <name>`
