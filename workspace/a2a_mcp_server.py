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

# Top-level (not inside main()) so the wheel rewriter expands this to
# `import molecule_runtime.inbox as inbox`. A local `import inbox as _x`
# would expand to `import molecule_runtime.inbox as inbox as _x`,
# which is invalid — see scripts/build_runtime_package.py:rewrite_imports.
import inbox

from a2a_tools import (
    tool_check_task_status,
    tool_commit_memory,
    tool_delegate_task,
    tool_delegate_task_async,
    tool_get_workspace_info,
    tool_inbox_peek,
    tool_inbox_pop,
    tool_list_peers,
    tool_recall_memory,
    tool_send_message_to_user,
    tool_wait_for_message,
)
from platform_tools.registry import TOOLS as _PLATFORM_TOOL_SPECS

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
#
# Built once at import time from the platform_tools registry. The MCP
# `description` field is the spec's `short` line — that's the unified
# tool description used by both the MCP tool listing AND the bullet
# rendering in the agent-facing system-prompt section. The deeper
# `when_to_use` guidance is appended to the system prompt only (it's
# too long to live in MCP `description` without bloating every
# tool-list response the model sees).

TOOLS = [
    {
        "name": _spec.name,
        "description": _spec.short,
        "inputSchema": _spec.input_schema,
    }
    for _spec in _PLATFORM_TOOL_SPECS
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
    elif name == "wait_for_message":
        return await tool_wait_for_message(
            arguments.get("timeout_secs", 60.0),
        )
    elif name == "inbox_peek":
        return await tool_inbox_peek(
            arguments.get("limit", 10),
        )
    elif name == "inbox_pop":
        return await tool_inbox_pop(
            arguments.get("activity_id", ""),
        )
    return f"Unknown tool: {name}"


# --- MCP Notification bridge ---

# `notifications/claude/channel` matches the contract used by the
# molecule-mcp-claude-channel bun bridge (server.ts:509). Claude Code's
# MCP runtime treats this method as a conversation interrupt — `content`
# becomes the agent turn, `meta` is structured metadata. Notification-
# capable hosts (Claude Code today; any compliant client tomorrow)
# get push UX automatically; pollers (`wait_for_message` / `inbox_peek`)
# still work unchanged. See task #46 + the deprecation path documented
# in workspace/inbox.py:set_notification_callback.
_CHANNEL_NOTIFICATION_METHOD = "notifications/claude/channel"


def _build_initialize_result() -> dict:
    """MCP initialize handshake result.

    The ``experimental.claude/channel`` capability declaration is what
    tells Claude Code's MCP client to route our
    ``notifications/claude/channel`` emissions as conversation
    interrupts (push UX). Without it the notification arrives over the
    wire but is silently dropped instead of becoming a ``<channel>``
    tag in the next agent turn — matching the
    "Notification arrives but Claude Code doesn't surface it" failure
    mode anticipated in molecule-core#2444. Mirrors the contract
    declared by the molecule-mcp-claude-channel bun bridge
    (server.ts:374).
    """
    return {
        "protocolVersion": "2024-11-05",
        "capabilities": {
            "tools": {"listChanged": False},
            "experimental": {"claude/channel": {}},
        },
        "serverInfo": {"name": "a2a-delegation", "version": "1.0.0"},
    }


def _build_channel_notification(msg: dict) -> dict:
    """Transform an ``InboxMessage.to_dict()`` into the MCP notification
    envelope expected by Claude Code's channel-bridge contract.

    Pure function so the wire shape is unit-testable without spinning
    up an asyncio loop. The wire-up in ``main()`` just composes this
    with ``asyncio.run_coroutine_threadsafe``.
    """
    return {
        "jsonrpc": "2.0",
        "method": _CHANNEL_NOTIFICATION_METHOD,
        "params": {
            "content": msg.get("text", ""),
            "meta": {
                "source": "molecule",
                "kind": msg.get("kind", ""),
                "peer_id": msg.get("peer_id", ""),
                "method": msg.get("method", ""),
                "activity_id": msg.get("activity_id", ""),
                "ts": msg.get("created_at", ""),
            },
        },
    }


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

    # Wire the inbox → MCP notification bridge. Inbox poller (daemon
    # thread) calls into here when a new activity row lands; we
    # schedule the notification onto the asyncio loop and best-effort
    # fire it on the same stdout the responses go to.
    loop = asyncio.get_running_loop()

    async def _emit_notification(payload: dict) -> None:
        data = json.dumps(payload) + "\n"
        writer.write(data.encode())
        try:
            await writer.drain()
        except Exception:  # noqa: BLE001
            # Closed pipe (host disconnected) shouldn't crash the
            # inbox poller; let it sit until the host reconnects.
            pass

    def _on_inbox_message(msg: dict) -> None:
        try:
            asyncio.run_coroutine_threadsafe(
                _emit_notification(_build_channel_notification(msg)),
                loop,
            )
        except RuntimeError:
            # Loop closed during shutdown — best-effort, swallow.
            pass

    inbox.set_notification_callback(_on_inbox_message)

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
                        "result": _build_initialize_result(),
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


def cli_main() -> None:  # pragma: no cover
    """Synchronous wrapper around the async MCP stdio loop.

    Called by ``mcp_cli.main`` (the ``molecule-mcp`` console-script
    entry point in scripts/build_runtime_package.py) AFTER env
    validation and the standalone register + heartbeat thread setup.
    Direct callers (in-container code that already validated env and
    runs heartbeat.py separately) can also invoke this — it's the
    smallest possible "run the MCP stdio JSON-RPC loop" surface.

    Wheel-smoke gates in scripts/wheel_smoke.py pin the importability
    of this name (alongside ``mcp_cli.main``) so a silent rename can't
    break every external-runtime operator's MCP install — the 0.1.16
    ``main_sync`` rename incident is the cautionary precedent.
    """
    asyncio.run(main())


if __name__ == "__main__":  # pragma: no cover
    cli_main()
