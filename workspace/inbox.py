"""In-memory inbox + background poller for the standalone molecule-mcp path.

Purpose
-------
The universal MCP server (a2a_mcp_server.py) is OUTBOUND-ONLY by default —
it gives an MCP-aware agent the same A2A delegation, peer-discovery, and
memory tools that container-bound runtimes already have. There is no
inbound delivery path: when the canvas user types a message or a peer
sends an A2A request, the activity lands on the platform but the
standalone agent never sees it.

This module closes that gap WITHOUT requiring a tunnel or a public agent
URL. A daemon thread polls ``/workspaces/:id/activity?type=a2a_receive``
on the platform and stages new rows in an in-memory deque. Three new MCP
tools (``inbox_peek``, ``inbox_pop``, ``wait_for_message``) let the
agent observe the queue.

Why a poller (not push)
-----------------------
runtime=external workspaces have ``delivery_mode="poll"`` — the platform
records inbound A2A in ``activity_logs`` but does not call back to the
agent. A poller is the only inbound surface that works without the
operator exposing a public URL through a tunnel. 5s cadence matches
the molecule-mcp-claude-channel plugin's POLL_INTERVAL — it's already
proven on staging for the channel-based delivery path.

Cursor model
------------
``activity_logs.id`` is the cursor (server-assigned, monotonic). We
persist it to ``${CONFIGS_DIR}/.mcp_inbox_cursor`` so an agent restart
doesn't replay the last 10 minutes of inbound traffic and re-act on
already-handled messages. On 410 (cursor pruned) we drop back to
``since_secs=600`` for a bounded backlog and let the cursor advance
naturally from there.

Scope
-----
Standalone molecule-mcp ONLY. The in-container runtime has its own
push delivery (main.py + canvas WebSocket); we never want both
running at once or a single message would be delivered twice. The
caller (mcp_cli.main) gates activation explicitly via
``activate(state)``; in-container code that imports this module by
accident gets a no-op until activate is called.
"""

from __future__ import annotations

import json
import logging
import os
import threading
import time
from collections import deque
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

import configs_dir

logger = logging.getLogger(__name__)

# Poll cadence. 5s mirrors the molecule-mcp-claude-channel plugin's
# proven default — fast enough that a canvas user typing "are you
# there?" gets picked up before they refresh, slow enough that 12
# requests/min won't trip rate limits or wake mobile devices.
POLL_INTERVAL_SECONDS = 5.0

# Initial backlog window for the first poll AND the recovery path
# after a stale-cursor 410. 10 minutes is enough to cover a brief
# crash/restart without flooding a long-idle workspace with hours of
# stale chat.
INITIAL_BACKLOG_SECONDS = 600

# Hard cap on the in-memory deque. The poller is bounded by the
# server's per-page limit (default 100) and the agent typically pops
# faster than the operator types, so an idle workspace shouldn't
# exceed a handful. The cap protects against runaway growth if the
# agent process stops calling pop.
MAX_QUEUED_MESSAGES = 200


@dataclass
class InboxMessage:
    """One inbound A2A message staged for the agent.

    Mirrors the shape the agent sees via inbox_peek / wait_for_message.
    Fields are derived from the activity_logs row by ``_from_activity``.
    """

    activity_id: str
    text: str
    peer_id: str  # empty string = canvas user; non-empty = peer workspace_id
    method: str  # JSON-RPC method ("message/send", "tasks/send", etc.)
    created_at: str  # RFC3339 timestamp from the activity row

    def to_dict(self) -> dict[str, Any]:
        return {
            "activity_id": self.activity_id,
            "text": self.text,
            "peer_id": self.peer_id,
            "kind": "peer_agent" if self.peer_id else "canvas_user",
            "method": self.method,
            "created_at": self.created_at,
        }


@dataclass
class InboxState:
    """Thread-safe queue of pending inbound messages.

    Producer: the poller thread, calling ``record(message)``.
    Consumers: the MCP tool handlers, calling ``peek``, ``pop``,
    or ``wait``. Synchronization is via a single ``threading.Lock``
    (cheap — every operation is O(n) over a small deque) plus an
    ``Event`` that wakes ``wait`` callers when a new message lands.
    """

    cursor_path: Path
    """File path that persists ``activity_logs.id`` of the most
    recently observed row, so a restart doesn't replay backlog."""

    _queue: deque[InboxMessage] = field(default_factory=lambda: deque(maxlen=MAX_QUEUED_MESSAGES))
    _lock: threading.Lock = field(default_factory=threading.Lock)
    _arrival: threading.Event = field(default_factory=threading.Event)
    _cursor: str | None = None
    _cursor_loaded: bool = False

    def load_cursor(self) -> str | None:
        """Read the persisted cursor from disk. Cached after first call.

        Missing/unreadable file → None (poller will fall back to the
        initial-backlog window). We never raise: a corrupt cursor is
        less bad than the inbox refusing to start.
        """
        with self._lock:
            if self._cursor_loaded:
                return self._cursor
            try:
                if self.cursor_path.is_file():
                    self._cursor = self.cursor_path.read_text().strip() or None
            except OSError as exc:
                logger.warning("inbox: failed to read cursor %s: %s", self.cursor_path, exc)
                self._cursor = None
            self._cursor_loaded = True
            return self._cursor

    def save_cursor(self, activity_id: str) -> None:
        """Persist the cursor. Best-effort — log + continue on failure.

        Loss of the cursor on a write failure means an extra page of
        backlog after restart, never a stuck poller. Silent-fail
        would mask a permission misconfiguration on the operator's
        configs dir; warn loudly so they can fix it.
        """
        with self._lock:
            self._cursor = activity_id
            self._cursor_loaded = True
        try:
            self.cursor_path.parent.mkdir(parents=True, exist_ok=True)
            tmp = self.cursor_path.with_suffix(self.cursor_path.suffix + ".tmp")
            tmp.write_text(activity_id)
            tmp.replace(self.cursor_path)
        except OSError as exc:
            logger.warning("inbox: failed to persist cursor to %s: %s", self.cursor_path, exc)

    def reset_cursor(self) -> None:
        """Forget the cursor. Used after a 410 from the activity API."""
        with self._lock:
            self._cursor = None
            self._cursor_loaded = True
        try:
            if self.cursor_path.is_file():
                self.cursor_path.unlink()
        except OSError as exc:
            logger.warning("inbox: failed to delete cursor %s: %s", self.cursor_path, exc)

    def record(self, message: InboxMessage) -> None:
        """Append a message, wake any waiter, and fire the notification
        callback (if registered) for push-UX-capable hosts.

        Skips a row whose activity_id we've already queued — defensive
        against the poller racing with the consumer + cursor save. The
        dedupe short-circuits BEFORE the notification fires, so a
        notification-capable host doesn't see duplicate push events on
        backlog overlap.
        """
        with self._lock:
            for existing in self._queue:
                if existing.activity_id == message.activity_id:
                    return
            self._queue.append(message)
            self._arrival.set()
        # Fire notification AFTER releasing the lock so the callback
        # is free to do anything (including calling back into inbox)
        # without deadlock. Best-effort: a raising callback must not
        # prevent the message from landing in the queue — observability
        # is more important than push delivery.
        cb = _NOTIFICATION_CALLBACK
        if cb is not None:
            try:
                cb(message.to_dict())
            except Exception:
                logger.warning(
                    "inbox: notification callback raised", exc_info=True
                )

    def peek(self, limit: int = 10) -> list[InboxMessage]:
        """Return up to ``limit`` pending messages without removing them."""
        if limit <= 0:
            limit = 10
        with self._lock:
            return list(self._queue)[:limit]

    def pop(self, activity_id: str) -> InboxMessage | None:
        """Remove a specific message. Idempotent; returns None if absent.

        We require the caller to specify which message it handled
        rather than auto-popping the head — preserves observability
        when the agent reads several but only handles one.
        """
        with self._lock:
            for existing in list(self._queue):
                if existing.activity_id == activity_id:
                    self._queue.remove(existing)
                    if not self._queue:
                        self._arrival.clear()
                    return existing
        return None

    def wait(self, timeout_secs: float) -> InboxMessage | None:
        """Block until a message is available or timeout elapses.

        Returns the head message WITHOUT popping; the caller decides
        whether to pop after acting on it. Same shape as Python's
        Queue.get with timeout, but non-destructive so a peek-style
        agent can still inspect with peek/pop.
        """
        # Fast path: queue already has something.
        with self._lock:
            if self._queue:
                return self._queue[0]
            self._arrival.clear()

        triggered = self._arrival.wait(timeout=max(0.0, timeout_secs))
        if not triggered:
            return None
        with self._lock:
            return self._queue[0] if self._queue else None


# ---------------------------------------------------------------------------
# Module singleton — set by mcp_cli before MCP server starts.
# ---------------------------------------------------------------------------
#
# In-container callers don't activate; the inbox tools detect the
# unset singleton and return an informational error rather than
# breaking the dispatch path.

_STATE: InboxState | None = None


# Notification bridge — set by the universal MCP server (a2a_mcp_server.py)
# at startup so that new inbox arrivals can be pushed to notification-
# capable hosts (Claude Code) as MCP `notifications/claude/channel`
# events. Kept module-level (rather than a method on InboxState) so the
# inbox doesn't need to know about MCP — a thin pluggable seam.
#
# Defaults to None: in-container runtimes that don't activate the inbox
# also don't push notifications, and tests start clean. The wheel's
# wiring is exercised by tests/test_a2a_mcp_server.py + the bridge
# tests below.
_NOTIFICATION_CALLBACK: Callable[[dict], None] | None = None


def set_notification_callback(cb: Callable[[dict], None] | None) -> None:
    """Register (or clear) the per-message notification callback.

    The callback receives ``InboxMessage.to_dict()`` for each new
    arrival — same shape ``inbox_peek`` returns to the agent, so a
    bridge can build its MCP notification payload without re-deriving
    fields.

    Best-effort: a raising callback does NOT prevent the message from
    landing in the queue (see ``InboxState.record``). Pass ``None`` to
    clear (used by tests + the wheel's shutdown path).
    """
    global _NOTIFICATION_CALLBACK
    _NOTIFICATION_CALLBACK = cb


def activate(state: InboxState) -> None:
    """Register an InboxState as the singleton this module exposes.

    Idempotent within a process: re-activating with the same state is
    a no-op; activating with a DIFFERENT state replaces the singleton
    + logs at WARNING (the only legitimate caller is mcp_cli at
    startup; double-activate usually means a test/runtime mix-up).
    """
    global _STATE
    if _STATE is state:
        return
    if _STATE is not None:
        logger.warning("inbox: replacing existing singleton state")
    _STATE = state


def get_state() -> InboxState | None:
    """Return the active InboxState, or None if the runtime never activated.

    Tool implementations call this and surface a clear "(inbox not
    enabled)" message to the agent when None — keeps the in-container
    path's tool dispatch from raising on an inbox-tool call that the
    agent shouldn't have made anyway.
    """
    return _STATE


# ---------------------------------------------------------------------------
# Activity → InboxMessage adapter
# ---------------------------------------------------------------------------
#
# The platform's a2a_proxy logs request_body as the JSON-RPC envelope
# it forwarded to the workspace. Three shapes have been observed in
# the wild (verified against workspace-server's logA2ASuccess in
# a2a_proxy_helpers.go on 2026-04-29) — handle all three before
# falling back to summary so a peer message at least surfaces SOMETHING.


def _extract_text(request_body: Any, summary: str | None) -> str:
    """Pull the human-readable text out of an A2A activity row.

    Mirrors molecule-mcp-claude-channel/server.ts:445 (extractText) so
    canvas-user messages and peer-agent messages render identically
    across both inbound channels.
    """
    if not isinstance(request_body, dict):
        return summary or "(empty A2A message)"

    candidates: list[Any] = []
    params = request_body.get("params") if isinstance(request_body.get("params"), dict) else None
    if params:
        message = params.get("message") if isinstance(params.get("message"), dict) else None
        if message:
            candidates.append(message.get("parts"))
        candidates.append(params.get("parts"))
    candidates.append(request_body.get("parts"))

    # The A2A protocol's part discriminator field varies between SDK
    # versions: a2a-sdk v0 uses ``type``, v1 uses ``kind``. The platform's
    # activity_logs preserves whichever the original sender used, so we
    # accept either. Verified live against a hosted SaaS workspace on
    # 2026-04-30 — every canvas-user message arrived with ``kind`` and
    # the type-only filter was silently falling through to summary.
    for parts in candidates:
        if isinstance(parts, list):
            text = "".join(
                p.get("text", "")
                for p in parts
                if isinstance(p, dict)
                and (p.get("kind") == "text" or p.get("type") == "text")
            )
            if text:
                return text
    return summary or "(empty A2A message)"


def _is_self_notify_row(row: dict[str, Any]) -> bool:
    """Return True if ``row`` is the agent's own send_message_to_user
    POST surfacing back through the activity API.

    The shape (workspace-server handlers/activity.go, ``Notify`` writer):
        method='notify' AND no peer (source_id is None or '')

    Matched on both fields together so a future caller using
    ``method='notify'`` for a different purpose with a real peer_id
    still passes through.
    """
    if row.get("method") != "notify":
        return False
    source_id = row.get("source_id")
    return source_id is None or source_id == ""


def message_from_activity(row: dict[str, Any]) -> InboxMessage:
    """Convert one /activity row into an InboxMessage."""
    request_body = row.get("request_body")
    if isinstance(request_body, str):
        # The Go handler returns request_body as json.RawMessage; httpx
        # deserializes that to a dict already. But some legacy paths or
        # mocked servers may return it as a string — handle defensively.
        try:
            request_body = json.loads(request_body)
        except (TypeError, ValueError):
            request_body = None

    return InboxMessage(
        activity_id=str(row.get("id", "")),
        text=_extract_text(request_body, row.get("summary")),
        peer_id=row.get("source_id") or "",
        method=row.get("method") or "",
        created_at=str(row.get("created_at", "")),
    )


# ---------------------------------------------------------------------------
# Poller — daemon thread that fills the queue from the activity API
# ---------------------------------------------------------------------------


def _poll_once(
    state: InboxState,
    platform_url: str,
    workspace_id: str,
    headers: dict[str, str],
    timeout_secs: float = 10.0,
) -> int:
    """One poll iteration. Returns number of new messages enqueued.

    Idempotent and stateless apart from the InboxState passed in —
    safe to call from tests with a stub state + a real httpx mock.
    """
    import httpx

    url = f"{platform_url}/workspaces/{workspace_id}/activity"
    params: dict[str, str] = {"type": "a2a_receive"}
    cursor = state.load_cursor()
    if cursor:
        params["since_id"] = cursor
    else:
        params["since_secs"] = str(INITIAL_BACKLOG_SECONDS)

    try:
        with httpx.Client(timeout=timeout_secs) as client:
            resp = client.get(url, params=params, headers=headers)
    except Exception as exc:  # noqa: BLE001
        logger.warning("inbox poller: GET /activity failed: %s", exc)
        return 0

    if resp.status_code == 410:
        # Cursor pruned — drop back to the backlog window. The next
        # poll picks up wherever the activity API has rows now.
        logger.info(
            "inbox poller: cursor %s expired (410); resetting to since_secs=%d",
            cursor,
            INITIAL_BACKLOG_SECONDS,
        )
        state.reset_cursor()
        return 0

    if resp.status_code >= 400:
        logger.warning(
            "inbox poller: HTTP %d from /activity: %s",
            resp.status_code,
            (resp.text or "")[:200],
        )
        return 0

    try:
        rows = resp.json()
    except ValueError as exc:
        logger.warning("inbox poller: non-JSON response: %s", exc)
        return 0
    if not isinstance(rows, list):
        return 0

    # since_id mode returns ASC (oldest first). since_secs mode returns
    # DESC; reverse so we record in chronological order and the cursor
    # we save is the freshest row.
    if cursor is None:
        rows = list(reversed(rows))

    new_count = 0
    last_id: str | None = None
    for row in rows:
        if not isinstance(row, dict):
            continue
        if _is_self_notify_row(row):
            # The workspace-server's `/notify` handler writes the agent's
            # own send_message_to_user POSTs to activity_logs with
            # activity_type='a2a_receive', method='notify', and no
            # source_id, so the canvas chat-history loader can restore
            # those bubbles after a page reload (handlers/activity.go,
            # comment block at line 428). The activity API exposes that
            # filter only on type, so the same row otherwise lands in
            # this poll and gets pushed back to the agent — confirmed
            # live 2026-05-01: agent observed its own outbound as an
            # inbound `← molecule: Agent message: ...`. Filter here
            # belt-and-braces; the long-term fix is upstream renaming
            # the activity_type to `agent_outbound` (molecule-core
            # #2469). Once that lands, this filter becomes redundant
            # but stays in place because it only excludes rows we never
            # want, so removing it would just be churn.
            #
            # NB: still call save_cursor for these rows below — we
            # advance past them so the next poll doesn't keep re-seeing
            # the same self-notify on every iteration.
            last_id = str(row.get("id", "")) or last_id
            continue
        message = message_from_activity(row)
        if not message.activity_id:
            continue
        state.record(message)
        last_id = message.activity_id
        new_count += 1

    if last_id is not None:
        state.save_cursor(last_id)
    return new_count


def _poll_loop(
    state: InboxState,
    platform_url: str,
    workspace_id: str,
    interval: float = POLL_INTERVAL_SECONDS,
    stop_event: threading.Event | None = None,
) -> None:
    """Daemon-thread body: poll forever until stop_event fires.

    auth_headers() is rebuilt every iteration so a token rotation via
    env var or .auth_token file is picked up without a restart. Cheap
    (a dict + an env read).
    """
    from platform_auth import auth_headers

    while True:
        try:
            _poll_once(state, platform_url, workspace_id, auth_headers())
        except Exception as exc:  # noqa: BLE001
            logger.warning("inbox poller: iteration crashed: %s", exc)
        if stop_event is not None and stop_event.wait(interval):
            return
        if stop_event is None:
            time.sleep(interval)


def start_poller_thread(
    state: InboxState,
    platform_url: str,
    workspace_id: str,
    interval: float = POLL_INTERVAL_SECONDS,
) -> threading.Thread:
    """Spawn the poller as a daemon thread. Returns the Thread handle.

    daemon=True so the poller dies with the main process — same
    rationale as mcp_cli's heartbeat thread (no leaks, no stale
    workspace writes after the operator hits Ctrl-C).
    """
    t = threading.Thread(
        target=_poll_loop,
        args=(state, platform_url, workspace_id, interval),
        name="molecule-mcp-inbox-poller",
        daemon=True,
    )
    t.start()
    return t


def default_cursor_path() -> Path:
    """Standard cursor location: ``<resolved configs dir>/.mcp_inbox_cursor``.

    Resolved via configs_dir so the cursor lives next to .auth_token
    + .platform_inbound_secret regardless of whether the runtime is
    in-container (/configs) or external (~/.molecule-workspace).
    """
    return configs_dir.resolve() / ".mcp_inbox_cursor"
