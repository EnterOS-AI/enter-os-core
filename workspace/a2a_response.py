"""SSOT for A2A response parsing — typed variants over the wire shape.

Pre-fix every parser site reimplemented dispatch over the workspace-server's
A2A response envelopes, and each one had a slightly different blind spot.
``a2a_client.send_a2a_message`` (#2967) missed the poll-mode ``queued``
envelope; ``a2a_cli.py`` missed it too. This module centralises the
dispatch into a typed variant so every caller goes through one parser.

On-the-wire envelopes are unchanged. Pre-typed clients (e.g. external
consumers running their own JSON-RPC parser) continue to work; only
internal client parsing is centralised here.

Variant taxonomy:

    A2AResult     — JSON-RPC success: ``{"result": {"parts": [...]}}``
    A2AError      — JSON-RPC error:   ``{"error": {"code": N, "message": "..."}}``
                    OR gateway error: ``{"error": "<string>"}`` (no code)
    A2AQueued     — Poll-mode short-circuit:
                    ``{"status": "queued", "delivery_mode": "poll", "method": "..."}``
                    Delivery acknowledged, consumption pending. NOT an error.
    A2AMalformed  — Wire shape didn't match any known variant.
                    Operator-actionable (points at a server bug or wire drift).
                    The parser must NEVER raise — Malformed is the catch-all.

Design constraints:

* **Total** — ``parse_a2a_response`` never raises. Non-dict roots, missing
  keys, wrong-type fields all collapse to ``A2AMalformed``.
* **Pure** — no I/O. Safe to call from the hot path of any A2A caller.
* **Frozen** — variants are ``@dataclass(frozen=True)``; no caller can
  mutate a parsed response.

See issue #2967 for the design doc + rollout plan.
"""
from __future__ import annotations

import logging
from dataclasses import dataclass, field
from typing import Any, Union

logger = logging.getLogger(__name__)

# Snippet cap for malformed-response forensics. 200 chars is enough to
# eyeball the shape of an unexpected envelope without dragging an entire
# response body into logs / error strings (which could be enormous if the
# server somehow returns HTML on a misrouted request).
_DEFAULT_MAX_SNIPPET = 200


@dataclass(frozen=True)
class A2AResult:
    """Successful peer response carrying text content.

    ``text`` is the first text part's content; ``parts`` is the full
    parts list so callers that need image / tool-call parts can still
    reach them. Empty parts and missing text both yield ``text == ""``.
    """

    text: str
    parts: list = field(default_factory=list)


@dataclass(frozen=True)
class A2AError:
    """Peer or gateway returned a structured error.

    ``code`` is ``None`` when the server emitted a bare ``{"error":
    "<string>"}`` envelope (the gateway-error shape) — ``message``
    still carries the human-readable text in that case. JSON-RPC
    errors with both fields surface ``code`` as the integer JSON-RPC
    error code.
    """

    message: str
    code: int | None = None


@dataclass(frozen=True)
class A2AQueued:
    """Poll-mode short-circuit: server queued the message for the
    peer's next inbox poll. Delivery acknowledged, consumption pending.

    NOT an error. Callers should treat this as a normal outcome and
    surface it as ``"queued for poll-mode peer (method=...)"`` or
    similar — never wrap in ``_A2A_ERROR_PREFIX``.
    """

    method: str
    delivery_mode: str  # always "poll" today; carrier for future modes


@dataclass(frozen=True)
class A2AMalformed:
    """Wire shape didn't match any known variant.

    ``raw_snippet`` is a redacted ≤200-char snippet of the envelope
    for forensics. WARNING-level log at the parser entry point;
    operator should investigate (server bug, peer protocol drift,
    or genuinely-unknown response shape).
    """

    raw_snippet: str


A2AResponse = Union[A2AResult, A2AError, A2AQueued, A2AMalformed]


def parse_a2a_response(
    data: Any,
    *,
    max_snippet: int = _DEFAULT_MAX_SNIPPET,
    target: str | None = None,
) -> A2AResponse:
    """Total, pure parser for the workspace-server A2A response envelope.

    Args:
        data: JSON-decoded response body. Any type — non-dict roots,
            wrong-type fields, missing keys are all handled.
        max_snippet: Cap for ``A2AMalformed.raw_snippet``. Default 200 chars.
        target: Optional URL the response came from, included in log
            records for traceability. Pure: never affects the return value.

    Returns:
        Exactly one of ``A2AResult | A2AError | A2AQueued | A2AMalformed``.
        Never raises.

    Logs:
        - ``A2AResult``  → DEBUG with ``parts_len, text_len``
        - ``A2AError``   → INFO  with ``code, message_redacted``
        - ``A2AQueued``  → INFO  with ``method, delivery_mode``
        - ``A2AMalformed`` → **WARNING** with the raw snippet (operator attn)
    """
    if not isinstance(data, dict):
        snippet = str(data)[:max_snippet] if data is not None else ""
        logger.warning(
            "a2a_response: malformed root (not a dict) target=%s snippet=%r",
            target, snippet,
        )
        return A2AMalformed(raw_snippet=snippet)

    # JSON-RPC success: result key present.
    # Defensive: result may be a non-dict (server bug); parts may be
    # non-list; first part may be non-dict / missing text. None of these
    # collapse to Malformed — they degrade to "(no response)" instead, so
    # a peer that returns {result: {}} doesn't trigger a server-bug alert.
    if "result" in data:
        result = data["result"] if isinstance(data["result"], dict) else {}
        parts = result.get("parts", [])
        if not isinstance(parts, list):
            parts = []
        text = ""
        if parts and isinstance(parts[0], dict):
            raw_text = parts[0].get("text", "")
            text = raw_text if isinstance(raw_text, str) else ""
        logger.debug(
            "a2a_response: result target=%s parts_len=%d text_len=%d",
            target, len(parts), len(text),
        )
        return A2AResult(text=text, parts=parts)

    # JSON-RPC structured error OR gateway-string error.
    if "error" in data:
        err = data["error"]
        if isinstance(err, dict):
            raw_msg = err.get("message", "")
            msg = raw_msg.strip() if isinstance(raw_msg, str) else ""
            raw_code = err.get("code")
            code = raw_code if isinstance(raw_code, int) else None
            logger.info(
                "a2a_response: error target=%s code=%s message=%r",
                target, code, msg[:max_snippet],
            )
            return A2AError(message=msg, code=code)
        if isinstance(err, str):
            logger.info(
                "a2a_response: error (gateway-string) target=%s message=%r",
                target, err[:max_snippet],
            )
            return A2AError(message=err, code=None)
        # err is some other type (number / list / null) — coerce to string
        msg = str(err)[:max_snippet]
        logger.info(
            "a2a_response: error (coerced) target=%s message=%r",
            target, msg,
        )
        return A2AError(message=msg, code=None)

    # Poll-mode short-circuit envelope.
    # Both keys required — a partial envelope (only status="queued" but
    # no delivery_mode, or vice versa) is malformed, NOT silently treated
    # as queued. Pins the contract: server must emit the pair atomically.
    if data.get("status") == "queued" and data.get("delivery_mode") == "poll":
        raw_method = data.get("method")
        method = raw_method if isinstance(raw_method, str) else "message/send"
        logger.info(
            "a2a_response: queued target=%s method=%s delivery_mode=poll",
            target, method,
        )
        return A2AQueued(method=method, delivery_mode="poll")

    # Catch-all.
    snippet = str(data)[:max_snippet]
    logger.warning(
        "a2a_response: malformed (no known variant) target=%s snippet=%r",
        target, snippet,
    )
    return A2AMalformed(raw_snippet=snippet)
