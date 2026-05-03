"""Tests for config.py — workspace configuration loading."""

import os

import pytest
import yaml

from config import (
    A2AConfig,
    ComplianceConfig,
    DelegationConfig,
    ObservabilityConfig,
    SandboxConfig,
    WorkspaceConfig,
    load_config,
)


def test_load_config_basic(tmp_path):
    """load_config reads a YAML file and returns a WorkspaceConfig."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(
        yaml.dump(
            {
                "name": "Test Agent",
                "description": "A test workspace",
                "version": "2.0.0",
                "tier": 3,
                "model": "openai:gpt-4o",
                "skills": ["seo", "writing"],
                "tools": ["delegation", "sandbox"],
                "prompt_files": ["SOUL.md", "TOOLS.md"],
            }
        )
    )

    cfg = load_config(str(tmp_path))
    assert cfg.name == "Test Agent"
    assert cfg.description == "A test workspace"
    assert cfg.version == "2.0.0"
    assert cfg.tier == 3
    assert cfg.model == "openai:gpt-4o"
    assert cfg.skills == ["seo", "writing"]
    assert cfg.tools == ["delegation", "sandbox"]
    assert cfg.prompt_files == ["SOUL.md", "TOOLS.md"]


def test_load_config_defaults(tmp_path):
    """Missing fields fall back to WorkspaceConfig defaults."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({}))

    cfg = load_config(str(tmp_path))
    assert cfg.name == "Workspace"
    assert cfg.description == ""
    assert cfg.version == "1.0.0"
    assert cfg.tier == 1
    assert cfg.model == "anthropic:claude-opus-4-7"
    assert cfg.skills == []
    assert cfg.tools == []
    assert cfg.prompt_files == []
    assert cfg.sub_workspaces == []


def test_load_config_model_env_override(tmp_path, monkeypatch):
    """MODEL_PROVIDER env var overrides the model from YAML."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({"model": "openai:gpt-4o"}))

    monkeypatch.setenv("MODEL_PROVIDER", "google:gemini-2.0-flash")
    cfg = load_config(str(tmp_path))
    assert cfg.model == "google:gemini-2.0-flash"


def test_load_config_model_no_env(tmp_path, monkeypatch):
    """Without MODEL_PROVIDER, model comes from YAML."""
    monkeypatch.delenv("MODEL_PROVIDER", raising=False)
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({"model": "openai:gpt-4o"}))

    cfg = load_config(str(tmp_path))
    assert cfg.model == "openai:gpt-4o"


def test_runtime_config_model_falls_back_to_top_level(tmp_path, monkeypatch):
    """When YAML omits runtime_config.model, fall back to the top-level
    resolved model.

    Without this fallback, SaaS workspaces silently boot with the
    adapter's hard-coded default — claude-code-default reads
    ``runtime_config.model or "sonnet"``, so even a user who picks Opus
    in the canvas Config tab gets Sonnet on the next restart. Root
    cause: the CP user-data script regenerates /configs/config.yaml
    at every boot with only ``name``, ``runtime``, ``a2a`` keys
    (intentionally minimal so it doesn't carry stale state), losing
    runtime_config.model. MODEL_PROVIDER is plumbed as an env var, so
    picking it up via the top-level resolved ``model`` keeps the
    selection sticky across restarts.
    """
    monkeypatch.delenv("MODEL_PROVIDER", raising=False)
    config_yaml = tmp_path / "config.yaml"
    # Top-level model set, runtime_config.model NOT set — exactly the
    # shape the CP user-data writes after restart.
    config_yaml.write_text(yaml.dump({"model": "anthropic:claude-opus-4-7"}))

    cfg = load_config(str(tmp_path))
    assert cfg.runtime_config.model == "anthropic:claude-opus-4-7"


def test_runtime_config_model_yaml_wins_over_top_level(tmp_path, monkeypatch):
    """When YAML explicitly sets runtime_config.model, it takes precedence
    over the top-level model. Tests the fallback is only a fallback —
    not a clobber that would break workspaces with intentionally
    different runtime_config.model vs top-level model values.
    """
    monkeypatch.delenv("MODEL_PROVIDER", raising=False)
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(
        yaml.dump(
            {
                "model": "anthropic:claude-opus-4-7",
                "runtime_config": {"model": "openai:gpt-4o"},
            }
        )
    )

    cfg = load_config(str(tmp_path))
    # Top-level still resolves to its own value.
    assert cfg.model == "anthropic:claude-opus-4-7"
    # runtime_config.model wins — fallback only fires when YAML is empty.
    assert cfg.runtime_config.model == "openai:gpt-4o"


def test_runtime_config_model_env_wins_over_explicit_yaml(tmp_path, monkeypatch):
    """When BOTH MODEL_PROVIDER env AND runtime_config.model in YAML are set,
    MODEL_PROVIDER wins. Pins the intentional precedence inversion shipped
    in PR #2538 (2026-05-02): the canvas-picked model is the source of
    truth, not the template's verbatim default. A self-hosted operator who
    wants the YAML value to win MUST also unset MODEL_PROVIDER — the env
    var is the operator's "current intent" signal, the YAML is a baked-in
    default.

    Without this pin, a future refactor could quietly restore the old
    YAML-wins order and re-introduce Bug B (canvas-picked model silently
    dropped for templated workspaces)."""
    monkeypatch.setenv("MODEL_PROVIDER", "minimax/MiniMax-M2.7")
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(
        yaml.dump(
            {
                "model": "anthropic:claude-opus-4-7",
                "runtime_config": {"model": "openai:gpt-4o"},
            }
        )
    )

    cfg = load_config(str(tmp_path))
    # Top-level still resolves to MODEL_PROVIDER (existing behavior).
    assert cfg.model == "minimax/MiniMax-M2.7"
    # And runtime_config.model now ALSO follows MODEL_PROVIDER, even
    # though YAML had an explicit different value. This is the
    # intentional inversion — the canvas pick beats the template.
    assert cfg.runtime_config.model == "minimax/MiniMax-M2.7"


def test_runtime_config_model_picks_up_env_via_top_level(tmp_path, monkeypatch):
    """End-to-end path the canvas Save+Restart relies on: user picks
    a model → workspace_secrets.MODEL_PROVIDER updated → CP user-data
    re-renders /configs/config.yaml WITHOUT runtime_config.model →
    workspace boots with MODEL_PROVIDER env var. The top-level model
    resolves from MODEL_PROVIDER (line 277), then runtime_config.model
    falls back to that. Adapter sees the user's selection.

    This is the regression test for the canvas-side feedback
    "Provisioner doesn't read model from config.yaml and doesn't set
    MODEL env var. Without MODEL, the adapter defaults to sonnet and
    bypasses the mimo routing." (2026-04-30).
    """
    monkeypatch.setenv("MODEL_PROVIDER", "minimax/abab7-chat-preview")
    config_yaml = tmp_path / "config.yaml"
    # CP-shaped minimal config.yaml: only name + runtime + a2a, NO
    # top-level model, NO runtime_config.model.
    config_yaml.write_text(
        yaml.dump(
            {
                "name": "Test Agent",
                "runtime": "claude-code",
                "a2a": {"port": 8000, "streaming": True},
            }
        )
    )

    cfg = load_config(str(tmp_path))
    assert cfg.model == "minimax/abab7-chat-preview"
    # The adapter (claude-code-default reads runtime_config.model or "sonnet")
    # now sees the user's selected model instead of "sonnet".
    assert cfg.runtime_config.model == "minimax/abab7-chat-preview"


# ===== Provider field (Option B — explicit `provider:` alongside `model:`) =====
#
# Why a separate `provider` field at all (we already parse the slug prefix off
# `model`)? Three reasons:
#   1. Custom model aliases that don't carry a recognizable prefix (e.g., a
#      tenant-specific name routed through a gateway) need an explicit signal.
#   2. Adapters were each implementing their own slug-parse — hermes's
#      derive-provider.sh, claude-code's adapter-default branch, etc. One
#      resolution point in load_config kills that drift class.
#   3. The canvas Provider dropdown needs a stable storage field that doesn't
#      get clobbered every time the user picks a new model.
#
# Backward compat: when `provider:` is absent, fall back to slug derivation,
# so existing config.yaml files keep working without a migration.


def test_provider_default_empty_when_bare_model(tmp_path, monkeypatch):
    """Bare model names (no `:` or `/` separator) yield an empty provider —
    the signal for "let the adapter decide". Don't guess.
    """
    monkeypatch.delenv("LLM_PROVIDER", raising=False)
    monkeypatch.delenv("MODEL_PROVIDER", raising=False)
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({"model": "claude-opus-4-7"}))

    cfg = load_config(str(tmp_path))
    assert cfg.provider == ""
    assert cfg.runtime_config.provider == ""


def test_provider_derived_from_colon_slug(tmp_path, monkeypatch):
    """`provider:model` shape (Anthropic/OpenAI/Google convention) derives
    the provider from the prefix when no explicit `provider:` is set.
    Exercises the backward-compat path for every existing config.yaml in
    the wild.
    """
    monkeypatch.delenv("LLM_PROVIDER", raising=False)
    monkeypatch.delenv("MODEL_PROVIDER", raising=False)
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({"model": "anthropic:claude-opus-4-7"}))

    cfg = load_config(str(tmp_path))
    assert cfg.provider == "anthropic"
    # runtime_config.provider inherits the same way runtime_config.model does.
    assert cfg.runtime_config.provider == "anthropic"


def test_provider_derived_from_slash_slug(tmp_path, monkeypatch):
    """`provider/model` shape (HuggingFace/Minimax convention) derives the
    provider from the prefix when no explicit `provider:` is set.
    """
    monkeypatch.delenv("LLM_PROVIDER", raising=False)
    monkeypatch.delenv("MODEL_PROVIDER", raising=False)
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({"model": "minimax/abab7-chat-preview"}))

    cfg = load_config(str(tmp_path))
    assert cfg.provider == "minimax"
    assert cfg.runtime_config.provider == "minimax"


def test_provider_yaml_explicit_wins_over_derived(tmp_path, monkeypatch):
    """Explicit YAML `provider:` overrides the slug-prefix derivation —
    needed when the model name's prefix doesn't match the actual gateway
    (e.g., an `anthropic:claude-opus-4-7` model routed through a custom
    gateway slug).
    """
    monkeypatch.delenv("LLM_PROVIDER", raising=False)
    monkeypatch.delenv("MODEL_PROVIDER", raising=False)
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(
        yaml.dump(
            {
                "model": "anthropic:claude-opus-4-7",
                "provider": "custom-gateway",
            }
        )
    )

    cfg = load_config(str(tmp_path))
    # Slug prefix says "anthropic" but the explicit field wins.
    assert cfg.provider == "custom-gateway"
    assert cfg.runtime_config.provider == "custom-gateway"


def test_provider_env_override_beats_yaml_and_derived(tmp_path, monkeypatch):
    """`LLM_PROVIDER` env var beats both YAML and slug derivation.
    This is the path the canvas Save+Restart cycle relies on: the user
    picks a provider in the canvas Provider dropdown, the platform sets
    `LLM_PROVIDER` on the workspace, and the next CP-driven restart picks
    it up regardless of what's in the regenerated /configs/config.yaml.
    """
    monkeypatch.setenv("LLM_PROVIDER", "minimax")
    monkeypatch.delenv("MODEL_PROVIDER", raising=False)
    config_yaml = tmp_path / "config.yaml"
    # YAML says one thing, slug says another, env wins.
    config_yaml.write_text(
        yaml.dump(
            {
                "model": "anthropic:claude-opus-4-7",
                "provider": "openai",
            }
        )
    )

    cfg = load_config(str(tmp_path))
    assert cfg.provider == "minimax"
    assert cfg.runtime_config.provider == "minimax"


def test_runtime_config_provider_yaml_wins_over_top_level(tmp_path, monkeypatch):
    """An explicit `runtime_config.provider` takes precedence over the
    top-level resolved provider — same fallback shape as `model`. Needed
    when a workspace wants the top-level model/provider to stay
    user-visible while pinning the runtime to a different gateway.
    """
    monkeypatch.delenv("LLM_PROVIDER", raising=False)
    monkeypatch.delenv("MODEL_PROVIDER", raising=False)
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(
        yaml.dump(
            {
                "model": "anthropic:claude-opus-4-7",
                "runtime_config": {"provider": "openai"},
            }
        )
    )

    cfg = load_config(str(tmp_path))
    # Top-level still derives from the slug.
    assert cfg.provider == "anthropic"
    # runtime_config.provider explicit override wins.
    assert cfg.runtime_config.provider == "openai"


def test_provider_default_from_default_model(tmp_path, monkeypatch):
    """When config.yaml is empty, the WorkspaceConfig default model
    (`anthropic:claude-opus-4-7`) yields provider=`anthropic`. Pins the
    "no config" boot path to a sensible derived provider.
    """
    monkeypatch.delenv("LLM_PROVIDER", raising=False)
    monkeypatch.delenv("MODEL_PROVIDER", raising=False)
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({}))

    cfg = load_config(str(tmp_path))
    assert cfg.model == "anthropic:claude-opus-4-7"
    assert cfg.provider == "anthropic"
    assert cfg.runtime_config.provider == "anthropic"


def test_delegation_config_defaults(tmp_path):
    """DelegationConfig nested defaults are applied."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({}))

    cfg = load_config(str(tmp_path))
    assert cfg.delegation.retry_attempts == 3
    assert cfg.delegation.retry_delay == 5.0
    assert cfg.delegation.timeout == 120.0
    assert cfg.delegation.escalate is True


def test_delegation_config_override(tmp_path):
    """Delegation values from YAML override defaults."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(
        yaml.dump(
            {"delegation": {"retry_attempts": 5, "timeout": 60.0, "escalate": False}}
        )
    )

    cfg = load_config(str(tmp_path))
    assert cfg.delegation.retry_attempts == 5
    assert cfg.delegation.timeout == 60.0
    assert cfg.delegation.escalate is False
    # retry_delay still default
    assert cfg.delegation.retry_delay == 5.0


def test_a2a_config_defaults(tmp_path):
    """A2AConfig nested defaults are applied."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({}))

    cfg = load_config(str(tmp_path))
    assert cfg.a2a.port == 8000
    assert cfg.a2a.streaming is True
    assert cfg.a2a.push_notifications is True


def test_a2a_config_override(tmp_path):
    """A2A values from YAML override defaults."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(
        yaml.dump({"a2a": {"port": 9000, "streaming": False}})
    )

    cfg = load_config(str(tmp_path))
    assert cfg.a2a.port == 9000
    assert cfg.a2a.streaming is False
    assert cfg.a2a.push_notifications is True


def test_sandbox_config_defaults(tmp_path):
    """SandboxConfig nested defaults are applied."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({}))

    cfg = load_config(str(tmp_path))
    assert cfg.sandbox.backend == "subprocess"
    assert cfg.sandbox.memory_limit == "256m"
    assert cfg.sandbox.timeout == 30


def test_sandbox_config_override(tmp_path):
    """Sandbox values from YAML override defaults."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(
        yaml.dump({"sandbox": {"backend": "docker", "memory_limit": "512m", "timeout": 60}})
    )

    cfg = load_config(str(tmp_path))
    assert cfg.sandbox.backend == "docker"
    assert cfg.sandbox.memory_limit == "512m"
    assert cfg.sandbox.timeout == 60


def test_load_config_file_not_found(tmp_path):
    """load_config raises FileNotFoundError when config.yaml is missing."""
    import pytest

    with pytest.raises(FileNotFoundError):
        load_config(str(tmp_path))


def test_load_config_env_path(tmp_path, monkeypatch):
    """load_config reads from WORKSPACE_CONFIG_PATH env var when no arg given."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({"name": "EnvAgent"}))

    monkeypatch.setenv("WORKSPACE_CONFIG_PATH", str(tmp_path))
    cfg = load_config()  # no argument
    assert cfg.name == "EnvAgent"


def test_initial_prompt_inline(tmp_path):
    """initial_prompt reads inline string from YAML."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({"initial_prompt": "Wake up and clone the repo"}))

    cfg = load_config(str(tmp_path))
    assert cfg.initial_prompt == "Wake up and clone the repo"


def test_initial_prompt_from_file(tmp_path):
    """initial_prompt_file reads prompt from a file."""
    prompt_file = tmp_path / "init.md"
    prompt_file.write_text("Clone repo and read CLAUDE.md")
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({"initial_prompt_file": "init.md"}))

    cfg = load_config(str(tmp_path))
    assert cfg.initial_prompt == "Clone repo and read CLAUDE.md"


def test_initial_prompt_inline_overrides_file(tmp_path):
    """Inline initial_prompt takes precedence over initial_prompt_file."""
    prompt_file = tmp_path / "init.md"
    prompt_file.write_text("From file")
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({
        "initial_prompt": "From inline",
        "initial_prompt_file": "init.md",
    }))

    cfg = load_config(str(tmp_path))
    assert cfg.initial_prompt == "From inline"


def test_initial_prompt_default_empty(tmp_path):
    """initial_prompt defaults to empty string when not specified."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({}))

    cfg = load_config(str(tmp_path))
    assert cfg.initial_prompt == ""


def test_initial_prompt_file_missing(tmp_path):
    """initial_prompt_file gracefully handles missing file."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({"initial_prompt_file": "nonexistent.md"}))

    cfg = load_config(str(tmp_path))
    assert cfg.initial_prompt == ""


def test_shared_context_default(tmp_path):
    """shared_context defaults to empty list when not specified in YAML."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({}))

    cfg = load_config(str(tmp_path))
    assert cfg.shared_context == []


def test_shared_context_from_yaml(tmp_path):
    """shared_context reads file paths from YAML."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(
        yaml.dump({"shared_context": ["guidelines.md", "architecture.md"]})
    )

    cfg = load_config(str(tmp_path))
    assert cfg.shared_context == ["guidelines.md", "architecture.md"]


# ===== Compliance default lock (#2059) =====
#
# PR #2056 flipped ComplianceConfig.mode default from "" to "owasp_agentic"
# so every shipped template gets prompt-injection detection + PII redaction
# by default. These tests pin the new default at all four entry points so
# a silent revert (or a refactor that reintroduces the old no-op default)
# fails fast instead of shipping a workspace with compliance silently off.


def test_compliance_dataclass_default():
    """ComplianceConfig() — no args — must default to owasp_agentic + detect."""
    cfg = ComplianceConfig()
    assert cfg.mode == "owasp_agentic"
    assert cfg.prompt_injection == "detect"


@pytest.mark.parametrize(
    "yaml_payload, expected_mode",
    [
        # No `compliance:` key at all — full default path.
        ({}, "owasp_agentic"),
        # Explicit empty block — exercises load_config's
        # `.get("mode", "owasp_agentic")` default-fill at config.py:377.
        # Common shape during template editing.
        ({"compliance": {}}, "owasp_agentic"),
        # Documented opt-out: explicit `mode: ""` disables compliance.
        ({"compliance": {"mode": ""}}, ""),
    ],
    ids=["yaml_omits_block", "yaml_block_empty", "yaml_explicit_optout"],
)
def test_compliance_default_via_load_config(tmp_path, yaml_payload, expected_mode):
    """load_config honors the owasp_agentic default at every yaml shape and
    still respects explicit opt-out."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump(yaml_payload))

    cfg = load_config(str(tmp_path))
    assert cfg.compliance.mode == expected_mode
    # prompt_injection was never overridden in any payload — must stay at
    # the dataclass default regardless of the mode value.
    assert cfg.compliance.prompt_injection == "detect"


# ===== Observability block (#119 PR-1) =====
#
# Hermes-style declarative block grouping cadence + verbosity knobs into one
# place. Schema-only in this PR — wiring into heartbeat.py / main.py lands in
# PR-3. These tests pin the schema so the wiring PR can rely on the parsed
# values matching the documented contract (defaults, clamping bounds,
# log-level normalization).


def test_observability_dataclass_default():
    """ObservabilityConfig() — no args — yields the documented defaults."""
    cfg = ObservabilityConfig()
    assert cfg.heartbeat_interval_seconds == 30
    assert cfg.log_level == "INFO"


def test_observability_default_when_yaml_omits_block(tmp_path):
    """No ``observability:`` key in YAML → dataclass defaults."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(yaml.dump({}))

    cfg = load_config(str(tmp_path))
    assert cfg.observability.heartbeat_interval_seconds == 30
    assert cfg.observability.log_level == "INFO"


def test_observability_explicit_yaml_override(tmp_path):
    """Explicit YAML values flow through load_config to ObservabilityConfig."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(
        yaml.dump(
            {
                "observability": {
                    "heartbeat_interval_seconds": 60,
                    "log_level": "DEBUG",
                }
            }
        )
    )

    cfg = load_config(str(tmp_path))
    assert cfg.observability.heartbeat_interval_seconds == 60
    assert cfg.observability.log_level == "DEBUG"


def test_observability_partial_override_keeps_other_defaults(tmp_path):
    """Setting only heartbeat preserves the log_level default — and vice versa."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(
        yaml.dump({"observability": {"heartbeat_interval_seconds": 45}})
    )

    cfg = load_config(str(tmp_path))
    assert cfg.observability.heartbeat_interval_seconds == 45
    assert cfg.observability.log_level == "INFO"


@pytest.mark.parametrize(
    "raw, expected",
    [
        # In-band values pass through unchanged.
        (5, 5),
        (30, 30),
        (300, 300),
        # Below floor → clamped up to 5s. Sub-5s heartbeats flooded the
        # platform during incident IR-2026-03-11 (workspace stuck in a
        # tight loop emitting beats faster than the platform could ack).
        (1, 5),
        (0, 5),
        (-7, 5),
        # Above ceiling → clamped down to 300s. >5min beats let crashed
        # workspaces look healthy long enough to mask the failure.
        (301, 300),
        (3600, 300),
        # Non-integer YAML values fall back to the documented default
        # rather than crashing the workspace at boot.
        ("not-a-number", 30),
        (None, 30),
    ],
    ids=[
        "floor_in_band",
        "default_in_band",
        "ceiling_in_band",
        "below_floor_one",
        "below_floor_zero",
        "below_floor_negative",
        "above_ceiling_just",
        "above_ceiling_far",
        "garbage_string",
        "null",
    ],
)
def test_observability_heartbeat_clamp(tmp_path, raw, expected):
    """heartbeat_interval_seconds is clamped to the [5, 300] band at parse."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(
        yaml.dump({"observability": {"heartbeat_interval_seconds": raw}})
    )

    cfg = load_config(str(tmp_path))
    assert cfg.observability.heartbeat_interval_seconds == expected


def test_observability_log_level_uppercased(tmp_path):
    """Lowercase or mixed-case log levels normalize to the canonical form
    Python's ``logging`` module expects, so operators can write either
    ``debug`` or ``DEBUG`` in YAML without surprise."""
    config_yaml = tmp_path / "config.yaml"
    config_yaml.write_text(
        yaml.dump({"observability": {"log_level": "debug"}})
    )

    cfg = load_config(str(tmp_path))
    assert cfg.observability.log_level == "DEBUG"
