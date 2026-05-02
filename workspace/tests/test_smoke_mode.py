"""Tests for smoke_mode — the executor-stub boot smoke (issue #2275).

These tests exercise the helper module directly. The end-to-end path
(main.py invoking run_executor_smoke + sys.exit) is not unit-tested
here because main() is `# pragma: no cover` and integration-shaped;
that path is covered by the publish-template-image.yml smoke step
(which is the production gate this helper exists for).

Note on a2a-sdk: conftest.py stubs out a2a.* modules with minimal
shims that don't include `a2a.server.context.ServerCallContext` or
`a2a.types.SendMessageRequest` (the real-SDK-only symbols
_build_stub_context needs). Tests that want to verify the
`run_executor_smoke` control flow patch _build_stub_context to
sidestep the real construction; tests that NEED the real SDK
construction skip when those symbols aren't reachable.
"""
from __future__ import annotations

import asyncio
import sys
from unittest.mock import patch

import pytest

import smoke_mode


def _real_a2a_sdk_available() -> bool:
    """True when the real a2a-sdk types needed by _build_stub_context
    are importable. The conftest's a2a stubs intentionally don't
    include these — they're only present in the published wheel's
    runtime env or when a2a-sdk is installed alongside the test."""
    try:
        from a2a.server.context import ServerCallContext  # noqa: F401
        from a2a.types import SendMessageRequest  # noqa: F401
        return True
    except ImportError:
        return False


# ─── is_smoke_mode ─────────────────────────────────────────────────────


@pytest.mark.parametrize("env_value", ["1", "true", "yes", "on", "TRUE", "Yes", "ON"])
def test_is_smoke_mode_truthy_values(env_value: str, monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setenv("MOLECULE_SMOKE_MODE", env_value)
    assert smoke_mode.is_smoke_mode() is True


@pytest.mark.parametrize("env_value", ["0", "false", "no", "off", "", "  "])
def test_is_smoke_mode_falsy_values(env_value: str, monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setenv("MOLECULE_SMOKE_MODE", env_value)
    assert smoke_mode.is_smoke_mode() is False


def test_is_smoke_mode_unset(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.delenv("MOLECULE_SMOKE_MODE", raising=False)
    assert smoke_mode.is_smoke_mode() is False


# ─── _SMOKE_TIMEOUT_SECS bad-env-var resilience ────────────────────────


def test_smoke_timeout_falls_back_when_env_value_is_malformed(
    monkeypatch: pytest.MonkeyPatch,
):
    """A typo'd MOLECULE_SMOKE_TIMEOUT_SECS must not crash production
    boot. main.py imports smoke_mode unconditionally — before the
    is_smoke_mode() check — so float()-at-module-load would SystemExit
    every workspace if the env value were bad."""
    import importlib
    monkeypatch.setenv("MOLECULE_SMOKE_TIMEOUT_SECS", "not-a-float")
    reloaded = importlib.reload(smoke_mode)
    try:
        assert reloaded._SMOKE_TIMEOUT_SECS == 5.0
    finally:
        # Restore module to clean default for other tests.
        monkeypatch.delenv("MOLECULE_SMOKE_TIMEOUT_SECS", raising=False)
        importlib.reload(smoke_mode)


# ─── _build_stub_context (real-SDK-only) ───────────────────────────────


@pytest.mark.skipif(
    not _real_a2a_sdk_available(),
    reason="conftest stubs a2a.* without ServerCallContext / SendMessageRequest; real SDK only",
)
def test_build_stub_context_returns_request_context_with_message():
    """Stub must produce a RequestContext that has a non-empty message
    payload — otherwise extract_message_text returns empty and the
    executor takes the early-exit branch instead of exercising the
    full import tree."""
    context, _queue = smoke_mode._build_stub_context()
    assert context.message is not None
    parts = context.message.parts
    assert len(parts) == 1
    assert parts[0].text == "smoke test"


@pytest.mark.skipif(
    not _real_a2a_sdk_available(),
    reason="conftest stubs a2a.* without ServerCallContext / SendMessageRequest; real SDK only",
)
def test_build_stub_context_returns_event_queue():
    from a2a.server.events import EventQueue
    _, queue = smoke_mode._build_stub_context()
    assert isinstance(queue, EventQueue)


# ─── run_executor_smoke — control flow with stubbed context ────────────
#
# These tests patch _build_stub_context to return sentinel objects, so
# they don't depend on the real a2a-sdk being present. The executor
# stubs ignore ctx + queue.


class _RaisingExecutor:
    def __init__(self, exc: Exception):
        self._exc = exc

    async def execute(self, context, event_queue) -> None:  # noqa: ARG002
        raise self._exc


class _BlockingExecutor:
    """Simulates an LLM network call that the smoke timeout cuts short."""

    async def execute(self, context, event_queue) -> None:  # noqa: ARG002
        await asyncio.Event().wait()


class _CleanExecutor:
    async def execute(self, context, event_queue) -> None:  # noqa: ARG002
        return None


@pytest.fixture
def stub_build():
    """Replace _build_stub_context with a no-op so execute() gets
    sentinel ctx/queue. Tests can override this fixture's behavior
    via monkeypatch when they need a different shape."""
    sentinel_ctx = object()
    sentinel_queue = object()
    with patch.object(
        smoke_mode, "_build_stub_context",
        lambda: (sentinel_ctx, sentinel_queue),
    ):
        yield


@pytest.mark.asyncio
async def test_smoke_passes_on_timeout(stub_build, monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(smoke_mode, "_SMOKE_TIMEOUT_SECS", 0.1)
    code = await smoke_mode.run_executor_smoke(_BlockingExecutor())
    assert code == 0


@pytest.mark.asyncio
async def test_smoke_passes_on_clean_return(stub_build):
    code = await smoke_mode.run_executor_smoke(_CleanExecutor())
    assert code == 0


@pytest.mark.asyncio
async def test_smoke_fails_on_import_error(stub_build):
    """The exact regression class issue #2275 exists to catch — a lazy
    import inside execute() that the static smoke missed."""
    code = await smoke_mode.run_executor_smoke(
        _RaisingExecutor(ImportError("cannot import name 'FilePart' from 'a2a.types'"))
    )
    assert code == 1


@pytest.mark.asyncio
async def test_smoke_fails_on_module_not_found_error(stub_build):
    code = await smoke_mode.run_executor_smoke(
        _RaisingExecutor(ModuleNotFoundError("No module named 'temporalio'"))
    )
    assert code == 1


@pytest.mark.asyncio
async def test_smoke_passes_on_non_import_runtime_error(stub_build):
    """Auth errors, validation errors, anything-not-an-import-error
    pass — those are caught by adapter-level tests, not by this gate."""
    code = await smoke_mode.run_executor_smoke(
        _RaisingExecutor(RuntimeError("ANTHROPIC_API_KEY missing"))
    )
    assert code == 0


@pytest.mark.asyncio
async def test_smoke_passes_on_value_error(stub_build):
    code = await smoke_mode.run_executor_smoke(
        _RaisingExecutor(ValueError("bad config"))
    )
    assert code == 0


@pytest.mark.asyncio
async def test_smoke_fails_when_stub_context_build_breaks(monkeypatch: pytest.MonkeyPatch):
    """If a2a-sdk's own SendMessageRequest / RequestContext can't be
    constructed (e.g. SDK migration broke the constructor), that's
    exactly the regression class this gate exists for — fail loud."""

    def _fail_build():
        raise ImportError("simulated: a2a.types refactored mid-publish")

    monkeypatch.setattr(smoke_mode, "_build_stub_context", _fail_build)
    code = await smoke_mode.run_executor_smoke(_CleanExecutor())
    assert code == 1


# ─── runtime_wedge integration (universal turn-smoke, task #131) ───────
#
# These tests pin the post-execute wedge-check that upgrades a
# provisional PASS to FAIL when an adapter has marked the runtime
# wedged via `runtime_wedge.mark_wedged()`. Without this gate, the
# PR-25-class regression (claude_agent_sdk init wedge from a malformed
# CLI argv) shipped to GHCR because the smoke saw the outer wait_for
# timeout as "imports healthy, hit a network boundary."


class _MarkWedgedThenRaiseExecutor:
    """Mimics the claude_sdk_executor wedge path: catches the SDK's
    `Control request timeout: initialize`, calls
    `runtime_wedge.mark_wedged()` from the catch arm, then re-raises
    a sanitized error. The smoke must surface this as FAIL even
    though the outer exception class (`RuntimeError` here) would
    otherwise be a PASS-on-non-import-error.
    """

    def __init__(self, reason: str):
        self._reason = reason

    async def execute(self, context, event_queue) -> None:  # noqa: ARG002
        import runtime_wedge
        runtime_wedge.mark_wedged(self._reason)
        raise RuntimeError("sanitized adapter error after wedge")


class _MarkWedgedThenBlockExecutor:
    """Mimics a wedge that fires inside a still-running execute() —
    the adapter marks wedged, then continues to await something
    network-shaped that the outer wait_for cuts short. The pre-fix
    smoke returned 0 here ('timed out past import-tree') even though
    the runtime had already self-reported wedged.
    """

    def __init__(self, reason: str):
        self._reason = reason

    async def execute(self, context, event_queue) -> None:  # noqa: ARG002
        import runtime_wedge
        runtime_wedge.mark_wedged(self._reason)
        await asyncio.Event().wait()


# Note: runtime_wedge state is reset before/after every test by the
# autouse `_reset_runtime_wedge_between_tests` fixture in conftest.py
# so individual wedge tests don't need an explicit fixture argument.


@pytest.mark.asyncio
async def test_smoke_fails_when_adapter_marked_wedged_via_exception(
    stub_build,
):
    """PR-25 regression class: adapter catches SDK init wedge, marks
    runtime_wedge, raises a sanitized error. Outer exception class
    (`RuntimeError`) is non-import → would have been PASS pre-fix.
    Post-fix: post-run wedge check overrides PASS → FAIL."""
    code = await smoke_mode.run_executor_smoke(
        _MarkWedgedThenRaiseExecutor("claude SDK init timeout — restart workspace"),
    )
    assert code == 1


@pytest.mark.asyncio
async def test_smoke_fails_when_adapter_marked_wedged_then_blocks(
    stub_build, monkeypatch: pytest.MonkeyPatch,
):
    """Same wedge class as above but the adapter doesn't raise — it
    keeps awaiting (e.g. waiting on a control-message reply that will
    never come). Outer wait_for cuts short → would have been PASS-on-
    timeout pre-fix. Post-fix: wedge check upgrades to FAIL.
    """
    monkeypatch.setattr(smoke_mode, "_SMOKE_TIMEOUT_SECS", 0.1)
    code = await smoke_mode.run_executor_smoke(
        _MarkWedgedThenBlockExecutor("hermes init handshake timed out"),
    )
    assert code == 1


@pytest.mark.asyncio
async def test_smoke_passes_when_runtime_wedge_is_clean_after_clean_execute(
    stub_build,
):
    """Belt-and-braces: wedge-clean + clean execute() must still PASS.
    Pins that the new check is additive — it doesn't accidentally
    fail healthy executions (e.g. by treating "no runtime_wedge import"
    as a wedge)."""
    code = await smoke_mode.run_executor_smoke(_CleanExecutor())
    assert code == 0


def test_check_runtime_wedge_returns_none_when_module_missing(
    monkeypatch: pytest.MonkeyPatch,
):
    """Direct test for the import-resilience contract — the helper
    must swallow ImportError so a corrupt install doesn't crash the
    smoke gate. Catch is narrowed to (ImportError, ModuleNotFoundError)
    so a SIGNATURE drift surfaces; this test only pins the missing-
    module case.

    Defensive: drop runtime_wedge from sys.modules cache before
    patching __import__. Without the cache evict, an earlier test in
    the same file that already imported runtime_wedge would let the
    `from runtime_wedge import ...` here resolve from the cache and
    skip __import__ entirely — the test would pass for the wrong
    reason and a real regression (catch arm removed) wouldn't surface.
    """
    import builtins
    monkeypatch.delitem(sys.modules, "runtime_wedge", raising=False)
    real_import = builtins.__import__

    def _raising_import(name, *args, **kwargs):
        if name == "runtime_wedge":
            raise ImportError("simulated: runtime_wedge unavailable")
        return real_import(name, *args, **kwargs)

    monkeypatch.setattr(builtins, "__import__", _raising_import)
    assert smoke_mode._check_runtime_wedge() is None


def test_check_runtime_wedge_returns_reason_when_marked():
    """When an adapter has called runtime_wedge.mark_wedged(reason),
    the helper returns that reason verbatim so the smoke can surface
    it in the FAIL log line."""
    import runtime_wedge
    runtime_wedge.mark_wedged("explicit test reason")
    assert smoke_mode._check_runtime_wedge() == "explicit test reason"


def test_check_runtime_wedge_returns_none_when_clean():
    """Pre-condition for the additive contract: helper must return
    None (not the empty string from `wedge_reason()`) when no adapter
    has marked the runtime wedged, so the caller's `is not None`
    check works."""
    assert smoke_mode._check_runtime_wedge() is None
