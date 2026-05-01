"""Heartbeat loop — alive signal + delegation status checker.

Every 30 seconds:
1. Send heartbeat to platform (alive signal with current_task, error_rate)
2. Check pending delegations — any results back?
3. Store completed delegation results for the agent to pick up

Resilient: recreates HTTP client on failure, auto-restarts on crash.
"""

import asyncio
import json
import logging
import os
import time
from pathlib import Path

import httpx

from platform_auth import auth_headers, refresh_cache, self_source_headers


def _runtime_state_payload() -> dict:
    """Build the {runtime_state, sample_error} portion of the heartbeat
    body when SOME adapter executor has marked itself wedged. Returns
    an empty dict when the runtime is healthy so the heartbeat payload
    doesn't grow fields the platform doesn't need.

    Source of truth is runtime_wedge (lives in molecule-runtime,
    independent of any specific adapter). Pre task #87 this imported
    from claude_sdk_executor — that worked because the executor was
    bundled into molecule-runtime, but blocked moving it to the
    claude-code template repo. The runtime_wedge module is now the
    cross-cutting wedge-state holder; adapters mark/clear via it,
    heartbeat reads it.

    Imported lazily so a workspace whose runtime image somehow ships
    without runtime_wedge (corrupt install, mid-rolling-deploy state)
    keeps heartbeating — a missing import means "no wedge info; assume
    healthy."
    """
    try:
        from runtime_wedge import is_wedged, wedge_reason
    except Exception:
        return {}
    if not is_wedged():
        return {}
    return {
        "runtime_state": "wedged",
        # sample_error doubles as the human-readable banner text on the
        # canvas's degraded card — keep it short and actionable.
        "sample_error": wedge_reason(),
    }


def _runtime_metadata_payload() -> dict:
    """Build the {runtime_metadata} portion of the heartbeat body —
    adapter-declared capabilities + per-capability override values
    (idle timeout, etc.). The platform reads this to route capabilities
    to the right owner: native (adapter) vs fallback (platform).

    Returns an empty dict if the adapter can't be loaded or introspected.
    Heartbeat must NEVER fail because of capability discovery — observability
    is more important than capability accuracy. The platform falls through
    to its own defaults when fields are missing.

    See project memory `project_runtime_native_pluggable.md` and
    workspace/adapter_base.py:RuntimeCapabilities.
    """
    try:
        from adapters import get_adapter
        # ADAPTER_MODULE wins over the runtime arg in get_adapter — pass
        # an empty string to force the env-var path.
        adapter_cls = get_adapter("")
        adapter = adapter_cls()
        caps = adapter.capabilities()
        meta: dict = {"capabilities": caps.to_dict()}
        idle = adapter.idle_timeout_override()
        # Only include the override when it's a positive integer. None /
        # zero / negative falls through to the platform's global default
        # (env A2A_IDLE_TIMEOUT_SECONDS, default 5min) — that "absent
        # field = use default" contract is what keeps the wire small.
        if isinstance(idle, int) and idle > 0:
            meta["idle_timeout_seconds"] = idle
        return {"runtime_metadata": meta}
    except Exception as e:
        # debug-level: missing ADAPTER_MODULE in dev / test envs is normal
        logger.debug("runtime_metadata: failed to read adapter caps: %s", e)
        return {}


logger = logging.getLogger(__name__)


def _persist_inbound_secret_from_heartbeat(resp) -> None:
    """Persist ``platform_inbound_secret`` from a heartbeat response, if any.

    The platform's heartbeat handler (workspace-server PR #2421) returns
    the secret on every beat — mirrors /registry/register so a workspace
    whose secret was lazy-healed on the platform side picks it up within
    one heartbeat tick instead of requiring a runtime restart.

    Without this delivery path the chat-upload code path's "secret was
    just minted, will pick up on next heartbeat" 503 message is a lie
    and the workspace stays 401-forever until the operator restarts the
    runtime. Caught 2026-04-30 on the hongmingwang tenant — the
    standalone wrapper (mcp_cli.py) got the same change in #2421 but
    the in-container heartbeat (this file) was missed in the first
    pass.

    Failure is non-fatal: if the body isn't JSON, doesn't carry the
    field, or the disk write fails, the next heartbeat retries. This
    matches the cold-start register flow in main.py:319-323.
    """
    try:
        body = resp.json()
    except Exception:
        return
    if not isinstance(body, dict):
        return
    secret = body.get("platform_inbound_secret")
    if not secret:
        return
    try:
        from platform_inbound_auth import save_inbound_secret

        save_inbound_secret(secret)
    except Exception as exc:
        logger.warning(
            "heartbeat: persist inbound secret failed: %s", exc
        )


HEARTBEAT_INTERVAL = 30  # seconds
MAX_CONSECUTIVE_FAILURES = 10
MAX_SEEN_DELEGATION_IDS = 200
SELF_MESSAGE_COOLDOWN = 60  # seconds — minimum between self-messages to prevent loops
# Shared path — adapter executors (in their template repos) read this
# same file via executor_helpers.read_delegation_results so heartbeat-
# delivered async delegation results land in the next agent turn.
DELEGATION_RESULTS_FILE = os.environ.get("DELEGATION_RESULTS_FILE", "/tmp/delegation_results.jsonl")


class HeartbeatLoop:
    def __init__(self, platform_url: str, workspace_id: str):
        self.platform_url = platform_url
        self.workspace_id = workspace_id
        self.start_time = time.time()
        self.error_count = 0
        self.request_count = 0
        self.active_tasks = 0
        self.current_task = ""
        self.sample_error = ""
        self._task = None
        self._consecutive_failures = 0
        self._seen_delegation_ids: set[str] = set()
        self._last_self_message_time = 0.0
        self._parent_name: str | None = None  # Cached after first lookup

    @property
    def error_rate(self) -> float:
        if self.request_count == 0:
            return 0.0
        return self.error_count / self.request_count

    def record_error(self, error: str):
        self.error_count += 1
        self.request_count += 1
        self.sample_error = error

    def record_success(self):
        self.request_count += 1

    def start(self):
        self._task = asyncio.create_task(self._loop())
        self._task.add_done_callback(self._on_done)

    def _on_done(self, task):
        if not task.cancelled() and task.exception():
            logger.error("Heartbeat loop died: %s — restarting", task.exception())
            self._task = asyncio.create_task(self._loop())
            self._task.add_done_callback(self._on_done)

    async def stop(self):
        if self._task:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass

    async def _loop(self):
        while True:
            client = None
            try:
                client = httpx.AsyncClient(timeout=10.0)
                while True:
                    # 1. Send heartbeat (Phase 30.1: include auth header if token known)
                    try:
                        body = {
                            "workspace_id": self.workspace_id,
                            "error_rate": self.error_rate,
                            "sample_error": self.sample_error,
                            "active_tasks": self.active_tasks,
                            "current_task": self.current_task,
                            "uptime_seconds": int(time.time() - self.start_time),
                        }
                        # Layer the runtime-wedge fields on top so a
                        # non-empty sample_error from the wedge wins
                        # over the (typically empty) heartbeat
                        # sample_error field. The platform reads
                        # runtime_state to flip status → degraded.
                        body.update(_runtime_state_payload())
                        body.update(_runtime_metadata_payload())
                        resp = await client.post(
                            f"{self.platform_url}/registry/heartbeat",
                            json=body,
                            headers=auth_headers(),
                        )
                        self.error_count = 0
                        self.request_count = 0
                        self._consecutive_failures = 0
                        # 2026-04-30: persist the platform_inbound_secret
                        # if the heartbeat response carries one. Mirrors
                        # the cold-start register flow in main.py:319-323
                        # and closes the recovery path for workspaces
                        # whose secret was lazy-healed on the platform
                        # side after register-time. Without this, the
                        # workspace stays 401-forever on chat upload
                        # until restart. See workspace-server PR #2421
                        # for the server-side delivery change.
                        _persist_inbound_secret_from_heartbeat(resp)
                    except Exception as e:
                        self._consecutive_failures += 1
                        # Issue #1877: if heartbeat 401'd, re-read the token from disk
                        # and retry once. This handles the platform's token-rotation race
                        # where WriteFilesToContainer hasn't finished writing the new
                        # token before the runtime boots and caches the old value.
                        is_401 = False
                        if isinstance(e, httpx.HTTPStatusError) and e.response.status_code == 401:
                            is_401 = True
                        if is_401:
                            logger.warning("Heartbeat 401 for %s — refreshing token cache and retrying once", self.workspace_id)
                            refresh_cache()
                            try:
                                retry_body = {
                                    "workspace_id": self.workspace_id,
                                    "error_rate": self.error_rate,
                                    "sample_error": self.sample_error,
                                    "active_tasks": self.active_tasks,
                                    "current_task": self.current_task,
                                    "uptime_seconds": int(time.time() - self.start_time),
                                }
                                retry_body.update(_runtime_state_payload())
                                retry_resp = await client.post(
                                    f"{self.platform_url}/registry/heartbeat",
                                    json=retry_body,
                                    headers=auth_headers(),
                                )
                                self._consecutive_failures = 0
                                self.request_count += 1
                                _persist_inbound_secret_from_heartbeat(retry_resp)
                            except Exception:
                                # Retry also failed — fall through to the normal
                                # failure tracking below.
                                pass
                        if self._consecutive_failures <= 3 or self._consecutive_failures % MAX_CONSECUTIVE_FAILURES == 0:
                            logger.warning("Heartbeat failed (%d consecutive): %s", self._consecutive_failures, e)
                        if self._consecutive_failures >= MAX_CONSECUTIVE_FAILURES:
                            logger.info("Heartbeat: recreating HTTP client after %d failures", self._consecutive_failures)
                            try:
                                await client.aclose()
                            except Exception:
                                pass
                            break

                    # 2. Check delegation status
                    try:
                        await self._check_delegations(client)
                    except Exception as e:
                        logger.debug("Delegation check failed: %s", e)

                    await asyncio.sleep(HEARTBEAT_INTERVAL)

            except asyncio.CancelledError:
                raise
            except Exception as e:
                logger.error("Heartbeat loop error: %s — retrying in 30s", e)
                await asyncio.sleep(HEARTBEAT_INTERVAL)
            finally:
                if client:
                    try:
                        await client.aclose()
                    except Exception:
                        pass

    async def _check_delegations(self, client: httpx.AsyncClient):
        """Check for completed delegations and store results for the agent."""
        try:
            resp = await client.get(
                f"{self.platform_url}/workspaces/{self.workspace_id}/delegations",
                headers=auth_headers(),
            )
            if resp.status_code != 200:
                return

            delegations = resp.json()
            if not isinstance(delegations, list):
                return

            new_results = []
            for d in delegations:
                did = d.get("delegation_id", "")
                status = d.get("status", "")

                if not did or did in self._seen_delegation_ids:
                    continue

                if status in ("completed", "failed"):
                    # Fix B (Cycle 5): validate source_id before accepting delegation
                    # results. Only process delegations that THIS workspace created
                    # (source_id == self.workspace_id). Attacker-crafted delegation
                    # records with a foreign source_id cannot inject instructions.
                    source_id = d.get("source_id", "")
                    if source_id != self.workspace_id:
                        logger.warning(
                            "Heartbeat: skipping delegation %s — source_id %r does not "
                            "match this workspace %r; possible injection attempt",
                            did, source_id, self.workspace_id,
                        )
                        self._seen_delegation_ids.add(did)  # mark seen so we don't warn again
                        continue

                    self._seen_delegation_ids.add(did)
                    new_results.append({
                        "delegation_id": did,
                        "target_id": d.get("target_id", ""),
                        "source_id": source_id,
                        "status": status,
                        "summary": d.get("summary", ""),
                        "response_preview": d.get("response_preview", ""),
                        "error": d.get("error", ""),
                        "timestamp": time.time(),
                    })

            # Evict old seen IDs if over limit
            if len(self._seen_delegation_ids) > MAX_SEEN_DELEGATION_IDS:
                # Keep most recent half
                self._seen_delegation_ids = set(list(self._seen_delegation_ids)[MAX_SEEN_DELEGATION_IDS // 2:])

            if new_results:
                # Append to results file for context injection on next message
                with open(DELEGATION_RESULTS_FILE, "a") as f:
                    for r in new_results:
                        f.write(json.dumps(r) + "\n")
                logger.info("Heartbeat: %d new delegation results — triggering self-message", len(new_results))

                # Build a summary message for the agent.
                # Fix B (Cycle 5): do NOT embed raw response_preview text in
                # user-role A2A messages — that is the prompt-injection vector.
                # Instead reference only the delegation ID and status; the agent
                # reads full content from DELEGATION_RESULTS_FILE which was
                # written above from trusted platform data.
                summary_lines = []
                for r in new_results:
                    line = f"- [{r['status']}] Delegation {r['delegation_id'][:8]}: {r['summary'][:80]}"
                    if r.get("error"):
                        line += f"\n  Error: {r['error'][:100]}"
                    summary_lines.append(line)

                # Look up parent workspace (cached after first call)
                if self._parent_name is None:
                    try:
                        parent_resp = await client.get(
                            f"{self.platform_url}/workspaces/{self.workspace_id}",
                            headers=auth_headers(),
                        )
                        if parent_resp.status_code == 200:
                            parent_id = parent_resp.json().get("parent_id", "")
                            if parent_id:
                                parent_info = await client.get(
                                    f"{self.platform_url}/workspaces/{parent_id}",
                                    headers=auth_headers(),
                                )
                                if parent_info.status_code == 200:
                                    self._parent_name = parent_info.json().get("name", "")
                        if self._parent_name is None:
                            self._parent_name = ""  # No parent — cache empty
                    except Exception:
                        pass  # Will retry next cycle
                parent_name = self._parent_name or ""

                report_instruction = ""
                if parent_name:
                    report_instruction = (
                        f"\n\nIMPORTANT: Report these results back to your parent '{parent_name}' "
                        f"by delegating a summary to them. Use delegate_task or delegate_task_async "
                        f"with a concise status report. Also use send_message_to_user to notify the user."
                    )
                else:
                    report_instruction = (
                        "\n\nReport results using send_message_to_user to notify the user."
                    )

                trigger_msg = (
                    "Delegation results are ready. Review them and take appropriate action:\n"
                    + "\n".join(summary_lines)
                    + report_instruction
                )

                # Send A2A self-message to wake the agent.
                # Minimum 60s between self-messages to avoid spam, but always send
                # when there are genuinely NEW results to process.
                now = time.time()
                if now - self._last_self_message_time < SELF_MESSAGE_COOLDOWN:
                    logger.debug("Heartbeat: self-message cooldown (60s), will retry next cycle")
                else:
                    self._last_self_message_time = now
                    try:
                        # self_source_headers() adds X-Workspace-ID so the
                        # platform tags this row source=agent, not canvas
                        # — see platform_auth.py for the full rationale.
                        await client.post(
                            f"{self.platform_url}/workspaces/{self.workspace_id}/a2a",
                            json={
                                "method": "message/send",
                                "params": {
                                    "message": {
                                        "role": "user",
                                        "parts": [{"type": "text", "text": trigger_msg}],
                                    },
                                },
                            },
                            headers=self_source_headers(self.workspace_id),
                            timeout=120.0,
                        )
                        logger.info("Heartbeat: self-message sent to process delegation results")
                    except Exception as e:
                        logger.warning("Heartbeat: failed to send self-message: %s", e)

                # Also push notification to user via canvas
                for r in new_results:
                    try:
                        msg = f"Delegation {r['status']}: {r['summary'][:100]}"
                        if r.get("response_preview"):
                            msg += f"\nResult: {r['response_preview'][:200]}"
                        await client.post(
                            f"{self.platform_url}/workspaces/{self.workspace_id}/notify",
                            json={"message": msg, "type": "delegation_result"},
                            headers=auth_headers(),
                        )
                    except Exception:
                        pass

        except Exception as e:
            logger.debug("Delegation check error: %s", e)
