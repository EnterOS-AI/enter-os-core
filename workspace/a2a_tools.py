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
    discover_peer,
    get_peers,
    get_workspace_info,
    send_a2a_message,
)
from builtin_tools.security import _redact_secrets


# ---------------------------------------------------------------------------
# RBAC helpers (mirror builtin_tools/audit.py for a2a_tools isolation)
# ---------------------------------------------------------------------------

_ROLE_PERMISSIONS = {
    "admin": {"delegate", "approve", "memory.read", "memory.write"},
    "operator": {"delegate", "approve", "memory.read", "memory.write"},
    "read-only": {"memory.read"},
    "no-delegation": {"approve", "memory.read", "memory.write"},
    "no-approval": {"delegate", "memory.read", "memory.write"},
    "memory-readonly": {"memory.read"},
}


def _get_workspace_tier() -> int:
    """Return the workspace tier from config (0 = root, 1+ = tenant)."""
    try:
        from config import load_config

        cfg = load_config()
        return getattr(cfg, "tier", 1)
    except Exception:
        return int(os.environ.get("WORKSPACE_TIER", 1))


def _check_memory_write_permission() -> bool:
    """Return True if this workspace's RBAC roles grant memory.write."""
    try:
        from config import load_config

        cfg = load_config()
        roles = list(getattr(cfg, "rbac", None).roles or ["operator"])
        allowed = dict(getattr(cfg, "rbac", None).allowed_actions or {})
    except Exception:
        # Fail closed: deny when config is unavailable
        roles = ["operator"]
        allowed = {}

    for role in roles:
        if role == "admin":
            return True
        if role in allowed:
            if "memory.write" in allowed[role]:
                return True
        elif role in _ROLE_PERMISSIONS and "memory.write" in _ROLE_PERMISSIONS[role]:
            return True
    return False


def _check_memory_read_permission() -> bool:
    """Return True if this workspace's RBAC roles grant memory.read."""
    try:
        from config import load_config

        cfg = load_config()
        roles = list(getattr(cfg, "rbac", None).roles or ["operator"])
        allowed = dict(getattr(cfg, "rbac", None).allowed_actions or {})
    except Exception:
        roles = ["operator"]
        allowed = {}

    for role in roles:
        if role == "admin":
            return True
        if role in allowed:
            if "memory.read" in allowed[role]:
                return True
        elif role in _ROLE_PERMISSIONS and "memory.read" in _ROLE_PERMISSIONS[role]:
            return True
    return False


def _is_root_workspace() -> bool:
    """Return True if this workspace is tier 0 (root/root-org)."""
    return _get_workspace_tier() == 0


def _auth_headers_for_heartbeat() -> dict[str, str]:
    """Return Phase 30.1 auth headers; tolerate platform_auth being absent
    in older installs (e.g. during rolling upgrade)."""
    try:
        from platform_auth import auth_headers
        return auth_headers()
    except Exception:
        return {}


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


async def tool_delegate_task(workspace_id: str, task: str) -> str:
    """Delegate a task to another workspace via A2A (synchronous — waits for response)."""
    if not workspace_id or not task:
        return "Error: workspace_id and task are required"

    # Discover the target
    peer = await discover_peer(workspace_id)
    if not peer:
        return f"Error: workspace {workspace_id} not found or not accessible (check access control)"

    target_url = peer.get("url", "")
    if not target_url:
        return f"Error: workspace {workspace_id} has no URL (may be offline)"

    # Report delegation start — include the task text for traceability
    peer_name = peer.get("name") or _peer_names.get(workspace_id) or workspace_id[:8]
    _peer_names[workspace_id] = peer_name  # cache for future use
    # Brief summary for canvas display — just the delegation target
    await report_activity("a2a_send", workspace_id, f"Delegating to {peer_name}", task_text=task)

    # Send A2A message and log the full round-trip
    result = await send_a2a_message(target_url, task)

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


async def tool_delegate_task_async(workspace_id: str, task: str) -> str:
    """Delegate a task via the platform's async delegation API (fire-and-forget).

    Uses POST /workspaces/:id/delegate which runs the A2A request in the background.
    Results are tracked in the platform DB and broadcast via WebSocket.
    Use check_task_status to poll for results.
    """
    if not workspace_id or not task:
        return "Error: workspace_id and task are required"

    # Idempotency key: SHA-256 of (workspace_id, task) so that a restarted agent
    # firing the same delegation gets the same key and the platform returns the
    # existing delegation_id instead of creating a duplicate. Fixes #1456.
    idem_key = hashlib.sha256(f"{workspace_id}:{task}".encode()).hexdigest()[:32]

    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.post(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/delegate",
                json={"target_id": workspace_id, "task": task, "idempotency_key": idem_key},
                headers=_auth_headers_for_heartbeat(),
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


async def tool_check_task_status(workspace_id: str, task_id: str) -> str:
    """Check delegations for this workspace via the platform API.

    Args:
        workspace_id: Ignored (kept for backward compat). Checks this workspace's delegations.
        task_id: Optional delegation_id to filter. If empty, returns all recent delegations.
    """
    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.get(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/delegations",
                headers=_auth_headers_for_heartbeat(),
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


async def _upload_chat_files(client: httpx.AsyncClient, paths: list[str]) -> tuple[list[dict], str | None]:
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
    try:
        resp = await client.post(
            f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/chat/uploads",
            files=files_payload,
            headers=_auth_headers_for_heartbeat(),
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


async def tool_send_message_to_user(message: str, attachments: list[str] | None = None) -> str:
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
    """
    if not message:
        return "Error: message is required"
    try:
        async with httpx.AsyncClient(timeout=60.0) as client:
            uploaded, upload_err = await _upload_chat_files(client, attachments or [])
            if upload_err:
                return upload_err
            payload: dict = {"message": message}
            if uploaded:
                payload["attachments"] = uploaded
            resp = await client.post(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/notify",
                json=payload,
                headers=_auth_headers_for_heartbeat(),
            )
            if resp.status_code == 200:
                if uploaded:
                    return f"Message sent to user with {len(uploaded)} attachment(s)"
                return "Message sent to user"
            return f"Error: platform returned {resp.status_code}"
    except Exception as e:
        return f"Error sending message: {e}"


async def tool_list_peers() -> str:
    """List all workspaces this agent can communicate with."""
    peers = await get_peers()
    if not peers:
        return "No peers available (this workspace may be isolated)"
    lines = []
    for p in peers:
        status = p.get("status", "unknown")
        role = p.get("role", "")
        # Cache name for use in delegate_task
        _peer_names[p["id"]] = p["name"]
        lines.append(f"- {p['name']} (ID: {p['id']}, status: {status}, role: {role})")
    return "\n".join(lines)


async def tool_get_workspace_info() -> str:
    """Get this workspace's own info."""
    info = await get_workspace_info()
    return json.dumps(info, indent=2)


async def tool_commit_memory(content: str, scope: str = "LOCAL") -> str:
    """Save important information to persistent memory.

    GLOBAL scope is writable only by root workspaces (tier == 0).
    RBAC memory.write permission is required for all scope levels.
    The source workspace_id is embedded in every record so the platform
    can enforce cross-workspace isolation and audit trail.
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

    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.post(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/memories",
                json={
                    "content": content,
                    "scope": scope,
                    # Embed source workspace so the platform can namespace-isolate
                    # and audit cross-workspace writes (GH#1610 fix).
                    "workspace_id": WORKSPACE_ID,
                },
                headers=_auth_headers_for_heartbeat(),
            )
            data = resp.json()
            if resp.status_code in (200, 201):
                return json.dumps({"success": True, "id": data.get("id"), "scope": scope})
            return f"Error: {data.get('error', resp.text)}"
    except Exception as e:
        return f"Error saving memory: {e}"


async def tool_recall_memory(query: str = "", scope: str = "") -> str:
    """Search persistent memory for previously saved information.

    RBAC memory.read permission is required (mirrors builtin_tools/memory.py).
    The workspace_id is sent as a query parameter so the platform can
    cross-validate it against the auth token and defend against any future
    path traversal / cross-tenant read bugs in the platform itself.
    """
    # RBAC: require memory.read permission (mirrors builtin_tools/memory.py)
    if not _check_memory_read_permission():
        return (
            "Error: RBAC — this workspace does not have the 'memory.read' "
            "permission for this operation."
        )

    params: dict[str, str] = {"workspace_id": WORKSPACE_ID}
    if query:
        params["q"] = query
    if scope:
        params["scope"] = scope.upper()
    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.get(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/memories",
                params=params,
                headers=_auth_headers_for_heartbeat(),
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
