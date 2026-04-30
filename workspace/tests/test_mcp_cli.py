"""Tests for workspace/mcp_cli.py — the molecule-mcp console-script
entry-point validator.

The wrapper exists to surface a friendly missing-env error before
a2a_client.py:22's module-level RuntimeError fires. Regressions here
ship a poor first-run UX to every external-runtime operator.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

import mcp_cli


@pytest.fixture(autouse=True)
def _isolate(monkeypatch, tmp_path):
    """Each test starts with no Molecule env vars set + a fresh
    CONFIGS_DIR pointing at an empty tmpdir."""
    for var in ("WORKSPACE_ID", "PLATFORM_URL", "MOLECULE_WORKSPACE_TOKEN"):
        monkeypatch.delenv(var, raising=False)
    monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
    yield


def _run_main_capturing_exit(capsys) -> tuple[int, str]:
    """Call mcp_cli.main and return (exit_code, stderr).

    main() is supposed to sys.exit on missing env. Any non-exit return
    means it tried to run the real MCP loop, which we don't want in a
    unit test (and which would also fail because we never set the
    mandatory env).
    """
    with pytest.raises(SystemExit) as exc_info:
        mcp_cli.main()
    captured = capsys.readouterr()
    code = exc_info.value.code if isinstance(exc_info.value.code, int) else 1
    return code, captured.err


def test_missing_workspace_id_exits_with_message(capsys):
    code, err = _run_main_capturing_exit(capsys)
    assert code == 2, f"expected exit code 2, got {code}"
    assert "WORKSPACE_ID" in err
    assert "PLATFORM_URL" in err  # also missing
    assert "MOLECULE_WORKSPACE_TOKEN" in err  # also missing


def test_only_workspace_id_missing(capsys, monkeypatch):
    monkeypatch.setenv("PLATFORM_URL", "http://localhost:8080")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "tok")
    code, err = _run_main_capturing_exit(capsys)
    assert code == 2
    # Only WORKSPACE_ID should appear in the "currently missing" list.
    assert "Currently missing: WORKSPACE_ID" in err


def test_only_platform_url_missing(capsys, monkeypatch):
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "tok")
    code, err = _run_main_capturing_exit(capsys)
    assert code == 2
    assert "Currently missing: PLATFORM_URL" in err


def test_only_token_missing(capsys, monkeypatch):
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "http://localhost:8080")
    code, err = _run_main_capturing_exit(capsys)
    assert code == 2
    assert "MOLECULE_WORKSPACE_TOKEN" in err


def test_token_file_satisfies_token_requirement(capsys, monkeypatch, tmp_path):
    """Token from CONFIGS_DIR/.auth_token must be accepted (in-container
    path)."""
    (tmp_path / ".auth_token").write_text("file-token")
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "http://localhost:8080")
    # No MOLECULE_WORKSPACE_TOKEN — but file exists. Validation should
    # pass; we then short-circuit before importing the heavy module by
    # patching the import to a no-op spy.

    spy_called: dict[str, bool] = {"called": False}

    def fake_cli_main():
        spy_called["called"] = True

    # Patch the heavy import to avoid actually running the MCP server.
    # mcp_cli does the import lazily inside main(), so we monkeypatch
    # sys.modules to inject a fake a2a_mcp_server.
    import types
    fake_module = types.ModuleType("a2a_mcp_server")
    fake_module.cli_main = fake_cli_main
    monkeypatch.setitem(sys.modules, "a2a_mcp_server", fake_module)

    mcp_cli.main()  # should NOT exit
    assert spy_called["called"], "expected cli_main to be invoked when env+file are valid"


def test_env_token_satisfies_token_requirement(capsys, monkeypatch):
    """Token from env must be accepted (external-runtime path)."""
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "http://localhost:8080")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "env-token")

    spy_called: dict[str, bool] = {"called": False}

    def fake_cli_main():
        spy_called["called"] = True

    import types
    fake_module = types.ModuleType("a2a_mcp_server")
    fake_module.cli_main = fake_cli_main
    monkeypatch.setitem(sys.modules, "a2a_mcp_server", fake_module)

    mcp_cli.main()
    assert spy_called["called"]


def test_whitespace_only_env_treated_as_missing(capsys, monkeypatch):
    """An accidentally-empty env var (WORKSPACE_ID="   ") must NOT be
    considered set — otherwise the error would surface deep inside an
    HTTP call instead of in this validator."""
    monkeypatch.setenv("WORKSPACE_ID", "   ")
    monkeypatch.setenv("PLATFORM_URL", "http://localhost:8080")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "tok")
    code, err = _run_main_capturing_exit(capsys)
    assert code == 2
    assert "WORKSPACE_ID" in err


def test_help_lists_canvas_tokens_tab_pointer(capsys):
    """Operator must know WHERE to get a token. The help mentions the
    canvas Tokens tab so they can self-recover without asking on
    Slack."""
    code, err = _run_main_capturing_exit(capsys)
    assert code == 2
    assert "Tokens tab" in err or "canvas" in err.lower()
