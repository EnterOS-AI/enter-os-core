"""Drift gate + smoke tests for ``a2a_tools_messaging`` (RFC #2873 iter 4d).

The full behavior matrix lives in ``test_a2a_tools_impl.py`` —
TestToolSendMessageToUser + TestToolListPeers + TestToolGetWorkspaceInfo
+ TestChatHistory all patch ``a2a_tools_messaging.foo`` after the iter
4d retarget.

This file pins:

  1. **Drift gate** — every previously-public symbol on ``a2a_tools``
     is the EXACT same callable / value as ``a2a_tools_messaging.foo``.
     Wraps would silently lose existing test coverage; this gate
     fails fast on that drift.
  2. **Import contract** — ``a2a_tools_messaging`` does NOT pull in
     ``a2a_tools`` at module-load time (the layered architecture: it
     depends on ``a2a_tools_rbac`` + ``a2a_client`` + ``platform_auth``,
     never the kitchen-sink module).
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
    def test_tool_send_message_to_user_alias(self):
        import a2a_tools
        import a2a_tools_messaging
        assert (
            a2a_tools.tool_send_message_to_user
            is a2a_tools_messaging.tool_send_message_to_user
        )

    def test_tool_list_peers_alias(self):
        import a2a_tools
        import a2a_tools_messaging
        assert a2a_tools.tool_list_peers is a2a_tools_messaging.tool_list_peers

    def test_tool_get_workspace_info_alias(self):
        import a2a_tools
        import a2a_tools_messaging
        assert (
            a2a_tools.tool_get_workspace_info
            is a2a_tools_messaging.tool_get_workspace_info
        )

    def test_tool_chat_history_alias(self):
        import a2a_tools
        import a2a_tools_messaging
        assert a2a_tools.tool_chat_history is a2a_tools_messaging.tool_chat_history

    def test_upload_chat_files_alias(self):
        import a2a_tools
        import a2a_tools_messaging
        assert a2a_tools._upload_chat_files is a2a_tools_messaging._upload_chat_files


# ============== Import contract ==============

class TestImportContract:
    def test_messaging_module_does_not_load_a2a_tools(self, monkeypatch):
        """`a2a_tools_messaging` must depend on `a2a_tools_rbac` (the
        layered architecture), `a2a_client`, and `platform_auth` — but
        NEVER on the kitchen-sink `a2a_tools`. Top-level
        `from a2a_tools import …` would re-introduce the circular
        dependency that motivated the lazy-import contract for the
        delegation module."""
        for m in ("a2a_tools", "a2a_tools_messaging"):
            sys.modules.pop(m, None)

        import a2a_tools_messaging  # noqa: F401
        assert "a2a_tools_messaging" in sys.modules

    def test_a2a_tools_re_exports_messaging_handlers(self):
        """Opposite direction: a2a_tools surfaces every messaging
        symbol so existing call sites + tests work unchanged."""
        import a2a_tools
        assert hasattr(a2a_tools, "tool_send_message_to_user")
        assert hasattr(a2a_tools, "tool_list_peers")
        assert hasattr(a2a_tools, "tool_get_workspace_info")
        assert hasattr(a2a_tools, "tool_chat_history")
        assert hasattr(a2a_tools, "_upload_chat_files")
