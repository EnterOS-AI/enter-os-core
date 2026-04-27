"""Async delegation tool for sending tasks to peer workspaces via A2A.

Delegations are non-blocking: the tool fires the A2A request in the background
and returns immediately with a task_id. The agent can check status anytime via
check_delegation_status, or just continue working and check later.

When the delegate responds, the result is stored and the agent is notified
via a status update.
"""

import asyncio
import os
import uuid
from dataclasses import dataclass, field
from enum import Enum
from typing import Optional

import httpx
from langchain_core.tools import tool

from builtin_tools.audit import check_permission, get_workspace_roles, log_event
from builtin_tools.telemetry import (
    A2A_SOURCE_WORKSPACE,
    A2A_TARGET_WORKSPACE,
    A2A_TASK_ID,
    WORKSPACE_ID_ATTR,
    get_current_traceparent,
    get_tracer,
    inject_trace_headers,
)

PLATFORM_URL = os.environ.get("PLATFORM_URL", "http://host.docker.internal:8080")
WORKSPACE_ID = os.environ.get("WORKSPACE_ID", "")
DELEGATION_RETRY_ATTEMPTS = int(os.environ.get("DELEGATION_RETRY_ATTEMPTS", "3"))
DELEGATION_RETRY_DELAY = float(os.environ.get("DELEGATION_RETRY_DELAY", "5.0"))
DELEGATION_TIMEOUT = float(os.environ.get("DELEGATION_TIMEOUT", "300.0"))


class DelegationStatus(str, Enum):
    PENDING = "pending"
    IN_PROGRESS = "in_progress"
    # QUEUED: peer's a2a-proxy returned HTTP 202 + {queued: true}, meaning
    # the peer is mid-task and the request was placed in a drain queue.
    # The reply will arrive via the platform's stitch path when the
    # peer finishes its current work. The LLM should WAIT, not retry,
    # and definitely not fall back to doing the work itself — see the
    # check_delegation_status docstring for the prompt-side guidance.
    QUEUED = "queued"
    COMPLETED = "completed"
    FAILED = "failed"


@dataclass
class DelegationTask:
    task_id: str
    workspace_id: str
    task_description: str
    status: DelegationStatus = DelegationStatus.PENDING
    result: Optional[str] = None
    error: Optional[str] = None


# In-memory store of delegation tasks for this workspace
_delegations: dict[str, DelegationTask] = {}
_background_tasks: set[asyncio.Task] = set()
MAX_DELEGATION_HISTORY = 100
logger = __import__("logging").getLogger(__name__)


def _evict_old_delegations():
    """Remove completed/failed delegations when store exceeds MAX_DELEGATION_HISTORY."""
    if len(_delegations) <= MAX_DELEGATION_HISTORY:
        return
    # Evict oldest completed/failed first
    removable = [
        tid for tid, d in _delegations.items()
        if d.status in (DelegationStatus.COMPLETED, DelegationStatus.FAILED)
    ]
    for tid in removable[:len(_delegations) - MAX_DELEGATION_HISTORY]:
        del _delegations[tid]


def _on_task_done(task: asyncio.Task):
    """Callback for background tasks — log unhandled exceptions."""
    _background_tasks.discard(task)
    if not task.cancelled() and task.exception():
        logger.error("Delegation background task failed: %s", task.exception())


async def _notify_completion(task_id: str, target_workspace_id: str, status: str):
    """Push notification to platform when delegation completes/fails."""
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            await client.post(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/notify",
                json={
                    "type": "delegation_complete",
                    "task_id": task_id,
                    "target_workspace_id": target_workspace_id,
                    "status": status,
                },
            )
    except Exception as e:
        logger.debug("Delegation notify failed (best-effort): %s", e)


async def _record_delegation_on_platform(task_id: str, target_workspace_id: str, task: str):
    """Register the delegation in the platform's activity_logs (#64 fix).

    Best-effort POST to /workspaces/<self>/delegations/record. The agent still
    fires A2A directly for speed + OTEL propagation, but the platform's
    GET /delegations endpoint now mirrors the same set an agent's local
    check_delegation_status sees.
    """
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            await client.post(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/delegations/record",
                json={
                    "target_id": target_workspace_id,
                    "task": task,
                    "delegation_id": task_id,
                },
            )
    except Exception as e:
        logger.debug("Delegation record failed (best-effort): %s", e)


async def _refresh_queued_from_platform(task_id: str) -> bool:
    """Lazy-refresh a QUEUED delegation's local state from the platform.

    Called by check_delegation_status when local status is QUEUED. The
    platform's drain stitch (a2a_queue.go) updates the delegate_result
    activity_logs row when a queued delegation eventually completes,
    but it has no callback to this runtime — without this lazy refresh,
    the LLM polling check_delegation_status would see "queued" forever
    even after the platform has the result.

    Returns True if the local delegation was updated to a terminal state
    (completed/failed), False otherwise. Best-effort — network/parse
    errors leave the local state untouched and let the next call retry.
    """
    delegation = _delegations.get(task_id)
    if not delegation:
        return False
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            resp = await client.get(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/delegations",
                headers={},
            )
            if resp.status_code != 200:
                return False
            entries = resp.json()
            if not isinstance(entries, list):
                return False
    except Exception as e:
        logger.debug("refresh queued delegation %s: %s", task_id, e)
        return False
    # Find the latest delegate_result row matching our task_id.
    # Platform list is newest-first; the first match is the freshest.
    for entry in entries:
        if entry.get("delegation_id") != task_id:
            continue
        if entry.get("type") != "delegation":
            continue
        # Only delegate_result rows carry the eventual outcome; the
        # initial 'delegate' row stays at status='pending' even after
        # the result lands. Filtering on summary text is brittle, but
        # the rows from the LIST endpoint don't include `method`. The
        # `delegate_result` rows are the ones with `error` (failure)
        # or `response_preview` (success) populated — pick those.
        status = entry.get("status", "")
        if status == "completed":
            delegation.status = DelegationStatus.COMPLETED
            delegation.result = entry.get("response_preview", "")
            await _notify_completion(task_id, delegation.workspace_id, "completed")
            return True
        if status == "failed":
            delegation.status = DelegationStatus.FAILED
            delegation.error = entry.get("error", "")
            await _notify_completion(task_id, delegation.workspace_id, "failed")
            return True
        # status == "queued" / "pending" / "dispatched": platform hasn't
        # resolved yet; leave local state unchanged so the next poll
        # retries. Don't break — keep scanning in case there's a newer
        # entry for the same task_id (possible if the same delegation
        # was retried).
    return False


async def _update_delegation_on_platform(task_id: str, status: str, error: str = "", response_preview: str = ""):
    """Mirror status changes to the platform's activity_logs (#64 fix).

    Paired with _record_delegation_on_platform — fires on completion/failure
    so the platform view stays in sync with the agent's local dict.
    """
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            await client.post(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/delegations/{task_id}/update",
                json={
                    "status": status,
                    "error": error,
                    "response_preview": response_preview[:500],
                },
            )
    except Exception as e:
        logger.debug("Delegation update failed (best-effort): %s", e)


async def _execute_delegation(task_id: str, workspace_id: str, task: str):
    """Background coroutine that sends the A2A request and stores the result."""
    delegation = _delegations[task_id]
    delegation.status = DelegationStatus.IN_PROGRESS

    # #64: register on the platform so GET /workspaces/<self>/delegations
    # sees the same set as check_delegation_status. Best-effort — platform
    # unreachability must not block the actual A2A delegation.
    await _record_delegation_on_platform(task_id, workspace_id, task)

    tracer = get_tracer()
    with tracer.start_as_current_span("task_delegate") as delegate_span:
        delegate_span.set_attribute(WORKSPACE_ID_ATTR, WORKSPACE_ID)
        delegate_span.set_attribute(A2A_SOURCE_WORKSPACE, WORKSPACE_ID)
        delegate_span.set_attribute(A2A_TARGET_WORKSPACE, workspace_id)
        delegate_span.set_attribute(A2A_TASK_ID, task_id)

        async with httpx.AsyncClient(timeout=DELEGATION_TIMEOUT) as client:
            # Discover target URL
            try:
                discover_resp = await client.get(
                    f"{PLATFORM_URL}/registry/discover/{workspace_id}",
                    headers={"X-Workspace-ID": WORKSPACE_ID},
                )
                if discover_resp.status_code != 200:
                    delegation.status = DelegationStatus.FAILED
                    delegation.error = f"Discovery failed: HTTP {discover_resp.status_code}"
                    log_event(event_type="delegation", action="delegate", resource=workspace_id,
                              outcome="failure", trace_id=task_id, reason="discovery_error")
                    return

                target_url = discover_resp.json().get("url")
                if not target_url:
                    delegation.status = DelegationStatus.FAILED
                    delegation.error = "No URL for workspace"
                    return
            except Exception as e:
                delegation.status = DelegationStatus.FAILED
                delegation.error = f"Discovery error: {e}"
                return

            # Send A2A with retry
            outgoing_headers = inject_trace_headers({
                "Content-Type": "application/json",
                "X-Workspace-ID": WORKSPACE_ID,
            })
            traceparent = get_current_traceparent()

            last_error = None
            for attempt in range(DELEGATION_RETRY_ATTEMPTS):
                try:
                    a2a_resp = await client.post(
                        target_url,
                        headers=outgoing_headers,
                        json={
                            "jsonrpc": "2.0",
                            "method": "message/send",
                            "id": f"delegation-{task_id}-{attempt}",
                            "params": {
                                "message": {
                                    "role": "ROLE_USER",
                                    "parts": [{"kind": "text", "text": task}],
                                    "messageId": f"msg-{task_id}-{attempt}",
                                },
                                "metadata": {
                                    "parent_task_id": task_id,
                                    "source_workspace_id": WORKSPACE_ID,
                                    "traceparent": traceparent,
                                },
                            },
                        },
                    )

                    # HTTP 202 + {queued: true} = peer's a2a-proxy
                    # accepted the request but the peer's runtime is
                    # mid-task. Platform-side drain will deliver the
                    # reply asynchronously. Mark QUEUED locally so
                    # check_delegation_status can surface that state
                    # to the LLM with explicit "wait, don't bypass"
                    # guidance. Do NOT mark FAILED — the request is
                    # alive in the platform's queue, not lost.
                    #
                    # Without this branch, the loop falls through, the
                    # `if "error" in result` line below references an
                    # unbound `result`, and the eventual FAILED status
                    # leads the LLM to conclude the peer is permanently
                    # unavailable — at which point it does the delegated
                    # work itself, defeating the whole orchestration.
                    if a2a_resp.status_code == 202:
                        try:
                            queued_body = a2a_resp.json()
                        except Exception:
                            queued_body = {}
                        if queued_body.get("queued") is True:
                            delegation.status = DelegationStatus.QUEUED
                            log_event(
                                event_type="delegation", action="delegate",
                                resource=workspace_id, outcome="queued",
                                trace_id=task_id, attempt=attempt + 1,
                            )
                            await _notify_completion(task_id, workspace_id, "queued")
                            await _update_delegation_on_platform(
                                task_id, "queued", "", "",
                            )
                            return

                    if a2a_resp.status_code == 200:
                        try:
                            result = a2a_resp.json()
                        except Exception:
                            delegation.status = DelegationStatus.FAILED
                            delegation.error = "Invalid JSON response"
                            return

                        if "result" in result:
                            task_result = result["result"]
                            artifacts = task_result.get("artifacts", [])
                            texts = []
                            for artifact in artifacts:
                                for part in artifact.get("parts", []):
                                    if part.get("kind") == "text":
                                        texts.append(part["text"])
                            # Also check top-level parts
                            for part in task_result.get("parts", []):
                                if part.get("kind") == "text":
                                    texts.append(part["text"])

                            delegation.status = DelegationStatus.COMPLETED
                            delegation.result = "\n".join(texts) if texts else str(task_result)
                            log_event(event_type="delegation", action="delegate", resource=workspace_id,
                                      outcome="success", trace_id=task_id, attempt=attempt + 1)
                            await _notify_completion(task_id, workspace_id, "completed")
                            # #64: mirror to platform activity_logs so
                            # GET /delegations shows the completion state.
                            await _update_delegation_on_platform(
                                task_id, "completed", "",
                                delegation.result or "",
                            )
                            return

                        if "error" in result:
                            last_error = result["error"].get("message", str(result["error"]))
                            break

                except (httpx.ConnectError, httpx.TimeoutException) as e:
                    last_error = str(e)
                    if attempt < DELEGATION_RETRY_ATTEMPTS - 1:
                        await asyncio.sleep(DELEGATION_RETRY_DELAY * (attempt + 1))
                    continue

            delegation.status = DelegationStatus.FAILED
            delegation.error = str(last_error)
            log_event(event_type="delegation", action="delegate", resource=workspace_id,
                      outcome="failure", trace_id=task_id, last_error=str(last_error))
            await _notify_completion(task_id, workspace_id, "failed")
            # #64: mirror failure to platform activity_logs.
            await _update_delegation_on_platform(
                task_id, "failed", str(last_error), "",
            )


@tool
async def delegate_to_workspace(
    workspace_id: str,
    task: str,
) -> dict:
    """Delegate a task to a peer workspace via A2A protocol (non-blocking).

    Sends the task in the background and returns immediately with a task_id.
    Use check_delegation_status to poll for the result, or continue working
    and check later. The delegate works independently.

    Args:
        workspace_id: The ID of the target workspace to delegate to.
        task: The task description to send to the peer.

    Returns:
        A dict with task_id and status="delegated". Use check_delegation_status(task_id) to get results.
    """
    task_id = str(uuid.uuid4())

    # RBAC check
    roles, custom_perms = get_workspace_roles()
    if not check_permission("delegate", roles, custom_perms):
        log_event(event_type="rbac", action="rbac.deny", resource=workspace_id,
                  outcome="denied", trace_id=task_id, attempted_action="delegate", roles=roles)
        return {"success": False, "error": f"RBAC: no 'delegate' permission. Roles: {roles}"}

    log_event(event_type="delegation", action="delegate", resource=workspace_id,
              outcome="dispatched", trace_id=task_id, task_preview=task[:200])

    # Store the delegation and launch background task
    delegation = DelegationTask(
        task_id=task_id,
        workspace_id=workspace_id,
        task_description=task[:200],
    )
    _delegations[task_id] = delegation
    _evict_old_delegations()

    bg_task = asyncio.create_task(_execute_delegation(task_id, workspace_id, task))
    _background_tasks.add(bg_task)
    bg_task.add_done_callback(_on_task_done)

    return {
        "success": True,
        "task_id": task_id,
        "status": "delegated",
        "message": f"Task delegated to {workspace_id}. Use check_delegation_status('{task_id}') to get the result when ready.",
    }


@tool
async def check_delegation_status(
    task_id: str = "",
) -> dict:
    """Check the status of a delegated task, or list all active delegations.

    Status semantics — IMPORTANT:

    - "pending" / "in_progress" → peer is actively working. Wait and check again.
    - "queued" → peer's a2a-proxy accepted the call but the peer is
      processing a prior task. The reply WILL arrive — the platform's
      drain re-dispatches when the peer is free. This tool transparently
      polls the platform for the eventual outcome on each call, so
      keep polling check_delegation_status periodically and you'll see
      the status flip to "completed" / "failed" automatically.
      Do NOT retry the delegation. Do NOT do the work yourself.
      Acknowledge to the user that the peer is busy and will reply,
      then continue with other delegations or check back later.
    - "completed" → result is in the `result` field.
    - "failed" → real failure (network, peer crashed, etc.). The
      `error` field has the cause. Only fall back to doing the work
      yourself if status is "failed", never if status is "queued".

    Args:
        task_id: The task_id returned by delegate_to_workspace. If empty, lists all delegations.

    Returns:
        Status and result (if completed) of the delegation.
    """
    if not task_id:
        # List all delegations
        summary = []
        for tid, d in _delegations.items():
            entry = {
                "task_id": tid,
                "workspace_id": d.workspace_id,
                "status": d.status.value,
                "task": d.task_description,
            }
            if d.status == DelegationStatus.COMPLETED:
                entry["result_preview"] = (d.result or "")[:200]
            if d.status == DelegationStatus.FAILED:
                entry["error"] = d.error
            summary.append(entry)
        return {"delegations": summary, "count": len(summary)}

    delegation = _delegations.get(task_id)
    if not delegation:
        return {"error": f"No delegation found with task_id {task_id}"}

    # Lazy refresh for QUEUED entries: the platform's drain stitch
    # updates its activity_logs row when the queued delegation
    # eventually completes, but doesn't push back to this runtime.
    # Without this refresh, the LLM polling here would see "queued"
    # forever even after the result is available — exactly the bug
    # the upstream director-bypass docstring guidance warned against.
    if delegation.status == DelegationStatus.QUEUED:
        await _refresh_queued_from_platform(task_id)
        # delegation is the same dict entry — _refresh mutates in-place.

    result = {
        "task_id": task_id,
        "workspace_id": delegation.workspace_id,
        "status": delegation.status.value,
        "task": delegation.task_description,
    }

    if delegation.status == DelegationStatus.COMPLETED:
        result["result"] = delegation.result
    elif delegation.status == DelegationStatus.FAILED:
        result["error"] = delegation.error

    return result
