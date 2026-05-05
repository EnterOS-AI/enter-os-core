"""Heartbeat + register thread for the standalone ``molecule-mcp`` wrapper.

Extracted from ``mcp_cli.py`` (RFC #2873 iter 3) so the heartbeat /
register concern lives in its own module. The console-script entry
``mcp_cli:main`` still drives the spawn, but the loop body, auth-failure
escalation, and inbound-secret persistence now live here so they can be
read, tested, and replaced independently of the orchestrator.

Public surface:

* ``HEARTBEAT_INTERVAL_SECONDS`` — cadence constant.
* ``build_agent_card(workspace_id)`` — payload helper.
* ``platform_register(platform_url, workspace_id, token)`` — one-shot
  POST /registry/register at startup.
* ``start_heartbeat_thread(platform_url, workspace_id, token)`` — spawn
  the daemon thread.
"""
from __future__ import annotations

import logging
import os
import sys
import threading
import time

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
HEARTBEAT_AUTH_LOUD_THRESHOLD = 3
HEARTBEAT_AUTH_RELOG_INTERVAL = 20


def build_agent_card(workspace_id: str) -> dict:
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


def platform_register(platform_url: str, workspace_id: str, token: str) -> None:
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
        "agent_card": build_agent_card(workspace_id),
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


def heartbeat_loop(
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
                log_heartbeat_auth_failure(
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
                persist_inbound_secret_from_heartbeat(resp)
        except Exception as exc:  # noqa: BLE001
            logger.warning("molecule-mcp: heartbeat failed: %s", exc)
        time.sleep(interval)


def log_heartbeat_auth_failure(count: int, workspace_id: str, status_code: int) -> None:
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
    if count < HEARTBEAT_AUTH_LOUD_THRESHOLD:
        logger.warning(
            "molecule-mcp: heartbeat HTTP %d (auth failure %d/%d) — "
            "token may be revoked. Will retry; if persistent, regenerate "
            "from canvas → Tokens.",
            status_code, count, HEARTBEAT_AUTH_LOUD_THRESHOLD,
        )
        return
    # At or past the threshold — this is the loud actionable error.
    if count == HEARTBEAT_AUTH_LOUD_THRESHOLD or (
        count - HEARTBEAT_AUTH_LOUD_THRESHOLD
    ) % HEARTBEAT_AUTH_RELOG_INTERVAL == 0:
        logger.error(
            "molecule-mcp: %d consecutive heartbeat auth failures (HTTP %d) — "
            "the token in MOLECULE_WORKSPACE_TOKEN has been REVOKED, likely "
            "because workspace %s was deleted server-side. The MCP server is "
            "still running but every platform call will fail. Regenerate the "
            "workspace + token from the canvas (Tokens tab), update your MCP "
            "config, and restart your runtime.",
            count, status_code, workspace_id,
        )


def persist_inbound_secret_from_heartbeat(resp: object) -> None:
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


def start_heartbeat_thread(
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
        target=heartbeat_loop,
        args=(platform_url, workspace_id, token),
        name="molecule-mcp-heartbeat",
        daemon=True,
    )
    t.start()
    return t
