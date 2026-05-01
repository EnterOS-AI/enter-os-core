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
"""
from __future__ import annotations

import logging
import os
import sys
import threading
import time
from pathlib import Path

logger = logging.getLogger(__name__)

# Heartbeat cadence. Must be tighter than healthsweep's stale window
# (currently 60-90s — see registry/healthsweep.go) by a comfortable
# margin so a single missed heartbeat doesn't flip awaiting_agent.
# 20s gives the operator's network 3 attempts within the budget; long
# enough that it doesn't spam, short enough to recover quickly after
# laptop sleep.
HEARTBEAT_INTERVAL_SECONDS = 20.0


def _platform_register(platform_url: str, workspace_id: str, token: str) -> None:
    """One-shot register at startup; fails fast on auth errors.

    Lifts the workspace from ``awaiting_agent`` to ``online`` for
    operators who never ran the curl-register snippet. Safe to call
    repeatedly: the platform's register handler is an upsert that
    just refreshes ``url``, ``agent_card``, and ``status``.

    Failure model (post-review):
        - 401 / 403  → ``sys.exit(3)`` immediately. The operator's
          token is wrong; silently looping in a broken state would
          make this hard to diagnose because the MCP tools would 401
          on every call too. Hard-fail is the kindest option.
        - Other 4xx/5xx → log a warning + continue. The heartbeat
          thread will surface persistent failures; transient platform
          blips shouldn't abort the MCP loop.
        - Network / transport errors → log + continue. Same reasoning.

    Origin header is required by the SaaS edge WAF; without it
    /registry/register currently still works (it's on the WAF
    allowlist), but the heartbeat path needs Origin and we want one
    consistent header set across both calls.
    """
    try:
        import httpx
    except ImportError:
        # httpx is a transitive dep via a2a-sdk; if missing, the MCP
        # server won't import either. Let the caller's later import
        # surface the real error.
        return

    payload = {
        "id": workspace_id,
        "url": "",
        "agent_card": {"name": f"molecule-mcp-{workspace_id[:8]}", "skills": []},
        "delivery_mode": "poll",
    }
    headers = {
        "Authorization": f"Bearer {token}",
        "Origin": platform_url,
        "Content-Type": "application/json",
    }
    try:
        with httpx.Client(timeout=10.0) as client:
            resp = client.post(
                f"{platform_url}/registry/register",
                json=payload,
                headers=headers,
            )
        if resp.status_code in (401, 403):
            print(
                f"molecule-mcp: register rejected with HTTP {resp.status_code} — "
                f"the token in MOLECULE_WORKSPACE_TOKEN is invalid for workspace "
                f"{workspace_id}. Regenerate from the canvas → Tokens tab.",
                file=sys.stderr,
            )
            sys.exit(3)
        if resp.status_code >= 400:
            logger.warning(
                "molecule-mcp: register POST returned HTTP %d: %s",
                resp.status_code,
                (resp.text or "")[:200],
            )
        else:
            logger.info(
                "molecule-mcp: registered workspace %s with platform",
                workspace_id,
            )
    except SystemExit:
        raise
    except Exception as exc:  # noqa: BLE001
        logger.warning("molecule-mcp: register POST failed: %s", exc)


def _heartbeat_loop(
    platform_url: str,
    workspace_id: str,
    token: str,
    interval: float = HEARTBEAT_INTERVAL_SECONDS,
) -> None:
    """Daemon thread body: POST /registry/heartbeat every ``interval``s.

    Failures are logged at WARNING and the loop continues. The thread
    exits when the main process does (daemon=True). Each iteration
    rebuilds the payload + headers — cheap and ensures token rotation
    via env var (rare but possible) is picked up on the next tick.
    """
    try:
        import httpx
    except ImportError:
        return

    start_time = time.time()
    while True:
        body = {
            "workspace_id": workspace_id,
            "error_rate": 0.0,
            "sample_error": "",
            "active_tasks": 0,
            "uptime_seconds": int(time.time() - start_time),
        }
        headers = {
            "Authorization": f"Bearer {token}",
            "Origin": platform_url,
            "Content-Type": "application/json",
        }
        try:
            with httpx.Client(timeout=10.0) as client:
                resp = client.post(
                    f"{platform_url}/registry/heartbeat",
                    json=body,
                    headers=headers,
                )
            if resp.status_code >= 400:
                logger.warning(
                    "molecule-mcp: heartbeat HTTP %d: %s",
                    resp.status_code,
                    (resp.text or "")[:200],
                )
            else:
                _persist_inbound_secret_from_heartbeat(resp)
        except Exception as exc:  # noqa: BLE001
            logger.warning("molecule-mcp: heartbeat failed: %s", exc)
        time.sleep(interval)


def _persist_inbound_secret_from_heartbeat(resp: object) -> None:
    """Persist ``platform_inbound_secret`` from a heartbeat response, if any.

    The platform's heartbeat handler returns the secret on every beat
    (mirroring /registry/register) so a workspace that lazy-healed the
    secret on the platform side — typical recovery path for a workspace
    whose row had a NULL ``platform_inbound_secret`` after a partial
    bootstrap — picks it up within one heartbeat tick instead of
    requiring a runtime restart.

    Without this delivery path the chat-upload code path's "secret was
    just minted, will pick up on next heartbeat" 503 message is a lie
    and the workspace stays 401-forever until the operator restarts
    the runtime. Caught 2026-04-30 on hongmingwang tenant.

    Failure is non-fatal: if the body isn't JSON, doesn't carry the
    field, or the disk write fails, the next heartbeat retries. This
    matches the cold-start register flow in main.py:319-323.
    """
    try:
        body = resp.json()
    except Exception:  # noqa: BLE001
        return
    if not isinstance(body, dict):
        return
    secret = body.get("platform_inbound_secret")
    if not secret:
        return
    try:
        from platform_inbound_auth import save_inbound_secret

        save_inbound_secret(secret)
    except Exception as exc:  # noqa: BLE001
        logger.warning(
            "molecule-mcp: persist inbound secret from heartbeat failed: %s", exc
        )


def _start_heartbeat_thread(
    platform_url: str,
    workspace_id: str,
    token: str,
) -> threading.Thread:
    """Start the heartbeat daemon thread. Returns the Thread handle.

    The MCP stdio loop runs in the foreground (asyncio); this thread
    runs alongside it. ``daemon=True`` so when the operator hits
    Ctrl-C / closes the runtime, the heartbeat dies with it instead
    of leaking and writing to a stale workspace.
    """
    t = threading.Thread(
        target=_heartbeat_loop,
        args=(platform_url, workspace_id, token),
        name="molecule-mcp-heartbeat",
        daemon=True,
    )
    t.start()
    return t


def _print_missing_env_help(missing: list[str], have_token_file: bool) -> None:
    print("molecule-mcp: missing required environment.\n", file=sys.stderr)
    print("Set the following before running molecule-mcp:", file=sys.stderr)
    print("  WORKSPACE_ID                — your workspace UUID (from canvas)", file=sys.stderr)
    print(
        "  PLATFORM_URL                — base URL of your Molecule platform "
        "(e.g. https://your-tenant.staging.moleculesai.app)",
        file=sys.stderr,
    )
    if not have_token_file:
        print(
            "  MOLECULE_WORKSPACE_TOKEN    — bearer token for this workspace "
            "(canvas → Tokens tab)",
            file=sys.stderr,
        )
    print("", file=sys.stderr)
    print(f"Currently missing: {', '.join(missing)}", file=sys.stderr)


def main() -> None:
    """Entry point for the ``molecule-mcp`` console script.

    Returns nothing — calls ``sys.exit`` on validation failure or on
    normal completion of the underlying MCP server loop.
    """
    missing: list[str] = []
    if not os.environ.get("WORKSPACE_ID", "").strip():
        missing.append("WORKSPACE_ID")
    if not os.environ.get("PLATFORM_URL", "").strip():
        missing.append("PLATFORM_URL")
    # Token can come from env OR file — only flag when both are absent.
    # Mirrors platform_auth.get_token's resolution order (file-first,
    # env-fallback).
    configs_dir = Path(os.environ.get("CONFIGS_DIR", "/configs"))
    has_token_file = (configs_dir / ".auth_token").is_file()
    has_token_env = bool(os.environ.get("MOLECULE_WORKSPACE_TOKEN", "").strip())
    if not has_token_file and not has_token_env:
        missing.append("MOLECULE_WORKSPACE_TOKEN (or CONFIGS_DIR/.auth_token)")

    if missing:
        _print_missing_env_help(missing, have_token_file=has_token_file)
        sys.exit(2)

    # Resolve the effective token: env wins (operator override), then
    # the on-disk file (in-container default). Mirrors
    # platform_auth.get_token's resolution order so we don't
    # double-implement.
    token = (
        os.environ.get("MOLECULE_WORKSPACE_TOKEN", "").strip()
        or _read_token_file()
    )
    workspace_id = os.environ["WORKSPACE_ID"].strip()
    platform_url = os.environ["PLATFORM_URL"].strip().rstrip("/")

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

    # Standalone-mode register + heartbeat. Skipped via env var so an
    # in-container caller (which has its own heartbeat loop) can reuse
    # this entry point without double-heartbeating. The wheel's main
    # console-script path always runs them; the
    # MOLECULE_MCP_DISABLE_HEARTBEAT escape hatch exists for tests +
    # the rare embedded use-case.
    if not os.environ.get("MOLECULE_MCP_DISABLE_HEARTBEAT", "").strip():
        _platform_register(platform_url, workspace_id, token)
        _start_heartbeat_thread(platform_url, workspace_id, token)

    # Inbox poller — the inbound side of the standalone path. Without
    # this thread, the universal MCP server is OUTBOUND-ONLY: an agent
    # can call delegate_task / send_message_to_user but never observe
    # canvas-user or peer-agent messages. The poller fills an in-memory
    # queue from the platform's /activity?type=a2a_receive endpoint;
    # the agent reads via wait_for_message / inbox_peek / inbox_pop.
    #
    # Same disable pattern as heartbeat: in-container callers (with
    # push delivery via canvas WebSocket) skip this to avoid duplicate
    # delivery; tests use the env to keep imports cheap.
    if not os.environ.get("MOLECULE_MCP_DISABLE_INBOX", "").strip():
        _start_inbox_poller(platform_url, workspace_id)

    # Env is valid — safe to import the heavy module now. Importing
    # earlier would trigger a2a_client.py:22's module-level RuntimeError
    # before our friendly help reaches the user.
    from a2a_mcp_server import cli_main
    cli_main()


def _start_inbox_poller(platform_url: str, workspace_id: str) -> None:
    """Activate the inbox singleton + spawn the poller daemon thread.

    Done lazily here (not at module import) because importing inbox
    pulls in platform_auth, which only resolves cleanly AFTER env
    validation succeeds. Activation is idempotent within a process,
    so a stray double-call (e.g. test harness re-entering main) is
    harmless.

    The poller thread is daemon=True — dies with the main process.
    """
    try:
        import inbox
    except ImportError as exc:
        logger.warning("molecule-mcp: inbox module unavailable: %s", exc)
        return

    state = inbox.InboxState(cursor_path=inbox.default_cursor_path())
    inbox.activate(state)
    inbox.start_poller_thread(state, platform_url, workspace_id)


def _read_token_file() -> str:
    """Read the token from ${CONFIGS_DIR}/.auth_token if present.

    Mirrors platform_auth._token_file but without importing the heavy
    module here (that import triggers a2a_client's WORKSPACE_ID guard
    which is fine after env validation, but cheaper to inline a 4-line
    file read than pull in the whole stack just for the path).
    """
    configs_dir = Path(os.environ.get("CONFIGS_DIR", "/configs"))
    path = configs_dir / ".auth_token"
    if not path.is_file():
        return ""
    try:
        return path.read_text().strip()
    except OSError:
        return ""


if __name__ == "__main__":  # pragma: no cover
    main()
