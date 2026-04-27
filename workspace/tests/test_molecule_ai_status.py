"""Tests for molecule_ai_status.py — CLI status updater.

Uses importlib.util.spec_from_file_location to load the real module, bypassing
conftest mocks.
"""

import importlib.util
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]


def _load_module(monkeypatch, *, platform_url="http://platform.test", workspace_id="ws-test"):
    """Load the real molecule_ai_status.py in isolation."""
    monkeypatch.setenv("PLATFORM_URL", platform_url)
    monkeypatch.setenv("WORKSPACE_ID", workspace_id)

    spec = importlib.util.spec_from_file_location(
        "_test_molecule_ai_status",
        ROOT / "molecule_ai_status.py",
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    # Patch module-level constants to match current env
    mod.PLATFORM_URL = platform_url
    mod.WORKSPACE_ID = workspace_id
    return mod


class _FakePost:
    """Fake synchronous httpx.post that records calls and returns a response stub."""

    def __init__(self, responses=None):
        self.calls = []
        self._responses = responses or []
        self._idx = 0

    def __call__(self, url, json=None, timeout=None, headers=None):
        # Phase 30.1 added a `headers` kwarg so the heartbeat can carry
        # the workspace auth token. Record it so tests can assert either
        # presence (authenticated) or absence (pre-token legacy).
        self.calls.append({"url": url, "json": json, "timeout": timeout, "headers": headers})
        # Return a dummy object (not inspected by set_status)
        return object()


# ---------------------------------------------------------------------------
# set_status with a real task string
# ---------------------------------------------------------------------------

class TestSetStatus:

    def test_set_status_with_task_posts_heartbeat_and_activity(self, monkeypatch, capsys):
        mod = _load_module(monkeypatch)

        fake_post = _FakePost()
        monkeypatch.setattr(mod.httpx, "post", fake_post)

        mod.set_status("Running audit...")

        assert len(fake_post.calls) == 2

        heartbeat_call = fake_post.calls[0]
        assert heartbeat_call["url"] == "http://platform.test/registry/heartbeat"
        assert heartbeat_call["json"]["workspace_id"] == "ws-test"
        assert heartbeat_call["json"]["current_task"] == "Running audit..."
        assert heartbeat_call["json"]["active_tasks"] == 1
        assert heartbeat_call["timeout"] == 5.0

        activity_call = fake_post.calls[1]
        assert activity_call["url"] == "http://platform.test/workspaces/ws-test/activity"
        assert activity_call["json"]["activity_type"] == "task_update"
        assert activity_call["json"]["summary"] == "Running audit..."
        assert activity_call["json"]["status"] == "ok"
        assert activity_call["timeout"] == 5.0

        # No stderr output
        captured = capsys.readouterr()
        assert captured.err == ""

    def test_set_status_empty_string_only_posts_heartbeat(self, monkeypatch, capsys):
        mod = _load_module(monkeypatch)

        fake_post = _FakePost()
        monkeypatch.setattr(mod.httpx, "post", fake_post)

        mod.set_status("")

        # Only heartbeat, no activity post
        assert len(fake_post.calls) == 1

        heartbeat_call = fake_post.calls[0]
        assert heartbeat_call["url"] == "http://platform.test/registry/heartbeat"
        assert heartbeat_call["json"]["current_task"] == ""
        assert heartbeat_call["json"]["active_tasks"] == 0

        captured = capsys.readouterr()
        assert captured.err == ""

    def test_set_status_exception_prints_to_stderr(self, monkeypatch, capsys):
        """When httpx raises, set_status catches it and prints to stderr."""
        mod = _load_module(monkeypatch)

        def exploding_post(url, json=None, timeout=None, headers=None):
            raise ConnectionError("platform unreachable")

        monkeypatch.setattr(mod.httpx, "post", exploding_post)

        # Should NOT raise
        mod.set_status("something")

        captured = capsys.readouterr()
        # Error prefix matches the canonical module-form invocation; the
        # legacy molecule-monorepo-status shell alias only existed in the
        # dev-only workspace/Dockerfile base image, never in shipped
        # template images, so the prefix was misleading.
        assert "molecule_ai_status: failed to update" in captured.err
        assert "platform unreachable" in captured.err

    def test_set_status_heartbeat_fields_are_correct(self, monkeypatch):
        """Verify all heartbeat JSON fields are present and correct."""
        mod = _load_module(monkeypatch)

        fake_post = _FakePost()
        monkeypatch.setattr(mod.httpx, "post", fake_post)

        mod.set_status("checking metrics")

        hb_json = fake_post.calls[0]["json"]
        assert hb_json["workspace_id"] == "ws-test"
        assert hb_json["current_task"] == "checking metrics"
        assert hb_json["active_tasks"] == 1
        assert hb_json["error_rate"] == 0
        assert hb_json["sample_error"] == ""
        assert hb_json["uptime_seconds"] == 0
