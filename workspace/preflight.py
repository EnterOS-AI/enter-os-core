"""Startup preflight checks for workspace runtime configs."""

import importlib
import os
from dataclasses import dataclass, field
from pathlib import Path

from config import WorkspaceConfig


def _validate_runtime_via_adapter(runtime: str) -> tuple[bool, str]:
    """Discover the installed adapter and confirm it matches the
    config's `runtime` field. Returns (ok, detail) — detail is the
    operator-actionable failure message when ok is False.

    Replaces the previous hardcoded SUPPORTED_RUNTIMES allowlist
    (claude-code / codex / ollama / langgraph / etc.). The static list
    couldn't keep up with new template repos: each new adapter required
    a code change in molecule-runtime to be 'supported', a violation of
    the universal-runtime principle (#87).

    Discovery uses the same ADAPTER_MODULE env var that production load
    paths consult (workspace/adapters/__init__.py:get_adapter). The
    adapter's static name() string is the source of truth — config.yaml
    just labels which one the operator expects, and the check warns on
    drift.

    Failure modes the function distinguishes (each gets a distinct
    operator-facing message so debugging is concrete):
      - ADAPTER_MODULE unset → "no adapter installed"
      - ADAPTER_MODULE set but module won't import → "import failed: …"
      - module imports but no Adapter class → "Adapter class missing"
      - Adapter.name() differs from config.runtime → drift warning
    """
    adapter_module = os.environ.get("ADAPTER_MODULE", "").strip()
    if not adapter_module:
        return False, (
            "ADAPTER_MODULE env var is unset — no adapter installed in this "
            f"image. Workspace declares runtime='{runtime}' but the runtime "
            "discovery path can't find any. In a template image this is set "
            "in the Dockerfile (ENV ADAPTER_MODULE=adapter); in dev, set it "
            "to your local adapter module name."
        )
    try:
        mod = importlib.import_module(adapter_module)
    except Exception as exc:
        return False, (
            f"ADAPTER_MODULE={adapter_module!r} is not importable: "
            f"{type(exc).__name__}: {exc}. Check the module path + that its "
            "dependencies installed cleanly."
        )
    adapter_cls = getattr(mod, "Adapter", None)
    if adapter_cls is None:
        return False, (
            f"ADAPTER_MODULE={adapter_module!r} imported, but no `Adapter` "
            "class is exported. Add `Adapter = YourAdapterClass` at module "
            "scope (convention from BaseAdapter docstring)."
        )
    try:
        adapter_name = adapter_cls.name()
    except Exception as exc:
        return False, (
            f"Adapter.name() raised {type(exc).__name__}: {exc}. The static "
            "name() classmethod must return the runtime identifier without "
            "side effects."
        )
    if not isinstance(adapter_name, str) or not adapter_name:
        return False, "Adapter.name() must return a non-empty string."
    if adapter_name != runtime:
        # Drift between config.yaml and the installed adapter is unusual
        # but not fatal — the adapter wins (it's what actually runs).
        # Operator-facing detail names both so they can fix whichever is
        # stale.
        return True, (
            f"Drift: config.yaml runtime={runtime!r} but installed Adapter "
            f"reports name={adapter_name!r}. The adapter wins; update "
            "config.yaml to match if the drift is unintended."
        )
    return True, ""


@dataclass
class PreflightIssue:
    severity: str
    title: str
    detail: str
    fix: str = ""


@dataclass
class PreflightReport:
    warnings: list[PreflightIssue] = field(default_factory=list)
    failures: list[PreflightIssue] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.failures


def run_preflight(config: WorkspaceConfig, config_path: str) -> PreflightReport:
    """Check the workspace config for obvious startup blockers."""
    report = PreflightReport()
    config_dir = Path(config_path)

    runtime_ok, runtime_detail = _validate_runtime_via_adapter(config.runtime)
    if not runtime_ok:
        report.failures.append(
            PreflightIssue(
                severity="fail",
                title="Runtime",
                detail=runtime_detail,
                fix=(
                    "Install the matching adapter (template repo's Dockerfile "
                    "should set ADAPTER_MODULE) or correct the runtime field in "
                    "config.yaml."
                ),
            )
        )
    elif runtime_detail:
        # ok=True with a detail = drift warning, not a failure.
        report.warnings.append(
            PreflightIssue(
                severity="warn",
                title="Runtime",
                detail=runtime_detail,
                fix="Update config.yaml runtime to match the installed Adapter.name().",
            )
        )

    if not 1 <= int(config.a2a.port) <= 65535:
        report.failures.append(
            PreflightIssue(
                severity="fail",
                title="A2A port",
                detail=f"Invalid A2A port: {config.a2a.port}",
                fix="Set a2a.port to a value between 1 and 65535.",
            )
        )

    # Check required environment variables (e.g. CLAUDE_CODE_OAUTH_TOKEN, OPENAI_API_KEY).
    # These are declared per-runtime in config.yaml and injected via the secrets API.
    required_env = getattr(config.runtime_config, "required_env", []) or []

    # Per-model override path. When the template's runtime_config declares
    # `models[]` (canvas Model dropdown), prefer the picked model's own
    # `required_env` over the top-level fallback. The picked model is
    # `runtime_config.model` (which already honors the MODEL_PROVIDER env
    # override at parse time — see config.py:RuntimeConfig.model resolution).
    # Match on `entry["id"]` case-insensitively because canvas-side ids
    # ("MiniMax-M2.7") and adapter-side normalization ("minimax-m2.7") drift
    # by case across registries.
    #
    # Bug surfaced 2026-05-02: claude-code-default top-level required_env
    # demands CLAUDE_CODE_OAUTH_TOKEN, but the user picked MiniMax and only
    # set MINIMAX_API_KEY. Without this lookup, preflight failed and the
    # workspace crash-looped despite the user having satisfied the picked
    # model's actual auth requirement.
    models = getattr(config.runtime_config, "models", None) or []
    picked_model = (getattr(config.runtime_config, "model", "") or "").strip()
    if models and picked_model:
        picked_lower = picked_model.lower()
        for entry in models:
            if not isinstance(entry, dict):
                continue
            entry_id = str(entry.get("id", "")).strip()
            if not entry_id:
                continue
            if entry_id.lower() != picked_lower:
                continue
            if "required_env" in entry:
                # Per-model required_env wins outright — do NOT union with the
                # top-level list. Templates use per-model entries precisely
                # to express that different models have *different* auth
                # paths (OAuth token vs API key vs third-party provider key);
                # unioning would re-introduce the very crash-loop this fix
                # closes. An explicit empty list means "no auth needed"
                # (e.g. local Ollama or self-hosted endpoints) and MUST
                # short-circuit the top-level fallback — that's why we key
                # off `"required_env" in entry` rather than truthiness.
                required_env = list(entry.get("required_env") or [])
            break

    # Smoke mode skips the auth-env block: the boot smoke (CI publish-image,
    # issue #2275) exercises executor.execute() against stub deps, never
    # hits the real provider, and CI cannot enumerate every adapter's auth
    # env without forming a maintenance treadmill. Hermes 2026-05-03 outage:
    # template smoke crashed for two cycles because molecule-ci injected
    # CLAUDE_CODE_OAUTH_TOKEN/ANTHROPIC_API_KEY/etc. but not HERMES_API_KEY.
    # Bypass here means new templates can ship without the workflow
    # learning their env names.
    smoke_mode = os.environ.get("MOLECULE_SMOKE_MODE", "").strip().lower() in (
        "1", "true", "yes", "on",
    )
    for env_var in required_env:
        if os.environ.get(env_var):
            continue
        if smoke_mode:
            report.warnings.append(
                PreflightIssue(
                    severity="warn",
                    title="Required env",
                    detail=f"Missing {env_var} (skipped — MOLECULE_SMOKE_MODE)",
                    fix="",
                )
            )
            continue
        report.failures.append(
            PreflightIssue(
                severity="fail",
                title="Required env",
                detail=f"Missing required environment variable: {env_var}",
                fix=f"Set {env_var} via the secrets API (global or workspace-level).",
            )
        )

    # Backward compat: if legacy auth_token_file is set, warn but don't block
    # if the token is available via required_env or auth_token_env.
    token_file = getattr(config.runtime_config, "auth_token_file", "")
    if token_file:
        token_path = config_dir / token_file
        if not token_path.exists():
            token_env = getattr(config.runtime_config, "auth_token_env", "")
            env_has_token = bool(token_env and os.environ.get(token_env))
            # Also check if any required_env is set (covers the new path)
            if not env_has_token and required_env:
                env_has_token = all(os.environ.get(e) for e in required_env)

            if not env_has_token:
                report.failures.append(
                    PreflightIssue(
                        severity="fail",
                        title="Auth token",
                        detail=f"Missing auth token file: {token_file}",
                        fix="Remove auth_token_file and use required_env + secrets API instead.",
                    )
                )

    prompt_files = config.prompt_files or ["system-prompt.md"]
    for prompt_file in prompt_files:
        prompt_path = config_dir / prompt_file
        if not prompt_path.exists():
            report.warnings.append(
                PreflightIssue(
                    severity="warn",
                    title="Prompt file",
                    detail=f"Missing prompt file: {prompt_file}",
                    fix="Add the file or remove it from prompt_files.",
                )
            )

    skills_dir = config_dir / "skills"
    for skill_name in config.skills:
        skill_path = skills_dir / skill_name / "SKILL.md"
        if not skill_path.exists():
            report.warnings.append(
                PreflightIssue(
                    severity="warn",
                    title="Skill",
                    detail=f"Missing skill package: {skill_name}",
                    fix="Restore the skill folder or remove it from config.yaml.",
                )
            )

    return report


def render_preflight_report(report: PreflightReport) -> None:
    """Print a concise startup report."""
    if not report.warnings and not report.failures:
        return

    print("Preflight checks:")
    for issue in report.failures:
        print(f"[FAIL] {issue.title}: {issue.detail}")
        if issue.fix:
            print(f"  Fix: {issue.fix}")
    for issue in report.warnings:
        print(f"[WARN] {issue.title}: {issue.detail}")
        if issue.fix:
            print(f"  Fix: {issue.fix}")
