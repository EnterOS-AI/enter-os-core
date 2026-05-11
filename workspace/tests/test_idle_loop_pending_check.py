"""Tests for issue #381: idle loop must not fire when delegation results are pending.

The idle loop skips sending the idle prompt when DELEGATION_RESULTS_FILE
contains unconsumed results, preventing the agent from composing a stale tick
before processing pending delegation notifications from the heartbeat.

Source: workspace/main.py:_run_idle_loop() pending-results guard.
"""
from __future__ import annotations

import json

import pytest


def check_results_pending(file_path: str) -> bool:
    """Mirror the guard logic from workspace/main.py:_run_idle_loop().

    Returns True if the results file exists and is non-empty,
    meaning the idle loop should skip this tick.
    """
    try:
        with open(file_path) as rf:
            rf.seek(0)
            content = rf.read().strip()
        return bool(content)
    except FileNotFoundError:
        return False


class TestIdleLoopPendingCheck:
    """Tests for the idle-loop pending-delegation-results guard."""

    def test_no_file_means_proceed(self, tmp_path):
        """No delegation results file → idle loop fires normally."""
        results_file = tmp_path / "delegation_results.jsonl"
        assert not check_results_pending(str(results_file))

    def test_empty_file_means_proceed(self, tmp_path):
        """Empty file → no pending results → idle loop fires."""
        results_file = tmp_path / "delegation_results.jsonl"
        results_file.write_text("", encoding="utf-8")
        assert not check_results_pending(str(results_file))

    def test_whitespace_only_file_means_proceed(self, tmp_path):
        """File with only whitespace → treated as empty → idle loop fires."""
        results_file = tmp_path / "delegation_results.jsonl"
        results_file.write_text("  \n  ", encoding="utf-8")
        assert not check_results_pending(str(results_file))

    def test_single_result_means_skip(self, tmp_path):
        """File with one delegation result → skip idle tick."""
        results_file = tmp_path / "delegation_results.jsonl"
        results_file.write_text(
            json.dumps({
                "status": "completed",
                "delegation_id": "del-abc",
                "summary": "Done",
            }) + "\n",
            encoding="utf-8",
        )
        assert check_results_pending(str(results_file))

    def test_multiple_results_means_skip(self, tmp_path):
        """File with multiple delegation results → skip idle tick."""
        results_file = tmp_path / "delegation_results.jsonl"
        results_file.write_text(
            json.dumps({"status": "completed", "delegation_id": "del-1", "summary": "A"})
            + "\n"
            + json.dumps({"status": "failed", "delegation_id": "del-2", "summary": "B"})
            + "\n",
            encoding="utf-8",
        )
        assert check_results_pending(str(results_file))

    def test_file_with_only_newline_means_proceed(self, tmp_path):
        """File with only a newline character → stripped to empty → fires."""
        results_file = tmp_path / "delegation_results.jsonl"
        results_file.write_text("\n", encoding="utf-8")
        assert not check_results_pending(str(results_file))
