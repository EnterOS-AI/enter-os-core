"""RFC #2873 iter 3 — drift gate + behavior tests for the post-split surface.

The bulk of the heartbeat / resolver behavior is exercised by
``test_mcp_cli.py`` and ``test_mcp_cli_multi_workspace.py`` through the
``mcp_cli._symbol`` back-compat aliases. This file pins:

  1. The split is **behavior-neutral via aliasing** — every previously-
     exposed ``mcp_cli._foo`` symbol is the SAME callable as the new
     module's authoritative function. If a refactor accidentally drops
     an alias or points it at a stale copy, this fails.

  2. ``mcp_inbox_pollers.start_inbox_pollers`` works for both single-
     workspace (legacy back-compat) and multi-workspace shapes.
     ``mcp_cli`` had no direct test for this branch before the split.
"""
from __future__ import annotations

import sys
import types

import pytest

import mcp_cli
import mcp_heartbeat
import mcp_inbox_pollers
import mcp_workspace_resolver


# ============== Drift gate: back-compat aliases point at the real fn ==============

class TestBackCompatAliases:
    """Pin that ``mcp_cli._foo is real_fn``. A test that re-implements
    the alias would still pass — the ``is`` check guarantees we didn't
    create a wrapper that drifts."""

    def test_heartbeat_aliases(self):
        assert mcp_cli._build_agent_card is mcp_heartbeat.build_agent_card
        assert mcp_cli._platform_register is mcp_heartbeat.platform_register
        assert mcp_cli._heartbeat_loop is mcp_heartbeat.heartbeat_loop
        assert mcp_cli._log_heartbeat_auth_failure is mcp_heartbeat.log_heartbeat_auth_failure
        assert (
            mcp_cli._persist_inbound_secret_from_heartbeat
            is mcp_heartbeat.persist_inbound_secret_from_heartbeat
        )
        assert mcp_cli._start_heartbeat_thread is mcp_heartbeat.start_heartbeat_thread

    def test_resolver_aliases(self):
        assert mcp_cli._resolve_workspaces is mcp_workspace_resolver.resolve_workspaces
        assert mcp_cli._print_missing_env_help is mcp_workspace_resolver.print_missing_env_help
        assert mcp_cli._read_token_file is mcp_workspace_resolver.read_token_file

    def test_inbox_pollers_alias(self):
        assert mcp_cli._start_inbox_pollers is mcp_inbox_pollers.start_inbox_pollers

    def test_constants_match(self):
        assert (
            mcp_cli.HEARTBEAT_INTERVAL_SECONDS
            == mcp_heartbeat.HEARTBEAT_INTERVAL_SECONDS
        )
        assert (
            mcp_cli._HEARTBEAT_AUTH_LOUD_THRESHOLD
            == mcp_heartbeat.HEARTBEAT_AUTH_LOUD_THRESHOLD
        )
        assert (
            mcp_cli._HEARTBEAT_AUTH_RELOG_INTERVAL
            == mcp_heartbeat.HEARTBEAT_AUTH_RELOG_INTERVAL
        )


# ============== mcp_inbox_pollers — both shapes + degraded import ==============

class _FakeInboxState:
    def __init__(self, **kwargs):
        self.kwargs = kwargs


def _install_fake_inbox(monkeypatch):
    """Inject a fake ``inbox`` module so we observe the spawn calls
    without pulling in the real platform_auth dependency tree."""
    activations: list[_FakeInboxState] = []
    spawned: list[tuple[_FakeInboxState, str, str]] = []
    cursor_paths: list[str] = []

    def default_cursor_path(wsid=None):
        # Mirror the real signature: optional wsid → distinct path per id,
        # absent → legacy single path.
        path = f"/tmp/.mcp_inbox_cursor.{wsid[:8]}" if wsid else "/tmp/.mcp_inbox_cursor"
        cursor_paths.append(path)
        return path

    def activate(state):
        activations.append(state)

    def start_poller_thread(state, platform_url, wsid):
        spawned.append((state, platform_url, wsid))

    fake = types.ModuleType("inbox")
    fake.InboxState = _FakeInboxState
    fake.activate = activate
    fake.default_cursor_path = default_cursor_path
    fake.start_poller_thread = start_poller_thread
    monkeypatch.setitem(sys.modules, "inbox", fake)
    return activations, spawned, cursor_paths


class TestStartInboxPollers:
    def test_single_workspace_uses_legacy_cursor_path(self, monkeypatch):
        """Back-compat exact: single-workspace mode reuses the legacy
        cursor filename so an existing operator's on-disk state isn't
        invalidated by upgrade."""
        activations, spawned, cursor_paths = _install_fake_inbox(monkeypatch)

        mcp_inbox_pollers.start_inbox_pollers(
            "https://test.moleculesai.app", ["ws-only-one"]
        )

        assert len(activations) == 1, "exactly one inbox.activate call"
        assert len(spawned) == 1, "exactly one poller thread spawned"
        # Single-workspace path uses default_cursor_path() with no arg —
        # the cursor_path captured here must be the legacy filename
        # (no per-ws suffix).
        assert cursor_paths == ["/tmp/.mcp_inbox_cursor"]
        # State carries cursor_path, not cursor_paths
        state = activations[0]
        assert state.kwargs == {"cursor_path": "/tmp/.mcp_inbox_cursor"}
        # Spawned poller is for the right workspace
        assert spawned[0] == (state, "https://test.moleculesai.app", "ws-only-one")

    def test_multi_workspace_uses_per_workspace_cursor_paths(self, monkeypatch):
        """Multi-workspace path: per-workspace cursor file, one shared
        InboxState. N pollers, each pointed at the same state so the
        agent's inbox_peek/pop sees a merged view."""
        activations, spawned, _ = _install_fake_inbox(monkeypatch)

        wsids = ["ws-aaaaaaaa", "ws-bbbbbbbb", "ws-cccccccc"]
        mcp_inbox_pollers.start_inbox_pollers(
            "https://test.moleculesai.app", wsids
        )

        # One state, one activate, three pollers
        assert len(activations) == 1
        assert len(spawned) == 3
        state = activations[0]
        # Multi-workspace state carries cursor_paths (mapping)
        assert "cursor_paths" in state.kwargs
        assert set(state.kwargs["cursor_paths"].keys()) == set(wsids)
        # All pollers share the same state
        for s, _url, _wsid in spawned:
            assert s is state
        # All workspace ids covered
        assert sorted(t[2] for t in spawned) == sorted(wsids)

    def test_inbox_module_unavailable_logs_and_returns(self, monkeypatch, caplog):
        """If ``import inbox`` fails (older install or stripped
        runtime), spawn must NOT raise — log a warning and continue.
        The MCP server can still serve outbound tools."""
        import logging

        # Force ImportError by injecting a module sentinel that raises.
        class _Boom:
            def __getattr__(self, _name):
                raise ImportError("inbox stripped from this build")

        # Setting sys.modules["inbox"] to a broken object isn't enough —
        # the import statement reads sys.modules first; if the entry is
        # truthy, Python returns it. We need to force the import to raise.
        # Easiest: pre-poison sys.modules so the `import inbox` line
        # raises by setting the entry to None (Python special-cases None
        # as "explicit ImportError").
        monkeypatch.setitem(sys.modules, "inbox", None)

        caplog.set_level(logging.WARNING, logger="mcp_inbox_pollers")
        # Should not raise.
        mcp_inbox_pollers.start_inbox_pollers(
            "https://test.moleculesai.app", ["ws-1"]
        )
        warnings = [r for r in caplog.records if r.levelno == logging.WARNING]
        assert any("inbox module unavailable" in r.message for r in warnings), (
            f"expected a 'inbox module unavailable' warning, got: "
            f"{[r.message for r in warnings]}"
        )


# ============== mcp_heartbeat.build_agent_card — short direct tests ==============

class TestBuildAgentCardDirect:
    """Spot-check the new module's public surface; the full test matrix
    lives in ``test_mcp_cli.py`` reaching through ``mcp_cli._build_agent_card``.
    """

    def test_default_card_shape(self, monkeypatch):
        for v in ("MOLECULE_AGENT_NAME", "MOLECULE_AGENT_DESCRIPTION", "MOLECULE_AGENT_SKILLS"):
            monkeypatch.delenv(v, raising=False)
        card = mcp_heartbeat.build_agent_card("8dad3e29-c32a-4ec7-9ea7-94fe2d2d98ec")
        assert card == {"name": "molecule-mcp-8dad3e29", "skills": []}

    def test_skills_csv_split_and_trim(self, monkeypatch):
        monkeypatch.setenv("MOLECULE_AGENT_SKILLS", "research, , code-review,memory-curation, ")
        card = mcp_heartbeat.build_agent_card("ws-1")
        assert card["skills"] == [
            {"name": "research"},
            {"name": "code-review"},
            {"name": "memory-curation"},
        ]


# ============== mcp_workspace_resolver — short direct tests ==============

class TestResolveWorkspacesDirect:
    @pytest.fixture(autouse=True)
    def _isolate(self, monkeypatch, tmp_path):
        for v in ("WORKSPACE_ID", "MOLECULE_WORKSPACE_TOKEN", "MOLECULE_WORKSPACES"):
            monkeypatch.delenv(v, raising=False)
        monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
        yield

    def test_single_workspace_via_env(self, monkeypatch):
        monkeypatch.setenv("WORKSPACE_ID", "ws-1")
        monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "tok")
        out, errors = mcp_workspace_resolver.resolve_workspaces()
        assert out == [("ws-1", "tok")]
        assert errors == []

    def test_multi_workspace_via_json_env(self, monkeypatch):
        monkeypatch.setenv(
            "MOLECULE_WORKSPACES",
            '[{"id":"ws-a","token":"a"},{"id":"ws-b","token":"b"}]',
        )
        out, errors = mcp_workspace_resolver.resolve_workspaces()
        assert out == [("ws-a", "a"), ("ws-b", "b")]
        assert errors == []


# ============== Token-from-file env var (issue #2934) ==============

class TestTokenFileEnv:
    """``MOLECULE_WORKSPACE_TOKEN_FILE`` lets operators keep the bearer
    out of shell history and out of MCP-host config plaintext (e.g.
    ~/.claude.json). Resolution order: inline TOKEN env > TOKEN_FILE
    env > ${CONFIGS_DIR}/.auth_token.
    """

    @pytest.fixture(autouse=True)
    def _isolate(self, monkeypatch, tmp_path):
        for v in (
            "WORKSPACE_ID",
            "MOLECULE_WORKSPACE_TOKEN",
            "MOLECULE_WORKSPACE_TOKEN_FILE",
            "MOLECULE_WORKSPACES",
        ):
            monkeypatch.delenv(v, raising=False)
        # Point CONFIGS_DIR at an empty tmp_path so the .auth_token
        # fallback returns "" — keeps the test cases unambiguous.
        monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
        yield tmp_path

    def test_token_file_env_resolves(self, monkeypatch, tmp_path):
        token_path = tmp_path / "token.txt"
        token_path.write_text("file-tok-123\n")  # trailing newline must strip
        monkeypatch.setenv("WORKSPACE_ID", "ws-1")
        monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN_FILE", str(token_path))
        out, errors = mcp_workspace_resolver.resolve_workspaces()
        assert out == [("ws-1", "file-tok-123")]
        assert errors == []

    def test_inline_token_takes_precedence_over_file(self, monkeypatch, tmp_path):
        # If both env vars are set, inline wins — matches the docstring's
        # documented order. (Operators sometimes set both during a
        # rotation; we want predictable behavior.)
        token_path = tmp_path / "token.txt"
        token_path.write_text("file-tok")
        monkeypatch.setenv("WORKSPACE_ID", "ws-1")
        monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "inline-tok")
        monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN_FILE", str(token_path))
        out, _ = mcp_workspace_resolver.resolve_workspaces()
        assert out == [("ws-1", "inline-tok")]

    def test_missing_file_returns_specific_error(self, monkeypatch, tmp_path):
        # Operator EXPLICITLY pointed TOKEN_FILE at a non-existent path —
        # surface the SPECIFIC failure (not the generic "set one of these
        # three vars" message). Otherwise they hit the silent failure mode
        # #2934 flagged ("a new user has no chance").
        bad_path = tmp_path / "does-not-exist"
        monkeypatch.setenv("WORKSPACE_ID", "ws-1")
        monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN_FILE", str(bad_path))
        out, errors = mcp_workspace_resolver.resolve_workspaces()
        assert out == []
        assert len(errors) == 1
        assert "MOLECULE_WORKSPACE_TOKEN_FILE" in errors[0]
        assert "does not exist" in errors[0]
        assert str(bad_path) in errors[0]

    def test_empty_file_returns_specific_error(self, monkeypatch, tmp_path):
        # Blank file — operator's intent was clearly the file path, so a
        # generic "no token" error would mask their config bug.
        token_path = tmp_path / "empty.txt"
        token_path.write_text("")
        monkeypatch.setenv("WORKSPACE_ID", "ws-1")
        monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN_FILE", str(token_path))
        out, errors = mcp_workspace_resolver.resolve_workspaces()
        assert out == []
        assert len(errors) == 1
        assert "MOLECULE_WORKSPACE_TOKEN_FILE" in errors[0]
        assert "is empty" in errors[0]

    def test_multi_line_file_rejected(self, monkeypatch, tmp_path):
        # CSV cell or accidental multi-token paste — would otherwise become
        # a malformed bearer that 401s against the platform with no
        # diagnostic. Reject upfront with a specific error.
        token_path = tmp_path / "junk.txt"
        token_path.write_text("tok-a tok-b\n")
        monkeypatch.setenv("WORKSPACE_ID", "ws-1")
        monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN_FILE", str(token_path))
        out, errors = mcp_workspace_resolver.resolve_workspaces()
        assert out == []
        assert len(errors) == 1
        assert "internal whitespace" in errors[0]

    def test_token_file_error_skips_configs_dir_fallback(
        self, monkeypatch, tmp_path
    ):
        # When TOKEN_FILE is explicitly set but broken, do NOT fall through
        # to a valid CONFIGS_DIR/.auth_token — the operator's intent is
        # clearly to use the file path; deferring to a different source
        # would mask their config error.
        configs_dir = tmp_path / "configs"
        configs_dir.mkdir()
        (configs_dir / ".auth_token").write_text("configs-tok")
        monkeypatch.setenv("CONFIGS_DIR", str(configs_dir))
        monkeypatch.setenv("WORKSPACE_ID", "ws-1")
        monkeypatch.setenv(
            "MOLECULE_WORKSPACE_TOKEN_FILE", str(tmp_path / "missing")
        )
        out, errors = mcp_workspace_resolver.resolve_workspaces()
        assert out == []
        # Specific TOKEN_FILE error — not the generic "no token" fallback
        # and crucially not the silent success of using configs-tok.
        assert len(errors) == 1
        assert "does not exist" in errors[0]

    def test_blank_env_var_treated_as_unset(self, monkeypatch):
        # Empty string is treated as "not set" — common pitfall when
        # users export an unset shell var.
        monkeypatch.setenv("WORKSPACE_ID", "ws-1")
        monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN_FILE", "")
        out, errors = mcp_workspace_resolver.resolve_workspaces()
        assert out == []
        assert errors

    def test_help_message_advertises_token_file(self, capsys):
        # Help text must mention TOKEN_FILE so a first-run operator
        # learns about the safer option without grepping the source.
        mcp_workspace_resolver.print_missing_env_help(
            ["WORKSPACE_ID", "MOLECULE_WORKSPACE_TOKEN"], have_token_file=False
        )
        err = capsys.readouterr().err
        assert "MOLECULE_WORKSPACE_TOKEN_FILE" in err
