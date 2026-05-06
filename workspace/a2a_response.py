"""Single source of truth for A2A ``/workspaces/<id>/a2a`` response shapes.

The workspace-server proxy at
``workspace-server/internal/handlers/a2a_proxy.go`` (the canonical
emitter) returns one of the following shapes for a single A2A call:

  * **JSON-RPC success** —
    ``{"jsonrpc": "2.0", "result": {...}, "id": "..."}``
    The agent's reply, passed through unchanged.

  * **JSON-RPC error** —
    ``{"jsonrpc": "2.0", "error": {"message": "...", "code": ...}, "id": "..."}``
    The agent reported a structured error.

  * **Poll-queued** (synthesized at proxy, RFC #2339 PR 2 — see
    ``a2a_proxy.go:402-406``) —
    ``{"status": "queued", "delivery_mode": "poll", "method": "..."}``
    The target is a poll-mode workspace (no public URL); the message
    was written to the platform's inbox queue. The target agent will
    fetch it via ``GET /activity?since_id=`` polling. NOT a failure —
    delivery succeeded, there's just no synchronous reply to relay.

  * **Platform error** — ``{"error": "...", "restarting": true?, "retry_after": int?}``
    HTTP-level failure synthesized by the proxy when the agent is
    unreachable, the container is restarting, or some other infrastructure
    failure happened. ``restarting=true`` flags the platform-initiated
    container-restart path.

  * **Malformed** — anything else. Surfaced explicitly so a future server
    change is loud rather than silent.

The ``parse(data)`` function classifies a pre-decoded JSON body into a
typed variant. Callers ``match`` on the variant and never re-implement
shape detection — that's the SSOT discipline.

# SSOT contract

This file is the Python half. The Go server emits these shapes today
via inline ``gin.H{...}`` literals. A future PR can introduce a Go
mirror (e.g. ``workspace-server/internal/models/a2a_response.go``)
with a typed marshaller — until then, **any change to the wire shape
must be reflected here** and gated by ``test_a2a_response.py``'s
fixture corpus. The corpus exists specifically so a one-sided edit
breaks CI.

# Why a typed model (vs. dict-key sniffing at every site)

The pre-2967 client at ``a2a_client.py:567-587`` sniffed for ``result``
or ``error`` keys inline and treated everything else as malformed —
which silently broke poll-mode peers (the queued envelope has neither
key). Inline sniffing per call site multiplies the surface area where
a new shape gets misclassified. A single typed parser with an
explicit ``Malformed`` escape hatch makes shape additions a
one-line change here + a fixture entry in the test corpus, instead of
a hunt through every parsing site in the runtime.
"""
from __future__ import annotations

import dataclasses
import logging
from typing import Any, Optional, Union

logger = logging.getLogger(__name__)


@dataclasses.dataclass(frozen=True)
class Result:
    """JSON-RPC success — agent's reply available synchronously.

    ``text`` is the convenience extraction from ``parts[0].text`` (the
    A2A multipart shape). ``parts`` is the full list, available for
    callers that need richer rendering (multiple parts, non-text parts).
    ``raw_result`` preserves the unparsed ``result`` field for any
    caller that needs it (e.g. activity-row response_body audit).
    """

    text: str
    parts: list[dict[str, Any]] = dataclasses.field(default_factory=list)
    raw_result: Optional[dict[str, Any]] = None


@dataclasses.dataclass(frozen=True)
class Error:
    """JSON-RPC error or platform-level error response.

    ``code`` is the JSON-RPC integer code when present, else None.
    ``restarting`` / ``retry_after`` are platform-restart-in-progress
    metadata: when both are set, the caller knows the container is
    being recycled and may surface a softer error to the user.
    """

    message: str
    code: Optional[int] = None
    restarting: bool = False
    retry_after: Optional[int] = None


@dataclasses.dataclass(frozen=True)
class Queued:
    """Platform poll-mode short-circuit — message accepted, peer will pick up async.

    Returned when the target workspace is registered as
    ``delivery_mode=poll`` (no public URL — typical for external
    standalone ``molecule-mcp`` runtimes). The message was written to
    the platform's inbox queue; the target agent will fetch it via
    ``GET /activity?since_id=`` polling.

    NOT a failure. Callers that expect a synchronous reply (the agent's
    response text) won't get one here — they should either:

      * Tolerate the absence of a reply (fire-and-forget semantics).
      * Fall back to the durable ``/workspaces/:id/delegate`` +
        ``/delegations`` polling path (see ``a2a_tools_delegation``'s
        ``_delegate_sync_via_polling``), which writes the same A2A
        request through the platform's executeDelegation goroutine
        and lets the caller poll for the result row.

    ``method`` echoes the request method (``message/send``, ``notify``,
    etc.) so callers can correlate.
    """

    method: str
    delivery_mode: str = "poll"


@dataclasses.dataclass(frozen=True)
class Malformed:
    """Server returned a body the parser can't classify.

    Carries the raw decoded payload for diagnostic logging. Callers
    typically render this as an error to the user (see
    ``send_a2a_message``) — but the Malformed variant is a separate
    type so logging / metrics can distinguish it from genuine
    JSON-RPC ``Error`` responses.
    """

    raw: Any  # whatever the server returned: dict / list / str / number / etc.


Variant = Union[Result, Error, Queued, Malformed]


# Field-name constants — the wire vocabulary. Single source of truth;
# the parser references these by name so a change here is a
# one-line edit instead of a hunt through string literals.
_KEY_RESULT = "result"
_KEY_ERROR = "error"
_KEY_STATUS = "status"
_KEY_DELIVERY_MODE = "delivery_mode"
_KEY_METHOD = "method"
_KEY_RESTARTING = "restarting"
_KEY_RETRY_AFTER = "retry_after"

_STATUS_QUEUED = "queued"
_DELIVERY_MODE_POLL = "poll"


def parse(data: Any) -> Variant:
    """Classify a pre-decoded ``/a2a`` JSON response into a typed variant.

    Never raises. Every branch is total: any input that doesn't match a
    known shape routes to ``Malformed`` so the caller can decide how
    to surface it.

    The order of checks matters:

      1. Non-dict input → Malformed (server contract is dict-shaped).
      2. Poll-queued envelope is checked BEFORE result/error because a
         server bug that sets both ``status=queued`` and ``result``
         should be loud, not silently treated as Result.
      3. ``result`` → Result (the JSON-RPC success path).
      4. ``error`` → Error (JSON-RPC error or platform error).
      5. Anything else → Malformed.
    """
    if not isinstance(data, dict):
        logger.warning(
            "a2a_response.parse: non-dict body — got %s",
            type(data).__name__,
        )
        return Malformed(raw=data)

    # Poll-queued envelope. Both keys must be present — the workspace
    # server sets them together; if only one is present the body is
    # ambiguous and we route to Malformed for visibility.
    if (
        data.get(_KEY_STATUS) == _STATUS_QUEUED
        and data.get(_KEY_DELIVERY_MODE) == _DELIVERY_MODE_POLL
    ):
        method_raw = data.get(_KEY_METHOD)
        method = str(method_raw) if method_raw is not None else "unknown"
        logger.info(
            "a2a_response.parse: queued for poll-mode peer (method=%s)",
            method,
        )
        return Queued(method=method)

    # JSON-RPC success.
    if _KEY_RESULT in data:
        result = data[_KEY_RESULT]
        if isinstance(result, dict):
            parts_raw = result.get("parts")
            parts = parts_raw if isinstance(parts_raw, list) else []
            text = ""
            if parts:
                first = parts[0]
                if isinstance(first, dict):
                    text_raw = first.get("text")
                    text = str(text_raw) if text_raw is not None else ""
            return Result(text=text, parts=parts, raw_result=result)
        # ``result`` present but not a dict — unusual but not an error;
        # surface as a Result with the value rendered to text.
        return Result(text=str(result), parts=[], raw_result=None)

    # JSON-RPC error or platform error.
    if _KEY_ERROR in data:
        err_raw = data[_KEY_ERROR]
        message = ""
        code: Optional[int] = None
        if isinstance(err_raw, dict):
            msg_raw = err_raw.get("message")
            if msg_raw is not None:
                message = str(msg_raw).strip()
            code_raw = err_raw.get("code")
            if isinstance(code_raw, int):
                code = code_raw
        elif isinstance(err_raw, str):
            message = err_raw.strip()
        else:
            message = str(err_raw)

        restarting = bool(data.get(_KEY_RESTARTING, False))
        retry_after_raw = data.get(_KEY_RETRY_AFTER)
        retry_after = retry_after_raw if isinstance(retry_after_raw, int) else None

        return Error(
            message=message,
            code=code,
            restarting=restarting,
            retry_after=retry_after,
        )

    logger.warning(
        "a2a_response.parse: unrecognized shape — keys=%s",
        sorted(data.keys()),
    )
    return Malformed(raw=data)
