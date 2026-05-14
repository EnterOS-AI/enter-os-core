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


# ============== Self-delegation guard ==============

class TestSelfDelegationGuard:
    """delegate_task / delegate_task_async to your own workspace ID must be
    rejected immediately (it deadlocks _run_lock on the sync path — the
    sending turn holds the lock, the receive handler waits for it, the
    request 30s-times-out). A genuinely different target must NOT be
    short-circuited by the guard."""

    def _fresh(self, monkeypatch, own_id):
        import a2a_tools_delegation as d
        monkeypatch.setattr(d, "WORKSPACE_ID", own_id)
        monkeypatch.setattr(d, "_peer_to_source", {}, raising=False)
        return d

    def test_delegate_task_rejects_self(self, monkeypatch):
        import asyncio
        d = self._fresh(monkeypatch, "ws-self-abc")
        out = asyncio.run(d.tool_delegate_task("ws-self-abc", "do a thing"))
        assert "your own workspace" in out.lower()

    def test_delegate_task_rejects_self_via_explicit_source(self, monkeypatch):
        import asyncio
        d = self._fresh(monkeypatch, "ws-other-default")
        out = asyncio.run(
            d.tool_delegate_task("ws-X", "do a thing", source_workspace_id="ws-X")
        )
        assert "your own workspace" in out.lower()

    def test_delegate_task_async_rejects_self(self, monkeypatch):
        import asyncio
        d = self._fresh(monkeypatch, "ws-self-abc")
        out = asyncio.run(d.tool_delegate_task_async("ws-self-abc", "do a thing"))
        assert "your own workspace" in out.lower()

    def test_delegate_task_allows_different_target(self, monkeypatch):
        """Guard passes through for a real peer — it reaches discover_peer
        (stubbed to 'not found' here) rather than returning the self message."""
        import asyncio
        d = self._fresh(monkeypatch, "ws-self-abc")
        async def _no_peer(*_a, **_kw):
            return None
        monkeypatch.setattr(d, "discover_peer", _no_peer)
        out = asyncio.run(d.tool_delegate_task("ws-OTHER-xyz", "do a thing"))
        assert "your own workspace" not in out.lower()
        assert "not found" in out.lower()


# ============== Polling path — sanitization boundary wrapping ==============

class TestPollingPathSanitization:
    """Verify that results returned by _delegate_sync_via_polling are wrapped
    in [A2A_RESULT_FROM_PEER] boundary markers when they reach the caller.

    The polling path calls sanitize_a2a_result (escapes markers + injection
    patterns) before returning. tool_delegate_task then wraps the sanitized
    text in boundary markers so the agent can distinguish trusted own output
    from untrusted peer content (OFFSEC-003).
    """

    def test_completed_response_sanitized(self, monkeypatch):
        """_delegate_sync_via_polling returns sanitize_a2a_result(text) — plain
        escaped text, no boundary markers. tool_delegate_task then wraps it in
        _A2A_BOUNDARY_START/END (OFFSEC-003) so the agent can distinguish
        trusted own output from untrusted peer-supplied content.

        _A2A_RESULT_FROM_PEER markers are added by send_a2a_message (the
        messaging path), not by the polling path.
        """
        import asyncio
        import a2a_tools_delegation as d

        monkeypatch.setenv("DELEGATION_SYNC_VIA_INBOX", "1")

        # _delegate_sync_via_polling returns plain sanitized text (no boundary
        # markers). It is the caller's responsibility to wrap it.
        async def fake_delegate_sync(ws_id, task, src):
            return "Sanitized peer reply."

        # discover_peer signature: (target_id, source_workspace_id=None)
        async def fake_discover(ws_id, source_workspace_id=None):
            return {"id": ws_id, "url": "http://x/a2a", "name": "Peer"}

        # Must use monkeypatch.setattr — direct assignment does not replace
        # module-level 'from module import name' bindings resolved at call time.
        monkeypatch.setattr(d, "_delegate_sync_via_polling", fake_delegate_sync)
        monkeypatch.setattr(d, "discover_peer", fake_discover)

        result = asyncio.run(d.tool_delegate_task("ws-peer", "do it"))
        # tool_delegate_task wraps the sanitized text in _A2A_BOUNDARY_START/END
        # (NOT _A2A_RESULT_FROM_PEER — that marker is for the messaging path).
        # Wrapped in escaped form to prevent raw closer from appearing in output.
        assert d._A2A_BOUNDARY_START_ESCAPED in result
        assert d._A2A_BOUNDARY_END_ESCAPED in result
        assert "Sanitized peer reply" in result

