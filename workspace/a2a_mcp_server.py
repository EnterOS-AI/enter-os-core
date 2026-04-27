#!/usr/bin/env python3
"""A2A MCP Server — runs inside each workspace container.

Exposes A2A delegation, peer discovery, and workspace info as MCP tools
so CLI-based runtimes (Claude Code, Codex) can communicate with other workspaces.

Launched automatically by main.py for CLI runtimes. Runs on stdio transport
and is configured as a local MCP server for the claude --print invocation.

Environment variables (set by the workspace container):
  WORKSPACE_ID  — this workspace's ID
  PLATFORM_URL  — platform API base URL (e.g. http://platform:8080)
"""

import asyncio
import json
import logging
import sys

from a2a_tools import (
    tool_check_task_status,
    tool_commit_memory,
    tool_delegate_task,
    tool_delegate_task_async,
    tool_get_workspace_info,
    tool_list_peers,
    tool_recall_memory,
    tool_send_message_to_user,
)

logger = logging.getLogger(__name__)

# Re-export constants and client functions so existing imports
# (e.g. tests that do `import a2a_mcp_server`) still work.
from a2a_client import (  # noqa: F401, E402
    PLATFORM_URL,
    WORKSPACE_ID,
    _A2A_ERROR_PREFIX,
    _peer_names,
    discover_peer,
    get_peers,
    get_workspace_info,
    send_a2a_message,
)
from a2a_tools import report_activity  # noqa: F401, E402

# --- Tool definitions (schemas) ---

TOOLS = [
    {
        "name": "delegate_task",
        "description": "Delegate a task to another workspace via A2A protocol and WAIT for the response. Use for quick tasks. The target must be a peer (sibling or parent/child). Use list_peers to find available targets.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "workspace_id": {
                    "type": "string",
                    "description": "Target workspace ID (from list_peers)",
                },
                "task": {
                    "type": "string",
                    "description": "The task description to send to the target workspace",
                },
            },
            "required": ["workspace_id", "task"],
        },
    },
    {
        "name": "delegate_task_async",
        "description": "Send a task to another workspace with a short timeout (fire-and-forget). Returns immediately — the target continues processing. Best when you don't need the result right away. Note: check_task_status may not work with all workspace implementations.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "workspace_id": {
                    "type": "string",
                    "description": "Target workspace ID (from list_peers)",
                },
                "task": {
                    "type": "string",
                    "description": "The task description to send to the target workspace",
                },
            },
            "required": ["workspace_id", "task"],
        },
    },
    {
        "name": "check_task_status",
        "description": "Check the status of a previously submitted async task via tasks/get. Note: only works if the target workspace's A2A implementation supports task persistence. May return 'not found' for completed tasks.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "workspace_id": {
                    "type": "string",
                    "description": "The workspace ID the task was sent to",
                },
                "task_id": {
                    "type": "string",
                    "description": "The task_id returned by delegate_task_async",
                },
            },
            "required": ["workspace_id", "task_id"],
        },
    },
    {
        "name": "list_peers",
        "description": "List all workspaces this agent can communicate with (siblings and parent/children). Returns name, ID, status, and role for each peer.",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "get_workspace_info",
        "description": "Get this workspace's own info — ID, name, role, tier, parent, status.",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "send_message_to_user",
        "description": "Send a message directly to the user's canvas chat — pushed instantly via WebSocket. Use this to: (1) acknowledge a task immediately ('Got it, I'll start working on this'), (2) send interim progress updates while doing long work, (3) deliver follow-up results after delegation completes, (4) attach files (zip, pdf, csv, image) for the user to download via the `attachments` field (NEVER paste file URLs in `message`). The message appears in the user's chat as if you're proactively reaching out.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "message": {
                    "type": "string",
                    # The "no URLs in message text" rule is the single biggest
                    # cause of bad chat UX: agents drop catbox.moe / file://
                    # / temporary upload-host links into the prose, the
                    # canvas renders them as plain markdown links the user
                    # can't preview, and SaaS deployments often can't even
                    # reach those external hosts. Every download MUST go
                    # through the structured `attachments` field below.
                    "description": (
                        "Caption text for the chat bubble. Required even when sending "
                        "attachments — set to a short label like 'Here's the build:' "
                        "or 'Done — see attached.'\n\n"
                        "DO NOT paste file URLs, download links, or container paths in "
                        "this string. Files MUST go through the `attachments` field, "
                        "which renders as a clickable download chip and works on SaaS "
                        "deployments where external file-host URLs (catbox.moe, file://, "
                        "etc.) are unreachable from the user's browser."
                    ),
                },
                "attachments": {
                    "type": "array",
                    "description": (
                        "REQUIRED for any file delivery. Pass absolute file paths inside "
                        "THIS container (e.g. ['/tmp/build.zip', '/workspace/report.pdf']) "
                        "— the platform uploads each file and returns a download chip "
                        "with the file's icon + name + size in the user's chat. The chip "
                        "works in SaaS deployments because the URL is platform-served, "
                        "not an external host.\n\n"
                        "USE THIS instead of: pasting URLs in `message`, base64-encoding "
                        "in the body, or telling the user to look at a path on disk. "
                        "If the file isn't already on disk, write it first (Bash, Write "
                        "tool, etc.) then pass its path here. 25 MB per file cap."
                    ),
                    "items": {"type": "string"},
                },
            },
            "required": ["message"],
        },
    },
    {
        "name": "commit_memory",
        "description": "Append a new memory row to persistent storage. Each call CREATES a row — does not overwrite existing memories with the same content. Use to remember decisions, task results, and context that should survive a restart. Scope: LOCAL (this workspace only), TEAM (parent + siblings), GLOBAL (entire org). GLOBAL writes require tier-0 (root) workspace; lower-tier callers get an RBAC error.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "content": {
                    "type": "string",
                    "description": "The information to remember — be detailed and specific",
                },
                "scope": {
                    "type": "string",
                    "enum": ["LOCAL", "TEAM", "GLOBAL"],
                    "description": "Memory scope (default: LOCAL)",
                },
            },
            "required": ["content"],
        },
    },
    {
        "name": "recall_memory",
        "description": "Substring-search persistent memory and return ALL matching rows (no pagination). Empty query returns every memory accessible at the given scope. Server-side filter is case-insensitive substring match on `content`. Use at the start of conversations to recall prior context — calling once with empty query is cheap and avoids missing relevant memories that don't match a narrow keyword.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "query": {
                    "type": "string",
                    "description": "Search query (empty returns all memories)",
                },
                "scope": {
                    "type": "string",
                    "enum": ["LOCAL", "TEAM", "GLOBAL", ""],
                    "description": "Filter by scope (empty returns all accessible)",
                },
            },
        },
    },
]


# --- Tool dispatch ---

async def handle_tool_call(name: str, arguments: dict) -> str:
    """Handle a tool call and return the result as text."""
    if name == "delegate_task":
        return await tool_delegate_task(
            arguments.get("workspace_id", ""),
            arguments.get("task", ""),
        )
    elif name == "delegate_task_async":
        return await tool_delegate_task_async(
            arguments.get("workspace_id", ""),
            arguments.get("task", ""),
        )
    elif name == "check_task_status":
        return await tool_check_task_status(
            arguments.get("workspace_id", ""),
            arguments.get("task_id", ""),
        )
    elif name == "send_message_to_user":
        raw_attachments = arguments.get("attachments")
        attachments: list[str] | None = None
        if isinstance(raw_attachments, list):
            # Defensive: filter to strings only — claude-code SDK occasionally
            # emits dicts here when the model misreads the schema. Drop the
            # bad entries rather than 500 the whole call.
            attachments = [p for p in raw_attachments if isinstance(p, str) and p]
        return await tool_send_message_to_user(
            arguments.get("message", ""),
            attachments=attachments,
        )
    elif name == "list_peers":
        return await tool_list_peers()
    elif name == "get_workspace_info":
        return await tool_get_workspace_info()
    elif name == "commit_memory":
        return await tool_commit_memory(
            arguments.get("content", ""),
            arguments.get("scope", "LOCAL"),
        )
    elif name == "recall_memory":
        return await tool_recall_memory(
            arguments.get("query", ""),
            arguments.get("scope", ""),
        )
    return f"Unknown tool: {name}"


# --- MCP Server (JSON-RPC over stdio) ---

async def main():  # pragma: no cover
    """Run MCP server on stdio — reads JSON-RPC requests, writes responses."""
    reader = asyncio.StreamReader()
    protocol = asyncio.StreamReaderProtocol(reader)
    await asyncio.get_event_loop().connect_read_pipe(lambda: protocol, sys.stdin)

    writer_transport, writer_protocol = await asyncio.get_event_loop().connect_write_pipe(
        asyncio.streams.FlowControlMixin, sys.stdout
    )
    writer = asyncio.StreamWriter(writer_transport, writer_protocol, None, asyncio.get_event_loop())

    async def write_response(response: dict):
        data = json.dumps(response) + "\n"
        writer.write(data.encode())
        await writer.drain()

    buffer = ""
    while True:
        try:
            chunk = await reader.read(65536)
            if not chunk:
                break
            buffer += chunk.decode(errors="replace")

            while "\n" in buffer:
                line, buffer = buffer.split("\n", 1)
                line = line.strip()
                if not line:
                    continue

                try:
                    request = json.loads(line)
                except json.JSONDecodeError:
                    continue

                req_id = request.get("id")
                method = request.get("method", "")

                if method == "initialize":
                    await write_response({
                        "jsonrpc": "2.0",
                        "id": req_id,
                        "result": {
                            "protocolVersion": "2024-11-05",
                            "capabilities": {"tools": {"listChanged": False}},
                            "serverInfo": {"name": "a2a-delegation", "version": "1.0.0"},
                        },
                    })

                elif method == "notifications/initialized":
                    pass  # No response needed

                elif method == "tools/list":
                    await write_response({
                        "jsonrpc": "2.0",
                        "id": req_id,
                        "result": {"tools": TOOLS},
                    })

                elif method == "tools/call":
                    params = request.get("params", {})
                    tool_name = params.get("name", "")
                    tool_args = params.get("arguments", {})
                    result_text = await handle_tool_call(tool_name, tool_args)
                    await write_response({
                        "jsonrpc": "2.0",
                        "id": req_id,
                        "result": {
                            "content": [{"type": "text", "text": result_text}],
                        },
                    })

                else:
                    await write_response({
                        "jsonrpc": "2.0",
                        "id": req_id,
                        "error": {"code": -32601, "message": f"Method not found: {method}"},
                    })

        except Exception as e:
            logger.error(f"MCP server error: {e}")
            break


if __name__ == "__main__":  # pragma: no cover
    asyncio.run(main())
