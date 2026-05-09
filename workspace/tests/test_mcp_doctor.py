"""Tests for the molecule-mcp doctor subcommand (#2934 item 6).

Each `check_*` function is unit-tested in isolation via env
manipulation. The integration test (`test_run_no_env_returns_1`) pins
the end-to-end exit code on a stripped environment — what an operator
running the command for the first time on an untouched shell sees.
"""
from __future__ import annotations

import os
import sys
from pathlib import Path
from unittest import mock

import pytest

# Workspace tests run from the workspace/ directory; mcp_doctor is
# imported with the same `import mcp_doctor` shape as the rest of
# the runtime (per pyproject's package layout).
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import mcp_doctor  # noqa: E402


def test_module_exposes_six_checks():
    """The doctor's checklist is six items today. Pin the count so
    a future PR that drops a check (e.g. silently merges two) gets
    flagged in review.
    """
    assert len(mcp_doctor.CHECKS) == 6


def test_check_python_version_passes_on_311_plus():
    """Pin the floor at 3.11 (matches the wheel's requires_python)."""
    with mock.patch.object(sys, "version_info", (3, 11, 0, "final", 0)):
        assert mcp_doctor.check_python_version() == "ok"
    with mock.patch.object(sys, "version_info", (3, 12, 5, "final", 0)):
        assert mcp_doctor.check_python_version() == "ok"


def test_check_python_version_fails_on_310():
    """3.10 is below the wheel's >=3.11 floor — must FAIL, not WARN.
    pip silently filters the wheel out on 3.10 with `from versions:
    none`, which reads as "package missing" — operators have spent
    45min chasing that. The doctor's job is to call this out
    explicitly.
    """
    with mock.patch.object(sys, "version_info", (3, 10, 12, "final", 0)):
        assert mcp_doctor.check_python_version() == "fail"


def test_check_env_vars_fails_when_all_unset(monkeypatch):
    monkeypatch.delenv("PLATFORM_URL", raising=False)
    monkeypatch.delenv("WORKSPACE_ID", raising=False)
    monkeypatch.delenv("MOLECULE_WORKSPACES", raising=False)
    monkeypatch.delenv("MOLECULE_WORKSPACE_TOKEN", raising=False)
    monkeypatch.delenv("MOLECULE_WORKSPACE_TOKEN_FILE", raising=False)
    assert mcp_doctor.check_env_vars() == "fail"


def test_check_env_vars_passes_with_token_env(monkeypatch):
    monkeypatch.setenv("PLATFORM_URL", "https://x.moleculesai.app")
    monkeypatch.setenv("WORKSPACE_ID", "ws-test")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "tok-abc")
    monkeypatch.delenv("MOLECULE_WORKSPACE_TOKEN_FILE", raising=False)
    monkeypatch.delenv("MOLECULE_WORKSPACES", raising=False)
    assert mcp_doctor.check_env_vars() == "ok"


def test_check_env_vars_passes_with_token_file(monkeypatch, tmp_path):
    """Ryan #2934 item 3 fix: token from a file (or keychain shim)
    instead of inline env var so secrets stay out of shell history.
    The doctor must accept that path equally with the inline form.
    """
    token_path = tmp_path / "token"
    token_path.write_text("tok-from-file")
    monkeypatch.setenv("PLATFORM_URL", "https://x.moleculesai.app")
    monkeypatch.setenv("WORKSPACE_ID", "ws-test")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN_FILE", str(token_path))
    monkeypatch.delenv("MOLECULE_WORKSPACE_TOKEN", raising=False)
    monkeypatch.delenv("MOLECULE_WORKSPACES", raising=False)
    assert mcp_doctor.check_env_vars() == "ok"


def test_check_platform_health_warns_when_url_unset(monkeypatch):
    monkeypatch.delenv("PLATFORM_URL", raising=False)
    assert mcp_doctor.check_platform_health() == "warn"


def test_check_platform_health_fails_on_missing_scheme(monkeypatch):
    """A bare hostname is the second-most-common config error after
    missing-token (per the snippet's NOTE on Origin/PLATFORM_URL).
    The error message must say 'missing scheme' — not 'DNS error' —
    so the operator can diagnose without inspecting the URL string.
    """
    monkeypatch.setenv("PLATFORM_URL", "x.moleculesai.app")
    assert mcp_doctor.check_platform_health() == "fail"


def test_check_register_skipped_without_env(monkeypatch):
    monkeypatch.delenv("PLATFORM_URL", raising=False)
    monkeypatch.delenv("WORKSPACE_ID", raising=False)
    monkeypatch.delenv("MOLECULE_WORKSPACE_TOKEN", raising=False)
    # Skipped (warn), NOT failed — failing here would double-count
    # the env-vars failure noise.
    assert mcp_doctor.check_register() == "warn"


def test_check_token_auth_uses_heartbeat_endpoint(monkeypatch):
    """Pin: doctor MUST hit /registry/heartbeat, not /registry/register.

    register is an UPSERT — using it from doctor would clobber the
    workspace's actual agent_card metadata until the real agent next
    calls register. heartbeat only updates last_heartbeat_at, which
    a normal molecule-mcp boot does every 20s anyway, so the doctor's
    extra heartbeat is indistinguishable from background traffic.

    This test pins the URL via a urllib mock so a future refactor
    that accidentally re-routes through /registry/register fails
    here at PR-review time, not after operators report
    "doctor-probe" briefly appearing as their agent name in canvas.
    """
    monkeypatch.setenv("PLATFORM_URL", "https://x.moleculesai.app")
    monkeypatch.setenv("WORKSPACE_ID", "ws-test")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "tok-abc")
    monkeypatch.delenv("MOLECULE_WORKSPACE_TOKEN_FILE", raising=False)

    captured: dict[str, object] = {}

    class _FakeResp:
        status = 200
        def __enter__(self): return self
        def __exit__(self, *a): pass

    def fake_urlopen(req, timeout=None):
        captured["full_url"] = req.full_url
        captured["method"] = req.get_method()
        return _FakeResp()

    monkeypatch.setattr(mcp_doctor.urllib_request, "urlopen", fake_urlopen)
    verdict = mcp_doctor.check_token_auth()
    assert verdict == "ok"
    assert captured["method"] == "POST"
    # The load-bearing assertion — must use heartbeat, never register.
    assert captured["full_url"].endswith("/registry/heartbeat"), (
        f"doctor must use /registry/heartbeat (idempotent), not register "
        f"(UPSERT — clobbers agent_card). Got: {captured['full_url']}"
    )
    assert "/registry/register" not in str(captured["full_url"]), (
        "doctor must NEVER POST to /registry/register — that's a UPSERT "
        "that overwrites agent_card metadata until the real agent next "
        "calls register."
    )


def test_resolve_token_returns_value_and_label_for_env(monkeypatch):
    """The single resolver returns both the value (for Bearer header)
    and a non-secret label (for the env-vars summary). Drift between
    label and value is the previous bug shape."""
    monkeypatch.setenv("PLATFORM_URL", "https://x.moleculesai.app")
    monkeypatch.setenv("MOLECULE_WORKSPACE_TOKEN", "secret-tok-abc")
    monkeypatch.delenv("MOLECULE_WORKSPACE_TOKEN_FILE", raising=False)
    val, label = mcp_doctor._resolve_token()
    assert val == "secret-tok-abc"
    assert label == "env MOLECULE_WORKSPACE_TOKEN"
    # Summary helper must agree with the resolver's source.
    assert mcp_doctor._resolve_token_summary() == label


def test_resolve_token_returns_none_when_missing(monkeypatch, tmp_path):
    monkeypatch.delenv("MOLECULE_WORKSPACE_TOKEN", raising=False)
    monkeypatch.delenv("MOLECULE_WORKSPACE_TOKEN_FILE", raising=False)
    # The .auth_token file at /configs/.auth_token (present in container env)
    # must not pollute the test. Patch configs_dir.resolve() to return a
    # bare temp dir so the disk-file fallback in _resolve_token() has
    # nothing to find.
    import configs_dir
    monkeypatch.setattr(configs_dir, "resolve", lambda: tmp_path)
    val, label = mcp_doctor._resolve_token()
    assert val is None
    assert label is None


def test_run_returns_1_when_any_fail(monkeypatch, capsys):
    """End-to-end: stripped environment → at least one FAIL →
    exit 1. Pin the exit-code contract so this is scriptable from
    CI / install-checks too.
    """
    for k in (
        "PLATFORM_URL",
        "WORKSPACE_ID",
        "MOLECULE_WORKSPACES",
        "MOLECULE_WORKSPACE_TOKEN",
        "MOLECULE_WORKSPACE_TOKEN_FILE",
    ):
        monkeypatch.delenv(k, raising=False)
    code = mcp_doctor.run()
    out = capsys.readouterr().out
    assert code == 1
    # The summary line must mention at least one failure count so
    # an automated wrapper can grep for it.
    assert "check(s) failed" in out
    # And the human-facing label must be present so someone reading
    # CI logs sees what the section is about, not a wall of [FAIL].
    assert "molecule-mcp doctor" in out
