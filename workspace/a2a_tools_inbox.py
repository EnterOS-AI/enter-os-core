"""Inbox tool handlers — single-concern slice of the a2a_tools surface.

Standalone-runtime path for inbound-message delivery (push-mode runtimes
get messages via the channel-tag synthesis in a2a_mcp_server). The
``InboxState`` singleton is set by ``mcp_cli`` before the MCP server
starts; in-container runtimes never call ``inbox.activate(...)`` so
``inbox.get_state()`` returns None and these tools surface an
informational error instead of raising.

When-to-use guidance for agents (mirrored in
``platform_tools/registry.py``):
  - ``wait_for_message``: block until a new inbound message arrives, then
    decide what to do with it; forms the loop ``wait → respond → wait``.
  - ``inbox_peek``: inspect the queue non-destructively.
  - ``inbox_pop``: remove a handled message by activity_id.

Extracted from ``a2a_tools.py`` in RFC #2873 iter 4e so the kitchen-sink
module shrinks to a back-compat shim. The extraction also makes the
``_enrich_inbound_for_agent`` helper unit-testable in isolation —
previously it was buried in ``a2a_tools`` and only exercised through
the inbox wrappers, leaving its peer-id-empty / cache-miss / registry-
unavailable branches under-covered.
"""
from __future__ import annotations

import asyncio
import json


# Surfaced when the inbox subsystem is not initialised. Returned by the
# three inbox tool wrappers below so the agent gets a clear "this
# runtime delivers via push" message instead of a NameError.
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
