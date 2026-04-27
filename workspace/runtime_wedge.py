"""Per-process runtime-wedge state.

Adapter executors that hit a non-recoverable wedge (e.g. claude-agent-sdk's
`Control request timeout: initialize` corrupting the client process's
internal state) call mark_wedged(reason). The heartbeat task reads
is_wedged() / wedge_reason() and forwards them in the heartbeat payload's
runtime_state field — the platform then flips workspace status to
`degraded` so the canvas surfaces a Restart hint instead of leaving the
user staring at a green dot while every chat hangs.

Module scope (not instance scope) is deliberate: the wedge is a property
of the Python process, not any particular executor. With one executor
per workspace process today this is the simplest lock-free
read+write fit. A future per-org multi-executor design could move this
to a shared registry.

This module lives in molecule-runtime (NOT in any adapter / template
repo) because:

  1. workspace/heartbeat.py reads it on every heartbeat — cross-cutting
     concern, runtime owns it.
  2. Multiple adapter executors can mark themselves wedged with their
     own reason; the runtime aggregates one flag for the platform.
  3. Decoupling from claude_sdk_executor is the prerequisite for the
     universal-runtime refactor (molecule-core task #87) — without
     this extraction, claude_sdk_executor.py couldn't move to its
     template repo because heartbeat would lose access to the wedge
     state.

Public API: mark_wedged(reason), clear_wedge(), is_wedged(),
wedge_reason(). The reset_for_test() helper is for unit tests only.

How to use from a NEW adapter (template repo)
---------------------------------------------

Hermes, Codex, LangGraph, or any future adapter that wants the same
"flip-to-degraded-on-fatal-wedge" UX should call mark_wedged + clear_wedge
from its executor. The runtime imports + heartbeat plumbing are already
in place — adapters do not change anything in molecule-runtime.

Minimum integration (~6 LOC inside the executor):

    # Import path:
    #   - In a TEMPLATE repo (the common case for new adapters), the
    #     runtime is installed via PyPI as `molecule-ai-workspace-runtime`,
    #     so the import is `from molecule_runtime.runtime_wedge import …`.
    #   - In molecule-core itself (when editing this repo's own
    #     workspace/ tree), the module is at the top level — import as
    #     `from runtime_wedge import …`.
    from molecule_runtime.runtime_wedge import mark_wedged, clear_wedge

    async def execute(self, ctx, queue):
        try:
            result = await self._run_query(ctx)
        except SomeFatalSdkError as e:
            # Pick a short, operator-actionable reason. This becomes the
            # banner text on the canvas's degraded card — keep it under
            # ~80 chars and name the recovery action when possible.
            mark_wedged(f"hermes init timeout — restart workspace ({e})")
            raise
        clear_wedge()  # observed-success → next heartbeat reports healthy
        return result

What you get for free:
  - Heartbeat payload sets runtime_state="wedged" + sample_error=<reason>
    on the next 30s tick.
  - registry.go's evaluateStatus flips the workspace to `degraded` and
    broadcasts WORKSPACE_DEGRADED so the canvas card turns yellow with
    your reason as the subtitle.
  - clear_wedge() on the next successful turn flips the workspace back
    to `online` automatically — no manual operator action.

What NOT to do:
  - Don't store wedge state in your adapter module. The platform-side
    consumer (heartbeat) imports from runtime_wedge by name; an adapter-
    local copy won't be observed.
  - Don't call mark_wedged for transient errors (rate limits, single
    failed network call). The whole point is "the SDK process is in a
    state that can only be cleared by restart" — false positives
    train operators to ignore the degraded banner.
  - Don't write your own clear logic. clear_wedge() is the only path
    the heartbeat watches; a custom flag won't propagate.

When wedge is the WRONG primitive: if the failure is per-request (the
SDK works for some inputs but not others), surface as a normal A2A
error response, not a wedge. Wedge means "every subsequent request in
this process will fail until restart."
"""
from __future__ import annotations

import logging

logger = logging.getLogger(__name__)


class _WedgeState:
    """Internal carrier for the wedge flag. Exposed only via the module-
    level helpers below; adapters never see this class.

    Wrapping the state in a class (instead of a bare module-level global)
    is forward-cover for the day a runtime hosts multiple executors per
    process — a future per-scope variant can hand out keyed instances
    without changing the public mark_wedged / clear_wedge / is_wedged /
    wedge_reason API. Today there's exactly one instance (_DEFAULT).
    """

    def __init__(self) -> None:
        # None = healthy; non-empty string = wedged with that human-
        # readable reason. Surfaced verbatim as the canvas's degraded-
        # card banner text via heartbeat.sample_error.
        self._reason: str | None = None

    def is_wedged(self) -> bool:
        return self._reason is not None

    def reason(self) -> str:
        return self._reason or ""

    def mark(self, reason: str) -> None:
        # First-write-wins: a subsequent identical-class wedge can't
        # overwrite a more specific initial reason so the operator-
        # visible banner stays stable.
        if self._reason is None:
            self._reason = reason
            logger.error(
                "runtime wedge detected: %s — workspace will report degraded until cleared",
                reason,
            )

    def clear(self) -> None:
        # No-op when not wedged (the common case — adapters call this
        # on every successful query).
        if self._reason is not None:
            logger.info(
                "runtime wedge cleared after successful operation — workspace will recover to online on next heartbeat",
            )
            self._reason = None

    def reset(self) -> None:
        # Unconditional clear — for test fixtures only. Skips the
        # info-level log line the production clear() path emits.
        self._reason = None


# Single shared instance backing the module-level helpers. Today there's
# one executor per workspace process so this fits perfectly; the class
# wrap above is the seam for any future per-scope variant.
_DEFAULT = _WedgeState()


def is_wedged() -> bool:
    """True if some adapter executor in this process has marked itself
    wedged. Sticky until the same executor calls clear_wedge() on
    observed recovery (or the process restarts)."""
    return _DEFAULT.is_wedged()


def wedge_reason() -> str:
    """Human-readable description of the wedge cause, or empty string
    when not wedged. Surfaced to the canvas via heartbeat sample_error."""
    return _DEFAULT.reason()


def mark_wedged(reason: str) -> None:
    """Flag the runtime as wedged. Only the FIRST call wins so a
    subsequent identical-class wedge can't overwrite a more specific
    initial reason — the operator-visible banner stays stable.

    Adapters call this from their executor's exception path when the
    SDK has hit a non-recoverable error class. Safe to call multiple
    times; the no-op when already wedged is intentional.
    """
    _DEFAULT.mark(reason)


def clear_wedge() -> None:
    """Auto-recovery: adapter calls this after an observed successful
    operation. The original wedge could be transient (single network
    blip during the SDK's first-message handshake), and a sticky-only
    flag would lock the workspace into degraded forever even after the
    SDK started working again. Clearing on observed success means the
    next heartbeat after a working query reports runtime_state empty
    and the platform flips status back to online.

    No-op when not wedged (the common case)."""
    _DEFAULT.clear()


def reset_for_test() -> None:
    """Test-only escape hatch. Production code clears the wedge via
    clear_wedge() on observed success; this helper is for unit tests
    that need to reset between cases without going through the full
    SDK round-trip."""
    _DEFAULT.reset()
