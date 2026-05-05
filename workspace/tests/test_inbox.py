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


# ---------------------------------------------------------------------------
# _is_self_notify_row + the echo-loop guard in _poll_once
# ---------------------------------------------------------------------------
#
# The workspace-server's `/notify` handler writes the agent's own
# send_message_to_user POSTs to activity_logs as activity_type=
# 'a2a_receive' with method='notify' and no source_id, so the canvas
# chat-history loader can restore those bubbles after a page reload.
# Without a guard, the poller picks them up and pushes them back as
# inbound — confirmed live 2026-05-01: the agent observed its own
# outbound as `← molecule: Agent message: ...`.
#
# These tests pin both the predicate (`_is_self_notify_row`) and the
# integrated behavior in `_poll_once` so a future refactor that drops
# either half breaks loudly. Long-term the upstream fix is renaming
# the activity_type at the workspace-server (#2469); this guard stays
# regardless because it only excludes rows we never want.


def test_is_self_notify_row_true_for_method_notify_no_peer():
    assert inbox._is_self_notify_row({"method": "notify", "source_id": None}) is True
    assert inbox._is_self_notify_row({"method": "notify", "source_id": ""}) is True
    # source_id key absent — same shape (None on .get).
    assert inbox._is_self_notify_row({"method": "notify"}) is True


def test_is_self_notify_row_false_for_real_canvas_inbound():
    """Real canvas-user message: method='message/send' (not notify),
    source_id None (no peer)."""
    row = {"method": "message/send", "source_id": None}
    assert inbox._is_self_notify_row(row) is False


def test_is_self_notify_row_false_for_real_peer_inbound():
    """Real peer-agent message: method='message/send' or 'tasks/send',
    source_id is the sender workspace UUID."""
    row = {"method": "tasks/send", "source_id": "ws-peer-uuid"}
    assert inbox._is_self_notify_row(row) is False


def test_is_self_notify_row_false_for_method_notify_with_peer():
    """Defensive: a future caller using method='notify' WITH a real
    peer_id is treated as a real inbound, not a self-notify. Drops the
    guard if upstream ever repurposes the method='notify' shape."""
    row = {"method": "notify", "source_id": "ws-peer-uuid"}
    assert inbox._is_self_notify_row(row) is False


def test_poll_once_skips_self_notify_rows(state: inbox.InboxState):
    """The integrated guard: a self-notify row in the activity payload
    must NOT land in the inbox queue. This is the regression pin for
    the 2026-05-01 echo-loop incident."""
    rows = [
        {
            "id": "act-real",
            "source_id": None,
            "method": "message/send",
            "summary": None,
            "request_body": {"parts": [{"type": "text", "text": "real inbound"}]},
            "created_at": "2026-04-30T22:00:00Z",
        },
        {
            "id": "act-self-notify",
            "source_id": None,
            "method": "notify",
            "summary": "Agent message: Hi! What can I help you with today?",
            "request_body": None,
            "created_at": "2026-04-30T22:00:01Z",
        },
    ]
    resp = _make_response(200, rows)
    p, _ = _patch_httpx(resp)
    with p:
        n = inbox._poll_once(state, "http://platform", "ws-1", {})

    # Only the real inbound counted; self-notify silently dropped.
    assert n == 1
    queue = state.peek(10)
    assert [m.activity_id for m in queue] == ["act-real"]


def test_poll_once_advances_cursor_past_self_notify(state: inbox.InboxState):
    """Cursor must advance past self-notify rows even though we don't
    enqueue them. Otherwise the next poll re-fetches the same self-
    notify on every iteration (until a real inbound arrives), wasting
    a request and pinning the cursor backward."""
    state.save_cursor("act-old")
    rows = [
        {
            "id": "act-self-notify",
            "source_id": None,
            "method": "notify",
            "summary": "Agent message: hello",
            "request_body": None,
            "created_at": "2026-04-30T22:00:00Z",
        },
    ]
    resp = _make_response(200, rows)
    p, _ = _patch_httpx(resp)
    with p:
        n = inbox._poll_once(state, "http://platform", "ws-1", {})

    assert n == 0
    assert state.peek(10) == []
    # Cursor must move past the skipped row so we don't re-poll it.
    assert state.load_cursor() == "act-self-notify"


def test_poll_once_self_notify_does_not_fire_notification(state: inbox.InboxState):
    """The notification callback (channel push to Claude Code etc.)
    must not fire for self-notify rows. Otherwise a notification-
    capable host gets the same echo loop the queue side avoids."""
    rows = [
        {
            "id": "act-self-notify",
            "source_id": None,
            "method": "notify",
            "summary": "Agent message: hello",
            "request_body": None,
            "created_at": "2026-04-30T22:00:00Z",
        },
    ]
    received: list[dict] = []
    inbox.set_notification_callback(received.append)
    try:
        resp = _make_response(200, rows)
        p, _ = _patch_httpx(resp)
        with p:
            inbox._poll_once(state, "http://platform", "ws-1", {})
    finally:
        inbox.set_notification_callback(None)

    assert received == [], (
        "self-notify rows must not surface as MCP notifications — "
        "doing so re-creates the echo loop on push-capable hosts"
    )


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


def test_default_cursor_path_falls_back_to_default(tmp_path, monkeypatch):
    """When CONFIGS_DIR is unset, the cursor path resolves through
    configs_dir.resolve() — /configs in-container, ~/.molecule-workspace
    on a non-container host. Issue #2458."""
    import os
    monkeypatch.delenv("CONFIGS_DIR", raising=False)
    fake_home = tmp_path / "home"
    fake_home.mkdir()
    monkeypatch.setenv("HOME", str(fake_home))
    path = inbox.default_cursor_path()
    if Path("/configs").exists() and os.access("/configs", os.W_OK):
        assert path == Path("/configs") / ".mcp_inbox_cursor"
    else:
        assert path == fake_home / ".molecule-workspace" / ".mcp_inbox_cursor"


# ---------------------------------------------------------------------------
# Notification callback bridge — push UX for notification-capable hosts
# ---------------------------------------------------------------------------
#
# `record()` is called from the poller daemon thread when a new activity
# row arrives. Notification-capable MCP hosts (Claude Code) want to be
# pushed a notification — the universal wheel registers a callback via
# `set_notification_callback()` that fires the MCP notification. Pollers
# (`wait_for_message`/`inbox_peek`) keep working unchanged.


@pytest.fixture(autouse=True)
def _reset_notification_callback():
    """Each test starts with no callback registered. Notification
    state must not leak across tests — same pattern as _reset_singleton."""
    inbox.set_notification_callback(None)
    yield
    inbox.set_notification_callback(None)


def test_record_fires_notification_callback_with_message_dict(state: inbox.InboxState):
    """When a callback is registered, record() invokes it with the
    canonical to_dict() shape — same shape inbox_peek returns to the
    agent. Callers can build MCP notification payloads from this
    without re-deriving fields."""
    received: list[dict] = []
    inbox.set_notification_callback(received.append)

    state.record(_msg("act-1", peer_id="ws-peer", text="hello"))

    assert len(received) == 1
    payload = received[0]
    assert payload["activity_id"] == "act-1"
    assert payload["text"] == "hello"
    assert payload["peer_id"] == "ws-peer"
    assert payload["kind"] == "peer_agent"  # to_dict derives this
    assert payload["method"] == "message/send"


def test_record_dedupe_does_not_refire_callback(state: inbox.InboxState):
    """The activity_id dedupe path must short-circuit BEFORE invoking
    the callback — otherwise a notification-capable host would see
    duplicate push events on poller backlog overlap."""
    received: list[dict] = []
    inbox.set_notification_callback(received.append)

    state.record(_msg("act-1"))
    state.record(_msg("act-1"))  # dedupe — same id

    assert len(received) == 1, (
        f"expected 1 callback (dedupe), got {len(received)} — "
        f"would cause duplicate Claude conversation interrupts"
    )


def test_record_callback_exception_does_not_break_inbox(state: inbox.InboxState):
    """A raising callback (e.g. asyncio loop closed mid-shutdown,
    serialization error on an exotic message) must NOT prevent the
    message from landing in the queue. Notification delivery is
    best-effort; inbox correctness is not negotiable."""

    def boom(_payload):
        raise RuntimeError("simulated callback failure")

    inbox.set_notification_callback(boom)

    # Must not raise, must still queue the message.
    state.record(_msg("act-1"))

    queued = state.peek(10)
    assert len(queued) == 1
    assert queued[0].activity_id == "act-1"


def test_record_no_callback_registered_is_no_op(state: inbox.InboxState):
    """When no callback is set (in-container path, or before
    activation), record() proceeds normally — no None-call crash."""
    # No set_notification_callback() in this test — autouse fixture
    # cleared any previous registration.
    state.record(_msg("act-1"))
    assert len(state.peek(10)) == 1


def test_set_notification_callback_replaces_previous(state: inbox.InboxState):
    """Re-registering the callback replaces the previous — only the
    latest callback fires. Test ensures the universal wheel can update
    the bridge if its asyncio loop is replaced (e.g. graceful restart)."""
    first: list[dict] = []
    second: list[dict] = []
    inbox.set_notification_callback(first.append)
    inbox.set_notification_callback(second.append)

    state.record(_msg("act-1"))

    assert len(first) == 0, "first callback should be unregistered"
    assert len(second) == 1, "second callback should receive the event"


def test_set_notification_callback_none_clears(state: inbox.InboxState):
    """Setting None clears the callback — used by tests + the wheel's
    shutdown path."""
    received: list[dict] = []
    inbox.set_notification_callback(received.append)
    inbox.set_notification_callback(None)

    state.record(_msg("act-1"))

    assert received == []


# ---------------------------------------------------------------------------
# Phase 2 — chat_upload_receive rows route to inbox_uploads.fetch_and_stage
# ---------------------------------------------------------------------------


def test_poll_once_skips_chat_upload_row_from_queue(state: inbox.InboxState, monkeypatch, tmp_path):
    """A row with method='chat_upload_receive' must NOT enqueue as a
    chat message — it's a side-effect telling the workspace to fetch
    bytes. Pin the contract so a refactor that flattens the row loop
    can't silently re-enqueue these as 'empty A2A message' rows."""
    import inbox_uploads
    monkeypatch.setattr(inbox_uploads, "CHAT_UPLOAD_DIR", str(tmp_path / "chat-uploads"))
    inbox_uploads.get_cache().clear()

    rows = [
        {
            "id": "act-1",
            "source_id": None,
            "method": "chat_upload_receive",
            "summary": "chat_upload_receive: foo.pdf",
            "request_body": {
                "file_id": "abc123",
                "name": "foo.pdf",
                "mimeType": "application/pdf",
                "size": 4,
                "uri": "platform-pending:ws-1/abc123",
            },
            "created_at": "2026-05-04T10:00:00Z",
        },
    ]
    resp = _make_response(200, rows)
    p, _ = _patch_httpx(resp)
    fetch_called = []

    def fake_fetch(row, **kwargs):
        fetch_called.append((row.get("id"), kwargs["workspace_id"]))
        return "workspace:/local/foo.pdf"

    with p, patch.object(inbox_uploads, "fetch_and_stage", fake_fetch):
        n = inbox._poll_once(state, "http://platform", "ws-1", {})

    # Not enqueued + cursor advanced.
    assert n == 0
    assert state.peek(10) == []
    assert state.load_cursor() == "act-1"
    # fetch_and_stage was invoked with the row and workspace_id.
    assert fetch_called == [("act-1", "ws-1")]


def test_poll_once_chat_upload_row_then_chat_message_rewrites_uri(state: inbox.InboxState, monkeypatch, tmp_path):
    """The classic ordering: upload-receive row first (lower id), chat
    message referencing platform-pending: URI second. The chat message
    that lands in the inbox must have its URI rewritten to the local
    workspace: URI before the agent sees it.
    """
    import inbox_uploads
    monkeypatch.setattr(inbox_uploads, "CHAT_UPLOAD_DIR", str(tmp_path / "chat-uploads"))
    cache = inbox_uploads.get_cache()
    cache.clear()

    # Pretend the fetch already populated the cache. (The real flow
    # populates it inside fetch_and_stage; we patch that to keep the
    # test focused on the rewrite contract.)
    cache.set("platform-pending:ws-1/abc123", "workspace:/workspace/.molecule/chat-uploads/xx-foo.pdf")

    rows = [
        {
            "id": "act-1",
            "source_id": None,
            "method": "chat_upload_receive",
            "summary": "chat_upload_receive: foo.pdf",
            "request_body": {
                "file_id": "abc123",
                "name": "foo.pdf",
                "mimeType": "application/pdf",
                "size": 4,
                "uri": "platform-pending:ws-1/abc123",
            },
            "created_at": "2026-05-04T10:00:00Z",
        },
        {
            "id": "act-2",
            "source_id": None,
            "method": "message/send",
            "summary": None,
            "request_body": {
                "params": {
                    "message": {
                        "parts": [
                            {"kind": "text", "text": "look at this"},
                            {
                                "kind": "file",
                                "file": {
                                    "uri": "platform-pending:ws-1/abc123",
                                    "name": "foo.pdf",
                                },
                            },
                        ]
                    }
                }
            },
            "created_at": "2026-05-04T10:00:01Z",
        },
    ]
    resp = _make_response(200, rows)
    p, _ = _patch_httpx(resp)

    def fake_fetch(row, **kwargs):
        return "workspace:/workspace/.molecule/chat-uploads/xx-foo.pdf"

    with p, patch.object(inbox_uploads, "fetch_and_stage", fake_fetch):
        n = inbox._poll_once(state, "http://platform", "ws-1", {})

    # Only the chat message is enqueued.
    assert n == 1
    queue = state.peek(10)
    assert len(queue) == 1
    msg = queue[0]
    assert msg.activity_id == "act-2"
    # The URI in the row's request_body was mutated by message_from_activity
    # → rewrite_request_body. Re-extracting reveals the rewritten value.
    rewritten = rows[1]["request_body"]["params"]["message"]["parts"][1]["file"]["uri"]
    assert rewritten == "workspace:/workspace/.molecule/chat-uploads/xx-foo.pdf"


def test_poll_once_chat_upload_row_advances_cursor_even_on_fetch_failure(
    state: inbox.InboxState, monkeypatch, tmp_path
):
    """A permanent network failure on /content must NOT stall the cursor
    — otherwise one bad upload blocks all real chat traffic for the
    workspace. fetch_and_stage returns None on failure, but the row is
    still considered handled from the cursor's perspective."""
    import inbox_uploads
    monkeypatch.setattr(inbox_uploads, "CHAT_UPLOAD_DIR", str(tmp_path / "chat-uploads"))

    rows = [
        {
            "id": "act-broken",
            "source_id": None,
            "method": "chat_upload_receive",
            "summary": "chat_upload_receive: doomed.pdf",
            "request_body": {
                "file_id": "doom",
                "name": "doomed.pdf",
                "uri": "platform-pending:ws-1/doom",
            },
            "created_at": "2026-05-04T10:00:00Z",
        },
    ]
    resp = _make_response(200, rows)
    p, _ = _patch_httpx(resp)

    def fake_fetch(row, **kwargs):
        return None  # network failure

    with p, patch.object(inbox_uploads, "fetch_and_stage", fake_fetch):
        inbox._poll_once(state, "http://platform", "ws-1", {})

    assert state.peek(10) == []
    assert state.load_cursor() == "act-broken"
