"""Inbox-poller spawn helpers for the standalone ``molecule-mcp`` wrapper.

Extracted from ``mcp_cli.py`` (RFC #2873 iter 3). The poller is the
INBOUND side of the standalone path — without it, the universal MCP
server is outbound-only (can call ``delegate_task`` /
``send_message_to_user``, never observes canvas-user / peer-agent
messages).

Public surface:

* ``start_inbox_pollers(platform_url, workspace_ids)`` — activate the
  inbox singleton and spawn one daemon poller per workspace.
"""
from __future__ import annotations

import logging

logger = logging.getLogger(__name__)


def start_inbox_pollers(platform_url: str, workspace_ids: list[str]) -> None:
    """Activate the inbox singleton + spawn one poller daemon thread per workspace.

    Done lazily here (not at module import) because importing inbox
    pulls in platform_auth, which only resolves cleanly AFTER env
    validation succeeds. Activation is idempotent within a process,
    so a stray double-call (e.g. test harness re-entering main) is
    harmless.

    The poller threads are daemon=True — die with the main process.

    Single-workspace path: one poller, single cursor file at the legacy
    location (``.mcp_inbox_cursor``). Cursor-key resolution falls back
    to the empty string for back-compat with operators whose existing
    on-disk cursor was written by the pre-multi-workspace code.

    Multi-workspace path: N pollers, each with its own cursor file
    keyed by ``workspace_id[:8]``. Cursors live next to each other in
    configs_dir so an operator inspecting state sees all of them
    together.
    """
    try:
        import inbox
    except ImportError as exc:
        logger.warning("molecule-mcp: inbox module unavailable: %s", exc)
        return

    if len(workspace_ids) <= 1:
        # Back-compat exact: single-workspace mode reuses the legacy
        # cursor filename + cursor_path constructor arg, so an existing
        # operator's on-disk state isn't invalidated by upgrade.
        wsid = workspace_ids[0]
        state = inbox.InboxState(cursor_path=inbox.default_cursor_path())
        inbox.activate(state)
        inbox.start_poller_thread(state, platform_url, wsid)
        return

    # Multi-workspace: per-workspace cursor file, one shared queue.
    cursor_paths = {wsid: inbox.default_cursor_path(wsid) for wsid in workspace_ids}
    state = inbox.InboxState(cursor_paths=cursor_paths)
    inbox.activate(state)
    for wsid in workspace_ids:
        inbox.start_poller_thread(state, platform_url, wsid)
