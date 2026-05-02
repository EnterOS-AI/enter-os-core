"""A2A protocol client — peer discovery, messaging, and workspace info.

Shared constants (WORKSPACE_ID, PLATFORM_URL) live here so that
a2a_tools and a2a_mcp_server can import them from a single place.
"""

import asyncio
import logging
import os
import random
import re
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

# Cache workspace ID → full peer record (id, name, role, status, url, ...).
# Populated by tool_list_peers and by the lazy registry lookup in
# enrich_peer_metadata. The notification-callback path (channel envelope
# enrichment) reads this cache on every inbound peer_agent push, so a
# bare ``dict[str, tuple[float, dict | None]]`` is the fastest read
# shape; entries carry their fetched-at timestamp so TTL eviction is
# in-line with the lookup. ``None`` as the record is the negative-cache
# sentinel: registry failure is cached for one TTL window so we don't
# re-fire the 2s-bounded GET on every push from a flaky peer.
_peer_metadata: dict[str, tuple[float, dict | None]] = {}

# How long an entry in ``_peer_metadata`` is treated as fresh. 5 minutes
# is the same window we use for delegation routing — long enough that a
# busy agent receiving repeated pushes from one peer doesn't hit the
# registry on every push, short enough that role/name renames propagate
# within a single agent session.
_PEER_METADATA_TTL_SECONDS = 300.0


def enrich_peer_metadata(peer_id: str, *, now: float | None = None) -> dict | None:
    """Return cached or freshly-fetched metadata for ``peer_id``.

    Sync helper — safe to call from the inbox poller's notification
    callback thread (which is not async). Hits the in-process cache
    first; on miss or TTL expiry, GETs ``/registry/discover/<peer_id>``
    synchronously with a tight timeout. Returns None on validation
    failure, network failure, or non-200 response so callers can
    degrade gracefully (the channel envelope falls back to the raw
    ``peer_id`` instead of crashing the push path).

    Negative caching: failure outcomes (4xx/5xx/non-JSON/network
    exception) are stored as ``(now, None)`` and treated as
    fresh-but-empty for the TTL window. Without this, a peer with a
    flaky/missing registry record would re-fire the 2s-bounded GET on
    EVERY push — turning the cache into a no-op for the exact failure
    scenarios it most needs to defend against.

    The fetched dict is stored as-is, so callers can read whatever
    fields the platform exposes (currently: ``id``, ``name``, ``role``,
    ``status``, ``url``). New fields surface automatically without a
    code change here.
    """
    canon = _validate_peer_id(peer_id)
    if canon is None:
        return None

    current = now if now is not None else time.monotonic()
    cached = _peer_metadata.get(canon)
    if cached is not None:
        fetched_at, record = cached
        if current - fetched_at < _PEER_METADATA_TTL_SECONDS:
            # Fresh entry — return whatever's there. ``None`` is the
            # negative-cache sentinel: caller treats absence of fields
            # the same as a registry miss, which is the desired UX.
            return record

    url = f"{PLATFORM_URL}/registry/discover/{canon}"
    try:
        with httpx.Client(timeout=2.0) as client:
            resp = client.get(url, headers={"X-Workspace-ID": WORKSPACE_ID, **auth_headers()})
    except Exception as exc:  # noqa: BLE001
        logger.debug("enrich_peer_metadata: GET %s failed: %s", url, exc)
        _peer_metadata[canon] = (current, None)
        return None

    if resp.status_code != 200:
        logger.debug(
            "enrich_peer_metadata: %s returned HTTP %d", url, resp.status_code
        )
        _peer_metadata[canon] = (current, None)
        return None

    try:
        data = resp.json()
    except Exception:  # noqa: BLE001
        _peer_metadata[canon] = (current, None)
        return None
    if not isinstance(data, dict):
        _peer_metadata[canon] = (current, None)
        return None

    _peer_metadata[canon] = (current, data)
    if name := data.get("name"):
        _peer_names[canon] = name
    return data


def _agent_card_url_for(peer_id: str) -> str:
    """Construct the platform-side agent-card URL for ``peer_id``.

    Uses the registry's discovery path so the agent receiving a push
    can hit a single endpoint to enumerate the sender's capabilities
    + role + URL. Same shape every workspace exposes regardless of
    runtime — claude-code, hermes, langchain wrappers all register
    through ``/registry/register`` and surface through ``/registry/discover``.
    """
    return f"{PLATFORM_URL}/registry/discover/{peer_id}"

# Sentinel prefix for errors originating from send_a2a_message / child agents.
# Used by delegate_task to distinguish real errors from normal response text.
_A2A_ERROR_PREFIX = "[A2A_ERROR] "

# Workspace IDs are UUIDs everywhere we generate them (platform's
# workspaces.id column, /registry/discover/:id route param, etc.) but
# the agent-facing tool surface receives them as free-form strings via
# tool args. ``_validate_peer_id`` enforces UUID-shape at the
# trust boundary so we never interpolate `..` or `/` into a URL path,
# never silently coerce malformed input into a 404, and surface a
# clear error to the agent rather than letting an HTTP 4xx bubble up
# from the platform with a generic error message.
#
# Lenient on case + whitespace because real-world peer-id strings
# come from list_peers/discover_peer responses (canonical lowercase)
# or hand-typed agent input (mixed-case acceptable). Strict on
# everything else.
_UUID_RE = re.compile(
    r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"
)


def _validate_peer_id(peer_id: str) -> str | None:
    """Return the canonicalised peer_id if valid, else None.

    Returning None instead of raising so callers in tool surfaces can
    convert to a friendly agent-facing string ("workspace_id is not a
    valid UUID") rather than crashing with a stack trace.
    """
    if not isinstance(peer_id, str):
        return None
    pid = peer_id.strip()
    if not _UUID_RE.match(pid):
        return None
    return pid.lower()


async def discover_peer(target_id: str) -> dict | None:
    """Discover a peer workspace's URL via the platform registry.

    Validates ``target_id`` is a UUID before constructing the URL — a
    malformed id can't reach the platform handler now, which both
    short-circuits an avoidable round-trip AND ensures we never
    interpolate path-traversal characters into the URL.
    """
    safe_id = _validate_peer_id(target_id)
    if safe_id is None:
        return None
    async with httpx.AsyncClient(timeout=10.0) as client:
        try:
            resp = await client.get(
                f"{PLATFORM_URL}/registry/discover/{safe_id}",
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


async def send_a2a_message(peer_id: str, message: str) -> str:
    """Send an A2A ``message/send`` to a peer workspace via the platform proxy.

    The target URL is constructed internally as
    ``${PLATFORM_URL}/workspaces/{peer_id}/a2a``. Going through the
    platform's A2A proxy is the only path that works for both
    in-container and external runtimes — see
    a2a_tools.tool_delegate_task for the rationale.

    Auto-retries up to _DELEGATE_MAX_ATTEMPTS times on transient
    transport-layer errors (RemoteProtocolError, ConnectError,
    ReadTimeout, etc.) with exponential-backoff + jitter, capped by
    _DELEGATE_TOTAL_BUDGET_S. Application-level failures (HTTP 4xx,
    JSON-RPC error response, malformed JSON) are NOT retried — they
    indicate a deterministic problem retry won't fix.
    """
    safe_id = _validate_peer_id(peer_id)
    if safe_id is None:
        return f"{_A2A_ERROR_PREFIX}invalid peer_id (expected UUID): {peer_id!r}"
    target_url = f"{PLATFORM_URL}/workspaces/{safe_id}/a2a"

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
    """Get this workspace's info from the platform.

    Distinguishes three failure shapes so callers can handle them
    distinctly (#2429):
      - 410 Gone        → workspace was deleted; re-onboard required
      - 404 / other     → workspace never existed (or transient)
      - exception       → network / auth failure
    """
    async with httpx.AsyncClient(timeout=10.0) as client:
        try:
            resp = await client.get(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}",
                headers=auth_headers(),
            )
            if resp.status_code == 200:
                return resp.json()
            if resp.status_code == 410:
                # #2429: platform returns 410 when status='removed'.
                # Surface "removed" + the actionable hint so callers
                # can prompt re-onboard instead of falling through to
                # "not found" — which made the 2026-04-30 incident
                # impossible to diagnose ("workspace not found" with
                # a workspace_id we KNEW we'd just registered).
                try:
                    body = resp.json()
                except Exception:
                    body = {}
                return {
                    "error": "removed",
                    "id": body.get("id", WORKSPACE_ID),
                    "removed_at": body.get("removed_at"),
                    "hint": body.get(
                        "hint",
                        "Workspace was deleted on the platform. "
                        "Regenerate workspace + token from the canvas → Tokens tab.",
                    ),
                }
            return {"error": "not found"}
        except Exception as e:
            return {"error": str(e)}
