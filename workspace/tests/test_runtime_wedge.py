"""Tests for runtime_wedge — the runtime-side wedge-state module that
heartbeat reads + adapter executors write. Extracted from claude_sdk_
executor (task #87 universal-runtime refactor) so the executor can move
to its template repo without breaking heartbeat.

The behavior is identical to the prior in-executor implementation; tests
pin the contract so the re-export shim in claude_sdk_executor.py can
later be deleted without surprise."""
import pytest

import runtime_wedge


@pytest.fixture(autouse=True)
def _reset():
    """Each test starts with a clean wedge state — production wedges are
    sticky-per-process, but cross-test bleed would couple unrelated cases."""
    runtime_wedge.reset_for_test()
    yield
    runtime_wedge.reset_for_test()


class TestRuntimeWedge:
    def test_starts_unwedged(self):
        assert runtime_wedge.is_wedged() is False
        assert runtime_wedge.wedge_reason() == ""

    def test_mark_wedged_sets_flag_and_reason(self):
        runtime_wedge.mark_wedged("SDK init timeout")
        assert runtime_wedge.is_wedged() is True
        assert runtime_wedge.wedge_reason() == "SDK init timeout"

    def test_first_mark_wins(self):
        # Stable banner text is more important than the most-recent
        # cause. A second wedge while already wedged should NOT
        # overwrite — operator sees the original (more diagnosable)
        # reason, not whatever the SDK said next.
        runtime_wedge.mark_wedged("SDK init timeout")
        runtime_wedge.mark_wedged("Subsequent identical-class wedge")
        assert runtime_wedge.wedge_reason() == "SDK init timeout"

    def test_clear_wedge_restores_healthy(self):
        # Auto-recovery: when the SDK starts working again, the next
        # heartbeat must report empty runtime_state so the platform
        # flips status from degraded back to online.
        runtime_wedge.mark_wedged("transient blip")
        runtime_wedge.clear_wedge()
        assert runtime_wedge.is_wedged() is False
        assert runtime_wedge.wedge_reason() == ""

    def test_clear_wedge_when_not_wedged_is_noop(self):
        # No-op safety — production calls clear_wedge() on every
        # successful query (~thousands of times per session); throwing
        # or logging when not wedged would spam.
        runtime_wedge.clear_wedge()
        runtime_wedge.clear_wedge()  # still safe twice in a row
        assert runtime_wedge.is_wedged() is False

    def test_re_marking_after_clear_is_allowed(self):
        # Real production path: SDK wedges, recovers, wedges again.
        # Each cycle should land cleanly (not silently drop).
        runtime_wedge.mark_wedged("first wedge")
        runtime_wedge.clear_wedge()
        runtime_wedge.mark_wedged("second wedge — different reason")
        assert runtime_wedge.is_wedged() is True
        assert runtime_wedge.wedge_reason() == "second wedge — different reason"


# TestClaudeSdkExecutorReExportShim removed alongside
# workspace/claude_sdk_executor.py — the shim served its one-release-
# cycle purpose during the universal-runtime refactor (#87 Phase 2).
# The executor + its shim now live in the claude-code template repo.
