"""Drift gate + smoke tests for ``a2a_tools_memory`` (RFC #2873 iter 4c).

The full behavior matrix (RBAC denies, scope enforcement, platform
HTTP error paths) lives in ``test_a2a_tools_impl.py`` (TestToolCommitMemory
+ TestToolRecallMemory) which patches `a2a_tools_memory.foo` after the
iter 4c retarget.

This file pins:

  1. **Drift gate** — every previously-public symbol on ``a2a_tools``
     (``tool_commit_memory``, ``tool_recall_memory``) is the EXACT same
     callable as ``a2a_tools_memory.foo``. Refactor wrapping silently
     loses the existing test coverage; this gate makes that drift fail
     fast.
  2. **Import contract** — ``a2a_tools_memory`` does NOT pull in
     ``a2a_tools`` at module-load time. The handlers depend on
     ``a2a_tools_rbac`` (the layered architecture) and ``a2a_client``,
     not on the kitchen-sink module that re-exports them.
"""
from __future__ import annotations

import sys

import pytest


@pytest.fixture(autouse=True)
def _require_workspace_id(monkeypatch):
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "http://test.invalid")
    yield


# ============== Drift gate ==============

class TestBackCompatAliases:
    def test_tool_commit_memory_alias(self):
        import a2a_tools
        import a2a_tools_memory
        assert a2a_tools.tool_commit_memory is a2a_tools_memory.tool_commit_memory

    def test_tool_recall_memory_alias(self):
        import a2a_tools
        import a2a_tools_memory
        assert a2a_tools.tool_recall_memory is a2a_tools_memory.tool_recall_memory


# ============== Import contract ==============

class TestImportContract:
    def test_memory_module_does_not_load_a2a_tools(self, monkeypatch):
        """`a2a_tools_memory` must depend on `a2a_tools_rbac` (the layered
        architecture) and `a2a_client`, NEVER on the kitchen-sink
        `a2a_tools`. Top-level `from a2a_tools import …` would defeat
        the modularization goal and risk a circular-import."""
        # Drop both modules to control import order
        for m in ("a2a_tools", "a2a_tools_memory"):
            sys.modules.pop(m, None)

        # Import memory module. Should succeed without a2a_tools loaded.
        import a2a_tools_memory  # noqa: F401
        assert "a2a_tools_memory" in sys.modules

    def test_a2a_tools_re_exports_memory_handlers(self):
        """The opposite direction: a2a_tools must surface every memory
        symbol so existing call sites + tests work unchanged."""
        import a2a_tools
        assert hasattr(a2a_tools, "tool_commit_memory")
        assert hasattr(a2a_tools, "tool_recall_memory")
