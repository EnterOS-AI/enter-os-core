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


async def _upload_chat_files(
    client: httpx.AsyncClient,
    paths: list[str],
    workspace_id: str | None = None,
) -> tuple[list[dict], str | None]:
    """Upload local file paths through /workspaces/<self>/chat/uploads.

    The platform stages each upload under /workspace/.molecule/chat-uploads
    (an "allowed root" the canvas knows how to render via the Download
    endpoint) and returns metadata the broadcast payload references.

    Why we route through upload instead of just passing the agent's path:
    the canvas's allowed-root list is /configs, /workspace, /home, /plugins
    — files at /tmp or /root would be unreachable. Uploading copies the
    bytes into an allowed root regardless of where the agent wrote them.

    Returns (attachments, error). On any failure the caller should NOT
    fire the notify — partial-attach would surface a half-rendered chip.
    """
    if not paths:
        return [], None
    files_payload: list[tuple[str, tuple[str, bytes, str]]] = []
    for p in paths:
        if not isinstance(p, str) or not p:
            return [], f"Error: invalid attachment path {p!r}"
        if not os.path.isfile(p):
            return [], f"Error: attachment not found: {p}"
        try:
            with open(p, "rb") as fh:
                data = fh.read()
        except OSError as e:
            return [], f"Error reading {p}: {e}"
        # Sniff mime from filename so the canvas can pick the right
        # icon / preview / inline-image renderer. Pre-fix this was
        # hardcoded application/octet-stream and chat_files.go's
        # Upload trusts whatever Content-Type the multipart part
        # carries — `mt := fh.Header.Get("Content-Type")` only falls
        # back to extension-sniffing when the header is empty. So a
        # hardcoded octet-stream meant every attachment lost its
        # real type forever, breaking the canvas chip's icon logic.
        mime_type, _ = mimetypes.guess_type(p)
        if not mime_type:
            mime_type = "application/octet-stream"
        files_payload.append(("files", (os.path.basename(p), data, mime_type)))
    target_workspace_id = (workspace_id or "").strip() or WORKSPACE_ID
    try:
        resp = await client.post(
            f"{PLATFORM_URL}/workspaces/{target_workspace_id}/chat/uploads",
            files=files_payload,
            headers=_auth_headers_for_heartbeat(target_workspace_id),
        )
    except Exception as e:
        return [], f"Error uploading attachments: {e}"
    if resp.status_code != 200:
        return [], f"Error: chat/uploads returned {resp.status_code}: {resp.text[:200]}"
    try:
        body = resp.json()
    except Exception as e:
        return [], f"Error parsing upload response: {e}"
    uploaded = body.get("files") or []
    if not isinstance(uploaded, list) or len(uploaded) != len(paths):
        return [], f"Error: upload returned {len(uploaded) if isinstance(uploaded, list) else 'invalid'} entries for {len(paths)} files"
    return uploaded, None


async def tool_send_message_to_user(
    message: str,
    attachments: list[str] | None = None,
    workspace_id: str | None = None,
) -> str:
    """Send a message directly to the user's canvas chat via WebSocket.

    Args:
        message: The text to display in the user's chat. Required even
            when sending attachments — set to a short caption like
            "Here's the build output:" or "Done — see attached."
        attachments: Optional list of absolute file paths inside this
            container. Each is uploaded to the platform and rendered
            in the canvas as a clickable download chip. Use this
            instead of pasting paths in the message text — paths
            render as plain text and the user can't click them.
            Examples:
              attachments=["/tmp/build-output.zip"]
              attachments=["/workspace/report.pdf", "/workspace/data.csv"]
        workspace_id: Optional. When the agent is registered in MULTIPLE
            workspaces (external multi-workspace MCP path), this
            selects which workspace's chat to deliver the message to —
            should match the ``arrival_workspace_id`` of the inbound
            message you're replying to so the user sees the reply in
            the same canvas they typed in. Single-workspace agents
            omit this; the message routes to the only registered
            workspace.
    """
    if not message:
        return "Error: message is required"
    target_workspace_id = (workspace_id or "").strip() or WORKSPACE_ID
    try:
        async with httpx.AsyncClient(timeout=60.0) as client:
            uploaded, upload_err = await _upload_chat_files(
                client, attachments or [], workspace_id=target_workspace_id,
            )
            if upload_err:
                return upload_err
            payload: dict = {"message": message}
            if uploaded:
                payload["attachments"] = uploaded
            resp = await client.post(
                f"{PLATFORM_URL}/workspaces/{target_workspace_id}/notify",
                json=payload,
                headers=_auth_headers_for_heartbeat(target_workspace_id),
            )
            if resp.status_code == 200:
                if uploaded:
                    return f"Message sent to user with {len(uploaded)} attachment(s)"
                return "Message sent to user"
            return f"Error: platform returned {resp.status_code}"
    except Exception as e:
        return f"Error sending message: {e}"


async def tool_list_peers(source_workspace_id: str | None = None) -> str:
    """List all workspaces this agent can communicate with.

    Behavior:
        - ``source_workspace_id`` set → list peers of that one workspace.
        - Unset, single-workspace mode → list peers of WORKSPACE_ID
          (the legacy path, unchanged).
        - Unset, multi-workspace mode (MOLECULE_WORKSPACES populated) →
          aggregate across every registered workspace, prefixing each
          peer with its source so the agent / user can see the full peer
          surface in one call.

    Side-effect: populates ``_peer_to_source`` so subsequent
    ``tool_delegate_task(target)`` auto-routes through the correct
    sending workspace without the agent needing ``source_workspace_id``.
    """
    sources: list[str]
    aggregate = False
    if source_workspace_id:
        sources = [source_workspace_id]
    else:
        registered = list_registered_workspaces()
        if len(registered) > 1:
            sources = registered
            aggregate = True
        else:
            sources = [WORKSPACE_ID]

    all_peers: list[tuple[str, dict]] = []  # (source, peer_record)
    diagnostics: list[tuple[str, str]] = []  # (source, diagnostic)
    for src in sources:
        peers, diagnostic = await get_peers_with_diagnostic(source_workspace_id=src)
        if peers:
            for p in peers:
                all_peers.append((src, p))
        elif diagnostic is not None:
            diagnostics.append((src, diagnostic))

    if not all_peers:
        if diagnostics:
            joined = "; ".join(f"[{src[:8]}] {d}" for src, d in diagnostics)
            return f"No peers found. {joined}"
        return (
            "You have no peers in the platform registry. "
            "(No parent, no children, no siblings registered.)"
        )

    lines = []
    for src, p in all_peers:
        status = p.get("status", "unknown")
        role = p.get("role", "")
        peer_id = p["id"]
        # Cache name for use in delegate_task
        _peer_names[peer_id] = p["name"]
        # Cache the source workspace so tool_delegate_task auto-routes
        _peer_to_source[peer_id] = src
        if aggregate:
            lines.append(
                f"- {p['name']} (ID: {peer_id}, status: {status}, role: {role}, via: {src[:8]})"
            )
        else:
            lines.append(f"- {p['name']} (ID: {peer_id}, status: {status}, role: {role})")
    return "\n".join(lines)


async def tool_get_workspace_info(source_workspace_id: str | None = None) -> str:
    """Get this workspace's own info.

    ``source_workspace_id`` selects which registered workspace to
    introspect when the agent is registered into multiple workspaces.
    Unset → falls back to module-level WORKSPACE_ID.
    """
    info = await get_workspace_info(source_workspace_id=source_workspace_id)
    return json.dumps(info, indent=2)


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


async def tool_chat_history(
    peer_id: str,
    limit: int = 20,
    before_ts: str = "",
    source_workspace_id: str | None = None,
) -> str:
    """Fetch the prior conversation with one peer.

    Hits ``/workspaces/<self>/activity?peer_id=<peer>&limit=<N>``
    against the workspace-server, which returns activity rows where
    the peer is either the sender (``source_id=peer`` — they sent us
    the message) or the recipient (``target_id=peer`` — we sent to
    them) of an A2A turn — both sides of the conversation in
    chronological order.

    Args:
        peer_id: The other workspace's UUID. Same value the agent
            sees as ``peer_id`` on a peer_agent push or ``workspace_id``
            on a delegate_task call.
        limit: Maximum rows to return; capped server-side at 500. The
            default of 20 covers \"most recent context for this peer\"
            without flooding the agent's context window.
        before_ts: Optional RFC3339 timestamp; only rows strictly
            older are returned. Used to page backward through long
            histories — pass the oldest ``ts`` from the previous
            response. Empty (default) returns the most recent ``limit``
            rows.
        source_workspace_id: Which registered workspace's activity log
            to query. Auto-routes via ``_peer_to_source`` cache when
            unset (the workspace this peer was discovered through);
            falls back to module-level WORKSPACE_ID for single-workspace
            operators.

    Returns a JSON-encoded list of activity rows (or an error string
    starting with ``Error:`` so the agent can branch). Each row carries
    ``activity_type``, ``source_id``, ``target_id``, ``method``,
    ``summary``, ``request_body``, ``response_body``, ``status``,
    ``created_at`` — same shape ``inbox_peek`` and the canvas chat
    loader already see.
    """
    if not peer_id or not isinstance(peer_id, str):
        return "Error: peer_id is required"
    if not isinstance(limit, int) or limit <= 0:
        limit = 20
    if limit > 500:
        limit = 500

    src = source_workspace_id or _peer_to_source.get(peer_id) or WORKSPACE_ID

    params: dict[str, str] = {
        "peer_id": peer_id,
        "limit": str(limit),
    }
    # Forward verbatim — the server route validates as RFC3339 at the
    # trust boundary and translates into a `created_at < $X` clause.
    if before_ts:
        params["before_ts"] = before_ts

    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.get(
                f"{PLATFORM_URL}/workspaces/{src}/activity",
                params=params,
                headers=_auth_headers_for_heartbeat(src),
            )
    except Exception as exc:  # noqa: BLE001
        return f"Error: chat_history request failed: {exc}"

    if resp.status_code == 400:
        # Trust-boundary rejection (malformed peer_id, etc.) — surface
        # the server's reason verbatim so the agent can correct itself.
        try:
            err = resp.json().get("error", "bad request")
        except Exception:  # noqa: BLE001
            err = "bad request"
        return f"Error: {err}"
    if resp.status_code >= 400:
        return f"Error: chat_history returned HTTP {resp.status_code}"

    try:
        rows = resp.json()
    except Exception:  # noqa: BLE001
        return "Error: chat_history response was not JSON"
    if not isinstance(rows, list):
        return "Error: chat_history response was not a list"

    # Server returns DESC (most recent first); reverse to chronological
    # so the agent reads the conversation top-down like a chat log.
    rows.reverse()
    return json.dumps(rows)


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
