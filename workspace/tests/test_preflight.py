"""Tests for preflight.py — workspace startup checks."""
import sys
import types

import pytest

from config import A2AConfig, RuntimeConfig, WorkspaceConfig
from preflight import run_preflight, render_preflight_report, PreflightIssue, PreflightReport


def make_config(**overrides):
    """Build a minimal workspace config for preflight tests."""
    base = WorkspaceConfig(
        name="Test Workspace",
        runtime="langgraph",
        runtime_config=RuntimeConfig(),
        skills=[],
        prompt_files=[],
        a2a=A2AConfig(port=8000),
    )
    for key, value in overrides.items():
        setattr(base, key, value)
    return base


_UNSET = object()


def install_fake_adapter(monkeypatch, name: str = "langgraph", *, raise_on_name: bool = False, no_class: bool = False, name_returns=_UNSET):
    """Install a fake adapter module + ADAPTER_MODULE env var so the
    runtime-discovery path in preflight finds it.

    Args:
      name: what Adapter.name() returns (default "langgraph" so the
            base config's runtime field passes the equality check).
      raise_on_name: if True, Adapter.name() raises (tests the catch path).
      no_class: if True, the module imports but exports no Adapter symbol.
      name_returns: override the literal value name() returns. Defaults
                    to a sentinel so that None is a passable test value
                    (else `if name_returns is not None` would skip the
                    None branch — exactly the bug this sentinel avoids).
    """
    # Each call uses a unique module name so monkeypatch's sys.modules
    # restoration doesn't accidentally reuse a prior test's fake when
    # the same `name` is requested twice in one test session.
    module_name = f"_fake_adapter_{name.replace('-', '_')}_{id(monkeypatch)}"
    fake_mod = types.ModuleType(module_name)

    if not no_class:
        if raise_on_name:
            class _Adapter:
                @staticmethod
                def name():
                    raise RuntimeError("boom")
        elif name_returns is not _UNSET:
            class _Adapter:
                @staticmethod
                def name():
                    return name_returns
        else:
            class _Adapter:
                @staticmethod
                def name():
                    return name
        fake_mod.Adapter = _Adapter

    monkeypatch.setitem(sys.modules, module_name, fake_mod)
    monkeypatch.setenv("ADAPTER_MODULE", module_name)


@pytest.fixture(autouse=True)
def _default_langgraph_adapter(monkeypatch, request):
    """Pre-install a langgraph adapter so existing tests that build a
    default WorkspaceConfig (runtime="langgraph") pass the discovery
    check without each test having to set ADAPTER_MODULE manually.

    Tests that need to assert a specific failure mode (no adapter, drift,
    missing class, etc.) opt out via the `no_default_adapter` marker:

        @pytest.mark.no_default_adapter
        def test_…(monkeypatch):
            ...
    """
    if "no_default_adapter" in request.keywords:
        return
    install_fake_adapter(monkeypatch, name="langgraph")


def test_run_preflight_with_matching_adapter_passes(tmp_path):
    """When ADAPTER_MODULE points to a module whose Adapter.name()
    matches config.runtime, preflight passes cleanly. Default fixture
    installs a langgraph adapter; the base config also says langgraph."""
    (tmp_path / "system-prompt.md").write_text("Base prompt.")
    (tmp_path / "skills").mkdir()

    config = make_config(prompt_files=["system-prompt.md"], skills=[])
    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert report.failures == []
    assert report.warnings == []


def test_run_preflight_unsupported_runtime_warns_about_drift(tmp_path):
    """When the runtime requested is not what the installed adapter
    reports, preflight returns the drift warning (not failure) — the
    adapter wins in production. The PRIOR static-list behavior would
    have hard-failed here, but the discovery-based check trusts the
    adapter and surfaces the mismatch as actionable info."""
    (tmp_path / "system-prompt.md").write_text("Base prompt.")
    # Default fixture installs Adapter.name() == "langgraph"; flip the
    # config to a different name so the drift warning fires.
    config = make_config(runtime="not-a-runtime", prompt_files=["system-prompt.md"])

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True  # drift, not fatal
    assert any(issue.title == "Runtime" and "Drift" in issue.detail for issue in report.warnings)


@pytest.mark.no_default_adapter
def test_run_preflight_no_adapter_module_fails(tmp_path, monkeypatch):
    """ADAPTER_MODULE unset → no adapter installed → preflight fails
    with an operator-actionable message naming the env var."""
    monkeypatch.delenv("ADAPTER_MODULE", raising=False)
    (tmp_path / "system-prompt.md").write_text("Base prompt.")
    config = make_config(prompt_files=["system-prompt.md"])

    report = run_preflight(config, str(tmp_path))

    assert report.ok is False
    runtime_failures = [i for i in report.failures if i.title == "Runtime"]
    assert len(runtime_failures) == 1
    assert "ADAPTER_MODULE" in runtime_failures[0].detail
    assert "unset" in runtime_failures[0].detail


@pytest.mark.no_default_adapter
def test_run_preflight_adapter_module_unimportable_fails(tmp_path, monkeypatch):
    """ADAPTER_MODULE set to a non-existent module → import error →
    preflight fails with the underlying exception type + message."""
    monkeypatch.setenv("ADAPTER_MODULE", "this_module_does_not_exist_for_test")
    (tmp_path / "system-prompt.md").write_text("Base prompt.")
    config = make_config(prompt_files=["system-prompt.md"])

    report = run_preflight(config, str(tmp_path))

    assert report.ok is False
    assert any(
        i.title == "Runtime" and "not importable" in i.detail
        for i in report.failures
    )


@pytest.mark.no_default_adapter
def test_run_preflight_adapter_module_missing_class_fails(tmp_path, monkeypatch):
    """Module imports but doesn't export `Adapter` → fail with the
    convention reminder. Pin the convention so a future refactor
    that renames the class doesn't silently bypass discovery."""
    install_fake_adapter(monkeypatch, name="langgraph", no_class=True)
    (tmp_path / "system-prompt.md").write_text("Base prompt.")
    config = make_config(prompt_files=["system-prompt.md"])

    report = run_preflight(config, str(tmp_path))

    assert report.ok is False
    assert any(
        i.title == "Runtime" and "no `Adapter` class" in i.detail
        for i in report.failures
    )


@pytest.mark.no_default_adapter
def test_run_preflight_adapter_name_raises_fails(tmp_path, monkeypatch):
    """Adapter.name() throwing must be caught — the static method
    must be side-effect-free per BaseAdapter contract."""
    install_fake_adapter(monkeypatch, raise_on_name=True)
    (tmp_path / "system-prompt.md").write_text("Base prompt.")
    config = make_config(prompt_files=["system-prompt.md"])

    report = run_preflight(config, str(tmp_path))

    assert report.ok is False
    assert any(
        i.title == "Runtime" and "name() raised" in i.detail
        for i in report.failures
    )


@pytest.mark.no_default_adapter
def test_run_preflight_adapter_name_non_string_fails(tmp_path, monkeypatch):
    """Adapter.name() returning None / int / etc. must fail — the
    runtime identifier is a string by contract and downstream code
    assumes that (config matching, log lines, etc.). Use 42 (int) as
    the returned value so the assertion is unambiguous; None would
    also work but int is more obviously a contract violation."""
    install_fake_adapter(monkeypatch, name_returns=42)
    (tmp_path / "system-prompt.md").write_text("Base prompt.")
    config = make_config(prompt_files=["system-prompt.md"])

    report = run_preflight(config, str(tmp_path))

    assert report.ok is False
    assert any(
        i.title == "Runtime" and "non-empty string" in i.detail
        for i in report.failures
    )


# ---------- required_env checks ----------


def test_required_env_present_passes(tmp_path, monkeypatch):
    """When all required_env vars are set, preflight passes."""
    monkeypatch.setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-test")

    config = make_config(
        runtime="claude-code",
        runtime_config=RuntimeConfig(required_env=["CLAUDE_CODE_OAUTH_TOKEN"]),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert not any(issue.title == "Required env" for issue in report.failures)


def test_required_env_missing_fails(tmp_path, monkeypatch):
    """When a required_env var is missing, preflight fails."""
    monkeypatch.delenv("CLAUDE_CODE_OAUTH_TOKEN", raising=False)

    config = make_config(
        runtime="claude-code",
        runtime_config=RuntimeConfig(required_env=["CLAUDE_CODE_OAUTH_TOKEN"]),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is False
    assert any(
        issue.title == "Required env" and "CLAUDE_CODE_OAUTH_TOKEN" in issue.detail
        for issue in report.failures
    )


def test_required_env_multiple_all_present_passes(tmp_path, monkeypatch):
    """Multiple required_env vars all present should pass."""
    monkeypatch.setenv("API_KEY_A", "key-a")
    monkeypatch.setenv("API_KEY_B", "key-b")

    config = make_config(
        runtime_config=RuntimeConfig(required_env=["API_KEY_A", "API_KEY_B"]),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True


def test_required_env_multiple_one_missing_fails(tmp_path, monkeypatch):
    """If any required_env var is missing, preflight fails with that var named."""
    monkeypatch.setenv("API_KEY_A", "key-a")
    monkeypatch.delenv("API_KEY_B", raising=False)

    config = make_config(
        runtime_config=RuntimeConfig(required_env=["API_KEY_A", "API_KEY_B"]),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is False
    assert any(
        issue.title == "Required env" and "API_KEY_B" in issue.detail
        for issue in report.failures
    )


def test_required_env_empty_list_passes(tmp_path):
    """Empty required_env means no env checks — always passes."""
    config = make_config(
        runtime_config=RuntimeConfig(required_env=[]),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True


# ---------- Legacy auth_token_file backward compat ----------


def test_legacy_auth_token_file_missing_no_env_fails(tmp_path, monkeypatch):
    """Legacy: missing auth_token_file with no env var should fail."""
    monkeypatch.delenv("CLAUDE_CODE_OAUTH_TOKEN", raising=False)

    config = make_config(
        runtime_config=RuntimeConfig(auth_token_file="secrets/token.txt"),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is False
    assert any(issue.title == "Auth token" for issue in report.failures)


def test_legacy_auth_token_file_missing_but_auth_token_env_passes(tmp_path, monkeypatch):
    """Legacy: missing file but auth_token_env set should pass."""
    monkeypatch.setenv("MY_AUTH_TOKEN", "fake-token")

    config = make_config(
        runtime_config=RuntimeConfig(
            auth_token_file="secrets/token.txt",
            auth_token_env="MY_AUTH_TOKEN",
        ),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True


def test_legacy_auth_token_file_missing_but_required_env_passes(tmp_path, monkeypatch):
    """Legacy: missing file but required_env satisfied should pass."""
    monkeypatch.setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-test")

    config = make_config(
        runtime="claude-code",
        runtime_config=RuntimeConfig(
            auth_token_file=".auth-token",
            required_env=["CLAUDE_CODE_OAUTH_TOKEN"],
        ),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True


def test_legacy_auth_token_file_exists_passes(tmp_path):
    """Legacy: when the file exists, it passes with no auth warnings."""
    (tmp_path / ".auth-token").write_text("sk-from-file")
    (tmp_path / "system-prompt.md").write_text("prompt")

    config = make_config(
        runtime_config=RuntimeConfig(auth_token_file=".auth-token"),
        prompt_files=["system-prompt.md"],
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert not any(issue.title == "Auth token" for issue in report.warnings)
    assert report.failures == []


# ---------- Other checks ----------


def test_run_preflight_missing_prompts_and_skills_warn(tmp_path):
    """Missing prompt files and skills should warn, not fail."""
    config = make_config(
        prompt_files=["missing-prompt.md"],
        skills=["missing-skill"],
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert report.failures == []
    assert any(issue.title == "Prompt file" for issue in report.warnings)
    assert any(issue.title == "Skill" for issue in report.warnings)


def test_run_preflight_valid_config_passes(tmp_path):
    """A fully populated config should pass with no issues."""
    (tmp_path / "system-prompt.md").write_text("Base prompt.")
    skill_dir = tmp_path / "skills" / "writing"
    skill_dir.mkdir(parents=True)
    (skill_dir / "SKILL.md").write_text("Write clearly.")

    config = make_config(
        prompt_files=["system-prompt.md"],
        skills=["writing"],
        runtime_config=RuntimeConfig(),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert report.failures == []
    assert report.warnings == []


def test_run_preflight_invalid_port_fails(tmp_path):
    """A port value of 0 is out of range and should trigger a failure."""
    config = make_config(
        a2a=A2AConfig(port=0),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is False
    assert any(issue.title == "A2A port" for issue in report.failures)


def test_render_preflight_report_with_failures(capsys):
    """render_preflight_report prints [FAIL] lines with fix hints."""
    report = PreflightReport(
        failures=[
            PreflightIssue(
                severity="fail",
                title="Runtime",
                detail="Unsupported runtime 'bogus'",
                fix="Choose a supported runtime.",
            )
        ],
        warnings=[],
    )

    render_preflight_report(report)

    captured = capsys.readouterr()
    assert "Preflight checks:" in captured.out
    assert "[FAIL] Runtime: Unsupported runtime 'bogus'" in captured.out
    assert "Fix: Choose a supported runtime." in captured.out


def test_render_preflight_report_with_warnings(capsys):
    """render_preflight_report prints [WARN] lines with fix hints."""
    report = PreflightReport(
        failures=[],
        warnings=[
            PreflightIssue(
                severity="warn",
                title="Prompt file",
                detail="Missing prompt file: missing.md",
                fix="Add the file or remove it from prompt_files.",
            )
        ],
    )

    render_preflight_report(report)

    captured = capsys.readouterr()
    assert "Preflight checks:" in captured.out
    assert "[WARN] Prompt file: Missing prompt file: missing.md" in captured.out
    assert "Fix: Add the file or remove it from prompt_files." in captured.out


def test_render_preflight_report_no_output_when_clean(capsys):
    """render_preflight_report prints nothing when there are no issues."""
    report = PreflightReport(failures=[], warnings=[])

    render_preflight_report(report)

    captured = capsys.readouterr()
    assert captured.out == ""
