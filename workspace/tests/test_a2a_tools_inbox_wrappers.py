"""Direct unit tests for the three inbox tool wrappers in ``a2a_tools``.

After RFC #2873 iter 4d (messaging extraction), ``a2a_tools.py`` is
mostly back-compat re-exports — the only behavior still defined here
is ``report_activity`` plus three thin wrappers around the inbox state
machine: ``tool_inbox_peek`` / ``tool_inbox_pop`` / ``tool_wait_for_message``.

These wrappers were never exercised at the module level, so the
critical-path coverage gate (75% per-file floor for MCP/inbox/auth)
dropped to 54% on iter 4d. This file pins each wrapper's behavior
directly so the floor is met without changing the gate.

The wrappers are ~40 LOC of glue. The full delivery behavior
(persistence, 410 recovery, etc.) is exercised in test_inbox.py.
"""
from __future__ import annotations

import asyncio
import json
from unittest.mock import MagicMock, patch

import pytest


@pytest.fixture(autouse=True)
def _require_workspace_id(monkeypatch):
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "http://test.invalid")
    yield


def _run(coro):
    return asyncio.get_event_loop().run_until_complete(coro)


# ---------------------------------------------------------------------------
# tool_inbox_peek
# ---------------------------------------------------------------------------


class TestToolInboxPeek:
    def test_returns_not_enabled_when_state_none(self):
        import a2a_tools

        with patch("inbox.get_state", return_value=None):
            out = _run(a2a_tools.tool_inbox_peek())
        assert "not enabled" in out

    def test_returns_json_array_of_messages(self):
        import a2a_tools

        msg1 = MagicMock()
        msg1.to_dict.return_value = {"activity_id": "a1", "kind": "canvas_user"}
        msg2 = MagicMock()
        msg2.to_dict.return_value = {"activity_id": "a2", "kind": "peer_agent"}

        fake_state = MagicMock()
        fake_state.peek.return_value = [msg1, msg2]

        with patch("inbox.get_state", return_value=fake_state):
            out = _run(a2a_tools.tool_inbox_peek(limit=5))
        # peek limit is forwarded
        fake_state.peek.assert_called_once_with(limit=5)
        parsed = json.loads(out)
        assert len(parsed) == 2
        assert parsed[0]["activity_id"] == "a1"

    def test_non_int_limit_falls_back_to_10(self):
        import a2a_tools

        fake_state = MagicMock()
        fake_state.peek.return_value = []
        with patch("inbox.get_state", return_value=fake_state):
            _run(a2a_tools.tool_inbox_peek(limit="garbage"))  # type: ignore[arg-type]
        fake_state.peek.assert_called_once_with(limit=10)


# ---------------------------------------------------------------------------
# tool_inbox_pop
# ---------------------------------------------------------------------------


class TestToolInboxPop:
    def test_returns_not_enabled_when_state_none(self):
        import a2a_tools

        with patch("inbox.get_state", return_value=None):
            out = _run(a2a_tools.tool_inbox_pop("act-1"))
        assert "not enabled" in out

    def test_rejects_empty_activity_id(self):
        import a2a_tools

        fake_state = MagicMock()
        with patch("inbox.get_state", return_value=fake_state):
            out = _run(a2a_tools.tool_inbox_pop(""))
        assert "activity_id is required" in out
        fake_state.pop.assert_not_called()

    def test_rejects_non_str_activity_id(self):
        import a2a_tools

        fake_state = MagicMock()
        with patch("inbox.get_state", return_value=fake_state):
            out = _run(a2a_tools.tool_inbox_pop(123))  # type: ignore[arg-type]
        assert "activity_id is required" in out
        fake_state.pop.assert_not_called()

    def test_returns_removed_true_when_popped(self):
        import a2a_tools

        fake_state = MagicMock()
        fake_state.pop.return_value = MagicMock()  # truthy = something was removed
        with patch("inbox.get_state", return_value=fake_state):
            out = _run(a2a_tools.tool_inbox_pop("act-7"))
        parsed = json.loads(out)
        assert parsed == {"removed": True, "activity_id": "act-7"}
        fake_state.pop.assert_called_once_with("act-7")

    def test_returns_removed_false_when_unknown(self):
        import a2a_tools

        fake_state = MagicMock()
        fake_state.pop.return_value = None
        with patch("inbox.get_state", return_value=fake_state):
            out = _run(a2a_tools.tool_inbox_pop("act-missing"))
        parsed = json.loads(out)
        assert parsed == {"removed": False, "activity_id": "act-missing"}


# ---------------------------------------------------------------------------
# tool_wait_for_message
# ---------------------------------------------------------------------------


class TestToolWaitForMessage:
    def test_returns_not_enabled_when_state_none(self):
        import a2a_tools

        with patch("inbox.get_state", return_value=None):
            out = _run(a2a_tools.tool_wait_for_message(timeout_secs=1.0))
        assert "not enabled" in out

    def test_timeout_payload_when_no_message(self):
        import a2a_tools

        fake_state = MagicMock()
        fake_state.wait.return_value = None
        with patch("inbox.get_state", return_value=fake_state):
            out = _run(a2a_tools.tool_wait_for_message(timeout_secs=0.1))
        parsed = json.loads(out)
        assert parsed["timeout"] is True
        assert parsed["timeout_secs"] == 0.1

    def test_returns_message_when_delivered(self):
        import a2a_tools

        msg = MagicMock()
        msg.to_dict.return_value = {"activity_id": "a-9", "kind": "peer_agent"}
        fake_state = MagicMock()
        fake_state.wait.return_value = msg
        with patch("inbox.get_state", return_value=fake_state):
            out = _run(a2a_tools.tool_wait_for_message(timeout_secs=2.0))
        parsed = json.loads(out)
        assert parsed["activity_id"] == "a-9"

    def test_timeout_clamped_to_300(self):
        import a2a_tools

        fake_state = MagicMock()
        fake_state.wait.return_value = None
        with patch("inbox.get_state", return_value=fake_state):
            _run(a2a_tools.tool_wait_for_message(timeout_secs=99999))
        # Whatever wait was called with, it must not exceed 300
        passed = fake_state.wait.call_args.args[0]
        assert passed == 300.0

    def test_timeout_clamped_to_zero_floor(self):
        import a2a_tools

        fake_state = MagicMock()
        fake_state.wait.return_value = None
        with patch("inbox.get_state", return_value=fake_state):
            _run(a2a_tools.tool_wait_for_message(timeout_secs=-5))
        passed = fake_state.wait.call_args.args[0]
        assert passed == 0.0

    def test_non_numeric_timeout_falls_back_to_60(self):
        import a2a_tools

        fake_state = MagicMock()
        fake_state.wait.return_value = None
        with patch("inbox.get_state", return_value=fake_state):
            _run(a2a_tools.tool_wait_for_message(timeout_secs="garbage"))  # type: ignore[arg-type]
        passed = fake_state.wait.call_args.args[0]
        assert passed == 60.0
