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


def test_required_env_missing_warns_does_not_fail(tmp_path, monkeypatch):
    """When a required_env var is missing, preflight WARNS but does not
    fail the boot. Pairs with PR #2756 (molecule-core): the workspace
    binds /.well-known/agent-card.json regardless of credentials and
    routes JSON-RPC to a -32603 'agent not configured' handler. Hard
    failing here would crash before the not-configured path even loads,
    leaving the workspace invisible — that's the failure mode that bit
    codex/openclaw bench 25335853189 on 2026-05-04 even after PR #2756."""
    monkeypatch.delenv("CLAUDE_CODE_OAUTH_TOKEN", raising=False)

    config = make_config(
        runtime="claude-code",
        runtime_config=RuntimeConfig(required_env=["CLAUDE_CODE_OAUTH_TOKEN"]),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert any(
        issue.title == "Required env" and "CLAUDE_CODE_OAUTH_TOKEN" in issue.detail
        for issue in report.warnings
    )
    assert not any(
        issue.title == "Required env" for issue in report.failures
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


def test_required_env_multiple_one_missing_warns(tmp_path, monkeypatch):
    """If any required_env var is missing, preflight warns with that var
    named (and does NOT fail). The eventual setup() failure is what
    actually surfaces to the user via the -32603 handler — preflight is
    just a logging signal for operators inspecting boot logs."""
    monkeypatch.setenv("API_KEY_A", "key-a")
    monkeypatch.delenv("API_KEY_B", raising=False)

    config = make_config(
        runtime_config=RuntimeConfig(required_env=["API_KEY_A", "API_KEY_B"]),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert any(
        issue.title == "Required env" and "API_KEY_B" in issue.detail
        for issue in report.warnings
    )


def test_required_env_empty_list_passes(tmp_path):
    """Empty required_env means no env checks — always passes."""
    config = make_config(
        runtime_config=RuntimeConfig(required_env=[]),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True


def test_required_env_skipped_in_smoke_mode(tmp_path, monkeypatch):
    """MOLECULE_SMOKE_MODE=1 demotes Required-env failures to warnings.

    Boot smoke (issue #2275) exercises executor.execute() against stub
    deps and never hits the real provider, so missing auth env is not
    a real blocker. Without this bypass, every adapter that introduces
    a new auth env var (HERMES_API_KEY, OPENROUTER_API_KEY, etc.)
    would silently break the publish-image gate until molecule-ci's
    fake-env list catches up — the 2026-05-03 hermes outage. The
    warning still surfaces in the report so unset env doesn't go
    completely silent.
    """
    monkeypatch.delenv("HERMES_API_KEY", raising=False)
    monkeypatch.setenv("MOLECULE_SMOKE_MODE", "1")

    config = make_config(
        runtime_config=RuntimeConfig(required_env=["HERMES_API_KEY"]),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert any(
        issue.title == "Required env" and "HERMES_API_KEY" in issue.detail
        for issue in report.warnings
    ), "smoke-mode bypass should still warn so unset env stays visible"
    assert not any(
        issue.title == "Required env" for issue in report.failures
    )


def test_required_env_smoke_mode_off_still_warns(tmp_path, monkeypatch):
    """Sanity: smoke bypass is OFF when MOLECULE_SMOKE_MODE is unset, but
    the warning still fires (and preflight no longer hard-fails — see
    test_required_env_missing_warns_does_not_fail for the rationale)."""
    monkeypatch.delenv("HERMES_API_KEY", raising=False)
    monkeypatch.delenv("MOLECULE_SMOKE_MODE", raising=False)

    config = make_config(
        runtime_config=RuntimeConfig(required_env=["HERMES_API_KEY"]),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert any(
        issue.title == "Required env" and "HERMES_API_KEY" in issue.detail
        for issue in report.warnings
    )
    assert not any(
        issue.title == "Required env" for issue in report.failures
    )


# ---------- Per-model required_env (models[] override) ----------


def test_per_model_required_env_wins_over_top_level(tmp_path, monkeypatch):
    """When `runtime_config.models[]` declares per-model `required_env` and
    the picked `model` matches an entry id, the entry's required_env wins
    over the top-level fallback. The 2026-05-02 MiniMax-on-claude-code bug:
    user picks MiniMax + sets MINIMAX_API_KEY, top-level demands
    CLAUDE_CODE_OAUTH_TOKEN — without this override path the workspace
    crash-loops on a stale top-level requirement."""
    monkeypatch.setenv("MINIMAX_API_KEY", "mx-test")
    monkeypatch.delenv("CLAUDE_CODE_OAUTH_TOKEN", raising=False)

    config = make_config(
        runtime="claude-code",
        runtime_config=RuntimeConfig(
            model="MiniMax-M2.7",
            required_env=["CLAUDE_CODE_OAUTH_TOKEN"],  # top-level fallback
            models=[
                {"id": "sonnet", "required_env": ["CLAUDE_CODE_OAUTH_TOKEN"]},
                {"id": "MiniMax-M2.7", "required_env": ["MINIMAX_API_KEY"]},
            ],
        ),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert not any(issue.title == "Required env" for issue in report.failures)


def test_top_level_required_env_used_when_no_models_declared(tmp_path, monkeypatch):
    """No `models[]` field → preserve the existing top-level behavior. This
    is the single-model template path — claude-code-default before it grew
    a Model dropdown, codex-default today, etc."""
    monkeypatch.delenv("CLAUDE_CODE_OAUTH_TOKEN", raising=False)

    config = make_config(
        runtime="claude-code",
        runtime_config=RuntimeConfig(
            model="sonnet",
            required_env=["CLAUDE_CODE_OAUTH_TOKEN"],
            models=[],
        ),
    )

    report = run_preflight(config, str(tmp_path))

    # Missing required_env is now a warning (workspace boots in
    # not-configured state); see test_required_env_missing_warns_does_not_fail.
    assert report.ok is True
    assert any(
        issue.title == "Required env" and "CLAUDE_CODE_OAUTH_TOKEN" in issue.detail
        for issue in report.warnings
    )


def test_top_level_used_when_picked_model_not_in_models_list(tmp_path, monkeypatch):
    """`models[]` declared but the picked `model` isn't listed → fall back
    to the top-level required_env. Defensive: protects against typos /
    template drift / a CP override that names a model the template doesn't
    enumerate. Never silently accept zero-auth in that case."""
    monkeypatch.delenv("CLAUDE_CODE_OAUTH_TOKEN", raising=False)

    config = make_config(
        runtime="claude-code",
        runtime_config=RuntimeConfig(
            model="some-unknown-model",
            required_env=["CLAUDE_CODE_OAUTH_TOKEN"],
            models=[
                {"id": "sonnet", "required_env": ["CLAUDE_CODE_OAUTH_TOKEN"]},
                {"id": "MiniMax-M2.7", "required_env": ["MINIMAX_API_KEY"]},
            ],
        ),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert any(
        issue.title == "Required env" and "CLAUDE_CODE_OAUTH_TOKEN" in issue.detail
        for issue in report.warnings
    )


def test_per_model_match_is_case_insensitive(tmp_path, monkeypatch):
    """Match `entry["id"]` against `runtime_config.model` case-insensitively
    — canvas surfaces `MiniMax-M2.7`, registries normalise to lowercase
    `minimax-m2.7`, MODEL_PROVIDER env may carry either. The match must
    not be brittle to that drift or templates ship preflight failures
    on a working auth setup."""
    monkeypatch.setenv("MINIMAX_API_KEY", "mx-test")
    monkeypatch.delenv("CLAUDE_CODE_OAUTH_TOKEN", raising=False)

    config = make_config(
        runtime="claude-code",
        runtime_config=RuntimeConfig(
            model="minimax-m2.7",  # lowercase
            required_env=["CLAUDE_CODE_OAUTH_TOKEN"],
            models=[
                {"id": "MiniMax-M2.7", "required_env": ["MINIMAX_API_KEY"]},  # mixed case
            ],
        ),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert not any(issue.title == "Required env" for issue in report.failures)


def test_per_model_match_with_no_required_env_key_falls_back_to_top_level(tmp_path, monkeypatch):
    """An entry that matches the picked model but has NO `required_env`
    key at all falls back to the top-level list. Distinct from the
    explicit-empty case below — many templates list a `name`/`description`
    per model without enumerating env vars when the auth is identical
    across the family, and we should not surprise them."""
    monkeypatch.setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-test")

    config = make_config(
        runtime="claude-code",
        runtime_config=RuntimeConfig(
            model="sonnet",
            required_env=["CLAUDE_CODE_OAUTH_TOKEN"],
            models=[
                {"id": "sonnet", "name": "Claude Sonnet"},  # no required_env key
            ],
        ),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert not any(issue.title == "Required env" for issue in report.failures)


def test_per_model_explicit_empty_required_env_means_no_auth(tmp_path, monkeypatch):
    """An entry with an explicit `required_env: []` means "this model
    needs no auth" — common for local Ollama, Llamafile, or self-hosted
    OpenAI-compat endpoints. This MUST short-circuit the top-level
    fallback or the template author can't express a zero-auth model
    without lying in the per-model list. Distinguished from the no-key
    case via `"required_env" in entry` (key presence, not truthiness)."""
    monkeypatch.delenv("CLAUDE_CODE_OAUTH_TOKEN", raising=False)

    config = make_config(
        runtime="claude-code",
        runtime_config=RuntimeConfig(
            model="local-llama",
            # Top-level requires an auth token — but the picked model is
            # a local one that genuinely needs none. Explicit-empty wins.
            required_env=["CLAUDE_CODE_OAUTH_TOKEN"],
            models=[
                {"id": "sonnet", "required_env": ["CLAUDE_CODE_OAUTH_TOKEN"]},
                {"id": "local-llama", "required_env": []},  # explicit zero-auth
            ],
        ),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert not any(issue.title == "Required env" for issue in report.failures)


def test_per_model_required_env_null_treated_as_empty_no_auth(tmp_path, monkeypatch):
    """YAML `required_env: null` deserializes to None — the parser falls
    through to `entry.get("required_env") or []`, so null behaves the
    same as explicit `[]` (zero-auth). Pins the parser tolerance —
    template authors who write `required_env:` without a value (common
    YAML mistake) get the no-auth path, not a confusing TypeError."""
    monkeypatch.delenv("CLAUDE_CODE_OAUTH_TOKEN", raising=False)

    config = make_config(
        runtime="claude-code",
        runtime_config=RuntimeConfig(
            model="local-llama",
            required_env=["CLAUDE_CODE_OAUTH_TOKEN"],
            models=[
                {"id": "local-llama", "required_env": None},  # null in YAML
            ],
        ),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert not any(issue.title == "Required env" for issue in report.failures)


# ---------- Legacy auth_token_file backward compat ----------


def test_legacy_auth_token_file_missing_no_env_warns(tmp_path, monkeypatch):
    """Legacy: missing auth_token_file with no env var emits a warning,
    not a hard failure. Same reasoning as
    test_required_env_missing_warns_does_not_fail — adapter.setup() is
    the authoritative auth check, preflight just surfaces the issue
    early in the boot log. The workspace still binds /agent-card and
    routes to the not-configured -32603 handler."""
    monkeypatch.delenv("CLAUDE_CODE_OAUTH_TOKEN", raising=False)

    config = make_config(
        runtime_config=RuntimeConfig(auth_token_file="secrets/token.txt"),
    )

    report = run_preflight(config, str(tmp_path))

    assert report.ok is True
    assert any(issue.title == "Auth token" for issue in report.warnings)
    assert not any(issue.title == "Auth token" for issue in report.failures)


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
