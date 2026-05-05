"""Delegation tool handlers — single-concern slice of the a2a_tools surface.

Extracted from ``a2a_tools.py`` (RFC #2873 iter 4b). Owns the three
delegation MCP tools + the RFC #2829 PR-5 sync-via-polling helper they
share.

Public surface:

* ``tool_delegate_task`` — synchronous delegation, waits for response.
* ``tool_delegate_task_async`` — fire-and-forget delegation; returns
  ``{delegation_id, ...}``.
* ``tool_check_task_status`` — poll the platform's ``/delegations`` log.

Internal:

* ``_delegate_sync_via_polling`` — durable async + poll for terminal
  status (RFC #2829 PR-5 cutover path; toggled by
  ``DELEGATION_SYNC_VIA_INBOX=1``).
* ``_SYNC_POLL_INTERVAL_S`` / ``_SYNC_POLL_BUDGET_S`` constants.

Circular-import note: this module calls ``report_activity`` from
``a2a_tools`` to emit activity rows around the delegate dispatch.
``a2a_tools`` imports the public symbols here at module-load time,
so we use a LAZY import for ``report_activity`` inside the function
that needs it. Without the lazy hop Python raises an ImportError
on first ``a2a_tools`` import.
"""
from __future__ import annotations

import hashlib
import json
import os

import httpx

from a2a_client import (
    PLATFORM_URL,
    WORKSPACE_ID,
    _A2A_ERROR_PREFIX,
    _peer_names,
    _peer_to_source,
    discover_peer,
    send_a2a_message,
)
from a2a_tools_rbac import auth_headers_for_heartbeat as _auth_headers_for_heartbeat


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

    # Lazy import: a2a_tools imports this module at top-level, so a
    # top-level import of report_activity from a2a_tools would create a
    # circular dependency at first-import time. Lazy resolution inside
    # the function body breaks the cycle without forcing a ground-up
    # restructure of the activity-reporting layer.
    from a2a_tools import report_activity

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
