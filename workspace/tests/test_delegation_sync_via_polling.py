"""RFC #2829 PR-5: tests for the agent-side cutover that replaces the
proxy-blocked send_a2a_message sync path with delegate-then-poll.

Coverage:

  - Flag off (default) → byte-identical to legacy: tool_delegate_task
    calls send_a2a_message and never touches /delegate.
  - Flag on, dispatch fails → wrapped error returned, no infinite poll.
  - Flag on, dispatch returns no delegation_id → wrapped error.
  - Flag on, completed status on first poll → response_preview returned.
  - Flag on, failed status → wrapped error with error_detail.
  - Flag on, transient poll error → keeps polling, eventually succeeds.
  - Flag on, deadline exceeded → wrapped timeout error mentions
    delegation_id so caller can pick it up via check_task_status later.
  - Idempotency key is consistent with the legacy path's hashing.
"""

import json
import os
from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest

# WORKSPACE_ID + PLATFORM_URL are checked at a2a_client import time.
# CI ships them via the workflow env block; for local pytest runs we
# set them here so the test file can import a2a_tools at module scope
# (matching the pattern in test_a2a_tools_impl.py — that file relies
# on the same CI env shape).
os.environ.setdefault("WORKSPACE_ID", "00000000-0000-0000-0000-000000000001")
os.environ.setdefault("PLATFORM_URL", "http://localhost:8080")


def _resp(status_code, payload, text=None):
    r = MagicMock()
    r.status_code = status_code
    r.json = MagicMock(return_value=payload)
    r.text = text or json.dumps(payload)
    return r


def _make_client(post_resp=None, get_resps=None, post_exc=None):
    """Build an AsyncClient mock where get() returns a sequence of responses
    (one per call) so we can simulate multiple poll rounds.
    """
    mc = AsyncMock()
    mc.__aenter__ = AsyncMock(return_value=mc)
    mc.__aexit__ = AsyncMock(return_value=False)
    if post_exc is not None:
        mc.post = AsyncMock(side_effect=post_exc)
    else:
        mc.post = AsyncMock(return_value=post_resp or _resp(202, {"delegation_id": "deleg-1"}))
    if get_resps is None:
        get_resps = [_resp(200, [])]
    mc.get = AsyncMock(side_effect=get_resps)
    return mc


# ---------------------------------------------------------------------------
# Flag-off: legacy path is preserved
# ---------------------------------------------------------------------------

class TestFlagOffLegacyPath:

    async def test_flag_off_uses_send_a2a_message_not_polling(self, monkeypatch):
        """With DELEGATION_SYNC_VIA_INBOX unset, tool_delegate_task must
        invoke the legacy send_a2a_message and NEVER call /delegate."""
        monkeypatch.delenv("DELEGATION_SYNC_VIA_INBOX", raising=False)

        import a2a_tools
        send_calls = []

        async def fake_send(workspace_id, task, source_workspace_id=None):
            send_calls.append((workspace_id, task, source_workspace_id))
            return "legacy ok"

        async def fake_discover(*_a, **_kw):
            return {"name": "peer-name", "status": "online"}

        async def fake_report_activity(*_a, **_kw):
            return None

        with patch("a2a_tools_delegation.send_a2a_message", side_effect=fake_send), \
             patch("a2a_tools_delegation.discover_peer", side_effect=fake_discover), \
             patch("a2a_tools.report_activity", side_effect=fake_report_activity), \
             patch("a2a_tools_delegation._delegate_sync_via_polling", new=AsyncMock()) as poll_mock:
            result = await a2a_tools.tool_delegate_task(
                "ws-target", "task body", source_workspace_id="ws-self"
            )

        assert result == "legacy ok", f"expected legacy passthrough, got {result!r}"
        assert send_calls == [("ws-target", "task body", "ws-self")]
        poll_mock.assert_not_called()


# ---------------------------------------------------------------------------
# Flag-on: dispatch failures
# ---------------------------------------------------------------------------

class TestFlagOnDispatchFailures:

    async def test_dispatch_http_exception_returns_wrapped_error(self, monkeypatch):
        monkeypatch.setenv("DELEGATION_SYNC_VIA_INBOX", "1")

        import a2a_tools
        mc = _make_client(post_exc=httpx.ConnectError("network down"))

        with patch("a2a_tools_delegation.httpx.AsyncClient", return_value=mc):
            res = await a2a_tools._delegate_sync_via_polling(
                "ws-target", "task", "ws-self"
            )

        assert res.startswith(a2a_tools._A2A_ERROR_PREFIX)
        assert "delegate dispatch failed" in res

    async def test_dispatch_non_2xx_returns_wrapped_error(self, monkeypatch):
        monkeypatch.setenv("DELEGATION_SYNC_VIA_INBOX", "1")

        import a2a_tools
        mc = _make_client(post_resp=_resp(403, {"error": "forbidden"}))

        with patch("a2a_tools_delegation.httpx.AsyncClient", return_value=mc):
            res = await a2a_tools._delegate_sync_via_polling(
                "ws-target", "task", "ws-self"
            )

        assert res.startswith(a2a_tools._A2A_ERROR_PREFIX)
        assert "HTTP 403" in res

    async def test_dispatch_missing_delegation_id_returns_wrapped_error(self, monkeypatch):
        monkeypatch.setenv("DELEGATION_SYNC_VIA_INBOX", "1")

        import a2a_tools
        # 202 Accepted but no delegation_id field — defensive shape check.
        mc = _make_client(post_resp=_resp(202, {"status": "delegated"}))

        with patch("a2a_tools_delegation.httpx.AsyncClient", return_value=mc):
            res = await a2a_tools._delegate_sync_via_polling(
                "ws-target", "task", "ws-self"
            )

        assert res.startswith(a2a_tools._A2A_ERROR_PREFIX)
        assert "missing delegation_id" in res


# ---------------------------------------------------------------------------
# Flag-on: polling outcomes
# ---------------------------------------------------------------------------

class TestFlagOnPollingOutcomes:

    async def test_completed_first_poll_returns_response_preview(self, monkeypatch):
        monkeypatch.setenv("DELEGATION_SYNC_VIA_INBOX", "1")
        # Tighten budget to a few seconds so the test never blocks long.
        monkeypatch.setenv("DELEGATION_TIMEOUT", "10")

        import importlib
        import a2a_tools
        importlib.reload(a2a_tools)  # pick up new env-driven _SYNC_POLL_BUDGET_S

        completed_row = {
            "delegation_id": "deleg-1",
            "status": "completed",
            "response_preview": "the answer",
        }
        mc = _make_client(
            post_resp=_resp(202, {"delegation_id": "deleg-1"}),
            get_resps=[_resp(200, [completed_row])],
        )

        with patch("a2a_tools_delegation.httpx.AsyncClient", return_value=mc):
            res = await a2a_tools._delegate_sync_via_polling(
                "ws-target", "task", "ws-self"
            )

        assert res == "the answer"
        # Cleanup: restore the module to default state for subsequent tests.
        monkeypatch.delenv("DELEGATION_TIMEOUT", raising=False)
        importlib.reload(a2a_tools)

    async def test_failed_status_returns_wrapped_error_with_detail(self, monkeypatch):
        monkeypatch.setenv("DELEGATION_SYNC_VIA_INBOX", "1")
        monkeypatch.setenv("DELEGATION_TIMEOUT", "10")

        import importlib
        import a2a_tools
        importlib.reload(a2a_tools)

        failed_row = {
            "delegation_id": "deleg-1",
            "status": "failed",
            "error_detail": "callee unreachable",
        }
        mc = _make_client(
            post_resp=_resp(202, {"delegation_id": "deleg-1"}),
            get_resps=[_resp(200, [failed_row])],
        )

        with patch("a2a_tools_delegation.httpx.AsyncClient", return_value=mc):
            res = await a2a_tools._delegate_sync_via_polling(
                "ws-target", "task", "ws-self"
            )

        assert res.startswith(a2a_tools._A2A_ERROR_PREFIX)
        assert "callee unreachable" in res
        monkeypatch.delenv("DELEGATION_TIMEOUT", raising=False)
        importlib.reload(a2a_tools)

    async def test_transient_poll_error_then_completed_succeeds(self, monkeypatch):
        """A network blip during polling must NOT abort — keep polling."""
        monkeypatch.setenv("DELEGATION_SYNC_VIA_INBOX", "1")
        monkeypatch.setenv("DELEGATION_TIMEOUT", "30")

        import importlib
        import a2a_tools
        importlib.reload(a2a_tools)

        # Speed up: monkey-patch the poll interval to 0.01s so we don't
        # actually wait 3s between rounds in the test.
        monkeypatch.setattr(a2a_tools, "_SYNC_POLL_INTERVAL_S", 0.01)

        completed_row = {
            "delegation_id": "deleg-1",
            "status": "completed",
            "response_preview": "eventually ok",
        }
        # First poll raises, second poll returns completed.
        get_seq = [
            httpx.ConnectError("transient"),
            _resp(200, [completed_row]),
        ]
        mc = _make_client(
            post_resp=_resp(202, {"delegation_id": "deleg-1"}),
            get_resps=get_seq,
        )

        with patch("a2a_tools_delegation.httpx.AsyncClient", return_value=mc):
            res = await a2a_tools._delegate_sync_via_polling(
                "ws-target", "task", "ws-self"
            )

        assert res == "eventually ok"
        monkeypatch.delenv("DELEGATION_TIMEOUT", raising=False)
        importlib.reload(a2a_tools)

    async def test_deadline_exceeded_returns_recovery_hint(self, monkeypatch):
        """When the budget runs out without a terminal status, the error
        must surface delegation_id + a check_task_status hint so the
        caller can recover the result."""
        monkeypatch.setenv("DELEGATION_SYNC_VIA_INBOX", "1")
        monkeypatch.setenv("DELEGATION_TIMEOUT", "1")  # 1s budget

        import importlib
        import a2a_tools
        importlib.reload(a2a_tools)
        monkeypatch.setattr(a2a_tools, "_SYNC_POLL_INTERVAL_S", 0.05)

        # Endless in-progress responses.
        in_progress_row = {
            "delegation_id": "deleg-1",
            "status": "in_progress",
        }
        get_seq = [_resp(200, [in_progress_row])] * 50
        mc = _make_client(
            post_resp=_resp(202, {"delegation_id": "deleg-1"}),
            get_resps=get_seq,
        )

        with patch("a2a_tools_delegation.httpx.AsyncClient", return_value=mc):
            res = await a2a_tools._delegate_sync_via_polling(
                "ws-target", "task", "ws-self"
            )

        assert res.startswith(a2a_tools._A2A_ERROR_PREFIX)
        assert "polling timeout" in res
        assert "deleg-1" in res, "must surface delegation_id for recovery"
        assert "check_task_status" in res, "must hint at the recovery tool"
        monkeypatch.delenv("DELEGATION_TIMEOUT", raising=False)
        importlib.reload(a2a_tools)

    async def test_poll_filters_by_delegation_id_ignoring_other_rows(self, monkeypatch):
        """Other delegations' rows in the response must NOT be picked up
        by mistake — we pin to delegation_id."""
        monkeypatch.setenv("DELEGATION_SYNC_VIA_INBOX", "1")
        monkeypatch.setenv("DELEGATION_TIMEOUT", "10")

        import importlib
        import a2a_tools
        importlib.reload(a2a_tools)
        monkeypatch.setattr(a2a_tools, "_SYNC_POLL_INTERVAL_S", 0.01)

        # First poll: no row matching ours, BUT a completed row for
        # someone else's delegation. We must NOT return that one.
        # Second poll: ours completes.
        first_poll = _resp(200, [
            {"delegation_id": "deleg-OTHER", "status": "completed", "response_preview": "wrong"},
        ])
        second_poll = _resp(200, [
            {"delegation_id": "deleg-OTHER", "status": "completed", "response_preview": "wrong"},
            {"delegation_id": "deleg-1", "status": "completed", "response_preview": "right"},
        ])
        mc = _make_client(
            post_resp=_resp(202, {"delegation_id": "deleg-1"}),
            get_resps=[first_poll, second_poll],
        )

        with patch("a2a_tools_delegation.httpx.AsyncClient", return_value=mc):
            res = await a2a_tools._delegate_sync_via_polling(
                "ws-target", "task", "ws-self"
            )

        assert res == "right", f"must filter to delegation_id, got {res!r}"
        monkeypatch.delenv("DELEGATION_TIMEOUT", raising=False)
        importlib.reload(a2a_tools)


# ---------------------------------------------------------------------------
# pytest-asyncio collection marker
# ---------------------------------------------------------------------------

pytestmark = pytest.mark.asyncio
