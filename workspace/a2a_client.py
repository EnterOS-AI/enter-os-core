"""A2A protocol client — peer discovery, messaging, and workspace info.

Shared constants (WORKSPACE_ID, PLATFORM_URL) live here so that
a2a_tools and a2a_mcp_server can import them from a single place.
"""

import asyncio
import logging
import os
import random
import time
import uuid

import httpx

from platform_auth import auth_headers, self_source_headers

logger = logging.getLogger(__name__)

_WORKSPACE_ID_raw = os.environ.get("WORKSPACE_ID")
if not _WORKSPACE_ID_raw:
    raise RuntimeError("WORKSPACE_ID environment variable is required but not set")
WORKSPACE_ID = _WORKSPACE_ID_raw
if os.path.exists("/.dockerenv") or os.environ.get("DOCKER_VERSION"):
    PLATFORM_URL = os.environ.get("PLATFORM_URL", "http://host.docker.internal:8080")
else:
    PLATFORM_URL = os.environ.get("PLATFORM_URL", "http://localhost:8080")

# Cache workspace ID → name mappings (populated by list_peers calls)
_peer_names: dict[str, str] = {}

# Sentinel prefix for errors originating from send_a2a_message / child agents.
# Used by delegate_task to distinguish real errors from normal response text.
_A2A_ERROR_PREFIX = "[A2A_ERROR] "


async def discover_peer(target_id: str) -> dict | None:
    """Discover a peer workspace's URL via the platform registry."""
    async with httpx.AsyncClient(timeout=10.0) as client:
        try:
            resp = await client.get(
                f"{PLATFORM_URL}/registry/discover/{target_id}",
                headers={"X-Workspace-ID": WORKSPACE_ID, **auth_headers()},
            )
            if resp.status_code == 200:
                return resp.json()
            return None
        except Exception as e:
            logger.error(f"Discovery failed for {target_id}: {e}")
            return None


# httpx exception classes that indicate a transient transport-layer
# failure worth retrying — the request never produced an application
# response, so a fresh attempt has a real chance of succeeding. Any
# error not in this tuple is treated as deterministic (HTTP-status,
# JSON parse, runtime-returned JSON-RPC error, etc.) and surfaced to
# the caller on the first try.
#
# Why each one belongs here:
#   - ConnectError / ConnectTimeout: peer's listening socket wasn't
#     ready (mid-restart, not yet bound). Fast failure, fast recovery.
#   - RemoteProtocolError: peer closed the TCP connection without
#     writing a response — observed on 2026-04-27 when a peer's prior
#     in-flight Claude SDK session aborted and the new request's
#     connection was reset mid-handler.
#   - ReadError / WriteError: TCP read/write socket error mid-flight,
#     typically a network blip on the Docker bridge or a peer worker
#     crash.
#   - ReadTimeout: peer didn't write ANY response bytes within the
#     300s read budget. Distinct from "peer is slow but progressing"
#     (which httpx surfaces as a successful read with chunked bytes).
#     Retry budget caps the worst case — see _DELEGATE_TOTAL_BUDGET_S.
_TRANSIENT_HTTP_ERRORS: tuple[type[Exception], ...] = (
    httpx.ConnectError,
    httpx.ConnectTimeout,
    httpx.ReadError,
    httpx.WriteError,
    httpx.RemoteProtocolError,
    httpx.ReadTimeout,
)

# Retry budget. Up to 5 attempts (1 initial + 4 retries) with
# exponential backoff (1, 2, 4, 8 seconds), each backoff jittered ±25%
# to prevent synchronized retry storms across siblings if a peer flaps.
# _DELEGATE_TOTAL_BUDGET_S caps cumulative wall-clock so a string of
# ReadTimeouts can't make the caller wait 25 minutes — once the
# deadline elapses we stop retrying even if attempts remain. 600s = 10
# minutes is the agreed worst case the caller can tolerate before
# falling back to "peer unavailable" handling in tool_delegate_task.
_DELEGATE_MAX_ATTEMPTS = 5
_DELEGATE_BACKOFF_BASE_S = 1.0
_DELEGATE_BACKOFF_CAP_S = 16.0
_DELEGATE_TOTAL_BUDGET_S = 600.0


def _delegate_backoff_seconds(attempt_zero_indexed: int) -> float:
    """Return the (jittered) backoff delay before retrying after the
    given attempt index (0 = backoff before retry #1).

    Pure function so the schedule is unit-testable without monkey-
    patching asyncio.sleep. Jitter is symmetric ±25% on top of the
    capped exponential — enough to break sync across simultaneous
    callers without making the schedule unpredictable.
    """
    base = min(_DELEGATE_BACKOFF_BASE_S * (2 ** attempt_zero_indexed), _DELEGATE_BACKOFF_CAP_S)
    jitter = base * (0.5 * random.random() - 0.25)
    return max(0.0, base + jitter)


def _format_a2a_error(exc: BaseException, target_url: str) -> str:
    """Format an httpx exception as an [A2A_ERROR] string.

    Some httpx exceptions stringify to empty (RemoteProtocolError,
    ConnectionReset variants) — the canvas would then render
    "[A2A_ERROR] " with no detail and the operator has no signal to
    act on. Always include the exception class name and the target
    URL so the activity log + Agent Comms panel have actionable
    information without a trip through container logs.
    """
    msg = str(exc).strip()
    type_name = type(exc).__name__
    if not msg:
        detail = f"{type_name} (no message — likely connection reset or silent timeout)"
    elif msg.startswith(f"{type_name}:") or msg.startswith(f"{type_name} "):
        # Already prefixed with the type — don't double-prefix.
        # Prefix-anchored check (not substring) so a message that
        # happens to mention some OTHER class name mid-string
        # (e.g. "got OSError on read") doesn't suppress our own
        # type prefix and lose the diagnostic signal.
        detail = msg
    else:
        detail = f"{type_name}: {msg}"
    return f"{_A2A_ERROR_PREFIX}{detail} [target={target_url}]"


async def send_a2a_message(target_url: str, message: str) -> str:
    """Send an A2A message/send to a target workspace.

    Auto-retries up to _DELEGATE_MAX_ATTEMPTS times on transient
    transport-layer errors (RemoteProtocolError, ConnectError,
    ReadTimeout, etc.) with exponential-backoff + jitter, capped by
    _DELEGATE_TOTAL_BUDGET_S. Application-level failures (HTTP 4xx,
    JSON-RPC error response, malformed JSON) are NOT retried — they
    indicate a deterministic problem retry won't fix.
    """
    # Fix F (Cycle 5 / H2 — flagged 5 consecutive audits): timeout=None allowed
    # a hung upstream to block the agent indefinitely. Use a generous but bounded
    # timeout: 30s connect + 300s read (long enough for slow LLM responses).
    timeout_cfg = httpx.Timeout(connect=30.0, read=300.0, write=30.0, pool=30.0)
    deadline = time.monotonic() + _DELEGATE_TOTAL_BUDGET_S
    last_exc: BaseException | None = None

    for attempt in range(_DELEGATE_MAX_ATTEMPTS):
        async with httpx.AsyncClient(timeout=timeout_cfg) as client:
            try:
                # self_source_headers() includes X-Workspace-ID so the
                # platform's a2a_receive logger records source_id =
                # WORKSPACE_ID. Otherwise peer-A2A messages — including
                # the case where target_url resolves to this workspace's
                # own /a2a — get logged with source_id=NULL and surface
                # in the recipient's My Chat tab as user-typed input.
                resp = await client.post(
                    target_url,
                    headers=self_source_headers(WORKSPACE_ID),
                    json={
                        "jsonrpc": "2.0",
                        "id": str(uuid.uuid4()),
                        "method": "message/send",
                        "params": {
                            "message": {
                                "role": "user",
                                "messageId": str(uuid.uuid4()),
                                "parts": [{"kind": "text", "text": message}],
                            }
                        },
                    },
                )
                data = resp.json()
                if "result" in data:
                    parts = data["result"].get("parts", [])
                    text = parts[0].get("text", "") if parts else "(no response)"
                    # Tag child-reported errors so the caller can detect them reliably
                    if text.startswith("Agent error:"):
                        return f"{_A2A_ERROR_PREFIX}{text}"
                    return text
                elif "error" in data:
                    err = data["error"]
                    msg = (err.get("message") or "").strip()
                    code = err.get("code")
                    if msg and code is not None:
                        detail = f"{msg} (code={code})"
                    elif msg:
                        detail = msg
                    elif code is not None:
                        detail = f"JSON-RPC error with no message (code={code})"
                    else:
                        detail = "JSON-RPC error with no message"
                    return f"{_A2A_ERROR_PREFIX}{detail} [target={target_url}]"
                return f"{_A2A_ERROR_PREFIX}unexpected response shape (no result, no error): {str(data)[:200]} [target={target_url}]"
            except _TRANSIENT_HTTP_ERRORS as e:
                last_exc = e
                attempts_remaining = _DELEGATE_MAX_ATTEMPTS - (attempt + 1)
                if attempts_remaining <= 0 or time.monotonic() >= deadline:
                    # Out of attempts OR out of total budget — surface
                    # the last error to the caller.
                    break
                delay = _delegate_backoff_seconds(attempt)
                # Don't sleep past the deadline — clamp.
                remaining = deadline - time.monotonic()
                if delay > remaining:
                    delay = max(0.0, remaining)
                logger.warning(
                    "send_a2a_message: transient %s on attempt %d/%d, retrying in %.1fs (target=%s)",
                    type(e).__name__,
                    attempt + 1,
                    _DELEGATE_MAX_ATTEMPTS,
                    delay,
                    target_url,
                )
                await asyncio.sleep(delay)
                continue
            except Exception as e:
                # Non-transient (HTTP-status, JSON parse, etc.) — don't retry.
                return _format_a2a_error(e, target_url)
    # Retries exhausted (or budget elapsed). last_exc must be set
    # because we only break out of the loop after assigning it.
    assert last_exc is not None  # noqa: S101
    return _format_a2a_error(last_exc, target_url)


async def get_peers_with_diagnostic() -> tuple[list[dict], str | None]:
    """Get this workspace's peers, returning (peers, diagnostic).

    diagnostic is None when the call succeeded (status 200, even if the list
    is empty). When peers is [] for a non-trivial reason (auth failure,
    workspace-id missing from registry, platform error, network error),
    diagnostic is a short human-readable string explaining what went wrong
    so callers can surface it instead of "may be isolated" — see #2397.

    The legacy get_peers() shim below preserves the bare-list contract for
    non-tool callers.
    """
    url = f"{PLATFORM_URL}/registry/{WORKSPACE_ID}/peers"
    async with httpx.AsyncClient(timeout=10.0) as client:
        try:
            resp = await client.get(
                url,
                headers={"X-Workspace-ID": WORKSPACE_ID, **auth_headers()},
            )
        except Exception as e:
            return [], f"Cannot reach platform at {PLATFORM_URL}: {e}"

        if resp.status_code == 200:
            try:
                data = resp.json()
            except Exception as e:
                return [], f"Platform returned 200 but body was not JSON: {e}"
            if not isinstance(data, list):
                return [], f"Platform returned 200 but body was not a list: {type(data).__name__}"
            return data, None

        if resp.status_code in (401, 403):
            return [], (
                f"Authentication to platform failed (HTTP {resp.status_code}). "
                "The workspace bearer token may be invalid — restarting the workspace usually re-mints it."
            )
        if resp.status_code == 404:
            return [], (
                f"Workspace ID {WORKSPACE_ID} is not registered with the platform (HTTP 404). "
                "Re-registration via the platform's /registry/register endpoint is needed."
            )
        if 500 <= resp.status_code < 600:
            return [], f"Platform error: HTTP {resp.status_code}."
        return [], f"Unexpected platform response: HTTP {resp.status_code}."


async def get_peers() -> list[dict]:
    """Get this workspace's peers from the platform registry.

    Bare-list shim over get_peers_with_diagnostic() — discards the diagnostic
    so callers that don't care about the failure reason (e.g. system-prompt
    bootstrap formatters) get the same shape they always had.
    """
    peers, _ = await get_peers_with_diagnostic()
    return peers


async def get_workspace_info() -> dict:
    """Get this workspace's info from the platform."""
    async with httpx.AsyncClient(timeout=10.0) as client:
        try:
            resp = await client.get(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}",
                headers=auth_headers(),
            )
            if resp.status_code == 200:
                return resp.json()
            return {"error": "not found"}
        except Exception as e:
            return {"error": str(e)}
