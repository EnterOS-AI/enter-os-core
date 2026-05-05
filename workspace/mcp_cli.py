"""Console-script entry point for the ``molecule-mcp`` universal MCP server.

Validates required environment BEFORE importing the heavy
``a2a_mcp_server`` module — that module triggers a ``RuntimeError`` at
import time when ``WORKSPACE_ID`` is unset (a2a_client.py:22), and
console-script entry-point shims surface it as an ugly traceback. This
wrapper catches the missing-env case early and prints actionable help
to stderr so an operator running ``molecule-mcp`` for the first time
gets the right pointer in the first 3 lines of output instead of a
20-line traceback.

Standalone-runtime contract: this wrapper is responsible for keeping
the workspace ALIVE on the platform side, not just exposing tools.
Concretely it:
    1. Calls ``POST /registry/register`` once at startup (idempotent —
       the upsert flips status awaiting_agent → online for an external
       workspace whose token matches).
    2. Spawns a daemon heartbeat thread that POSTs to
       ``POST /registry/heartbeat`` every 20s. Without continuous
       heartbeats the platform's healthsweep flips the workspace back
       to awaiting_agent (visible as OFFLINE in the canvas with a
       "Restart" CTA) within 60-90s.
    3. Runs the MCP stdio loop in the foreground.

Why threads + sync requests: the MCP stdio server is async. The
heartbeat work is fire-and-forget HTTP. A daemon thread is the
lowest-friction integration — no asyncio bridging, dies automatically
when the main process exits, and ``requests`` is already a transitive
dependency via ``a2a-sdk``.

In-container usage (``python -m molecule_runtime.a2a_mcp_server`` or
direct import) bypasses this wrapper — the workspace runtime has its
own heartbeat loop in ``heartbeat.py`` so we don't double-heartbeat.

Module layout (RFC #2873 iter 3 split):
    * ``mcp_heartbeat`` — register POST + heartbeat loop + auth-failure
      escalation + inbound-secret persistence.
    * ``mcp_workspace_resolver`` — env validation, single + multi-workspace
      resolution, operator-help printer, on-disk token-file read.
    * ``mcp_inbox_pollers`` — activate the inbox singleton + spawn one
      daemon poller per workspace.

This file keeps just ``main()`` plus thin re-exports of the private
symbols so existing tests' imports (``mcp_cli._build_agent_card``,
``mcp_cli._heartbeat_loop``, etc.) keep working without churn.
"""
from __future__ import annotations

import logging
import os
import sys

import configs_dir
import mcp_heartbeat
import mcp_inbox_pollers
import mcp_workspace_resolver

logger = logging.getLogger(__name__)

# Re-export public surface for back-compat with the pre-split callers
# and tests. The underscore-prefixed names mirror the names that
# existed in this module before the split — keeping them ensures
# `mcp_cli._build_agent_card`, `mcp_cli._heartbeat_loop`, etc.
# resolve identically to the new functions.
HEARTBEAT_INTERVAL_SECONDS = mcp_heartbeat.HEARTBEAT_INTERVAL_SECONDS
_HEARTBEAT_AUTH_LOUD_THRESHOLD = mcp_heartbeat.HEARTBEAT_AUTH_LOUD_THRESHOLD
_HEARTBEAT_AUTH_RELOG_INTERVAL = mcp_heartbeat.HEARTBEAT_AUTH_RELOG_INTERVAL

_build_agent_card = mcp_heartbeat.build_agent_card
_platform_register = mcp_heartbeat.platform_register
_heartbeat_loop = mcp_heartbeat.heartbeat_loop
_log_heartbeat_auth_failure = mcp_heartbeat.log_heartbeat_auth_failure
_persist_inbound_secret_from_heartbeat = mcp_heartbeat.persist_inbound_secret_from_heartbeat
_start_heartbeat_thread = mcp_heartbeat.start_heartbeat_thread

_resolve_workspaces = mcp_workspace_resolver.resolve_workspaces
_print_missing_env_help = mcp_workspace_resolver.print_missing_env_help
_read_token_file = mcp_workspace_resolver.read_token_file

_start_inbox_pollers = mcp_inbox_pollers.start_inbox_pollers


def main() -> None:
    """Entry point for the ``molecule-mcp`` console script.

    Returns nothing — calls ``sys.exit`` on validation failure or on
    normal completion of the underlying MCP server loop.

    Two registration shapes:
      * Single-workspace (legacy): ``WORKSPACE_ID`` + token env/file.
        Unchanged behavior.
      * Multi-workspace: ``MOLECULE_WORKSPACES`` JSON env var with N
        ``{"id": ..., "token": ...}`` entries. One register + heartbeat
        + inbox poller per entry; messages from any workspace land in
        the same agent inbox tagged with ``arrival_workspace_id``.
    """
    if not os.environ.get("PLATFORM_URL", "").strip():
        _print_missing_env_help(
            ["PLATFORM_URL"],
            have_token_file=(configs_dir.resolve() / ".auth_token").is_file(),
        )
        sys.exit(2)

    workspaces, errors = _resolve_workspaces()
    if errors or not workspaces:
        # Reuse the missing-env help printer for legacy WORKSPACE_ID +
        # token shape, which is what most first-run operators hit. For
        # MOLECULE_WORKSPACES errors, print directly so the JSON-shape
        # message isn't mangled into the WORKSPACE_ID-style help.
        if os.environ.get("MOLECULE_WORKSPACES", "").strip():
            print("molecule-mcp: invalid MOLECULE_WORKSPACES:", file=sys.stderr)
            for e in errors:
                print(f"  - {e}", file=sys.stderr)
        else:
            _print_missing_env_help(
                errors or ["WORKSPACE_ID", "MOLECULE_WORKSPACE_TOKEN"],
                have_token_file=(configs_dir.resolve() / ".auth_token").is_file(),
            )
        sys.exit(2)

    platform_url = os.environ["PLATFORM_URL"].strip().rstrip("/")

    # In multi-workspace mode the FIRST entry is treated as the
    # "primary" — it gets exported to a2a_client.py's module-level
    # WORKSPACE_ID (which gates a RuntimeError at import time) and is
    # used by tools that don't yet take an explicit workspace_id. PR-2
    # parameterizes those tools; for now this preserves existing
    # outbound-tool behavior unchanged for single-workspace operators
    # AND for the multi-workspace operator's first registered
    # workspace.
    primary_workspace_id, _primary_token = workspaces[0]
    os.environ["WORKSPACE_ID"] = primary_workspace_id

    # Configure logging so the operator sees register/heartbeat status
    # without needing to set up logging themselves. WARNING by default
    # keeps the steady-state quiet (only failures); MOLECULE_MCP_VERBOSE=1
    # surfaces register-success + per-tick heartbeat info for debugging.
    log_level = (
        logging.INFO
        if os.environ.get("MOLECULE_MCP_VERBOSE", "").strip()
        else logging.WARNING
    )
    logging.basicConfig(level=log_level, format="[molecule-mcp] %(message)s")

    # Populate the per-workspace token registry so heartbeat threads,
    # the inbox poller, and (later) outbound tools resolve the right
    # token for each workspace via ``platform_auth.auth_headers(wsid)``.
    # Done BEFORE register/heartbeat thread spawn so a thread that
    # races to fire its first request always sees its token.
    try:
        from platform_auth import register_workspace_token
        for wsid, tok in workspaces:
            register_workspace_token(wsid, tok)
    except ImportError:
        # Older installs that don't yet ship register_workspace_token —
        # multi-workspace resolution silently degrades to the legacy
        # single-token path; single-workspace operators see no change.
        logger.debug("platform_auth.register_workspace_token unavailable; skipping registry populate")

    # Standalone-mode register + heartbeat. Skipped via env var so an
    # in-container caller (which has its own heartbeat loop) can reuse
    # this entry point without double-heartbeating. The wheel's main
    # console-script path always runs them; the
    # MOLECULE_MCP_DISABLE_HEARTBEAT escape hatch exists for tests +
    # the rare embedded use-case.
    if not os.environ.get("MOLECULE_MCP_DISABLE_HEARTBEAT", "").strip():
        for wsid, tok in workspaces:
            _platform_register(platform_url, wsid, tok)
            _start_heartbeat_thread(platform_url, wsid, tok)

    # Inbox poller — the inbound side of the standalone path. Without
    # this thread, the universal MCP server is OUTBOUND-ONLY: an agent
    # can call delegate_task / send_message_to_user but never observe
    # canvas-user or peer-agent messages. One poller per workspace; all
    # of them write to the SAME shared inbox state so the agent's
    # inbox_peek/pop/wait tools see a merged view (each message tagged
    # with arrival_workspace_id so the agent can route the reply).
    #
    # Same disable pattern as heartbeat: in-container callers (with
    # push delivery via canvas WebSocket) skip this to avoid duplicate
    # delivery; tests use the env to keep imports cheap.
    if not os.environ.get("MOLECULE_MCP_DISABLE_INBOX", "").strip():
        _start_inbox_pollers(platform_url, [w[0] for w in workspaces])

    # Env is valid — safe to import the heavy module now. Importing
    # earlier would trigger a2a_client.py:22's module-level RuntimeError
    # before our friendly help reaches the user.
    from a2a_mcp_server import cli_main
    cli_main()


if __name__ == "__main__":  # pragma: no cover
    main()
