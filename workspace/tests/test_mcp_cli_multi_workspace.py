"""Tests for mcp_cli's multi-workspace resolution + parallel
register/heartbeat/poller spawning.

Single-workspace path is exhaustively covered in test_mcp_cli.py; this
file covers ONLY the new MOLECULE_WORKSPACES path so a regression that
breaks multi-workspace doesn't get hidden in a 1000-line test file.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

# Add workspace dir to path so `import mcp_cli` works regardless of pytest
# cwd. Mirrors the pattern in tests/conftest.py.
_THIS = Path(__file__).resolve()
sys.path.insert(0, str(_THIS.parent.parent))


@pytest.fixture(autouse=True)
def _isolate_env(monkeypatch):
    """Strip every env var the resolver looks at so each test starts clean.

    Tests set ONLY the vars they care about. Without this fixture an
    unrelated test that exported MOLECULE_WORKSPACES would silently
    influence the next test's outcome.
    """
    for var in (
        "MOLECULE_WORKSPACES",
        "WORKSPACE_ID",
        "MOLECULE_WORKSPACE_TOKEN",
        "PLATFORM_URL",
    ):
        monkeypatch.delenv(var, raising=False)


def _import_mcp_cli():
    # Late import so monkeypatch has scrubbed the env first.
    import importlib

    import mcp_cli

    return importlib.reload(mcp_cli)


class TestResolveWorkspaces:
    def test_multi_workspace_json_returns_pairs(self, monkeypatch):
        monkeypatch.setenv(
            "MOLECULE_WORKSPACES",
            json.dumps([
                {"id": "ws-a", "token": "tok-a"},
                {"id": "ws-b", "token": "tok-b"},
            ]),
        )
        mcp_cli = _import_mcp_cli()
        out, errors = mcp_cli._resolve_workspaces()
        assert errors == []
        assert out == [("ws-a", "tok-a"), ("ws-b", "tok-b")]

    def test_multi_workspace_ignores_legacy_env_vars(self, monkeypatch):
        # When MOLECULE_WORKSPACES is set, WORKSPACE_ID + token env are
        # ignored. This is the documented contract — JSON wins, no
        # silent merging of two sources.
        monkeypatch.setenv("WORKSPACE_ID", "should-be-ignored")
        monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "should-be-ignored")
        monkeypatch.setenv(
            "MOLECULE_WORKSPACES",
            json.dumps([{"id": "ws-only", "token": "tok-only"}]),
        )
        mcp_cli = _import_mcp_cli()
        out, errors = mcp_cli._resolve_workspaces()
        assert errors == []
        assert out == [("ws-only", "tok-only")]

    def test_invalid_json_returns_error(self, monkeypatch):
        monkeypatch.setenv("MOLECULE_WORKSPACES", "{not valid json")
        mcp_cli = _import_mcp_cli()
        out, errors = mcp_cli._resolve_workspaces()
        assert out == []
        assert any("not valid JSON" in e for e in errors)

    def test_non_array_returns_error(self, monkeypatch):
        monkeypatch.setenv("MOLECULE_WORKSPACES", '{"id":"ws","token":"tok"}')
        mcp_cli = _import_mcp_cli()
        out, errors = mcp_cli._resolve_workspaces()
        assert out == []
        assert any("non-empty JSON array" in e for e in errors)

    def test_empty_array_returns_error(self, monkeypatch):
        monkeypatch.setenv("MOLECULE_WORKSPACES", "[]")
        mcp_cli = _import_mcp_cli()
        out, errors = mcp_cli._resolve_workspaces()
        assert out == []
        assert any("non-empty JSON array" in e for e in errors)

    def test_missing_id_or_token_in_entry_returns_error(self, monkeypatch):
        monkeypatch.setenv(
            "MOLECULE_WORKSPACES",
            json.dumps([{"id": "ws-a"}, {"token": "tok-only"}]),
        )
        mcp_cli = _import_mcp_cli()
        out, errors = mcp_cli._resolve_workspaces()
        assert out == []
        assert len(errors) >= 2
        assert any("[0] missing 'id' or 'token'" in e for e in errors)
        assert any("[1] missing 'id' or 'token'" in e for e in errors)

    def test_duplicate_workspace_id_returns_error(self, monkeypatch):
        # Two registrations with the same workspace_id is almost
        # certainly an operator typo — heartbeat threads would race
        # against each other. Reject it loudly.
        monkeypatch.setenv(
            "MOLECULE_WORKSPACES",
            json.dumps([
                {"id": "ws-a", "token": "tok-1"},
                {"id": "ws-a", "token": "tok-2"},
            ]),
        )
        mcp_cli = _import_mcp_cli()
        out, errors = mcp_cli._resolve_workspaces()
        assert out == []
        assert any("duplicate workspace id" in e for e in errors)

    def test_legacy_single_workspace_via_env(self, monkeypatch):
        monkeypatch.setenv("WORKSPACE_ID", "legacy-ws")
        monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "legacy-tok")
        mcp_cli = _import_mcp_cli()
        out, errors = mcp_cli._resolve_workspaces()
        assert errors == []
        assert out == [("legacy-ws", "legacy-tok")]

    def test_legacy_no_workspace_id_returns_error(self, monkeypatch):
        monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "tok")
        mcp_cli = _import_mcp_cli()
        out, errors = mcp_cli._resolve_workspaces()
        assert out == []
        assert any("WORKSPACE_ID" in e for e in errors)

    def test_legacy_no_token_returns_error(self, monkeypatch, tmp_path):
        # Force configs_dir.resolve() to a clean dir so the .auth_token
        # fallback finds nothing.
        monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
        monkeypatch.setenv("WORKSPACE_ID", "ws")
        mcp_cli = _import_mcp_cli()
        out, errors = mcp_cli._resolve_workspaces()
        assert out == []
        assert any("MOLECULE_WORKSPACE_TOKEN" in e for e in errors)


class TestPlatformAuthRegistry:
    """The token registry is what wires per-workspace heartbeats /
    pollers / send_message_to_user to the right tenant. If this dies,
    all multi-workspace traffic 401s — guard tightly.
    """

    def setup_method(self):
        # Each test runs against a clean registry — clear_cache also
        # wipes the multi-workspace dict (see platform_auth changes).
        import platform_auth

        platform_auth.clear_cache()

    def test_register_and_lookup(self):
        import platform_auth

        platform_auth.register_workspace_token("ws-a", "tok-a")
        platform_auth.register_workspace_token("ws-b", "tok-b")
        assert platform_auth.get_workspace_token("ws-a") == "tok-a"
        assert platform_auth.get_workspace_token("ws-b") == "tok-b"
        assert platform_auth.get_workspace_token("ws-c") is None

    def test_auth_headers_routes_by_workspace(self, monkeypatch):
        import platform_auth

        monkeypatch.setenv("PLATFORM_URL", "https://example.test")
        platform_auth.register_workspace_token("ws-a", "tok-a")
        platform_auth.register_workspace_token("ws-b", "tok-b")

        a = platform_auth.auth_headers("ws-a")
        b = platform_auth.auth_headers("ws-b")
        assert a["Authorization"] == "Bearer tok-a"
        assert b["Authorization"] == "Bearer tok-b"
        assert a["Origin"] == "https://example.test"

    def test_auth_headers_with_no_arg_uses_legacy_path(self, monkeypatch, tmp_path):
        import platform_auth

        # Wipe the module-level token cache and redirect _token_file() to a
        # non-existent path so the env var isolation is clean. Without this,
        # the real /configs/.auth_token pollutes the result.
        platform_auth.clear_cache()
        monkeypatch.setattr(platform_auth, "_token_file", lambda: tmp_path / ".auth_token")
        monkeypatch.setenv("PLATFORM_URL", "https://example.test")
        monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "legacy-tok")
        # Multi-workspace registry populated, but auth_headers() with
        # no arg ignores it and uses the legacy resolution path. This
        # is the back-compat invariant for single-workspace tools that
        # haven't been updated yet to thread workspace_id through.
        platform_auth.register_workspace_token("ws-a", "tok-a")

        h = platform_auth.auth_headers()
        assert h["Authorization"] == "Bearer legacy-tok"

    def test_auth_headers_with_unknown_workspace_falls_back_to_legacy(
        self, monkeypatch, tmp_path
    ):
        import platform_auth

        # Wipe the module-level token cache and redirect _token_file() to a
        # non-existent path so the env var isolation is clean. Without this,
        # the real /configs/.auth_token pollutes the result.
        platform_auth.clear_cache()
        monkeypatch.setattr(platform_auth, "_token_file", lambda: tmp_path / ".auth_token")
        monkeypatch.setenv("PLATFORM_URL", "https://example.test")
        monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "legacy-tok")
        platform_auth.register_workspace_token("ws-a", "tok-a")

        # workspace_id arg points to a workspace NOT in the registry —
        # auth_headers falls back to the legacy single-workspace token
        # rather than 401-ing. Lets a single-workspace install accept
        # workspace_id args without crashing.
        h = platform_auth.auth_headers("ws-unknown")
        assert h["Authorization"] == "Bearer legacy-tok"

    def test_register_idempotent_same_token(self):
        import platform_auth

        platform_auth.register_workspace_token("ws-a", "tok-a")
        platform_auth.register_workspace_token("ws-a", "tok-a")
        assert platform_auth.get_workspace_token("ws-a") == "tok-a"

    def test_register_token_rotation(self):
        import platform_auth

        platform_auth.register_workspace_token("ws-a", "tok-old")
        platform_auth.register_workspace_token("ws-a", "tok-new")
        assert platform_auth.get_workspace_token("ws-a") == "tok-new"

    def test_clear_cache_wipes_registry(self):
        import platform_auth

        platform_auth.register_workspace_token("ws-a", "tok-a")
        platform_auth.clear_cache()
        assert platform_auth.get_workspace_token("ws-a") is None


class TestInboxStateMultiWorkspace:
    def test_per_workspace_cursor(self, tmp_path):
        import inbox

        path_a = tmp_path / ".cursor_a"
        path_b = tmp_path / ".cursor_b"
        state = inbox.InboxState(cursor_paths={"ws-a": path_a, "ws-b": path_b})

        state.save_cursor("activity-1", workspace_id="ws-a")
        state.save_cursor("activity-2", workspace_id="ws-b")

        assert path_a.read_text() == "activity-1"
        assert path_b.read_text() == "activity-2"
        assert state.load_cursor("ws-a") == "activity-1"
        assert state.load_cursor("ws-b") == "activity-2"

    def test_reset_only_targeted_workspace(self, tmp_path):
        import inbox

        path_a = tmp_path / ".cursor_a"
        path_b = tmp_path / ".cursor_b"
        state = inbox.InboxState(cursor_paths={"ws-a": path_a, "ws-b": path_b})
        state.save_cursor("a-1", workspace_id="ws-a")
        state.save_cursor("b-1", workspace_id="ws-b")

        state.reset_cursor(workspace_id="ws-a")

        assert not path_a.exists()
        assert path_b.read_text() == "b-1"
        assert state.load_cursor("ws-a") is None
        assert state.load_cursor("ws-b") == "b-1"

    def test_back_compat_single_workspace_cursor_path(self, tmp_path):
        # Single-workspace constructor (positional cursor_path=) still
        # works exactly as before. Cursor key is the empty string.
        import inbox

        path = tmp_path / ".legacy_cursor"
        state = inbox.InboxState(cursor_path=path)
        state.save_cursor("act-1")  # no workspace_id arg
        assert path.read_text() == "act-1"
        assert state.load_cursor() == "act-1"

    def test_arrival_workspace_id_in_message_to_dict(self):
        import inbox

        m = inbox.InboxMessage(
            activity_id="a1",
            text="hi",
            peer_id="",
            method="message/send",
            created_at="2026-05-04T15:00:00Z",
            arrival_workspace_id="ws-personal",
        )
        d = m.to_dict()
        assert d["arrival_workspace_id"] == "ws-personal"

    def test_arrival_workspace_id_omitted_when_empty(self):
        # Single-workspace consumers shouldn't see the new key in their
        # output — back-compat exact.
        import inbox

        m = inbox.InboxMessage(
            activity_id="a1",
            text="hi",
            peer_id="",
            method="message/send",
            created_at="2026-05-04T15:00:00Z",
        )
        d = m.to_dict()
        assert "arrival_workspace_id" not in d


class TestDefaultCursorPathPerWorkspace:
    def test_with_workspace_id_returns_namespaced_path(self, monkeypatch, tmp_path):
        # configs_dir.resolve() reads CONFIGS_DIR env; pin it so the
        # test doesn't depend on the operator's home dir.
        monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
        import inbox

        p_a = inbox.default_cursor_path("ws-aaaa11112222")
        p_b = inbox.default_cursor_path("ws-bbbb33334444")
        assert p_a != p_b
        # Names should disambiguate by 8-char prefix.
        assert "ws-aaaa1" in p_a.name
        assert "ws-bbbb3" in p_b.name

    def test_no_workspace_id_returns_legacy_filename(self, monkeypatch, tmp_path):
        monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
        import inbox

        # Legacy single-workspace operators must keep their existing on-disk
        # cursor — the filename is `.mcp_inbox_cursor` (no suffix).
        p = inbox.default_cursor_path()
        assert p.name == ".mcp_inbox_cursor"
