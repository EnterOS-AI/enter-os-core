"""Boot smoke mode — exercises the executor's full import tree without touching real platforms.

Why this exists (issue #2275): the existing `wheel_smoke.py` only IMPORTS
`molecule_runtime.main` at module scope. Lazy imports buried inside
`async def execute(...)` bodies (e.g. `from a2a.types import FilePart`)
NEVER evaluate at static-import time — they crash at first message
delivery in production.

The 2026-04-2x v0→v1 a2a-sdk migration shipped 5 such regressions in
templates that all looked fine at module-load smoke. This module fills
the gap by actually invoking `executor.execute(stub_ctx, stub_queue)`
once with a short timeout. If the import-tree is healthy the call
proceeds far enough to hit a network boundary (LLM call, etc.) and
times out — that's a *pass*. If a lazy import is broken, the call
raises `ImportError` / `ModuleNotFoundError` from inside the executor
body — that's a *fail*.

Activated by setting `MOLECULE_SMOKE_MODE=1` in the env. Wired into
`main.py` after `executor = await adapter.create_executor(...)` so the
full adapter setup path runs first; the smoke just adds one more
exercise step before exit.

CI usage (intended for `molecule-ci/.github/workflows/publish-template-image.yml`):
  docker run --rm \
    -e WORKSPACE_ID=fake -e MOLECULE_SMOKE_MODE=1 \
    "$IMAGE" molecule-runtime
"""
from __future__ import annotations

import asyncio
import logging
import os
import sys
from typing import Any

logger = logging.getLogger(__name__)


# Don't crash production boot if MOLECULE_SMOKE_TIMEOUT_SECS is malformed —
# main.py imports smoke_mode unconditionally (before the is_smoke_mode()
# check), so a typo'd value would otherwise SystemExit every workspace.
try:
    _SMOKE_TIMEOUT_SECS = float(os.environ.get("MOLECULE_SMOKE_TIMEOUT_SECS", "5.0"))
except ValueError:
    _SMOKE_TIMEOUT_SECS = 5.0


def is_smoke_mode() -> bool:
    """True iff MOLECULE_SMOKE_MODE is set to a truthy value.

    Recognises the standard truthy strings (`1`, `true`, `yes`,
    case-insensitive). An unset / empty / `0` env reads as False so
    the boot path takes the normal branch in production.
    """
    raw = os.environ.get("MOLECULE_SMOKE_MODE", "").strip().lower()
    return raw in ("1", "true", "yes", "on")


def _build_stub_context() -> tuple[Any, Any]:
    """Build a (RequestContext, EventQueue) pair stuffed with a minimal
    text message ("smoke test"). The Message is enough that
    `extract_message_text(context)` returns non-empty input, so the
    executor takes the "real" branch (not the empty-input early-exit)
    and exercises any lazy imports along that path.

    Imports happen at function scope so smoke_mode.py itself doesn't
    pull a2a-sdk into every consumer of the runtime — the wheel still
    boots without smoke mode active.
    """
    from a2a.helpers import new_text_message
    from a2a.server.agent_execution import RequestContext
    from a2a.server.context import ServerCallContext
    from a2a.server.events import EventQueue
    from a2a.types import SendMessageRequest

    message = new_text_message("smoke test")
    call_ctx = ServerCallContext()
    request = SendMessageRequest(message=message)
    context = RequestContext(call_ctx, request=request)
    queue = EventQueue()
    return context, queue


async def run_executor_smoke(executor: Any) -> int:
    """Invoke executor.execute() once with stub deps. Return an exit code.

    Returns:
      0 — import tree healthy. Either execution timed out (the
          expected outcome — we hit a network boundary like an LLM
          call) or completed cleanly. Either way, no broken imports.
      1 — broken lazy import detected. Re-raised as a clear log line
          so the publish gate's stderr captures the offending symbol.

    The 5-second timeout comes from `MOLECULE_SMOKE_TIMEOUT_SECS` env
    (default 5.0). Bump it via env if a slow adapter setup overlaps the
    first execute call. Don't make it too long — the publish workflow
    multiplies this across N templates.
    """
    print(
        f"[smoke-mode] invoking executor.execute(stub_ctx, stub_queue) "
        f"with {_SMOKE_TIMEOUT_SECS:.1f}s timeout to exercise lazy imports"
    )

    try:
        context, queue = _build_stub_context()
    except Exception as build_err:  # noqa: BLE001
        # If we can't even build the stub, the a2a-sdk import path is
        # broken — that's exactly the regression class this gate exists
        # for. Treat as a smoke failure.
        print(
            f"[smoke-mode] FAIL: stub-context build raised "
            f"{type(build_err).__name__}: {build_err}",
            file=sys.stderr,
        )
        return 1

    try:
        await asyncio.wait_for(
            executor.execute(context, queue),
            timeout=_SMOKE_TIMEOUT_SECS,
        )
    except (asyncio.TimeoutError, asyncio.CancelledError):
        # Timeout = imports healthy, execution was proceeding and hit
        # a network boundary or long await. Pass.
        print("[smoke-mode] PASS: timed out past import-tree (imports healthy)")
        return 0
    except (ImportError, ModuleNotFoundError) as imp_err:
        # The exact regression class issue #2275 exists to catch.
        print(
            f"[smoke-mode] FAIL: lazy import broken in execute(): "
            f"{type(imp_err).__name__}: {imp_err}",
            file=sys.stderr,
        )
        return 1
    except Exception as other_err:  # noqa: BLE001
        # Anything else (auth errors, validation errors, runtime bugs)
        # is downstream of the import gate. Pass — these are caught by
        # the relevant adapter-level tests, not by this smoke.
        print(
            f"[smoke-mode] PASS: execute() raised "
            f"{type(other_err).__name__} past import-tree (not an import error)"
        )
        return 0
    else:
        print("[smoke-mode] PASS: execute() completed within timeout (imports + body OK)")
        return 0
