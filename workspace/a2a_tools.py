"""A2A MCP tool implementations — the body of each tool handler.

Imports shared client functions and constants from a2a_client.
"""

import hashlib
import json
import mimetypes
import os
import uuid

import httpx

from a2a_client import (
    PLATFORM_URL,
    WORKSPACE_ID,
    _A2A_ERROR_PREFIX,
    _peer_names,
    _peer_to_source,
    discover_peer,
    get_peers,
    get_peers_with_diagnostic,
    get_workspace_info,
    send_a2a_message,
)
from builtin_tools.security import _redact_secrets
from platform_auth import list_registered_workspaces


# ---------------------------------------------------------------------------
# RBAC + auth helpers — extracted to a2a_tools_rbac (RFC #2873 iter 4a).
# Re-exported here under the legacy underscore names so existing tests'
# patch("a2a_tools._check_memory_write_permission", …) and call sites
# inside this module that resolve bare names against the module-level
# namespace continue to work unchanged.
# ---------------------------------------------------------------------------
from a2a_tools_rbac import (  # noqa: E402  (import after the from-a2a_client block)
    _auth_headers_for_heartbeat,
    _check_memory_read_permission,
    _check_memory_write_permission,
    _get_workspace_tier,
    _is_root_workspace,
    _ROLE_PERMISSIONS,
)


# Per-field caps on the heartbeat / activity payload. Borrowed from
# hermes-agent's design discipline: cap ONCE in the helper, not at every
# call site, so a future caller adding error_detail can't accidentally
# DoS activity_logs by pasting a 4MB stack trace + base64 image.
#
# Why these specific limits:
#   - error_detail (4096): hermes' value. Long enough for a multi-frame
#     stack trace, short enough that 100 errors in 5min is < 500KB total.
#   - summary (256): summary is a one-liner shown in the canvas card +
#     activity row. 256 covers UTF-8 emoji + a sentence.
#   - response_text (NOT capped): this is the agent's actual reply
#     content. Capping would silently truncate user-visible output.
_MAX_ERROR_DETAIL_CHARS = 4096
_MAX_SUMMARY_CHARS = 256


async def report_activity(
    activity_type: str, target_id: str = "", summary: str = "", status: str = "ok",
    task_text: str = "", response_text: str = "", error_detail: str = "",
):
    """Report activity to the platform for live progress tracking."""
    # Defensive caps in the helper itself so every caller benefits — see
    # _MAX_ERROR_DETAIL_CHARS / _MAX_SUMMARY_CHARS comments above.
    if error_detail and len(error_detail) > _MAX_ERROR_DETAIL_CHARS:
        error_detail = error_detail[:_MAX_ERROR_DETAIL_CHARS]
    if summary and len(summary) > _MAX_SUMMARY_CHARS:
        summary = summary[:_MAX_SUMMARY_CHARS]
    try:
        async with httpx.AsyncClient(timeout=5.0) as client:
            payload: dict = {
                "activity_type": activity_type,
                "source_id": WORKSPACE_ID,
                "target_id": target_id,
                "method": "message/send",
                "summary": summary,
                "status": status,
            }
            if task_text:
                payload["request_body"] = {"task": task_text}
            if response_text:
                payload["response_body"] = {"result": response_text}
            if error_detail:
                # error_detail is a top-level activity row column on the
                # platform (handlers/activity.go). Surfacing the cleaned
                # exception string here lets the Activity tab render a
                # red error chip + the cause without forcing the user
                # to scroll into the raw response_body JSON.
                payload["error_detail"] = error_detail
            await client.post(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/activity",
                json=payload,
                headers=_auth_headers_for_heartbeat(),
            )
            # Also push current_task via heartbeat for canvas card display
            if summary:
                await client.post(
                    f"{PLATFORM_URL}/registry/heartbeat",
                    json={
                        "workspace_id": WORKSPACE_ID,
                        "current_task": summary,
                        "active_tasks": 1,
                        "error_rate": 0,
                        "sample_error": "",
                        "uptime_seconds": 0,
                    },
                    headers=_auth_headers_for_heartbeat(),
                )
    except Exception:
        pass  # Best-effort — don't block delegation on activity reporting


# Delegation tool handlers — extracted to a2a_tools_delegation
# (RFC #2873 iter 4b). Re-imported here so call sites + tests that
# reference ``a2a_tools.tool_delegate_task`` /
# ``a2a_tools._delegate_sync_via_polling`` keep resolving identically.
from a2a_tools_delegation import (  # noqa: E402  (import after the from-a2a_client block)
    _SYNC_POLL_BUDGET_S,
    _SYNC_POLL_INTERVAL_S,
    _delegate_sync_via_polling,
    tool_check_task_status,
    tool_delegate_task,
    tool_delegate_task_async,
)


# Messaging tool handlers — extracted to a2a_tools_messaging
# (RFC #2873 iter 4d). Re-imported here so call sites + tests that
# reference ``a2a_tools.tool_send_message_to_user`` /
# ``tool_list_peers`` / ``tool_get_workspace_info`` /
# ``tool_chat_history`` / ``_upload_chat_files`` keep resolving
# identically.
from a2a_tools_messaging import (  # noqa: E402  (import after the top-of-module imports)
    _upload_chat_files,
    tool_chat_history,
    tool_get_workspace_info,
    tool_list_peers,
    tool_send_message_to_user,
)


# Memory tool handlers — extracted to a2a_tools_memory (RFC #2873 iter 4c).
# Re-imported here so call sites + tests that reference
# ``a2a_tools.tool_commit_memory`` / ``tool_recall_memory`` keep
# resolving identically.
from a2a_tools_memory import (  # noqa: E402  (import after the top-of-module imports)
    tool_commit_memory,
    tool_recall_memory,
)


# ---------------------------------------------------------------------------
# Inbox tools — inbound delivery for the standalone molecule-mcp path.
# ---------------------------------------------------------------------------
#
# The InboxState singleton is set by mcp_cli before the MCP server starts
# (see workspace/inbox.py for the rationale). In-container runtimes never
# call ``inbox.activate(...)``, so ``inbox.get_state()`` returns None and
# these tools surface an informational error rather than raising.
#
# When-to-use guidance (mirrored in platform_tools/registry.py): agents
# in standalone-runtime mode should call ``wait_for_message`` to block
# on the next inbound message after they've emitted a reply, forming
# the loop ``wait → respond → wait``. ``inbox_peek`` is for inspecting
# the queue without consuming; ``inbox_pop`` removes a handled message.

_INBOX_NOT_ENABLED_MSG = (
    "Error: inbox polling is not enabled in this runtime. The standalone "
    "molecule-mcp wrapper activates it; in-container runtimes receive "
    "messages via push delivery and do not need these tools."
)


def _enrich_inbound_for_agent(d: dict) -> dict:
    """Add peer_name / peer_role / agent_card_url to a poll-path message.

    The PUSH path (a2a_mcp_server._build_channel_notification) already
    enriches the meta dict with these fields, so a Claude Code host
    with channel-push sees them. The POLL path goes through
    InboxMessage.to_dict, which is intentionally identity-free (the
    storage layer doesn't know about the registry cache). Without this
    helper, every non-Claude-Code MCP client that uses inbox_peek /
    wait_for_message gets a plain message and the receiving agent
    can't tell who's writing — breaking the contract documented in
    a2a_mcp_server.py:303-345 ("In both paths the same fields apply").

    Cache-first non-blocking enrichment (same shape as push): on cache
    miss the helper returns the bare message; the next call within the
    5-min TTL hits the warm cache. Failure to enrich is non-fatal —
    the agent still gets text + peer_id + kind + activity_id, just
    without the friendly identity.
    """
    peer_id = d.get("peer_id") or ""
    if not peer_id:
        # canvas_user — no peer to enrich; helper returns the plain
        # message unchanged so the canvas reply path still works.
        return d
    try:
        from a2a_client import (  # local import — avoid module-load cycle
            _agent_card_url_for,
            enrich_peer_metadata_nonblocking,
        )
    except Exception:  # noqa: BLE001
        # If a2a_client is unavailable (test harness, partial install),
        # degrade gracefully — agent still gets the bare envelope.
        return d
    record = enrich_peer_metadata_nonblocking(peer_id)
    if record is not None:
        if name := record.get("name"):
            d["peer_name"] = name
        if role := record.get("role"):
            d["peer_role"] = role
    # agent_card_url is constructable from peer_id alone — surface it
    # even when registry enrichment misses, so the receiving agent has
    # a single endpoint to hit for the peer's full capability list.
    d["agent_card_url"] = _agent_card_url_for(peer_id)
    return d

async def tool_inbox_peek(limit: int = 10) -> str:
    """Return up to ``limit`` pending inbound messages without removing them."""
    import inbox  # local import — avoids a circular dep at module load

    state = inbox.get_state()
    if state is None:
        return _INBOX_NOT_ENABLED_MSG
    messages = state.peek(limit=limit if isinstance(limit, int) else 10)
    return json.dumps([_enrich_inbound_for_agent(m.to_dict()) for m in messages])


async def tool_inbox_pop(activity_id: str) -> str:
    """Remove a message from the inbox queue by activity_id."""
    import inbox

    state = inbox.get_state()
    if state is None:
        return _INBOX_NOT_ENABLED_MSG
    if not isinstance(activity_id, str) or not activity_id:
        return "Error: activity_id is required."
    removed = state.pop(activity_id)
    if removed is None:
        return json.dumps({"removed": False, "activity_id": activity_id})
    return json.dumps({"removed": True, "activity_id": activity_id})


async def tool_wait_for_message(timeout_secs: float = 60.0) -> str:
    """Block until a new message arrives or ``timeout_secs`` elapses.

    Returns the head message non-destructively; the agent decides
    whether to ``inbox_pop`` it after acting.
    """
    import asyncio

    import inbox

    state = inbox.get_state()
    if state is None:
        return _INBOX_NOT_ENABLED_MSG

    try:
        timeout = float(timeout_secs)
    except (TypeError, ValueError):
        timeout = 60.0
    # Cap at 300s — Claude Code's default tool timeout is ~10min, and
    # blocking longer than 5min wastes the prompt cache window for
    # nothing useful. Operators who want longer can call repeatedly.
    timeout = max(0.0, min(timeout, 300.0))

    # The threading.Event-based wait would block the asyncio loop.
    # Run it on the default executor so the MCP server can keep
    # processing other JSON-RPC requests while we sleep.
    loop = asyncio.get_running_loop()
    message = await loop.run_in_executor(None, state.wait, timeout)
    if message is None:
        return json.dumps({"timeout": True, "timeout_secs": timeout})
    return json.dumps(_enrich_inbound_for_agent(message.to_dict()))
