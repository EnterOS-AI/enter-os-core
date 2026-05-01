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
