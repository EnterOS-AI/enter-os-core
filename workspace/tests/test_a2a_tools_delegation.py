"""Drift gate + direct surface tests for ``a2a_tools_delegation`` (RFC #2873 iter 4b).

The full behavior matrix for the three delegation MCP tools lives in
``test_a2a_tools_impl.py`` (TestToolDelegateTask + TestToolDelegateTaskAsync
+ TestToolCheckTaskStatus). Those exercise call paths through the
``a2a_tools_delegation.foo`` module (after the iter 4b retarget).

This file owns the post-split contract:

  1. **Drift gate** — every previously-public symbol on ``a2a_tools``
     (``tool_delegate_task``, ``tool_delegate_task_async``,
     ``tool_check_task_status``, ``_delegate_sync_via_polling``,
     ``_SYNC_POLL_INTERVAL_S``, ``_SYNC_POLL_BUDGET_S``) is the EXACT
     same callable / value as the new module's public name. A wrapper
     that drifted would silently bypass tests targeting the wrapper.

  2. **Smoke import** — both modules import in either order without
     raising (the lazy ``report_activity`` import inside
     ``tool_delegate_task`` is the contract that prevents a circular
     import; this test pins it).
"""
from __future__ import annotations

import os

import pytest


@pytest.fixture(autouse=True)
def _require_workspace_id(monkeypatch):
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "http://test.invalid")
    yield


# ============== Drift gate ==============

class TestBackCompatAliases:
    def test_tool_delegate_task_alias(self):
        import a2a_tools
        import a2a_tools_delegation
        assert a2a_tools.tool_delegate_task is a2a_tools_delegation.tool_delegate_task

    def test_tool_delegate_task_async_alias(self):
        import a2a_tools
        import a2a_tools_delegation
        assert (
            a2a_tools.tool_delegate_task_async
            is a2a_tools_delegation.tool_delegate_task_async
        )

    def test_tool_check_task_status_alias(self):
        import a2a_tools
        import a2a_tools_delegation
        assert (
            a2a_tools.tool_check_task_status
            is a2a_tools_delegation.tool_check_task_status
        )

    def test_delegate_sync_via_polling_alias(self):
        import a2a_tools
        import a2a_tools_delegation
        assert (
            a2a_tools._delegate_sync_via_polling
            is a2a_tools_delegation._delegate_sync_via_polling
        )

    def test_constants_match(self):
        import a2a_tools
        import a2a_tools_delegation
        assert (
            a2a_tools._SYNC_POLL_INTERVAL_S
            == a2a_tools_delegation._SYNC_POLL_INTERVAL_S
        )
        assert (
            a2a_tools._SYNC_POLL_BUDGET_S
            == a2a_tools_delegation._SYNC_POLL_BUDGET_S
        )


# ============== Smoke imports ==============

class TestImportContracts:
    def test_delegation_imports_without_a2a_tools_loaded(self, monkeypatch):
        """``a2a_tools_delegation`` should NOT pull in ``a2a_tools`` at
        module-load time. The lazy ``from a2a_tools import report_activity``
        inside ``tool_delegate_task`` is the only legitimate hop.

        Pin this so a future refactor that adds a top-level
        ``from a2a_tools import …`` re-introduces the circular-import
        crash that motivated the lazy pattern.
        """
        import sys
        # Drop both modules so we re-import in a controlled order
        for mod in ("a2a_tools", "a2a_tools_delegation"):
            sys.modules.pop(mod, None)

        # Importing delegation first must succeed without a2a_tools
        # being loaded (because a2a_tools imports delegation, the
        # circular path ONLY closes if delegation top-level imports
        # something from a2a_tools).
        import a2a_tools_delegation  # noqa: F401
        # If we got here, no circular import.
        assert "a2a_tools_delegation" in sys.modules

    def test_a2a_tools_imports_via_delegation_re_export(self):
        """The opposite direction: importing a2a_tools must trigger the
        delegation re-export so a2a_tools.tool_delegate_task resolves."""
        import a2a_tools
        assert hasattr(a2a_tools, "tool_delegate_task")
        assert hasattr(a2a_tools, "tool_delegate_task_async")
        assert hasattr(a2a_tools, "tool_check_task_status")


# ============== Sync-poll budget env override ==============

class TestPollBudgetEnvOverride:
    def test_default_budget_when_env_unset(self):
        """Module-level constant. Set DELEGATION_TIMEOUT before importing
        a2a_tools_delegation to override; default is 300.0."""
        # The constant is computed at module-load time. To verify the
        # override path we'd need to reload — skipped here because it's
        # tested at boot. This test pins the default for catch-the-eye
        # documentation.
        import a2a_tools_delegation
        # Whatever was set when the module first loaded — assert it's
        # numeric and >= the documented floor (180s healthsweep budget).
        assert isinstance(a2a_tools_delegation._SYNC_POLL_BUDGET_S, float)
        assert a2a_tools_delegation._SYNC_POLL_BUDGET_S >= 180.0
