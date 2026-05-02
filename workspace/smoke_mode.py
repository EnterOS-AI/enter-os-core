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

Universal wedge gate (task #131): timeout-as-pass alone misses init
wedges where the SDK process spins for 60s+ on a malformed argv
(claude-agent-sdk PR #25 class). After every result path, the smoke
consults `runtime_wedge.is_wedged()` — adapters opt-in by calling
`runtime_wedge.mark_wedged(reason)` from their executor's wedge catch
arm, and the smoke upgrades the provisional PASS to FAIL when the
flag is set. Non-opt-in adapters keep working as before — the check
is additive.

Activated by setting `MOLECULE_SMOKE_MODE=1` in the env. Wired into
`main.py` after `executor = await adapter.create_executor(...)` so the
full adapter setup path runs first; the smoke just adds one more
exercise step before exit.

CI usage (intended for `molecule-ci/.github/workflows/publish-template-image.yml`):
  docker run --rm \
    -e WORKSPACE_ID=fake -e MOLECULE_SMOKE_MODE=1 \
    -e MOLECULE_SMOKE_TIMEOUT_SECS=90 \
    "$IMAGE" molecule-runtime
The 90s timeout is calibrated to claude-agent-sdk's 60s
`initialize()` handshake — adapters with shorter init can lower it.
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


def _check_runtime_wedge() -> str | None:
    """Return the wedge reason if any adapter has marked the runtime
    wedged during this smoke run, or None when healthy.

    Universal turn-smoke (task #131): adapters that hit an unrecoverable
    init wedge (e.g. claude-agent-sdk's `Control request timeout:
    initialize` after a malformed CLI argv) call
    `runtime_wedge.mark_wedged(reason)`. The smoke gate consults this
    flag at the end of every result path — pre-existing PASS branches
    are upgraded to FAIL when the flag is set, so a wedge that was
    triggered inside a still-running execute() (timeout branch) or
    inside a non-import exception (PASS-on-other-error branch) gets
    surfaced instead of silently shipping a broken image to GHCR.

    Lazy import: the runtime may be installed without runtime_wedge in
    a corrupt-rolling-deploy state, in which case "no wedge info"
    reads as "assume healthy" — same fail-open posture heartbeat.py
    takes for the same reason.

    Catch is narrowed to import errors only — a signature change
    (`is_wedged` removed/renamed, `wedge_reason` returning the wrong
    type) must NOT silently degrade to "no wedge info." The runtime's
    structural snapshot test (workspace/tests/test_runtime_wedge_signature.py,
    task #169) carries the API-drift load: any rename surfaces there
    as a snapshot mismatch instead of letting the smoke gate go blind.
    """
    try:
        from runtime_wedge import is_wedged, wedge_reason
    except (ImportError, ModuleNotFoundError):
        return None
    if is_wedged():
        return wedge_reason()
    return None


async def run_executor_smoke(executor: Any) -> int:
    """Invoke executor.execute() once with stub deps. Return an exit code.

    Returns:
      0 — import tree healthy AND no adapter marked the runtime wedged.
          Either execution timed out (the expected outcome — we hit a
          network boundary like an LLM call) or completed cleanly.
      1 — broken lazy import detected, OR an adapter marked the
          runtime wedged via runtime_wedge.mark_wedged(). Re-raised
          as a clear log line so the publish gate's stderr captures
          the offending symbol or wedge reason.

    The 5-second timeout comes from `MOLECULE_SMOKE_TIMEOUT_SECS` env
    (default 5.0). Bump it via env when the failure mode under test is
    an init handshake that takes longer than 5s to give up — e.g.
    claude-agent-sdk's 60s `initialize()` timeout needs ~90s here so
    the SDK marks itself wedged before our outer wait_for fires.
    The publish workflow sets this value per-template via env.
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

    # Outcome of executor.execute() — narrowed to exit code by the
    # post-run wedge check below. Pre-wedge-check exit code: 0 for
    # PASS-shaped paths (timeout, clean return, non-import exception),
    # 1 for FAIL-shaped paths (import error). Wedge check upgrades
    # PASS → FAIL when the runtime self-reports wedged.
    try:
        await asyncio.wait_for(
            executor.execute(context, queue),
            timeout=_SMOKE_TIMEOUT_SECS,
        )
    except (asyncio.TimeoutError, asyncio.CancelledError):
        # Timeout = imports healthy, execution was proceeding and hit
        # a network boundary or long await. Provisionally PASS — but
        # also check runtime_wedge below: an adapter whose init wedge
        # fires inside the timeout window still needs to FAIL the gate.
        pre_wedge_code = 0
        pre_wedge_msg = "timed out past import-tree (imports healthy)"
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
        # is downstream of the import gate. Provisionally PASS — these
        # are caught by adapter-level tests, NOT by this gate, EXCEPT
        # when the adapter also called runtime_wedge.mark_wedged() on
        # the way out (the PR-25-class wedge — SDK init failure inside
        # execute()). The post-run wedge check below catches that.
        pre_wedge_code = 0
        pre_wedge_msg = (
            f"execute() raised {type(other_err).__name__} "
            "past import-tree (not an import error)"
        )
    else:
        pre_wedge_code = 0
        pre_wedge_msg = "execute() completed within timeout (imports + body OK)"

    wedge_reason_str = _check_runtime_wedge()
    if wedge_reason_str is not None:
        # Adapter self-reported wedge — overrides any provisional PASS.
        # This is the path that catches the PR-25-class regression
        # (claude_agent_sdk init wedge from a malformed CLI argv) that
        # otherwise looks like a benign network-call timeout to the
        # outer wait_for.
        print(
            f"[smoke-mode] FAIL: runtime self-reported wedged after execute(): "
            f"{wedge_reason_str}",
            file=sys.stderr,
        )
        return 1

    print(f"[smoke-mode] PASS: {pre_wedge_msg}")
    return pre_wedge_code
