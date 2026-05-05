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


# RFC #2829 PR-5 cutover constants. The poll cadence + timeout are
# intentionally generous: 3s gives the platform's executeDelegation
# goroutine room to dispatch + the callee to respond + the result to
# write to activity_logs without thrashing the platform with rapid
# polls; the budget matches the legacy DELEGATION_TIMEOUT (300s) so
# operators don't see behavior change beyond "no more 600s timeouts".
_SYNC_POLL_INTERVAL_S = 3.0
_SYNC_POLL_BUDGET_S = float(os.environ.get("DELEGATION_TIMEOUT", "300.0"))


async def _delegate_sync_via_polling(
    workspace_id: str,
    task: str,
    src: str,
) -> str:
    """RFC #2829 PR-5: durable async delegation + poll for terminal status.

    Sidesteps the platform proxy's blocking `message/send` HTTP path that
    hits a hard 600s ceiling. Instead:

      1. POST /workspaces/<src>/delegate (async, returns 202 + delegation_id)
         — platform's executeDelegation goroutine handles A2A dispatch in
         the background. No client-side timeout dependency on the platform
         holding a connection open.
      2. Poll GET /workspaces/<src>/delegations every 3s for a row with
         matching delegation_id reaching terminal status (completed/failed).
      3. Return the response_preview text on completed; surface error_detail
         on failed (with the same _A2A_ERROR_PREFIX wrapping the legacy
         path uses, so caller error-detection logic is unchanged).

    Both /delegate and /delegations are existing endpoints — this helper
    just composes them into a polling synchronous facade. The result is
    available the moment the platform writes the terminal status row;
    no extra latency vs. the legacy proxy-blocked path on fast cases.
    """
    import asyncio
    import time

    idem_key = hashlib.sha256(f"{src}:{workspace_id}:{task}".encode()).hexdigest()[:32]

    # 1. Dispatch via /delegate (the async, durable path).
    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.post(
                f"{PLATFORM_URL}/workspaces/{src}/delegate",
                json={
                    "target_id": workspace_id,
                    "task": task,
                    "idempotency_key": idem_key,
                },
                headers=_auth_headers_for_heartbeat(src),
            )
    except Exception as e:  # pylint: disable=broad-except
        return f"{_A2A_ERROR_PREFIX}delegate dispatch failed: {e}"

    if resp.status_code != 202 and resp.status_code != 200:
        return f"{_A2A_ERROR_PREFIX}delegate dispatch failed: HTTP {resp.status_code} {resp.text[:200]}"

    try:
        dispatch = resp.json()
    except Exception as e:  # pylint: disable=broad-except
        return f"{_A2A_ERROR_PREFIX}delegate dispatch returned non-JSON: {e}"

    delegation_id = dispatch.get("delegation_id", "")
    if not delegation_id:
        return f"{_A2A_ERROR_PREFIX}delegate dispatch missing delegation_id: {dispatch}"

    # 2. Poll for terminal status with a deadline. Each poll is a cheap
    # /delegations GET — bounded by the platform's existing rate limit.
    deadline = time.monotonic() + _SYNC_POLL_BUDGET_S
    last_status = "unknown"
    while time.monotonic() < deadline:
        try:
            async with httpx.AsyncClient(timeout=10.0) as client:
                poll = await client.get(
                    f"{PLATFORM_URL}/workspaces/{src}/delegations",
                    headers=_auth_headers_for_heartbeat(src),
                )
        except Exception as e:  # pylint: disable=broad-except
            # Transient — keep polling. The platform IS holding the
            # delegation row; we just lost a network request.
            last_status = f"poll-error: {e}"
            await asyncio.sleep(_SYNC_POLL_INTERVAL_S)
            continue

        if poll.status_code != 200:
            last_status = f"poll HTTP {poll.status_code}"
            await asyncio.sleep(_SYNC_POLL_INTERVAL_S)
            continue

        try:
            rows = poll.json()
        except Exception as e:  # pylint: disable=broad-except
            last_status = f"poll non-JSON: {e}"
            await asyncio.sleep(_SYNC_POLL_INTERVAL_S)
            continue

        # /delegations returns a flat list of delegation events. Filter to
        # our delegation_id; pick the first terminal one. The list may
        # have multiple rows per delegation_id (one for the original
        # dispatch, one per status update); we want the latest terminal.
        if not isinstance(rows, list):
            await asyncio.sleep(_SYNC_POLL_INTERVAL_S)
            continue
        terminal = None
        for r in rows:
            if not isinstance(r, dict):
                continue
            if r.get("delegation_id") != delegation_id:
                continue
            status = (r.get("status") or "").lower()
            last_status = status
            if status in ("completed", "failed"):
                terminal = r
                break
        if terminal:
            if (terminal.get("status") or "").lower() == "completed":
                return terminal.get("response_preview") or ""
            err = (
                terminal.get("error_detail")
                or terminal.get("summary")
                or "delegation failed"
            )
            return f"{_A2A_ERROR_PREFIX}{err}"

        await asyncio.sleep(_SYNC_POLL_INTERVAL_S)

    # Budget exhausted — the platform's row is still in flight (or queued).
    # Surface as an error so the caller can decide to retry or fall back;
    # the platform DOES still have the durable row, so the work isn't
    # lost — it'll complete eventually and a future check_task_status
    # will surface the result.
    return (
        f"{_A2A_ERROR_PREFIX}polling timeout after {_SYNC_POLL_BUDGET_S}s "
        f"(delegation_id={delegation_id}, last_status={last_status}); "
        f"the platform is still working on it — call check_task_status('{delegation_id}') to retrieve later"
    )


async def tool_delegate_task(
    workspace_id: str,
    task: str,
    source_workspace_id: str | None = None,
) -> str:
    """Delegate a task to another workspace via A2A (synchronous — waits for response).

    ``source_workspace_id`` selects which registered workspace this
    delegation originates from — drives auth + the X-Workspace-ID source
    header so the platform's a2a_proxy logs the correct sender. Single-
    workspace operators leave it None and routing falls back to the
    module-level WORKSPACE_ID.
    """
    if not workspace_id or not task:
        return "Error: workspace_id and task are required"

    # Auto-route: if source not specified, look up which registered
    # workspace last saw this peer (populated by tool_list_peers). Falls
    # back to the legacy WORKSPACE_ID for single-workspace operators.
    src = source_workspace_id or _peer_to_source.get(workspace_id) or None

    # Discover the target. discover_peer is the access-control gate +
    # name/status lookup. The peer's reported ``url`` field is NOT used
    # for routing — see send_a2a_message, which constructs the URL via
    # the platform's A2A proxy.
    peer = await discover_peer(workspace_id, source_workspace_id=src)
    if not peer:
        return f"Error: workspace {workspace_id} not found or not accessible (check access control)"

    if (peer.get("status") or "").lower() == "offline":
        return f"Error: workspace {workspace_id} is offline"

    # Report delegation start — include the task text for traceability
    peer_name = peer.get("name") or _peer_names.get(workspace_id) or workspace_id[:8]
    _peer_names[workspace_id] = peer_name  # cache for future use
    # Brief summary for canvas display — just the delegation target
    await report_activity("a2a_send", workspace_id, f"Delegating to {peer_name}", task_text=task)

    # RFC #2829 PR-5: agent-side cutover. When DELEGATION_SYNC_VIA_INBOX=1,
    # use the platform's durable async delegation API (POST /delegate +
    # poll /delegations) instead of the proxy-blocked message/send path.
    # This sidesteps the 600s message/send timeout class that broke
    # iteration-14/90-style long-running delegations on 2026-05-05.
    #
    # Default off — staging-canary first, flip default after PR-2's
    # result-push flag (DELEGATION_RESULT_INBOX_PUSH) has been on for
    # ≥1 week without incident.
    if os.environ.get("DELEGATION_SYNC_VIA_INBOX") == "1":
        result = await _delegate_sync_via_polling(workspace_id, task, src or WORKSPACE_ID)
    else:
        # send_a2a_message routes through ${PLATFORM_URL}/workspaces/{id}/a2a
        # (the platform proxy) so the same code works for in-container and
        # external (standalone molecule-mcp) callers.
        result = await send_a2a_message(workspace_id, task, source_workspace_id=src)

    # Detect delegation failures — wrap them clearly so the calling agent
    # can decide to retry, use another peer, or handle the task itself.
    is_error = result.startswith(_A2A_ERROR_PREFIX)
    # Strip the sentinel prefix so error_detail is the human-readable
    # cause directly. The Activity tab's red error chip surfaces this
    # without the user having to scroll into the raw response JSON.
    #
    # Cap at 4096 chars before sending — the platform's
    # activity_logs.error_detail column is unbounded TEXT and a
    # malicious or buggy peer could otherwise stream an arbitrarily
    # large error message into the caller's activity log. 4096 is
    # comfortably above any real exception traceback we've seen and
    # well below an obvious-DoS threshold.
    error_detail = result[len(_A2A_ERROR_PREFIX):].strip()[:4096] if is_error else ""
    await report_activity(
        "a2a_receive", workspace_id,
        f"{peer_name} responded ({len(result)} chars)" if not is_error else f"{peer_name} failed: {error_detail[:120]}",
        task_text=task, response_text=result,
        status="error" if is_error else "ok",
        error_detail=error_detail,
    )
    if is_error:
        return (
            f"DELEGATION FAILED to {peer_name}: {result}\n"
            f"You should either: (1) try a different peer, (2) handle this task yourself, "
            f"or (3) inform the user that {peer_name} is unavailable and provide your best answer."
        )
    return result


async def tool_delegate_task_async(
    workspace_id: str,
    task: str,
    source_workspace_id: str | None = None,
) -> str:
    """Delegate a task via the platform's async delegation API (fire-and-forget).

    Uses POST /workspaces/:id/delegate which runs the A2A request in the background.
    Results are tracked in the platform DB and broadcast via WebSocket.
    Use check_task_status to poll for results.

    ``source_workspace_id`` selects the sending workspace (which one of
    this agent's registered workspaces gets logged as the originator);
    auto-routes via the peer→source cache when omitted.
    """
    if not workspace_id or not task:
        return "Error: workspace_id and task are required"

    src = source_workspace_id or _peer_to_source.get(workspace_id) or WORKSPACE_ID

    # Idempotency key: SHA-256 of (source, target, task) so that a
    # restarted agent firing the same delegation gets the same key and
    # the platform returns the existing delegation_id instead of
    # creating a duplicate. Fixes #1456. Source is in the key so the
    # SAME task delegated from two different registered workspaces
    # produces two distinct delegations (the right behavior — one per
    # tenant audit trail).
    idem_key = hashlib.sha256(f"{src}:{workspace_id}:{task}".encode()).hexdigest()[:32]

    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.post(
                f"{PLATFORM_URL}/workspaces/{src}/delegate",
                json={"target_id": workspace_id, "task": task, "idempotency_key": idem_key},
                headers=_auth_headers_for_heartbeat(src),
            )
            if resp.status_code == 202:
                data = resp.json()
                return json.dumps({
                    "delegation_id": data.get("delegation_id", ""),
                    "workspace_id": workspace_id,
                    "status": "delegated",
                    "note": "Task delegated. The platform runs it in the background. Use check_task_status to poll for results.",
                })
            else:
                return f"Error: delegation failed with status {resp.status_code}: {resp.text[:200]}"
    except Exception as e:
        return f"Error: delegation failed — {e}"


async def tool_check_task_status(
    workspace_id: str,
    task_id: str,
    source_workspace_id: str | None = None,
) -> str:
    """Check delegations for this workspace via the platform API.

    Args:
        workspace_id: Ignored (kept for backward compat). Checks
            ``source_workspace_id``'s delegations (the workspace that
            FIRED the delegations), not the target's.
        task_id: Optional delegation_id to filter. If empty, returns all recent delegations.
        source_workspace_id: Which registered workspace's delegation log
            to query. Defaults to the module-level WORKSPACE_ID.
    """
    src = source_workspace_id or WORKSPACE_ID
    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.get(
                f"{PLATFORM_URL}/workspaces/{src}/delegations",
                headers=_auth_headers_for_heartbeat(src),
            )
            if resp.status_code != 200:
                return f"Error: failed to check delegations ({resp.status_code})"
            delegations = resp.json()
            if task_id:
                # Filter by delegation_id
                matching = [d for d in delegations if d.get("delegation_id") == task_id]
                if matching:
                    return json.dumps(matching[0])
                return json.dumps({"status": "not_found", "delegation_id": task_id})
            # Return all recent delegations
            summary = []
            for d in delegations[:10]:
                summary.append({
                    "delegation_id": d.get("delegation_id", ""),
                    "target_id": d.get("target_id", ""),
                    "status": d.get("status", ""),
                    "summary": d.get("summary", ""),
                    "response_preview": d.get("response_preview", ""),
                })
            return json.dumps({"delegations": summary, "count": len(delegations)})
    except Exception as e:
        return f"Error checking delegations: {e}"


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


async def tool_commit_memory(
    content: str,
    scope: str = "LOCAL",
    source_workspace_id: str | None = None,
) -> str:
    """Save important information to persistent memory.

    GLOBAL scope is writable only by root workspaces (tier == 0).
    RBAC memory.write permission is required for all scope levels.
    The source workspace_id is embedded in every record so the platform
    can enforce cross-workspace isolation and audit trail.

    ``source_workspace_id`` selects which registered workspace this
    memory belongs to when the agent is registered into multiple
    workspaces (PR-1 / multi-workspace mode). When unset, falls back
    to the module-level WORKSPACE_ID — single-workspace operators see
    no behaviour change.
    """
    if not content:
        return "Error: content is required"
    content = _redact_secrets(content)
    scope = scope.upper()
    if scope not in ("LOCAL", "TEAM", "GLOBAL"):
        scope = "LOCAL"

    # RBAC: require memory.write permission (mirrors builtin_tools/memory.py)
    if not _check_memory_write_permission():
        return (
            "Error: RBAC — this workspace does not have the 'memory.write' "
            "permission for this operation."
        )

    # Scope enforcement: only root workspaces (tier 0) can write GLOBAL memory.
    # This prevents tenant workspaces from poisoning org-wide memory (GH#1610).
    if scope == "GLOBAL" and not _is_root_workspace():
        return (
            "Error: RBAC — only root workspaces (tier 0) can write to GLOBAL scope. "
            "Non-root workspaces may use LOCAL or TEAM scope."
        )

    src = source_workspace_id or WORKSPACE_ID
    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.post(
                f"{PLATFORM_URL}/workspaces/{src}/memories",
                json={
                    "content": content,
                    "scope": scope,
                    # Embed source workspace so the platform can namespace-isolate
                    # and audit cross-workspace writes (GH#1610 fix).
                    "workspace_id": src,
                },
                headers=_auth_headers_for_heartbeat(src),
            )
            data = resp.json()
            if resp.status_code in (200, 201):
                return json.dumps({"success": True, "id": data.get("id"), "scope": scope})
            return f"Error: {data.get('error', resp.text)}"
    except Exception as e:
        return f"Error saving memory: {e}"


async def tool_recall_memory(
    query: str = "",
    scope: str = "",
    source_workspace_id: str | None = None,
) -> str:
    """Search persistent memory for previously saved information.

    RBAC memory.read permission is required (mirrors builtin_tools/memory.py).
    The workspace_id is sent as a query parameter so the platform can
    cross-validate it against the auth token and defend against any future
    path traversal / cross-tenant read bugs in the platform itself.

    ``source_workspace_id`` selects which registered workspace's memories
    to search when the agent is registered into multiple workspaces.
    Unset → defaults to the module-level WORKSPACE_ID.
    """
    # RBAC: require memory.read permission (mirrors builtin_tools/memory.py)
    if not _check_memory_read_permission():
        return (
            "Error: RBAC — this workspace does not have the 'memory.read' "
            "permission for this operation."
        )

    src = source_workspace_id or WORKSPACE_ID
    params: dict[str, str] = {"workspace_id": src}
    if query:
        params["q"] = query
    if scope:
        params["scope"] = scope.upper()
    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.get(
                f"{PLATFORM_URL}/workspaces/{src}/memories",
                params=params,
                headers=_auth_headers_for_heartbeat(src),
            )
            data = resp.json()
            if isinstance(data, list):
                if not data:
                    return "No memories found."
                lines = []
                for m in data:
                    lines.append(f"[{m.get('scope', '?')}] {m.get('content', '')}")
                return "\n".join(lines)
            return json.dumps(data)
    except Exception as e:
        return f"Error recalling memory: {e}"


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


async def tool_inbox_peek(limit: int = 10) -> str:
    """Return up to ``limit`` pending inbound messages without removing them."""
    import inbox  # local import — avoids a circular dep at module load

    state = inbox.get_state()
    if state is None:
        return _INBOX_NOT_ENABLED_MSG
    messages = state.peek(limit=limit if isinstance(limit, int) else 10)
    return json.dumps([m.to_dict() for m in messages])


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
    return json.dumps(message.to_dict())
