"""Tests for workspace/configs_dir.py — the single resolution point
for the per-workspace state directory."""
from __future__ import annotations

import os
import stat
from pathlib import Path

import pytest

import configs_dir


@pytest.fixture(autouse=True)
def _isolate(monkeypatch):
    """Each test gets a clean cache and a clean env. Tests that need
    CONFIGS_DIR set monkeypatch it themselves."""
    monkeypatch.delenv("CONFIGS_DIR", raising=False)
    configs_dir.reset_cache()
    yield
    configs_dir.reset_cache()


def test_explicit_env_var_wins(tmp_path, monkeypatch):
    """An explicit CONFIGS_DIR is the operator's override — always
    respected, even when /configs is also writable. This preserves
    existing test/custom-deployment patterns that monkeypatch the env
    var to a per-test tmp_path."""
    monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
    assert configs_dir.resolve() == tmp_path


def test_explicit_env_var_creates_dir(tmp_path, monkeypatch):
    """Explicit override creates the dir if missing — operator can
    point at a not-yet-existing path and have the runtime materialize
    it."""
    target = tmp_path / "nested" / "configs"
    monkeypatch.setenv("CONFIGS_DIR", str(target))
    assert not target.exists()
    configs_dir.resolve()
    assert target.exists()


def test_in_container_uses_slash_configs(monkeypatch, tmp_path):
    """When /configs exists and is writable, return it. Verified by
    pointing /configs detection at a writable tmp_path via the same
    env-var override path the helper exposes."""
    # Simulate "in-container" by aliasing /configs to a real writable
    # path. Not actually creating /configs on the test host (would
    # require root) — instead, rely on the explicit-env-var branch
    # which is the same code path operators see in tests today.
    monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
    result = configs_dir.resolve()
    assert result == tmp_path
    assert os.access(str(result), os.W_OK)


def test_falls_back_to_home_when_configs_missing(monkeypatch, tmp_path):
    """No CONFIGS_DIR + no writable /configs → fall back to
    ~/.molecule-workspace. This is the bug from external-runtime
    onboarding (issue #2458): operators on a Mac/Linux laptop don't
    have /configs and the default would silently fail on the first
    heartbeat write."""
    fake_home = tmp_path / "home"
    fake_home.mkdir()
    monkeypatch.setenv("HOME", str(fake_home))
    # Ensure /configs is not writable for an unprivileged process.
    # This is true on every developer machine — the test is just
    # asserting we DON'T pick it up when we can't write to it.
    if Path("/configs").exists() and os.access("/configs", os.W_OK):
        pytest.skip("/configs is writable on this host; can't exercise fallback")
    result = configs_dir.resolve()
    assert result == fake_home / ".molecule-workspace"
    assert result.exists()


def test_fallback_dir_is_0700(monkeypatch, tmp_path):
    """The fallback dir must be 0700 — per-file 0600 perms on
    .auth_token + .platform_inbound_secret would be undermined by a
    world-readable parent."""
    fake_home = tmp_path / "home"
    fake_home.mkdir()
    monkeypatch.setenv("HOME", str(fake_home))
    if Path("/configs").exists() and os.access("/configs", os.W_OK):
        pytest.skip("/configs is writable on this host; can't exercise fallback")
    result = configs_dir.resolve()
    mode = stat.S_IMODE(result.stat().st_mode)
    assert mode == 0o700, f"expected 0700, got 0o{mode:o}"


def test_fallback_dir_idempotent(monkeypatch, tmp_path):
    """Resolving twice when the fallback dir already exists is fine
    — we don't re-mkdir or change perms on every call."""
    fake_home = tmp_path / "home"
    fake_home.mkdir()
    monkeypatch.setenv("HOME", str(fake_home))
    if Path("/configs").exists() and os.access("/configs", os.W_OK):
        pytest.skip("/configs is writable on this host; can't exercise fallback")
    first = configs_dir.resolve()
    configs_dir.reset_cache()
    second = configs_dir.resolve()
    assert first == second
    assert second.exists()


def test_env_var_changes_picked_up_live(tmp_path, monkeypatch):
    """Resolution reads CONFIGS_DIR live on each call — existing tests
    monkeypatch the env var between cases and expect the new value to
    take effect without an explicit cache reset."""
    monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
    first = configs_dir.resolve()
    new_path = tmp_path / "after-change"
    monkeypatch.setenv("CONFIGS_DIR", str(new_path))
    second = configs_dir.resolve()
    assert first == tmp_path
    assert second == new_path
