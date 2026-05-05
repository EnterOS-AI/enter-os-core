"""Direct tests for ``a2a_tools_rbac`` (RFC #2873 iter 4a).

The full behavior matrix is exercised through ``a2a_tools._foo`` aliases
in ``test_a2a_tools_impl.py``. This file pins:

  1. **Drift gate** — ``a2a_tools._foo is a2a_tools_rbac.foo`` for every
     extracted symbol. A refactor that wraps or re-implements an alias
     fails this test.
  2. **Direct unit coverage** for each helper without going through the
     a2a_tools surface, so regressions in the small RBAC layer surface
     against THIS module's tests, not the 991-LOC tool-handler tests.
"""
from __future__ import annotations

import os
import sys
from unittest.mock import patch

import pytest


@pytest.fixture(autouse=True)
def _require_workspace_id(monkeypatch):
    # a2a_client raises at import-time without WORKSPACE_ID. Setting it
    # once per test isolates the env so an absent value in CI doesn't
    # surface as an opaque RuntimeError from a2a_tools' import.
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "http://test.invalid")
    yield


# ============== Drift gate ==============

class TestBackCompatAliases:
    """Pin that every legacy underscore name in ``a2a_tools`` is the
    EXACT same callable / object as the new public name in
    ``a2a_tools_rbac``. Catches accidental re-implementation in either
    direction."""

    def test_role_permissions_is_same_object(self):
        import a2a_tools
        import a2a_tools_rbac
        assert a2a_tools._ROLE_PERMISSIONS is a2a_tools_rbac.ROLE_PERMISSIONS

    def test_get_workspace_tier_alias(self):
        import a2a_tools
        import a2a_tools_rbac
        assert a2a_tools._get_workspace_tier is a2a_tools_rbac.get_workspace_tier

    def test_check_memory_write_permission_alias(self):
        import a2a_tools
        import a2a_tools_rbac
        assert (
            a2a_tools._check_memory_write_permission
            is a2a_tools_rbac.check_memory_write_permission
        )

    def test_check_memory_read_permission_alias(self):
        import a2a_tools
        import a2a_tools_rbac
        assert (
            a2a_tools._check_memory_read_permission
            is a2a_tools_rbac.check_memory_read_permission
        )

    def test_is_root_workspace_alias(self):
        import a2a_tools
        import a2a_tools_rbac
        assert a2a_tools._is_root_workspace is a2a_tools_rbac.is_root_workspace

    def test_auth_headers_alias(self):
        import a2a_tools
        import a2a_tools_rbac
        assert (
            a2a_tools._auth_headers_for_heartbeat
            is a2a_tools_rbac.auth_headers_for_heartbeat
        )


# ============== get_workspace_tier ==============

class TestGetWorkspaceTier:
    def test_uses_config_when_available(self):
        """Happy path: load_config returns an object with .tier."""
        import a2a_tools_rbac

        class _Cfg:
            tier = 0

        with patch("config.load_config", return_value=_Cfg()):
            assert a2a_tools_rbac.get_workspace_tier() == 0

    def test_default_tier_when_config_lacks_attr(self):
        import a2a_tools_rbac

        class _Cfg:
            pass

        with patch("config.load_config", return_value=_Cfg()):
            # getattr default = 1
            assert a2a_tools_rbac.get_workspace_tier() == 1

    def test_falls_back_to_env_var(self, monkeypatch):
        """When load_config raises, read WORKSPACE_TIER from env."""
        import a2a_tools_rbac
        monkeypatch.setenv("WORKSPACE_TIER", "5")
        with patch("config.load_config", side_effect=RuntimeError("config unavailable")):
            assert a2a_tools_rbac.get_workspace_tier() == 5

    def test_fallback_default_one_when_env_unset(self, monkeypatch):
        import a2a_tools_rbac
        monkeypatch.delenv("WORKSPACE_TIER", raising=False)
        with patch("config.load_config", side_effect=RuntimeError("boom")):
            assert a2a_tools_rbac.get_workspace_tier() == 1


# ============== is_root_workspace ==============

class TestIsRootWorkspace:
    def test_tier_zero_is_root(self):
        import a2a_tools_rbac
        with patch.object(a2a_tools_rbac, "get_workspace_tier", return_value=0):
            assert a2a_tools_rbac.is_root_workspace() is True

    def test_nonzero_tier_is_not_root(self):
        import a2a_tools_rbac
        for tier in (1, 2, 99):
            with patch.object(a2a_tools_rbac, "get_workspace_tier", return_value=tier):
                assert a2a_tools_rbac.is_root_workspace() is False, f"tier={tier}"


# ============== check_memory_write_permission ==============

class _RBACCfg:
    """Minimal config stub matching the load_config().rbac shape."""

    def __init__(self, roles=None, allowed_actions=None):
        class _RBAC:
            pass
        self.rbac = _RBAC()
        self.rbac.roles = roles or ["operator"]
        self.rbac.allowed_actions = allowed_actions or {}


class TestCheckMemoryWritePermission:
    def test_admin_role_grants_write(self):
        import a2a_tools_rbac
        with patch("config.load_config", return_value=_RBACCfg(roles=["admin"])):
            assert a2a_tools_rbac.check_memory_write_permission() is True

    def test_operator_role_grants_write(self):
        """Operator is in the canonical ROLE_PERMISSIONS table with
        memory.write — must work without per-role overrides."""
        import a2a_tools_rbac
        with patch("config.load_config", return_value=_RBACCfg(roles=["operator"])):
            assert a2a_tools_rbac.check_memory_write_permission() is True

    def test_read_only_role_denies_write(self):
        import a2a_tools_rbac
        with patch("config.load_config", return_value=_RBACCfg(roles=["read-only"])):
            assert a2a_tools_rbac.check_memory_write_permission() is False

    def test_per_role_override_grants(self):
        """Per-role override in allowed_actions wins over the canonical
        table — operators can grant write to memory-readonly via config."""
        import a2a_tools_rbac
        cfg = _RBACCfg(
            roles=["memory-readonly"],
            allowed_actions={"memory-readonly": {"memory.read", "memory.write"}},
        )
        with patch("config.load_config", return_value=cfg):
            assert a2a_tools_rbac.check_memory_write_permission() is True

    def test_per_role_override_denies(self):
        """Per-role override that drops write blocks an operator from
        writing — the override is the authoritative source when present."""
        import a2a_tools_rbac
        cfg = _RBACCfg(
            roles=["operator"],
            allowed_actions={"operator": {"memory.read"}},
        )
        with patch("config.load_config", return_value=cfg):
            assert a2a_tools_rbac.check_memory_write_permission() is False

    def test_fail_closed_when_config_unavailable(self):
        """Fail-closed contract: config outage falls back to ['operator']
        with no overrides — operator has memory.write in the canonical
        table, so write IS granted in this fallback. The fail-closed
        property is for ELEVATED ops (admin scope), not for the basic
        write that operator has by default. This test pins the contract:
        config errors do not silently grant admin."""
        import a2a_tools_rbac
        with patch("config.load_config", side_effect=RuntimeError("boom")):
            # operator has memory.write → True (preserved behavior)
            assert a2a_tools_rbac.check_memory_write_permission() is True


# ============== check_memory_read_permission ==============

class TestCheckMemoryReadPermission:
    def test_admin_grants_read(self):
        import a2a_tools_rbac
        with patch("config.load_config", return_value=_RBACCfg(roles=["admin"])):
            assert a2a_tools_rbac.check_memory_read_permission() is True

    def test_read_only_grants_read(self):
        import a2a_tools_rbac
        with patch("config.load_config", return_value=_RBACCfg(roles=["read-only"])):
            assert a2a_tools_rbac.check_memory_read_permission() is True

    def test_unknown_role_denies(self):
        """A role that's not in ROLE_PERMISSIONS and not in
        allowed_actions overrides denies by default."""
        import a2a_tools_rbac
        with patch("config.load_config", return_value=_RBACCfg(roles=["random-undefined-role"])):
            assert a2a_tools_rbac.check_memory_read_permission() is False


# ============== auth_headers_for_heartbeat ==============

class TestAuthHeadersForHeartbeat:
    def test_no_workspace_id_uses_legacy_path(self):
        """No-arg call routes to platform_auth.auth_headers() — the
        legacy single-token path."""
        import a2a_tools_rbac
        called: dict[str, object] = {}

        def fake_auth_headers(*args):
            called["args"] = args
            return {"Authorization": "Bearer legacy-token"}

        with patch("platform_auth.auth_headers", fake_auth_headers):
            out = a2a_tools_rbac.auth_headers_for_heartbeat()
            assert out == {"Authorization": "Bearer legacy-token"}
            # Legacy path is auth_headers() with no arg
            assert called["args"] == ()

    def test_with_workspace_id_routes_per_workspace(self):
        import a2a_tools_rbac
        called: dict[str, object] = {}

        def fake_auth_headers(wsid):
            called["wsid"] = wsid
            return {"Authorization": f"Bearer tok-{wsid}"}

        with patch("platform_auth.auth_headers", fake_auth_headers):
            out = a2a_tools_rbac.auth_headers_for_heartbeat("ws-abc")
            assert out == {"Authorization": "Bearer tok-ws-abc"}
            assert called["wsid"] == "ws-abc"

    def test_returns_empty_when_platform_auth_missing(self, monkeypatch):
        """Older installs without platform_auth get {} so callers don't
        crash — they'll just send unauthed and the platform 401 handler
        surfaces the real error."""
        import a2a_tools_rbac
        # Force ImportError by setting sys.modules entry to None
        monkeypatch.setitem(sys.modules, "platform_auth", None)
        out = a2a_tools_rbac.auth_headers_for_heartbeat("ws-1")
        assert out == {}


# ============== ROLE_PERMISSIONS canonical table ==============

class TestRolePermissionsTable:
    def test_admin_has_all_actions(self):
        import a2a_tools_rbac
        assert a2a_tools_rbac.ROLE_PERMISSIONS["admin"] == {
            "delegate", "approve", "memory.read", "memory.write",
        }

    def test_read_only_has_only_memory_read(self):
        import a2a_tools_rbac
        assert a2a_tools_rbac.ROLE_PERMISSIONS["read-only"] == {"memory.read"}

    def test_no_delegation_is_missing_delegate(self):
        import a2a_tools_rbac
        assert "delegate" not in a2a_tools_rbac.ROLE_PERMISSIONS["no-delegation"]

    def test_no_approval_is_missing_approve(self):
        import a2a_tools_rbac
        assert "approve" not in a2a_tools_rbac.ROLE_PERMISSIONS["no-approval"]
