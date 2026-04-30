"""Tests for workspace/platform_auth.py (Phase 30.1)."""
from __future__ import annotations

import os
import stat
from pathlib import Path

import pytest

import platform_auth


@pytest.fixture(autouse=True)
def _isolate(tmp_path, monkeypatch):
    """Each test gets its own CONFIGS_DIR and a fresh in-process cache."""
    monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
    platform_auth.clear_cache()
    yield
    platform_auth.clear_cache()


def test_get_token_returns_none_when_file_absent(tmp_path):
    assert platform_auth.get_token() is None


def test_save_and_get_roundtrip(tmp_path):
    platform_auth.save_token("secret-abc123")
    assert platform_auth.get_token() == "secret-abc123"
    # File contents match exactly, no trailing newline
    assert (tmp_path / ".auth_token").read_text() == "secret-abc123"


def test_saved_file_is_0600(tmp_path):
    platform_auth.save_token("very-secret")
    mode = stat.S_IMODE((tmp_path / ".auth_token").stat().st_mode)
    assert mode == 0o600, f"expected 0600 mode, got 0o{mode:o}"


def test_save_token_strips_whitespace(tmp_path):
    platform_auth.save_token("  padded-token  \n")
    assert platform_auth.get_token() == "padded-token"


def test_save_token_rejects_empty():
    with pytest.raises(ValueError):
        platform_auth.save_token("")
    with pytest.raises(ValueError):
        platform_auth.save_token("   \n")


def test_save_token_idempotent(tmp_path):
    """Saving the same token twice must not change the file's mtime."""
    platform_auth.save_token("stable-token")
    path = tmp_path / ".auth_token"
    first_mtime = path.stat().st_mtime_ns
    # Force cache path to fire; save_token should no-op
    platform_auth.clear_cache()
    platform_auth.save_token("stable-token")
    assert path.stat().st_mtime_ns == first_mtime


def test_save_token_rotation_overwrites(tmp_path):
    platform_auth.save_token("token-v1")
    platform_auth.save_token("token-v2")
    assert platform_auth.get_token() == "token-v2"


def test_auth_headers_when_no_token_and_no_platform_is_empty(monkeypatch):
    monkeypatch.delenv("PLATFORM_URL", raising=False)
    assert platform_auth.auth_headers() == {}


def test_auth_headers_when_no_token_includes_origin(monkeypatch):
    """Origin must be set even without a token — the WAF gates ALL
    requests to /workspaces and /registry, including pre-token bootstrap
    register calls. Without Origin those would silently 404 from Next.js."""
    monkeypatch.setenv("PLATFORM_URL", "https://tenant.moleculesai.app")
    assert platform_auth.auth_headers() == {"Origin": "https://tenant.moleculesai.app"}


def test_auth_headers_format(monkeypatch):
    monkeypatch.delenv("PLATFORM_URL", raising=False)
    platform_auth.save_token("hello-world")
    assert platform_auth.auth_headers() == {"Authorization": "Bearer hello-world"}


def test_auth_headers_includes_origin_when_platform_url_set(monkeypatch):
    """Both Authorization and Origin land on the same dict so the
    SaaS edge WAF accepts every workspace-runtime request."""
    monkeypatch.setenv("PLATFORM_URL", "https://hongmingwang.moleculesai.app")
    platform_auth.save_token("tok")
    assert platform_auth.auth_headers() == {
        "Authorization": "Bearer tok",
        "Origin": "https://hongmingwang.moleculesai.app",
    }


def test_get_token_caches_after_first_disk_read(tmp_path, monkeypatch):
    path = tmp_path / ".auth_token"
    path.write_text("disk-token")

    # First call populates the cache
    assert platform_auth.get_token() == "disk-token"

    # Now mutate the file behind the cache's back.
    path.write_text("ignored-by-cache")
    # Subsequent calls return the cached value, NOT the new disk content.
    assert platform_auth.get_token() == "disk-token"

    # clear_cache() forces a re-read
    platform_auth.clear_cache()
    assert platform_auth.get_token() == "ignored-by-cache"


def test_get_token_handles_empty_file(tmp_path):
    (tmp_path / ".auth_token").write_text("")
    assert platform_auth.get_token() is None


def test_get_token_handles_whitespace_only_file(tmp_path):
    (tmp_path / ".auth_token").write_text("   \n\n   ")
    assert platform_auth.get_token() is None


def test_configs_dir_respected(tmp_path, monkeypatch):
    alt = tmp_path / "alt-configs"
    alt.mkdir()
    monkeypatch.setenv("CONFIGS_DIR", str(alt))
    platform_auth.clear_cache()
    platform_auth.save_token("where-does-it-land")
    assert (alt / ".auth_token").exists()
    assert not (tmp_path / ".auth_token").exists()


def test_default_configs_dir_fallback(tmp_path, monkeypatch):
    monkeypatch.delenv("CONFIGS_DIR", raising=False)
    # Can't actually write to /configs on a dev laptop, so just verify the
    # path resolution points there. Save will fail gracefully via mkdir+exist_ok.
    platform_auth.clear_cache()
    # We expect _token_file() to resolve under /configs when env is unset.
    path = platform_auth._token_file()
    assert str(path).startswith("/configs")


# ==================== MOLECULE_WORKSPACE_TOKEN env-var fallback ====================
# External-runtime path: operators running the universal MCP server outside
# a container have no /configs volume. They pass the token via env. The
# fallback must NOT override the file when both are present (in-container
# rotation must keep working) and MUST surface env when the file is absent.


def test_get_token_uses_env_when_file_absent(tmp_path, monkeypatch):
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "env-token-xyz")
    assert not (tmp_path / ".auth_token").exists()
    assert platform_auth.get_token() == "env-token-xyz"


def test_get_token_file_takes_priority_over_env(tmp_path, monkeypatch):
    """In-container rotation must keep working — file overrides env."""
    (tmp_path / ".auth_token").write_text("file-token")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "env-token-should-be-ignored")
    assert platform_auth.get_token() == "file-token"


def test_get_token_falls_back_to_env_when_file_empty(tmp_path, monkeypatch):
    """Empty file is equivalent to absent — env still fires."""
    (tmp_path / ".auth_token").write_text("")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "env-token-fallback")
    assert platform_auth.get_token() == "env-token-fallback"


def test_get_token_strips_env_whitespace(tmp_path, monkeypatch):
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "  padded-env-token  \n")
    assert platform_auth.get_token() == "padded-env-token"


def test_get_token_ignores_empty_env(tmp_path, monkeypatch):
    """Empty string env var is the same as unset — no false positive."""
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "")
    assert platform_auth.get_token() is None


def test_get_token_ignores_whitespace_only_env(tmp_path, monkeypatch):
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "   \n\n   ")
    assert platform_auth.get_token() is None


def test_env_token_caches_like_file_token(tmp_path, monkeypatch):
    """Once env-token is read, mutating env shouldn't affect cached value."""
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "first-env-token")
    assert platform_auth.get_token() == "first-env-token"
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "second-env-token")
    # Cache returns first value
    assert platform_auth.get_token() == "first-env-token"
    # clear_cache forces re-read of env
    platform_auth.clear_cache()
    assert platform_auth.get_token() == "second-env-token"


def test_auth_headers_works_with_env_token(tmp_path, monkeypatch):
    """Header construction must use the env-fallback token, not silently
    return {} when no file exists."""
    monkeypatch.delenv("PLATFORM_URL", raising=False)
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "external-bearer")
    assert platform_auth.auth_headers() == {"Authorization": "Bearer external-bearer"}
