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

import json
import logging
import os
import sys
import threading
import time
from pathlib import Path

import configs_dir

logger = logging.getLogger(__name__)

# Heartbeat cadence. Must be tighter than healthsweep's stale window
# (currently 60-90s — see registry/healthsweep.go) by a comfortable
# margin so a single missed heartbeat doesn't flip awaiting_agent.
# 20s gives the operator's network 3 attempts within the budget; long
# enough that it doesn't spam, short enough to recover quickly after
# laptop sleep.
HEARTBEAT_INTERVAL_SECONDS = 20.0

# After this many consecutive 401/403 heartbeats, escalate from
# WARNING to ERROR with re-onboard guidance. 3 ticks at 20s = ~1 minute
# of sustained auth failure — enough to rule out a transient platform
# blip but quick enough that an operator doesn't sit puzzled for 10
# minutes wondering why their MCP tools 401. Same threshold used for
# repeat-logging at 20-tick (~7 min) intervals so a long-running
# session that missed the first ERROR still sees the message.
_HEARTBEAT_AUTH_LOUD_THRESHOLD = 3
_HEARTBEAT_AUTH_RELOG_INTERVAL = 20


def _build_agent_card(workspace_id: str) -> dict:
    """Build the ``agent_card`` payload sent to /registry/register.

    Three optional env vars override the defaults so an operator can
    surface human-readable identity + capabilities to peers and the
    canvas Skills tab without code changes:

      * ``MOLECULE_AGENT_NAME`` — display name (defaults to
        ``molecule-mcp-{id[:8]}``). Surfaced in canvas workspace cards
        and ``list_peers`` output.
      * ``MOLECULE_AGENT_DESCRIPTION`` — one-liner about the agent's
        purpose. Rendered in canvas Details + Skills tabs.
      * ``MOLECULE_AGENT_SKILLS`` — comma-separated skill names
        (e.g. ``research,code-review,memory-curation``). Each name is
        expanded to a ``{"name": ...}`` skill object — the minimum
        shape that satisfies both ``shared_runtime.summarize_peers``
        (uses ``s["name"]``) and the canvas SkillsTab.tsx schema
        (id falls back to name when omitted). Empty / whitespace
        entries are dropped.

    Defaults match the previous hardcoded behaviour exactly so this
    is a strict superset — an operator who sets none of the env vars
    sees no change.
    """
    name = (os.environ.get("MOLECULE_AGENT_NAME") or "").strip()
    if not name:
        name = f"molecule-mcp-{workspace_id[:8]}"

    description = (os.environ.get("MOLECULE_AGENT_DESCRIPTION") or "").strip()

    skills_raw = (os.environ.get("MOLECULE_AGENT_SKILLS") or "").strip()
    skills: list[dict] = []
    if skills_raw:
        for s in skills_raw.split(","):
            label = s.strip()
            if label:
                skills.append({"name": label})

    card: dict = {"name": name, "skills": skills}
    if description:
        card["description"] = description
    return card


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
        "agent_card": _build_agent_card(workspace_id),
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
    consecutive_auth_failures = 0
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
            if resp.status_code in (401, 403):
                consecutive_auth_failures += 1
                _log_heartbeat_auth_failure(
                    consecutive_auth_failures, workspace_id, resp.status_code,
                )
            elif resp.status_code >= 400:
                # Non-auth HTTP error — log, but DO NOT touch the
                # auth-failure counter (5xx blips, 429, etc. are
                # transient and unrelated to token validity).
                logger.warning(
                    "molecule-mcp: heartbeat HTTP %d: %s",
                    resp.status_code,
                    (resp.text or "")[:200],
                )
            else:
                consecutive_auth_failures = 0
                _persist_inbound_secret_from_heartbeat(resp)
        except Exception as exc:  # noqa: BLE001
            logger.warning("molecule-mcp: heartbeat failed: %s", exc)
        time.sleep(interval)


def _log_heartbeat_auth_failure(count: int, workspace_id: str, status_code: int) -> None:
    """Escalate consecutive heartbeat 401/403s from quiet WARNING to
    actionable ERROR.

    The operator's first sign of trouble shouldn't be "tools 401 with no
    explanation" — that was the failure mode that motivated this code,
    triggered by a workspace being deleted server-side and its tokens
    revoked while the runtime kept heartbeating in silence.

    Cadence:
      * count < threshold: WARNING per tick (transient — could be a
        platform blip, don't shout yet)
      * count == threshold: ERROR with re-onboard instructions
        (the first signal the operator can't miss)
      * count > threshold and (count - threshold) % relog == 0: re-log
        ERROR (so a session that started after the first ERROR still
        sees the message scrolling past in their logs)
    """
    if count < _HEARTBEAT_AUTH_LOUD_THRESHOLD:
        logger.warning(
            "molecule-mcp: heartbeat HTTP %d (auth failure %d/%d) — "
            "token may be revoked. Will retry; if persistent, regenerate "
            "from canvas → Tokens.",
            status_code, count, _HEARTBEAT_AUTH_LOUD_THRESHOLD,
        )
        return
    # At or past the threshold — this is the loud actionable error.
    if count == _HEARTBEAT_AUTH_LOUD_THRESHOLD or (
        count - _HEARTBEAT_AUTH_LOUD_THRESHOLD
    ) % _HEARTBEAT_AUTH_RELOG_INTERVAL == 0:
        logger.error(
            "molecule-mcp: %d consecutive heartbeat auth failures (HTTP %d) — "
            "the token in MOLECULE_WORKSPACE_TOKEN has been REVOKED, likely "
            "because workspace %s was deleted server-side. The MCP server is "
            "still running but every platform call will fail. Regenerate the "
            "workspace + token from the canvas (Tokens tab), update your MCP "
            "config, and restart your runtime.",
            count, status_code, workspace_id,
        )


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


def _resolve_workspaces() -> tuple[list[tuple[str, str]], list[str]]:
    """Return the list of ``(workspace_id, token)`` pairs to register.

    Resolution order:

    1. ``MOLECULE_WORKSPACES`` env var — JSON array of
       ``{"id": "...", "token": "..."}`` objects. Activates the
       multi-workspace external-agent path (one process registered into
       N workspaces). When set, ``WORKSPACE_ID`` / ``MOLECULE_WORKSPACE_TOKEN``
       are IGNORED — the JSON is the source of truth.

    2. Single-workspace fallback — ``WORKSPACE_ID`` env var + token from
       ``MOLECULE_WORKSPACE_TOKEN`` or ``${CONFIGS_DIR}/.auth_token``.
       This is the pre-existing path; back-compat exact.

    Returns ``(workspaces, errors)``:
      * ``workspaces``: list of ``(workspace_id, token)`` — non-empty
        on the happy path.
      * ``errors``: human-readable strings describing what's missing /
        malformed. ``main()`` surfaces these with the same shape as
        ``_print_missing_env_help`` so the operator's first run gives
        actionable output.

    Why JSON env (not file): ergonomic for Claude Code MCP config (one
    string in ``mcpServers.molecule.env`` instead of a sidecar file)
    and for CI / launchers. A separate config-file path can be added
    later without breaking this.
    """
    raw = os.environ.get("MOLECULE_WORKSPACES", "").strip()
    if raw:
        try:
            parsed = json.loads(raw)
        except json.JSONDecodeError as exc:
            return [], [
                f"MOLECULE_WORKSPACES is not valid JSON ({exc.msg} at pos "
                f"{exc.pos}). Expected: '[{{\"id\":\"<wsid>\",\"token\":"
                f"\"<tok>\"}},{{...}}]'"
            ]
        if not isinstance(parsed, list) or not parsed:
            return [], [
                "MOLECULE_WORKSPACES must be a non-empty JSON array of "
                "{\"id\":\"...\",\"token\":\"...\"} objects"
            ]
        out: list[tuple[str, str]] = []
        seen: set[str] = set()
        errors: list[str] = []
        for i, entry in enumerate(parsed):
            if not isinstance(entry, dict):
                errors.append(
                    f"MOLECULE_WORKSPACES[{i}] is not an object — got {type(entry).__name__}"
                )
                continue
            wsid = str(entry.get("id", "")).strip()
            tok = str(entry.get("token", "")).strip()
            if not wsid or not tok:
                errors.append(
                    f"MOLECULE_WORKSPACES[{i}] missing 'id' or 'token'"
                )
                continue
            if wsid in seen:
                errors.append(
                    f"MOLECULE_WORKSPACES[{i}] duplicate workspace id {wsid!r}"
                )
                continue
            seen.add(wsid)
            out.append((wsid, tok))
        if errors:
            return [], errors
        return out, []

    # Single-workspace back-compat path.
    wsid = os.environ.get("WORKSPACE_ID", "").strip()
    if not wsid:
        return [], ["WORKSPACE_ID (or MOLECULE_WORKSPACES) is required"]
    tok = os.environ.get("MOLECULE_WORKSPACE_TOKEN", "").strip()
    if not tok:
        tok = _read_token_file()
    if not tok:
        return [], [
            "MOLECULE_WORKSPACE_TOKEN (or CONFIGS_DIR/.auth_token) is required"
        ]
    return [(wsid, tok)], []


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


def _start_inbox_pollers(platform_url: str, workspace_ids: list[str]) -> None:
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


def _read_token_file() -> str:
    """Read the token from the resolved configs dir's ``.auth_token`` if
    present.

    Mirrors platform_auth._token_file's location resolution but without
    importing the heavy module here (that import triggers a2a_client's
    WORKSPACE_ID guard which is fine after env validation, but cheaper
    to inline a 4-line file read than pull in the whole stack just for
    the path).
    """
    path = configs_dir.resolve() / ".auth_token"
    if not path.is_file():
        return ""
    try:
        return path.read_text().strip()
    except OSError:
        return ""


if __name__ == "__main__":  # pragma: no cover
    main()
