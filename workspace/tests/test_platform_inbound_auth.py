"""Unit tests for platform_inbound_auth — the workspace-side auth gate
on /internal/* routes."""
from __future__ import annotations

import os
from pathlib import Path

import pytest

import platform_inbound_auth
from platform_inbound_auth import (
    get_inbound_secret,
    inbound_authorized,
    reset_cache,
)


@pytest.fixture(autouse=True)
def _reset_cache_each_test():
    """get_inbound_secret caches the disk read on first call. Tests
    that overwrite the file or change CONFIGS_DIR need a clean slate."""
    reset_cache()
    yield
    reset_cache()


@pytest.fixture
def configs_dir(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
    return tmp_path


# ───────────── inbound_authorized — pure logic ─────────────

def test_authorized_happy_path():
    assert inbound_authorized("the-secret", "Bearer the-secret") is True


def test_unauthorized_missing_expected():
    """A missing secret file (None) MUST fail closed — the #2308 lesson:
    half-broken auth is worse than loud 503s."""
    assert inbound_authorized(None, "Bearer the-secret") is False


def test_unauthorized_empty_expected():
    assert inbound_authorized("", "Bearer the-secret") is False


def test_unauthorized_wrong_secret():
    assert inbound_authorized("the-secret", "Bearer wrong-secret") is False


def test_unauthorized_missing_bearer_prefix():
    """Bearer prefix is case-sensitive — matches the platform's
    wsauth.BearerTokenFromHeader contract."""
    assert inbound_authorized("the-secret", "the-secret") is False
    assert inbound_authorized("the-secret", "bearer the-secret") is False


def test_unauthorized_empty_header():
    assert inbound_authorized("the-secret", "") is False


# ───────────── get_inbound_secret — disk read ─────────────

def test_get_secret_reads_from_file(configs_dir: Path):
    (configs_dir / ".platform_inbound_secret").write_text("disk-secret")
    assert get_inbound_secret() == "disk-secret"


def test_get_secret_strips_trailing_whitespace(configs_dir: Path):
    """Operator-edited secret files commonly have trailing newlines.
    Strip on read so the constant-time compare doesn't reject."""
    (configs_dir / ".platform_inbound_secret").write_text("disk-secret\n  \n")
    assert get_inbound_secret() == "disk-secret"


def test_get_secret_returns_none_when_missing(configs_dir: Path):
    """File not present → None. Callers MUST treat None as fail-closed
    (mirrors transcript_auth.py:#328)."""
    assert get_inbound_secret() is None


def test_get_secret_returns_none_when_empty(configs_dir: Path):
    (configs_dir / ".platform_inbound_secret").write_text("")
    assert get_inbound_secret() is None


def test_get_secret_returns_none_when_whitespace_only(configs_dir: Path):
    (configs_dir / ".platform_inbound_secret").write_text("   \n  ")
    assert get_inbound_secret() is None


def test_get_secret_caches(configs_dir: Path):
    """Hot path: two reads should hit disk once. Verified by overwriting
    the file after the first read and confirming the cached value persists."""
    (configs_dir / ".platform_inbound_secret").write_text("first-value")
    assert get_inbound_secret() == "first-value"
    (configs_dir / ".platform_inbound_secret").write_text("second-value")
    assert get_inbound_secret() == "first-value"  # still cached
    reset_cache()
    assert get_inbound_secret() == "second-value"


def test_get_secret_default_dir_when_env_unset(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    """Default falls back to /configs. We can't write to /configs in the
    test sandbox; instead verify the path computation hits the default."""
    monkeypatch.delenv("CONFIGS_DIR", raising=False)
    assert platform_inbound_auth._secret_file() == Path("/configs/.platform_inbound_secret")


# ───────────── end-to-end: file → authorized ─────────────

def test_end_to_end_file_to_authorized(configs_dir: Path):
    """The two halves wire up: reading the file produces the value the
    request must present."""
    (configs_dir / ".platform_inbound_secret").write_text("e2e-secret")
    secret = get_inbound_secret()
    assert inbound_authorized(secret, "Bearer e2e-secret") is True
    assert inbound_authorized(secret, "Bearer not-this") is False


# ───────────── save_inbound_secret (RFC #2312 PR-F) ─────────────

from platform_inbound_auth import save_inbound_secret


def test_save_inbound_secret_writes_file(configs_dir: Path):
    save_inbound_secret("fresh-secret-from-register")
    assert (configs_dir / ".platform_inbound_secret").read_text() == "fresh-secret-from-register"


def test_save_inbound_secret_writes_0600_mode(configs_dir: Path):
    """File mode MUST be 0600. Anything else lets co-resident processes
    read the bearer the platform uses to call /internal/* endpoints."""
    save_inbound_secret("mode-test")
    mode = (configs_dir / ".platform_inbound_secret").stat().st_mode & 0o777
    assert mode == 0o600, f"expected 0600, got {oct(mode)}"


def test_save_inbound_secret_overwrites_existing(configs_dir: Path):
    """Idempotent — saving over an existing file replaces the content
    cleanly (atomic via tmp + rename)."""
    (configs_dir / ".platform_inbound_secret").write_text("old-value")
    save_inbound_secret("new-value")
    assert (configs_dir / ".platform_inbound_secret").read_text() == "new-value"


def test_save_inbound_secret_invalidates_cache(configs_dir: Path):
    """After saving, the next get_inbound_secret() must return the NEW
    value, not the cached old one. Otherwise rotation would be silently
    broken once we ever rotate."""
    (configs_dir / ".platform_inbound_secret").write_text("v1")
    assert get_inbound_secret() == "v1"  # primes cache
    save_inbound_secret("v2")
    assert get_inbound_secret() == "v2"  # cache invalidated, re-reads


def test_save_inbound_secret_empty_is_noop(configs_dir: Path):
    """An empty secret string is treated as 'platform didn't return one'
    and ignored — the existing file (if any) stays untouched."""
    (configs_dir / ".platform_inbound_secret").write_text("existing")
    save_inbound_secret("")
    assert (configs_dir / ".platform_inbound_secret").read_text() == "existing"


def test_save_inbound_secret_creates_parent_dir(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    """If CONFIGS_DIR doesn't exist yet (very first boot), save_inbound_secret
    creates it rather than KeyError-ing."""
    nonexistent = tmp_path / "fresh" / "configs"
    monkeypatch.setenv("CONFIGS_DIR", str(nonexistent))
    platform_inbound_auth.reset_cache()
    save_inbound_secret("bootstrap-value")
    assert (nonexistent / ".platform_inbound_secret").read_text() == "bootstrap-value"
