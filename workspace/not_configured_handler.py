"""Build a JSON-RPC handler that returns ``-32603 "agent not configured"``.

Used by the workspace runtime when ``adapter.setup()`` fails (most often
because an LLM credential is missing or rotated). Lets ``/.well-known/agent-card.json``
keep serving 200 — the workspace stays REACHABLE for canvas/operator
introspection — while message-send requests get a clear, immediate
error instead of silently timing out.

Kept as its own module so the behavior is unit-testable without booting
the whole runtime (main.py is ``# pragma: no cover``).
"""
from __future__ import annotations

from typing import Awaitable, Callable

from starlette.requests import Request
from starlette.responses import JSONResponse

from secret_redactor import redact_secrets


def make_not_configured_handler(
    reason: str | None,
) -> Callable[[Request], Awaitable[JSONResponse]]:
    """Return a Starlette POST handler that always 503s with JSON-RPC -32603.

    ``reason`` is surfaced in the JSON-RPC ``error.data`` field so canvas
    can render "agent not configured: <reason>" to the user. Pass the
    stringified ``adapter.setup()`` exception. ``None`` falls back to a
    generic "adapter.setup() failed".

    Secret redaction (issue molecule-core#2760): ``reason`` is run
    through ``secret_redactor.redact_secrets`` once, when the handler
    is built. If a future adapter author writes ``raise
    RuntimeError(f"auth failed for {token}")``, the token is replaced
    with ``<redacted-secret>`` BEFORE it lands in the response —
    closes the structural leak path PR #2756 introduced. Per-request
    hot path stays unchanged (one cached string, no re-redaction).

    The handler echoes the request's JSON-RPC ``id`` when present so a
    well-behaved JSON-RPC client can correlate the error to its request.
    Malformed bodies (non-JSON, missing id) get ``id: null`` per spec.
    """

    # Redact at handler-build time, not per-request, so the hot path
    # stays a constant lookup. The fallback string can't carry secrets
    # but we still pass it through redact_secrets() so a future change
    # to the fallback can't accidentally introduce a leak.
    fallback = redact_secrets(reason or "adapter.setup() failed")

    async def _handler(request: Request) -> JSONResponse:
        try:
            body = await request.json()
        except Exception:  # noqa: BLE001
            body = {}
        return JSONResponse(
            {
                "jsonrpc": "2.0",
                "id": body.get("id") if isinstance(body, dict) else None,
                "error": {
                    "code": -32603,
                    "message": "Internal error: agent not configured",
                    "data": fallback,
                },
            },
            status_code=503,
        )

    return _handler
