"""Tests for issue #381: idle loop must not fire when delegation results are pending.

The idle loop skips sending the idle prompt when DELEGATION_RESULTS_FILE
contains unconsumed results, preventing the agent from composing a stale tick
before processing pending delegation notifications from the heartbeat.

Source: ``workspace/main.py:_check_delegation_results_pending()`` (extracted from
``_run_idle_loop()`` guard; see PR #432 follow-up).

The guard is extracted into a module-level function so unit tests call the
real production logic directly — not a mirror copy.  This avoids the
test-mirror anti-pattern (issue #401) where a copied implementation
drifts from the production code it is supposed to test.
"""
from __future__ import annotations

import io
import json
from unittest.mock import patch

from main import _check_delegation_results_pending


class TestIdleLoopPendingCheck:
    """Tests for the idle-loop pending-delegation-results guard.

    Each test patches ``builtins.open`` so ``_check_delegation_results_pending``
    reads the controlled payload instead of the real DELEGATION_RESULTS_FILE.
    No filesystem side-effects.
    """

    def _patch_open(self, payload: str | None):
        """Patch builtins.open for _check_delegation_results_pending.

        Args:
            payload: file contents to return. None → FileNotFoundError.
        """
        if payload is None:
            return patch("builtins.open", side_effect=FileNotFoundError)
        else:
            fake_file = io.StringIO(payload)
            return patch("builtins.open", return_value=fake_file)

    def test_no_file_means_proceed(self):
        """No delegation results file → idle loop fires normally."""
        with self._patch_open(None):
            assert _check_delegation_results_pending() is False

    def test_empty_file_means_proceed(self):
        """Empty file → no pending results → idle loop fires."""
        with self._patch_open(""):
            assert _check_delegation_results_pending() is False

    def test_whitespace_only_file_means_proceed(self):
        """File with only whitespace → treated as empty → idle loop fires."""
        with self._patch_open("  \n  "):
            assert _check_delegation_results_pending() is False

    def test_single_result_means_skip(self):
        """File with one delegation result → skip idle tick."""
        payload = (
            json.dumps({
                "status": "completed",
                "delegation_id": "del-abc",
                "summary": "Done",
            }) + "\n"
        )
        with self._patch_open(payload):
            assert _check_delegation_results_pending() is True

    def test_multiple_results_means_skip(self):
        """File with multiple delegation results → skip idle tick."""
        payload = (
            json.dumps({"status": "completed", "delegation_id": "del-1", "summary": "A"})
            + "\n"
            + json.dumps({"status": "failed", "delegation_id": "del-2", "summary": "B"})
            + "\n"
        )
        with self._patch_open(payload):
            assert _check_delegation_results_pending() is True

    def test_file_with_only_newline_means_proceed(self):
        """File with only a newline character → stripped to empty → fires."""
        with self._patch_open("\n"):
            assert _check_delegation_results_pending() is False
