"""RBAC + auth-header helpers shared by all a2a_tools tool handlers.

Extracted from ``a2a_tools.py`` (RFC #2873 iter 4a). Centralises the
"what can this workspace do" + "how do I prove it on a platform call"
concerns into a single module so:

  * Future tools added under ``a2a_tools/`` see one obvious helper to
    call instead of re-implementing the role/tier check.
  * The role-permission table is in ONE place — adding a new role
    or capability touches one file, not every tool that gates on it.
  * Tests targeting these helpers don't have to import the whole
    991-LOC ``a2a_tools`` surface.

Public surface:

* ``ROLE_PERMISSIONS`` — canonical role → action set table.
* ``get_workspace_tier()`` — config-resolved tier (0 = root).
* ``check_memory_write_permission()`` — boolean.
* ``check_memory_read_permission()`` — boolean.
* ``is_root_workspace()`` — boolean (tier == 0).
* ``auth_headers_for_heartbeat(workspace_id=None)`` — auth-header dict
  with the multi-workspace registry lookup; tolerates ``platform_auth``
  missing on older installs (returns ``{}``).

Underscore-prefixed back-compat aliases (``_ROLE_PERMISSIONS``,
``_check_memory_write_permission``, etc.) match the names previously
exposed in ``a2a_tools`` so existing tests'
``patch("a2a_tools._foo", ...)`` continue to work via the re-exports
in ``a2a_tools.py``.
"""
from __future__ import annotations

import os


# Mirror ``builtin_tools/audit.py`` for a2a_tools isolation. Listed as a
# module-level constant rather than computed lazily so the table is
# discoverable in static analysis + ``grep``.
ROLE_PERMISSIONS: dict[str, set[str]] = {
    "admin": {"delegate", "approve", "memory.read", "memory.write"},
    "operator": {"delegate", "approve", "memory.read", "memory.write"},
    "read-only": {"memory.read"},
    "no-delegation": {"approve", "memory.read", "memory.write"},
    "no-approval": {"delegate", "memory.read", "memory.write"},
    "memory-readonly": {"memory.read"},
}


def get_workspace_tier() -> int:
    """Return the workspace tier from config (0 = root, 1+ = tenant)."""
    try:
        from config import load_config

        cfg = load_config()
        return getattr(cfg, "tier", 1)
    except Exception:
        return int(os.environ.get("WORKSPACE_TIER", 1))


def _resolve_role_state() -> tuple[list[str], dict]:
    """Return (roles, allowed_actions) from config.

    Fail-closed: if config is unavailable, fall back to an "operator"
    default with no per-role overrides. Operator has memory.read +
    memory.write but not the elevated approve/delegate over GLOBAL
    scope, so a config outage doesn't grant unexpected privileges.
    """
    try:
        from config import load_config

        cfg = load_config()
        roles = list(getattr(cfg, "rbac", None).roles or ["operator"])
        allowed = dict(getattr(cfg, "rbac", None).allowed_actions or {})
        return roles, allowed
    except Exception:
        return ["operator"], {}


def check_memory_write_permission() -> bool:
    """Return True if this workspace's RBAC roles grant memory.write."""
    roles, allowed = _resolve_role_state()
    for role in roles:
        if role == "admin":
            return True
        if role in allowed:
            if "memory.write" in allowed[role]:
                return True
        elif role in ROLE_PERMISSIONS and "memory.write" in ROLE_PERMISSIONS[role]:
            return True
    return False


def check_memory_read_permission() -> bool:
    """Return True if this workspace's RBAC roles grant memory.read."""
    roles, allowed = _resolve_role_state()
    for role in roles:
        if role == "admin":
            return True
        if role in allowed:
            if "memory.read" in allowed[role]:
                return True
        elif role in ROLE_PERMISSIONS and "memory.read" in ROLE_PERMISSIONS[role]:
            return True
    return False


def is_root_workspace() -> bool:
    """Return True if this workspace is tier 0 (root/root-org)."""
    return get_workspace_tier() == 0


def auth_headers_for_heartbeat(workspace_id: str | None = None) -> dict[str, str]:
    """Return Phase 30.1 auth headers; tolerate platform_auth being absent
    in older installs (e.g. during rolling upgrade).

    ``workspace_id`` selects the per-workspace token from the multi-
    workspace registry when set (PR-1: external agent registered in
    multiple workspaces). With no arg the legacy single-token path is
    unchanged.
    """
    try:
        from platform_auth import auth_headers
        return auth_headers(workspace_id) if workspace_id else auth_headers()
    except Exception:
        return {}


# ============== Back-compat aliases for the previous a2a_tools names ==============
# Tests + downstream call sites refer to the pre-extract names; aliasing
# keeps both forms valid. The new public names (no underscore prefix)
# are preferred for new code.

_ROLE_PERMISSIONS = ROLE_PERMISSIONS
_get_workspace_tier = get_workspace_tier
_check_memory_write_permission = check_memory_write_permission
_check_memory_read_permission = check_memory_read_permission
_is_root_workspace = is_root_workspace
_auth_headers_for_heartbeat = auth_headers_for_heartbeat
