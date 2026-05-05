"""Memory tool handlers — single-concern slice of the a2a_tools surface.

Extracted from ``a2a_tools.py`` (RFC #2873 iter 4c). Owns the two
agent-memory MCP tools:

  * ``tool_commit_memory`` — write to the workspace's persistent memory.
  * ``tool_recall_memory`` — search the workspace's persistent memory.

Both go through the platform's ``/workspaces/:id/memories`` endpoint;
the platform is the source of truth for namespace isolation + audit
trail. Local responsibility here is RBAC enforcement BEFORE hitting
the network so a denied operation surfaces a clear in-band error
instead of an opaque platform 403.

Imports the RBAC primitives from ``a2a_tools_rbac`` (iter 4a).
"""
from __future__ import annotations

import json

import httpx

from a2a_client import PLATFORM_URL, WORKSPACE_ID
from a2a_tools_rbac import (
    auth_headers_for_heartbeat as _auth_headers_for_heartbeat,
    check_memory_read_permission as _check_memory_read_permission,
    check_memory_write_permission as _check_memory_write_permission,
    is_root_workspace as _is_root_workspace,
)
from builtin_tools.security import _redact_secrets


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
