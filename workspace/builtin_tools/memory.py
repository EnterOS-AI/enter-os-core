"""HMA memory tools for agents.

Hierarchical Memory Architecture:
- LOCAL: private to this workspace, invisible to others
- TEAM: shared with parent + siblings (same team)
- GLOBAL: readable by all, writable by root workspaces only

RBAC enforcement
----------------
``commit_memory`` requires the ``"memory.write"`` action.
``recall_memory`` requires the ``"memory.read"`` action.
Roles are read from ``config.yaml`` under ``rbac.roles`` (default: operator).

Audit trail
-----------
Every memory operation appends a JSON Lines record to the audit log:

  memory / memory.write / allowed   — write permitted by RBAC
  memory / memory.write / success   — write committed successfully
  memory / memory.write / failure   — write failed (platform error)
  memory / memory.read  / allowed   — read permitted by RBAC
  memory / memory.read  / success   — search returned results
  memory / memory.read  / failure   — search failed (platform error)

RBAC denials emit ``rbac / rbac.deny / denied`` events instead.
"""

import json
import os
import uuid
from types import SimpleNamespace
from typing import Any

from langchain_core.tools import tool
from builtin_tools.awareness_client import build_awareness_client
from builtin_tools.audit import check_permission, get_workspace_roles, log_event
from builtin_tools.security import _redact_secrets
from builtin_tools.telemetry import MEMORY_QUERY, MEMORY_SCOPE, WORKSPACE_ID_ATTR, get_tracer

try:  # pragma: no cover - optional runtime dependency in lightweight test envs
    import httpx  # type: ignore
except ImportError:  # pragma: no cover
    httpx = SimpleNamespace(AsyncClient=None)

PLATFORM_URL = os.environ.get("PLATFORM_URL", "http://host.docker.internal:8080")
WORKSPACE_ID = os.environ.get("WORKSPACE_ID", "")


@tool
async def commit_memory(content: str, scope: str = "LOCAL") -> dict:
    """Store a fact in memory with a specific scope.

    Args:
        content: The fact or knowledge to remember.
        scope: Memory scope — LOCAL (private), TEAM (shared with team), or GLOBAL (company-wide, root only).
    """
    content = _redact_secrets(content)
    trace_id = str(uuid.uuid4())
    scope = scope.upper()
    if scope not in ("LOCAL", "TEAM", "GLOBAL"):
        return {"error": "scope must be LOCAL, TEAM, or GLOBAL"}

    # --- RBAC check -----------------------------------------------------------
    roles, custom_perms = get_workspace_roles()
    if not check_permission("memory.write", roles, custom_perms):
        log_event(
            event_type="rbac",
            action="rbac.deny",
            resource=scope,
            outcome="denied",
            trace_id=trace_id,
            attempted_action="memory.write",
            roles=roles,
        )
        return {
            "success": False,
            "error": (
                "RBAC: this workspace does not have the 'memory.write' permission. "
                f"Current roles: {roles}"
            ),
        }

    log_event(
        event_type="memory",
        action="memory.write",
        resource=scope,
        outcome="allowed",
        trace_id=trace_id,
        memory_scope=scope,
        content_length=len(content),
    )

    # ── OTEL: memory_write span ──────────────────────────────────────────────
    tracer = get_tracer()

    with tracer.start_as_current_span("memory_write") as mem_span:
        mem_span.set_attribute(WORKSPACE_ID_ATTR, WORKSPACE_ID)
        mem_span.set_attribute(MEMORY_SCOPE, scope)
        mem_span.set_attribute("memory.content_length", len(content))

        awareness_client = build_awareness_client()
        if awareness_client is not None:
            try:
                result = await awareness_client.commit(content, scope)
            except Exception as e:
                log_event(
                    event_type="memory",
                    action="memory.write",
                    resource=scope,
                    outcome="failure",
                    trace_id=trace_id,
                    memory_scope=scope,
                    error=str(e),
                )
                try:
                    mem_span.record_exception(e)
                except Exception:
                    pass
                return {"success": False, "error": str(e)}
        else:
            # #215-class bug: platform now gates /workspaces/:id/memories behind
            # workspace auth. Import auth_headers lazily (same pattern as the
            # activity-log path below) so test environments that don't ship
            # platform_auth still work.
            try:
                from platform_auth import auth_headers as _auth
                _headers = _auth()
            except Exception:
                _headers = {}
            async with httpx.AsyncClient(timeout=10.0) as client:
                try:
                    resp = await client.post(
                        f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/memories",
                        json={"content": content, "scope": scope},
                        headers=_headers,
                    )
                    if resp.status_code == 201:
                        result = {"success": True, "id": resp.json().get("id"), "scope": scope}
                    else:
                        result = {"success": False, "error": resp.json().get("error", resp.text)}
                except Exception as e:
                    log_event(
                        event_type="memory",
                        action="memory.write",
                        resource=scope,
                        outcome="failure",
                        trace_id=trace_id,
                        memory_scope=scope,
                        error=str(e),
                    )
                    try:
                        mem_span.record_exception(e)
                    except Exception:
                        pass
                    return {"success": False, "error": str(e)}

        if result.get("success"):
            mem_span.set_attribute("memory.id", result.get("id") or "")
            mem_span.set_attribute("memory.success", True)
            log_event(
                event_type="memory",
                action="memory.write",
                resource=scope,
                outcome="success",
                trace_id=trace_id,
                memory_scope=scope,
                memory_id=result.get("id"),
            )
            # #125: surface memory writes in /activity so the Canvas
            # "Agent Comms" tab shows what an agent chose to remember.
            # Fire-and-forget — failure here must not poison the tool
            # response since the memory write itself already succeeded.
            await _record_memory_activity(scope, content, result.get("id"))
            await _maybe_log_skill_promotion(content, scope, result)
        else:
            mem_span.set_attribute("memory.success", False)
            log_event(
                event_type="memory",
                action="memory.write",
                resource=scope,
                outcome="failure",
                trace_id=trace_id,
                memory_scope=scope,
                error=result.get("error"),
            )

        return result


@tool
async def recall_memory(query: str = "", scope: str = "") -> dict:
    """Search stored memories.

    Args:
        query: Text to search for (empty returns all).
        scope: Filter by scope — LOCAL, TEAM, GLOBAL, or empty for all accessible.
    """
    trace_id = str(uuid.uuid4())
    scope = scope.upper()
    if scope and scope not in ("LOCAL", "TEAM", "GLOBAL"):
        return {"error": "scope must be LOCAL, TEAM, GLOBAL, or empty"}

    # --- RBAC check -----------------------------------------------------------
    roles, custom_perms = get_workspace_roles()
    if not check_permission("memory.read", roles, custom_perms):
        log_event(
            event_type="rbac",
            action="rbac.deny",
            resource=scope or "all",
            outcome="denied",
            trace_id=trace_id,
            attempted_action="memory.read",
            roles=roles,
        )
        return {
            "success": False,
            "error": (
                "RBAC: this workspace does not have the 'memory.read' permission. "
                f"Current roles: {roles}"
            ),
        }

    log_event(
        event_type="memory",
        action="memory.read",
        resource=scope or "all",
        outcome="allowed",
        trace_id=trace_id,
        memory_scope=scope or "all",
        query_length=len(query),
    )

    # ── OTEL: memory_read span ───────────────────────────────────────────────
    tracer = get_tracer()

    with tracer.start_as_current_span("memory_read") as mem_span:
        mem_span.set_attribute(WORKSPACE_ID_ATTR, WORKSPACE_ID)
        mem_span.set_attribute(MEMORY_SCOPE, scope or "all")
        mem_span.set_attribute(MEMORY_QUERY, query[:256] if query else "")

        awareness_client = build_awareness_client()
        if awareness_client is not None:
            try:
                result = await awareness_client.search(query, scope)
                mem_span.set_attribute("memory.result_count", result.get("count", 0))
                mem_span.set_attribute("memory.success", result.get("success", False))
                log_event(
                    event_type="memory",
                    action="memory.read",
                    resource=scope or "all",
                    outcome="success" if result.get("success") else "failure",
                    trace_id=trace_id,
                    memory_scope=scope or "all",
                    result_count=result.get("count", 0),
                )
                return result
            except Exception as e:
                log_event(
                    event_type="memory",
                    action="memory.read",
                    resource=scope or "all",
                    outcome="failure",
                    trace_id=trace_id,
                    memory_scope=scope or "all",
                    error=str(e),
                )
                try:
                    mem_span.record_exception(e)
                except Exception:
                    pass
                return {"success": False, "error": str(e)}

        params = {}
        if query:
            params["q"] = query
        if scope:
            params["scope"] = scope.upper()

        # #215-class bug (search path): same fix as commit_memory above —
        # the platform gates GET /workspaces/:id/memories behind workspace
        # auth, so without auth_headers() every search silently 401s and the
        # agent thinks its backlog is empty (observed on Technical Researcher
        # idle-loop pilot 2026-04-15).
        try:
            from platform_auth import auth_headers as _auth
            _headers = _auth()
        except Exception:
            _headers = {}

        async with httpx.AsyncClient(timeout=10.0) as client:
            try:
                resp = await client.get(
                    f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/memories",
                    params=params,
                    headers=_headers,
                )
                if resp.status_code == 200:
                    memories = resp.json()
                    mem_span.set_attribute("memory.result_count", len(memories))
                    mem_span.set_attribute("memory.success", True)
                    log_event(
                        event_type="memory",
                        action="memory.read",
                        resource=scope or "all",
                        outcome="success",
                        trace_id=trace_id,
                        memory_scope=scope or "all",
                        result_count=len(memories),
                    )
                    return {
                        "success": True,
                        "count": len(memories),
                        "memories": memories,
                    }
                mem_span.set_attribute("memory.success", False)
                log_event(
                    event_type="memory",
                    action="memory.read",
                    resource=scope or "all",
                    outcome="failure",
                    trace_id=trace_id,
                    memory_scope=scope or "all",
                    http_status=resp.status_code,
                )
                return {"success": False, "error": resp.json().get("error", resp.text)}
            except Exception as e:
                log_event(
                    event_type="memory",
                    action="memory.read",
                    resource=scope or "all",
                    outcome="failure",
                    trace_id=trace_id,
                    memory_scope=scope or "all",
                    error=str(e),
                )
                try:
                    mem_span.record_exception(e)
                except Exception:
                    pass
                return {"success": False, "error": str(e)}


def _parse_promotion_packet(content: str) -> dict[str, Any] | None:
    """Return a structured memory packet when content looks like promotion metadata."""
    text = content.strip()
    if not text.startswith("{"):
        return None

    try:
        payload = json.loads(text)
    except json.JSONDecodeError:
        return None

    if not isinstance(payload, dict):  # pragma: no cover
        return None
    if not payload.get("promote_to_skill"):
        return None

    return payload


async def _record_memory_activity(scope: str, content: str, memory_id: str | None) -> None:
    """Surface a successful memory write as an activity row so the Canvas
    "Agent Comms" tab can display what an agent chose to remember.
    Fire-and-forget — never raises. #125.

    The summary is intentionally short (scope tag + first 80 chars of
    content with a ``…`` ellipsis when truncated) so the activity table
    stays readable; full content lives in ``agent_memories``.
    """
    workspace_id = WORKSPACE_ID.strip()
    platform_url = PLATFORM_URL.strip().rstrip("/")
    if not workspace_id or not platform_url:
        return

    preview = content.strip().replace("\n", " ")
    if len(preview) > 80:
        preview = preview[:80] + "…"
    summary = f"[{scope}] {preview}"

    # NOTE: target_id is a UUID column scoped to workspace_id references —
    # cannot hold awareness/memory IDs (which are arbitrary strings).
    # We embed the memory_id in the summary instead so it's still searchable.
    if memory_id:
        summary = f"{summary} (id={memory_id[:24]})"
    payload: dict[str, Any] = {
        "workspace_id": workspace_id,
        "activity_type": "memory_write",
        "summary": summary,
        "status": "ok",
    }

    try:
        try:
            from platform_auth import auth_headers as _auth
            _headers = _auth()
        except Exception:
            _headers = {}
        async with httpx.AsyncClient(timeout=5.0) as client:
            await client.post(
                f"{platform_url}/workspaces/{workspace_id}/activity",
                json=payload,
                headers=_headers,
            )
    except Exception:
        # Activity logging is purely observability — never poison the
        # tool response on a failure here. We don't even log_event the
        # failure since the memory write itself succeeded and that's
        # what matters to the caller.
        pass


async def _maybe_log_skill_promotion(content: str, scope: str, memory_result: dict) -> None:
    """Best-effort activity log for durable memory entries that should become skills."""
    packet = _parse_promotion_packet(content)
    if packet is None:
        return

    workspace_id = WORKSPACE_ID.strip()
    platform_url = PLATFORM_URL.strip().rstrip("/")
    if not workspace_id or not platform_url:
        return

    repetition_signal = packet.get("repetition_signal")
    summary = (
        packet.get("summary")
        or packet.get("title")
        or packet.get("what changed")
        or "Repeatable workflow promoted to skill candidate"
    )
    metadata: dict[str, Any] = {
        "source": "memory-curation",
        "scope": scope,
        "memory_id": memory_result.get("id"),
        "promote_to_skill": True,
        "repetition_signal": repetition_signal,
        "memory_packet": packet,
    }

    payload = {
        "activity_type": "skill_promotion",
        "method": "memory/skill-promotion",
        "summary": summary,
        "status": "ok",
        "source_id": workspace_id,
        "request_body": packet,
        "metadata": metadata,
    }

    try:
        async with httpx.AsyncClient(timeout=5.0) as client:
            await client.post(
                f"{platform_url}/workspaces/{workspace_id}/activity",
                json=payload,
            )
            await client.post(
                f"{platform_url}/registry/heartbeat",
                json={
                    "workspace_id": workspace_id,
                    "error_rate": 0,
                    "sample_error": "",
                    "active_tasks": 1,
                    "uptime_seconds": 0,
                    "current_task": f"Skill promotion: {summary}",
                },
            )
    except Exception:
        # Best-effort observability only. Memory commits must never fail because
        # the promotion log could not be written.
        return
