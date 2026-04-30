"""Tests for workspace/inbox.py — InboxState + activity API poller.

Covers the round-trip from a /activity row to an InboxMessage that the
agent observes via the three new MCP tools, plus the cursor-persistence
+ 410-recovery behavior that keeps the standalone molecule-mcp from
re-delivering already-handled messages after a restart.
"""
from __future__ import annotations

import threading
import time
from pathlib import Path
from typing import Any
from unittest.mock import MagicMock, patch

import pytest

import inbox


@pytest.fixture(autouse=True)
def _reset_singleton():
    """Each test starts with a clean module singleton + a fresh
    InboxState. Activation in one test must not leak into the next."""
    inbox._STATE = None
    yield
    inbox._STATE = None


@pytest.fixture()
def state(tmp_path: Path) -> inbox.InboxState:
    return inbox.InboxState(cursor_path=tmp_path / ".mcp_inbox_cursor")


# ---------------------------------------------------------------------------
# _extract_text — envelope shape coverage
# ---------------------------------------------------------------------------


def test_extract_text_jsonrpc_message_wrapper():
    body = {
        "jsonrpc": "2.0",
        "method": "message/send",
        "params": {"message": {"parts": [{"type": "text", "text": "hello"}]}},
    }
    assert inbox._extract_text(body, None) == "hello"


def test_extract_text_a2a_v1_kind_field():
    """A2A SDK v1 uses ``kind`` instead of ``type`` as the part
    discriminator. Hosted SaaS workspaces send the v1 shape today —
    this case is what live canvas-user messages look like in
    activity_logs.request_body."""
    body = {
        "params": {
            "message": {
                "role": "user",
                "parts": [{"kind": "text", "text": "hello from canvas"}],
            }
        }
    }
    assert inbox._extract_text(body, None) == "hello from canvas"


def test_extract_text_jsonrpc_params_parts():
    body = {"params": {"parts": [{"type": "text", "text": "from peer"}]}}
    assert inbox._extract_text(body, None) == "from peer"


def test_extract_text_shorthand_parts():
    body = {"parts": [{"type": "text", "text": "shorthand"}]}
    assert inbox._extract_text(body, None) == "shorthand"


def test_extract_text_concatenates_multiple_parts():
    body = {
        "parts": [
            {"type": "text", "text": "hello "},
            {"type": "text", "text": "world"},
            {"type": "image", "url": "https://example.invalid/x.png"},
        ]
    }
    assert inbox._extract_text(body, None) == "hello world"


def test_extract_text_falls_back_to_summary():
    assert inbox._extract_text(None, "fallback") == "fallback"
    assert inbox._extract_text({"unrelated": True}, "fallback") == "fallback"


def test_extract_text_returns_placeholder_when_nothing_usable():
    assert inbox._extract_text(None, None) == "(empty A2A message)"


# ---------------------------------------------------------------------------
# message_from_activity
# ---------------------------------------------------------------------------


def test_message_from_activity_canvas_user():
    row = {
        "id": "act-1",
        "source_id": None,
        "method": "message/send",
        "summary": "ignored",
        "request_body": {
            "params": {"message": {"parts": [{"type": "text", "text": "hi"}]}}
        },
        "created_at": "2026-04-30T22:00:00Z",
    }
    msg = inbox.message_from_activity(row)
    assert msg.activity_id == "act-1"
    assert msg.text == "hi"
    assert msg.peer_id == ""
    assert msg.method == "message/send"
    d = msg.to_dict()
    assert d["kind"] == "canvas_user"


def test_message_from_activity_peer_agent():
    row = {
        "id": "act-2",
        "source_id": "ws-peer-uuid",
        "method": "tasks/send",
        "summary": "delegate",
        "request_body": {"parts": [{"type": "text", "text": "do task"}]},
        "created_at": "2026-04-30T22:01:00Z",
    }
    msg = inbox.message_from_activity(row)
    assert msg.peer_id == "ws-peer-uuid"
    assert msg.to_dict()["kind"] == "peer_agent"


def test_message_from_activity_handles_string_request_body():
    row = {
        "id": "act-3",
        "source_id": None,
        "method": "message/send",
        "summary": None,
        "request_body": '{"parts": [{"type": "text", "text": "json string"}]}',
        "created_at": "2026-04-30T22:02:00Z",
    }
    assert inbox.message_from_activity(row).text == "json string"


# ---------------------------------------------------------------------------
# InboxState — queue + wait/peek/pop semantics
# ---------------------------------------------------------------------------


def _msg(activity_id: str, text: str = "", peer_id: str = "") -> inbox.InboxMessage:
    return inbox.InboxMessage(
        activity_id=activity_id,
        text=text or activity_id,
        peer_id=peer_id,
        method="message/send",
        created_at="2026-04-30T22:00:00Z",
    )


def test_record_then_peek(state: inbox.InboxState):
    state.record(_msg("a"))
    state.record(_msg("b"))
    out = state.peek(limit=10)
    assert [m.activity_id for m in out] == ["a", "b"]


def test_record_dedupes_by_activity_id(state: inbox.InboxState):
    state.record(_msg("a"))
    state.record(_msg("a"))  # same id — must drop the second
    assert len(state.peek(10)) == 1


def test_pop_removes_specific_message(state: inbox.InboxState):
    state.record(_msg("a"))
    state.record(_msg("b"))
    removed = state.pop("a")
    assert removed is not None and removed.activity_id == "a"
    remaining = state.peek(10)
    assert [m.activity_id for m in remaining] == ["b"]


def test_pop_missing_id_returns_none(state: inbox.InboxState):
    state.record(_msg("a"))
    # Bind the result before asserting so the call still runs under
    # ``python -O`` (which strips bare assert statements).
    result = state.pop("does-not-exist")
    assert result is None
    # Original message still present
    assert len(state.peek(10)) == 1


def test_wait_returns_existing_head_immediately(state: inbox.InboxState):
    state.record(_msg("a"))
    start = time.monotonic()
    msg = state.wait(timeout_secs=5.0)
    elapsed = time.monotonic() - start
    assert msg is not None and msg.activity_id == "a"
    assert elapsed < 0.5, f"wait should not block when queue non-empty (took {elapsed:.2f}s)"


def test_wait_blocks_until_message_arrives(state: inbox.InboxState):
    def producer():
        time.sleep(0.05)
        state.record(_msg("late"))

    threading.Thread(target=producer, daemon=True).start()
    msg = state.wait(timeout_secs=2.0)
    assert msg is not None and msg.activity_id == "late"


def test_wait_returns_none_on_timeout(state: inbox.InboxState):
    msg = state.wait(timeout_secs=0.05)
    assert msg is None


def test_wait_does_not_pop(state: inbox.InboxState):
    """wait() is non-destructive — caller decides when to inbox_pop."""
    state.record(_msg("a"))
    state.wait(timeout_secs=1.0)
    state.wait(timeout_secs=1.0)
    assert len(state.peek(10)) == 1


# ---------------------------------------------------------------------------
# Cursor persistence
# ---------------------------------------------------------------------------


def test_load_cursor_returns_none_when_file_absent(state: inbox.InboxState):
    assert state.load_cursor() is None


def test_save_then_load_cursor_round_trip(state: inbox.InboxState):
    state.save_cursor("act-cursor-1")
    # Reset the cached flag to force a re-read
    state._cursor_loaded = False
    state._cursor = None
    assert state.load_cursor() == "act-cursor-1"


def test_save_cursor_creates_parent_directory(tmp_path: Path):
    nested = tmp_path / "nested" / "configs" / ".mcp_inbox_cursor"
    state = inbox.InboxState(cursor_path=nested)
    state.save_cursor("act-x")
    assert nested.read_text() == "act-x"


def test_reset_cursor_deletes_file(state: inbox.InboxState):
    state.save_cursor("act-y")
    assert state.cursor_path.is_file()
    state.reset_cursor()
    assert not state.cursor_path.is_file()
    assert state.load_cursor() is None


# ---------------------------------------------------------------------------
# Module singleton
# ---------------------------------------------------------------------------


def test_get_state_returns_none_before_activate():
    assert inbox.get_state() is None


def test_activate_then_get_state(state: inbox.InboxState):
    inbox.activate(state)
    assert inbox.get_state() is state


def test_activate_idempotent(state: inbox.InboxState):
    inbox.activate(state)
    inbox.activate(state)  # same state — no-op, no warning expected
    assert inbox.get_state() is state


# ---------------------------------------------------------------------------
# _poll_once — HTTP behavior
# ---------------------------------------------------------------------------


def _make_response(status_code: int, json_body: Any = None, text: str = "") -> MagicMock:
    resp = MagicMock()
    resp.status_code = status_code
    if json_body is not None:
        resp.json.return_value = json_body
    else:
        resp.json.side_effect = ValueError("no json")
    resp.text = text
    return resp


def _patch_httpx(returning: MagicMock):
    """Replace httpx.Client with a context-manager mock that returns
    ``returning`` from .get(). Captures the GET call args for assertion."""
    client = MagicMock()
    client.__enter__ = MagicMock(return_value=client)
    client.__exit__ = MagicMock(return_value=False)
    client.get = MagicMock(return_value=returning)
    return patch("httpx.Client", return_value=client), client


def test_poll_once_fresh_start_uses_since_secs(state: inbox.InboxState):
    resp = _make_response(200, [])
    p, client = _patch_httpx(resp)
    with p:
        n = inbox._poll_once(state, "http://platform", "ws-1", {})
    assert n == 0
    _, kwargs = client.get.call_args
    assert kwargs["params"]["type"] == "a2a_receive"
    assert "since_secs" in kwargs["params"]
    assert "since_id" not in kwargs["params"]


def test_poll_once_with_cursor_uses_since_id(state: inbox.InboxState):
    state.save_cursor("act-existing")
    resp = _make_response(200, [])
    p, client = _patch_httpx(resp)
    with p:
        inbox._poll_once(state, "http://platform", "ws-1", {})
    _, kwargs = client.get.call_args
    assert kwargs["params"]["since_id"] == "act-existing"
    assert "since_secs" not in kwargs["params"]


def test_poll_once_410_resets_cursor(state: inbox.InboxState):
    state.save_cursor("act-stale")
    resp = _make_response(410, text="cursor pruned")
    p, _ = _patch_httpx(resp)
    with p:
        inbox._poll_once(state, "http://platform", "ws-1", {})
    assert state.load_cursor() is None
    assert not state.cursor_path.is_file()


def test_poll_once_records_messages_and_advances_cursor(state: inbox.InboxState):
    state.save_cursor("act-old")
    rows = [
        {
            "id": "act-1",
            "source_id": None,
            "method": "message/send",
            "summary": None,
            "request_body": {"parts": [{"type": "text", "text": "first"}]},
            "created_at": "2026-04-30T22:00:00Z",
        },
        {
            "id": "act-2",
            "source_id": "ws-peer",
            "method": "tasks/send",
            "summary": None,
            "request_body": {"parts": [{"type": "text", "text": "second"}]},
            "created_at": "2026-04-30T22:00:01Z",
        },
    ]
    resp = _make_response(200, rows)
    p, _ = _patch_httpx(resp)
    with p:
        n = inbox._poll_once(state, "http://platform", "ws-1", {})
    assert n == 2
    queue = state.peek(10)
    assert [m.activity_id for m in queue] == ["act-1", "act-2"]
    assert state.load_cursor() == "act-2"


def test_poll_once_500_does_not_raise(state: inbox.InboxState):
    resp = _make_response(500, text="boom")
    p, _ = _patch_httpx(resp)
    with p:
        n = inbox._poll_once(state, "http://platform", "ws-1", {})
    assert n == 0
    # Cursor untouched
    assert state.load_cursor() is None


def test_poll_once_handles_non_list_payload(state: inbox.InboxState):
    resp = _make_response(200, {"error": "unexpected"})
    p, _ = _patch_httpx(resp)
    with p:
        n = inbox._poll_once(state, "http://platform", "ws-1", {})
    assert n == 0


def test_poll_once_initial_backlog_reverses_to_chronological(state: inbox.InboxState):
    """When no cursor is set, /activity returns DESC; the poller must
    reverse so the saved cursor is the freshest row + record order
    is chronological."""
    rows_desc = [
        {
            "id": "act-newest",
            "source_id": None,
            "method": "message/send",
            "summary": None,
            "request_body": {"parts": [{"type": "text", "text": "newest"}]},
            "created_at": "2026-04-30T22:00:02Z",
        },
        {
            "id": "act-oldest",
            "source_id": None,
            "method": "message/send",
            "summary": None,
            "request_body": {"parts": [{"type": "text", "text": "oldest"}]},
            "created_at": "2026-04-30T22:00:00Z",
        },
    ]
    resp = _make_response(200, rows_desc)
    p, _ = _patch_httpx(resp)
    with p:
        inbox._poll_once(state, "http://platform", "ws-1", {})
    queue = state.peek(10)
    assert [m.activity_id for m in queue] == ["act-oldest", "act-newest"]
    # Cursor is the newest row, so the next poll picks up only what's
    # newer — re-restoring forward chronological progression.
    assert state.load_cursor() == "act-newest"


def test_start_poller_thread_is_daemon(state: inbox.InboxState):
    """Daemon flag is required so the poller dies with the parent
    process; a non-daemon poller would leak across `claude` restarts
    and write to a stale workspace."""
    resp = _make_response(200, [])
    p, _ = _patch_httpx(resp)
    with p, patch("platform_auth.auth_headers", return_value={}):
        # Use a very short interval so the loop body runs at least once
        # before we exit the test.
        t = inbox.start_poller_thread(state, "http://platform", "ws-1", interval=0.01)
        time.sleep(0.05)
    assert t.daemon is True
    assert t.is_alive()


# ---------------------------------------------------------------------------
# default_cursor_path respects CONFIGS_DIR
# ---------------------------------------------------------------------------


def test_default_cursor_path_uses_configs_dir(monkeypatch, tmp_path: Path):
    monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
    assert inbox.default_cursor_path() == tmp_path / ".mcp_inbox_cursor"


def test_default_cursor_path_falls_back_to_default(monkeypatch):
    monkeypatch.delenv("CONFIGS_DIR", raising=False)
    assert inbox.default_cursor_path() == Path("/configs") / ".mcp_inbox_cursor"
