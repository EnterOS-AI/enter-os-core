"""Messaging tool handlers — single-concern slice of the a2a_tools surface.

Extracted from ``a2a_tools.py`` (RFC #2873 iter 4d). Owns the four
human-and-peer messaging MCP tools + the chat-upload helper they share:

  * ``tool_send_message_to_user`` — push a canvas-chat message via the
    platform's ``/notify`` endpoint.
  * ``tool_list_peers`` — discover peers across one or many registered
    workspaces, with side-effect of populating ``_peer_to_source`` for
    delegate-task auto-routing.
  * ``tool_get_workspace_info`` — JSON-encode the workspace's own info.
  * ``tool_chat_history`` — fetch prior conversation rows with a peer.
  * ``_upload_chat_files`` — internal helper for the message-attachments
    code path; routes local file paths through the platform's
    ``/chat/uploads`` so the canvas can render them as download chips.

Imports the auth-header primitive from ``a2a_tools_rbac`` (iter 4a).
"""
from __future__ import annotations

import json
import mimetypes
import os

import httpx

from a2a_client import (
    PLATFORM_URL,
    WORKSPACE_ID,
    _peer_names,
    _peer_to_source,
    get_peers_with_diagnostic,
    get_workspace_info,
)
from a2a_tools_rbac import auth_headers_for_heartbeat as _auth_headers_for_heartbeat
from platform_auth import list_registered_workspaces


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
            default of 20 covers "most recent context for this peer"
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
