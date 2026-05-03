"""Load workspace configuration from config.yaml."""

import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional

import yaml


@dataclass
class RBACConfig:
    """Role-based access control settings for this workspace.

    ``roles`` declares what this workspace is *allowed* to do.  Each role
    name maps to a set of permitted actions.  Built-in roles are defined in
    ``tools/audit.ROLE_PERMISSIONS``; custom roles can be added via
    ``allowed_actions``.

    Built-in roles
    --------------
    admin           All actions (delegate, approve, memory.read, memory.write)
    operator        Same as admin — standard agent role  (default)
    read-only       memory.read only
    no-delegation   approve + memory.read + memory.write
    no-approval     delegate + memory.read + memory.write
    memory-readonly memory.read only

    Example config.yaml snippet::

        rbac:
          roles:
            - operator
          allowed_actions:
            analyst:
              - memory.read
              - memory.write
    """

    roles: list[str] = field(default_factory=lambda: ["operator"])
    """List of role names granted to this workspace."""

    allowed_actions: dict[str, list[str]] = field(default_factory=dict)
    """Custom role → [action, ...] overrides.  Takes precedence over built-ins."""


@dataclass
class HITLConfig:
    """Human-In-The-Loop settings loaded from the ``hitl:`` block in config.yaml.

    Example config.yaml snippet::

        hitl:
          channels:
            - type: dashboard       # always active
            - type: slack
              webhook_url: https://hooks.slack.com/services/…
            - type: email
              smtp_host: smtp.example.com
              from: alerts@example.com
              to: ops@example.com
          default_timeout: 300      # seconds
          bypass_roles: [admin]
    """
    channels: list[dict] = field(default_factory=lambda: [{"type": "dashboard"}])
    default_timeout: float = 300.0
    bypass_roles: list[str] = field(default_factory=list)


@dataclass
class DelegationConfig:
    retry_attempts: int = 3
    retry_delay: float = 5.0
    timeout: float = 120.0
    escalate: bool = True


@dataclass
class A2AConfig:
    port: int = 8000
    streaming: bool = True
    push_notifications: bool = True


@dataclass
class SandboxConfig:
    backend: str = "subprocess"  # subprocess | docker
    memory_limit: str = "256m"
    timeout: int = 30

@dataclass
class RuntimeConfig:
    """Configuration for CLI-based agent runtimes (claude-code, codex, ollama, custom)."""
    command: str = ""          # e.g. "claude", "codex", "ollama" (model goes in model field)
    args: list[str] = field(default_factory=list)  # additional CLI args
    required_env: list[str] = field(default_factory=list)  # env vars required to run (e.g. ["CLAUDE_CODE_OAUTH_TOKEN"])
    timeout: int = 0           # seconds (0 = no timeout — agents wait until done)
    model: str = ""            # model override for the CLI
    provider: str = ""         # explicit LLM provider (e.g., "anthropic", "openai",
                               # "minimax"). Falls back to the top-level resolved
                               # provider when empty. Adapters (hermes, claude-code,
                               # codex) prefer this over slug-parsing the model name.
    # Per-model entries surfaced in the canvas Model dropdown. Each entry is a
    # raw dict with at least ``id``; ``required_env`` is the per-model auth
    # list (e.g. ``{"id": "MiniMax-M2.7", "required_env": ["MINIMAX_API_KEY"]}``).
    # Preflight prefers an entry's ``required_env`` over the top-level
    # ``required_env`` when the picked ``model`` matches an entry's ``id``
    # (case-insensitive). The top-level list remains the fallback so single-
    # model templates need not migrate. Surfaced 2026-05-02 after a user
    # picked MiniMax in canvas, set MINIMAX_API_KEY, and still got booted
    # into a CLAUDE_CODE_OAUTH_TOKEN preflight failure.
    models: list[dict] = field(default_factory=list)
    # Deprecated — use required_env + secrets API instead. Kept for backward compat.
    auth_token_env: str = ""
    auth_token_file: str = ""


@dataclass
class GovernanceConfig:
    """Microsoft Agent Governance Toolkit integration settings.

    When ``enabled`` is True, Molecule AI's RBAC and audit trail are bridged
    to the Agent Governance Toolkit (agent-os-kernel) for policy evaluation.

    ``toolkit`` is reserved for future extensibility — only ``"microsoft"``
    is supported today.

    ``policy_mode`` controls enforcement:
      strict      RBAC *and* toolkit policy must both allow — strictest mode
      permissive  RBAC must allow; toolkit denials are logged but not enforced
      audit       RBAC only; toolkit evaluated and logged but never blocks

    ``policy_file`` path to a Rego (.rego), YAML (.yaml/.yml), or Cedar
    (.cedar) policy file, loaded into the PolicyEvaluator at startup.

    ``blocked_patterns`` is a list of regex patterns that the toolkit will
    always deny regardless of roles or policy.
    """

    enabled: bool = False
    toolkit: str = "microsoft"
    policy_endpoint: str = ""
    policy_mode: str = "audit"           # strict | permissive | audit
    policy_file: str = ""
    blocked_patterns: list[str] = field(default_factory=list)
    max_tool_calls_per_task: int = 50


@dataclass
class SecurityScanConfig:
    """Skill dependency security scanning settings.

    ``mode`` controls what happens when critical/high CVEs are found:

    block  — raise ``SkillSecurityError``; the skill is NOT loaded.
    warn   — emit a WARNING + audit event; the skill is loaded anyway (default).
    off    — skip scanning entirely (air-gapped or CI environments).

    Scanners tried in order: Snyk CLI (requires ``SNYK_TOKEN``), then
    pip-audit.  If neither is available the scan is silently skipped.

    Example config.yaml snippet::

        security_scan: warn         # shorthand string form
        # or verbose form:
        security_scan:
          mode: block
    """

    mode: str = "warn"
    """One of: block | warn | off."""

    fail_open_if_no_scanner: bool = True
    """When True (default), silently skip scanning if no scanner (snyk/pip-audit)
    is in PATH.  When False and mode='block', raise SkillSecurityError so that
    operators who require a CVE gate know the gate is absent.  Closes #268."""


@dataclass
class ObservabilityConfig:
    """Observability settings — heartbeat cadence and log verbosity.

    Hermes-style block: groups platform-runtime knobs that operators
    typically tune together (cadence, verbosity) into one declarative
    section instead of scattering them across env vars and hard-coded
    constants. Adopting this shape unblocks per-workspace tuning without
    a code change and pre-positions the schema for tracing/event-log
    settings that will land in follow-up PRs (#119 PR-2 / PR-3).

    Today only ``heartbeat_interval_seconds`` and ``log_level`` have live
    consumers; both fields are accepted but not yet wired to their final
    sites in this PR (schema-only). Wiring lands in PR-3 of the series.

    Example config.yaml snippet::

        observability:
          heartbeat_interval_seconds: 60
          log_level: DEBUG
    """

    heartbeat_interval_seconds: int = 30
    """Seconds between heartbeats sent to the platform. Default 30 matches
    ``workspace/heartbeat.py``'s long-standing constant. Lower values
    reduce platform-side detection latency for crashed workspaces; higher
    values reduce platform write load. Bounds: clamped to [5, 300] at
    parse time — outside that range the workspace either floods the
    platform or looks dead before the next beat."""

    log_level: str = "INFO"
    """Python ``logging`` level for the workspace runtime. Accepts the
    standard names (DEBUG, INFO, WARNING, ERROR, CRITICAL). Today the
    runtime reads ``LOG_LEVEL`` env; PR-3 of the #119 stack switches to
    this field with env still honored as an override for ops debugging."""


@dataclass
class ComplianceConfig:
    """OWASP Top 10 for Agentic Applications compliance settings.

    Default is ``mode: owasp_agentic`` + ``prompt_injection: detect``.
    The detect mode logs injection attempts as audit events without
    blocking the request — so there is no false-positive UX cost, only
    a gain in visibility. Operators opt into stricter ``block`` mode per
    workspace. To disable compliance entirely (not recommended), set
    ``mode: ""`` in config.yaml.

    Before 2026-04-24, the default was ``mode: ""`` (fully off). A
    review of the A2A inbound path showed that no shipped template set
    ``mode`` explicitly, so prompt-injection detection was silently
    disabled for every live workspace despite the machinery existing.
    Flipping the default to ``owasp_agentic`` with ``prompt_injection:
    detect`` closes that gap with zero user-visible behavior change.

    Example config.yaml snippet to opt OUT::

        compliance:
          mode: ""                       # disables all compliance checks

    Example config.yaml snippet to tighten::

        compliance:
          mode: owasp_agentic            # (default)
          prompt_injection: block        # (default: detect)
          max_tool_calls_per_task: 30
          max_task_duration_seconds: 180
    """

    mode: str = "owasp_agentic"
    """Enable compliance mode. ``owasp_agentic`` (default) activates the
    OA-01/OA-02/OA-03/OA-06 checks; ``""`` disables everything."""

    prompt_injection: str = "detect"
    """``detect`` logs injection attempts (default, zero UX cost);
    ``block`` raises PromptInjectionError before the agent sees the
    text. Operators can tighten to ``block`` per workspace."""

    max_tool_calls_per_task: int = 50
    """Maximum number of tool invocations per task before ExcessiveAgencyError."""

    max_task_duration_seconds: int = 300
    """Maximum wall-clock seconds per task before ExcessiveAgencyError."""


@dataclass
class WorkspaceConfig:
    name: str = "Workspace"
    description: str = ""
    role: str = ""
    """Human-readable role label for this agent (e.g. 'Senior Code Reviewer').
    Surfaced in AGENTS.md so peer agents can understand this workspace's purpose
    without reading the full system prompt. Falls back to description when empty."""
    version: str = "1.0.0"
    tier: int = 1
    model: str = "anthropic:claude-opus-4-7"
    provider: str = ""
    """Explicit LLM provider slug (e.g., ``anthropic``, ``openai``, ``minimax``).

    When empty, ``load_config`` derives it from the ``model`` slug prefix
    (``anthropic:claude-opus-4-7`` → ``anthropic``; ``minimax/abab7-chat`` →
    ``minimax``; bare model names → ``""``). Set explicitly via the canvas
    Provider dropdown or the ``LLM_PROVIDER`` env var when the model name
    is provider-ambiguous (e.g., a custom alias) or when an adapter needs
    a specific gateway distinct from the model namespace.
    """
    runtime: str = "langgraph"  # langgraph | claude-code | codex | ollama | custom
    runtime_config: RuntimeConfig = field(default_factory=RuntimeConfig)
    initial_prompt: str = ""
    """Auto-sent as the first A2A message after startup. Default empty = no auto-message.
    Can be an inline string or a file reference (initial_prompt_file in yaml)."""
    idle_prompt: str = ""
    """Auto-sent every `idle_interval_seconds` while the workspace has no active
    task (heartbeat.active_tasks == 0). Default empty = no idle loop. This is
    the reflection-on-completion / backlog-pull pattern from the Hermes/Letta
    playbook: the workspace self-wakes when idle, runs a lightweight reflection
    prompt, and either picks up queued work or stops. Cost scales with useful
    activity (the prompt returns quickly if there's nothing to do). Can be
    inline or a file reference via `idle_prompt_file`."""
    idle_interval_seconds: int = 600
    """How often the idle loop checks in (seconds). Default 600 (10 min).
    Ignored when idle_prompt is empty."""
    skills: list[str] = field(default_factory=list)
    plugins: list[str] = field(default_factory=list)  # installed plugin names
    tools: list[str] = field(default_factory=list)
    prompt_files: list[str] = field(default_factory=list)
    shared_context: list[str] = field(default_factory=list)
    a2a: A2AConfig = field(default_factory=A2AConfig)
    delegation: DelegationConfig = field(default_factory=DelegationConfig)
    sandbox: SandboxConfig = field(default_factory=SandboxConfig)
    rbac: RBACConfig = field(default_factory=RBACConfig)
    hitl: HITLConfig = field(default_factory=HITLConfig)
    governance: GovernanceConfig = field(default_factory=GovernanceConfig)
    security_scan: SecurityScanConfig = field(default_factory=SecurityScanConfig)
    compliance: ComplianceConfig = field(default_factory=ComplianceConfig)
    observability: ObservabilityConfig = field(default_factory=ObservabilityConfig)
    sub_workspaces: list[dict] = field(default_factory=list)
    effort: str = ""
    """Claude output effort level for the agentic loop: low | medium | high | xhigh | max.
    Empty string = not set (model default applies).  xhigh is the Opus 4.7 recommended
    default for long agentic tasks.  Passed as ``output_config.effort`` by ClaudeSDKExecutor."""
    task_budget: int = 0
    """Advisory total-token budget across the full agentic loop.  0 = not set.
    Must be >= 20000 when non-zero (API minimum).  When set, ClaudeSDKExecutor
    automatically adds the ``task-budgets-2026-03-13`` beta header."""


def _derive_provider_from_model(model: str) -> str:
    """Extract the provider slug prefix from a model identifier.

    Recognizes both ``provider:model`` (Anthropic / OpenAI / Google convention)
    and ``provider/model`` (HuggingFace / Minimax convention). Returns ``""``
    when the model has no recognizable separator — callers must treat empty
    as "use adapter default routing", not as a hard failure.
    """
    for sep in (":", "/"):
        if sep in model:
            return model.partition(sep)[0]
    return ""


def _clamp_heartbeat(value: object) -> int:
    """Coerce raw YAML/env input into the [5, 300]-second heartbeat band.

    Outside that band the workspace either floods the platform with
    sub-second beats or looks dead long before the next one — both
    real failure modes seen on incidents, neither benign. Coerce here
    so adapters and ``heartbeat.py`` can read the value without
    re-validating.
    """
    try:
        n = int(value)
    except (TypeError, ValueError):
        return 30
    return max(5, min(300, n))


def load_config(config_path: Optional[str] = None) -> WorkspaceConfig:
    """Load config from WORKSPACE_CONFIG_PATH or the given path."""
    if config_path is None:
        config_path = os.environ.get("WORKSPACE_CONFIG_PATH", "/configs")

    config_file = Path(config_path) / "config.yaml"
    if not config_file.exists():
        raise FileNotFoundError(f"Config file not found: {config_file}")

    with open(config_file) as f:
        raw = yaml.safe_load(f) or {}

    # Override model from env if provided
    model = os.environ.get("MODEL_PROVIDER", raw.get("model", "anthropic:claude-opus-4-7"))

    # Resolve top-level provider with this priority chain:
    #   1. ``LLM_PROVIDER`` env var (canvas Save+Restart sets this so the
    #      operator's choice survives a CP-driven restart even though the
    #      regenerated /configs/config.yaml drops most user fields).
    #   2. Explicit YAML ``provider:`` (an operator pinned it in the file).
    #   3. Derive from the model slug prefix for backward compat:
    #        ``anthropic:claude-opus-4-7`` → ``anthropic``
    #        ``minimax/abab7-chat-preview`` → ``minimax``
    #        bare model names → ``""``  (signals "use adapter default")
    # Empty after all three is fine — adapters that don't need an explicit
    # provider (langgraph, claude-code-default, codex) keep their existing
    # routing; adapters that do (hermes via derive-provider.sh) prefer this
    # over slug-parsing the model name.
    provider = (
        os.environ.get("LLM_PROVIDER")
        or raw.get("provider")
        or _derive_provider_from_model(model)
    )

    runtime = raw.get("runtime", "langgraph")
    runtime_raw = raw.get("runtime_config", {})

    a2a_raw = raw.get("a2a", {})
    delegation_raw = raw.get("delegation", {})
    sandbox_raw = raw.get("sandbox", {})
    rbac_raw = raw.get("rbac", {})
    hitl_raw = raw.get("hitl", {})
    governance_raw = raw.get("governance", {})
    # security_scan accepts both shorthand string ("warn") and dict ({"mode": "warn"})
    _ss_raw = raw.get("security_scan", {})
    security_scan_raw = _ss_raw if isinstance(_ss_raw, dict) else {"mode": str(_ss_raw)}
    compliance_raw = raw.get("compliance", {})
    observability_raw = raw.get("observability", {})

    # Resolve initial_prompt: inline string or file reference
    initial_prompt = raw.get("initial_prompt", "")
    initial_prompt_file = raw.get("initial_prompt_file", "")
    if not initial_prompt and initial_prompt_file:
        prompt_path = Path(config_path) / initial_prompt_file
        if prompt_path.exists():
            initial_prompt = prompt_path.read_text().strip()

    # Resolve idle_prompt: same pattern as initial_prompt
    idle_prompt = raw.get("idle_prompt", "")
    idle_prompt_file = raw.get("idle_prompt_file", "")
    if not idle_prompt and idle_prompt_file:
        idle_path = Path(config_path) / idle_prompt_file
        if idle_path.exists():
            idle_prompt = idle_path.read_text().strip()
    idle_interval_seconds = int(raw.get("idle_interval_seconds", 600))

    return WorkspaceConfig(
        name=raw.get("name", "Workspace"),
        description=raw.get("description", ""),
        role=raw.get("role", ""),
        version=raw.get("version", "1.0.0"),
        tier=int(raw.get("tier", 1)) if str(raw.get("tier", 1)).isdigit() else 1,
        model=model,
        provider=provider,
        runtime=runtime,
        initial_prompt=initial_prompt,
        idle_prompt=idle_prompt,
        idle_interval_seconds=idle_interval_seconds,
        runtime_config=RuntimeConfig(
            command=runtime_raw.get("command", ""),
            args=runtime_raw.get("args", []),
            required_env=runtime_raw.get("required_env", []),
            timeout=runtime_raw.get("timeout", 0),
            # Picked-model precedence (priority order):
            #   1. MODEL_PROVIDER env var — canvas-picked model, plumbed via
            #      workspace-server's secret-mint path or the universal
            #      MODEL/MODEL_PROVIDER env from applyRuntimeModelEnv. The
            #      operator's canvas selection MUST win over the template's
            #      baked-in default; previously the template's
            #      `runtime_config.model: sonnet` always won and the picked
            #      MiniMax/GLM/etc model was silently dropped (Bug B,
            #      surfaced 2026-05-02 during E2E).
            #   2. runtime_raw.model — explicit YAML override in the
            #      template's runtime_config.
            #   3. top-level `model` — already honors MODEL_PROVIDER (line
            #      359) but only when YAML lacks a top-level `model:`. This
            #      is the SaaS restart case (CP regenerates a minimal
            #      config.yaml on every boot, dropping runtime_config.model).
            # Centralising here means EVERY adapter gets the override for
            # free — no per-adapter env-reading code required.
            model=os.environ.get("MODEL_PROVIDER") or runtime_raw.get("model") or model,
            # Same fallback shape as ``model`` above: an explicit
            # ``runtime_config.provider`` wins; otherwise inherit the
            # top-level resolved provider so adapters see a single
            # consistent choice without each one re-implementing
            # env/YAML/slug-prefix resolution.
            provider=runtime_raw.get("provider") or provider,
            # Per-model entries (canvas Model dropdown source). Pass through
            # raw dicts so the schema can grow without a parser change. Only
            # entries that are dicts are kept — a malformed YAML element
            # (string, list, None) is silently dropped rather than raising,
            # matching the rest of this parser's lenient defaults.
            models=[m for m in (runtime_raw.get("models") or []) if isinstance(m, dict)],
            # Deprecated fields — kept for backward compat
            auth_token_env=runtime_raw.get("auth_token_env", ""),
            auth_token_file=runtime_raw.get("auth_token_file", ""),
        ),
        skills=raw.get("skills", []),
        plugins=raw.get("plugins", []),
        tools=raw.get("tools", []),
        prompt_files=raw.get("prompt_files", []),
        shared_context=raw.get("shared_context", []),
        a2a=A2AConfig(
            port=a2a_raw.get("port", 8000),
            streaming=a2a_raw.get("streaming", True),
            push_notifications=a2a_raw.get("push_notifications", True),
        ),
        delegation=DelegationConfig(
            retry_attempts=delegation_raw.get("retry_attempts", 3),
            retry_delay=delegation_raw.get("retry_delay", 5.0),
            timeout=delegation_raw.get("timeout", 120.0),
            escalate=delegation_raw.get("escalate", True),
        ),
        sandbox=SandboxConfig(
            backend=sandbox_raw.get("backend", "subprocess"),
            memory_limit=sandbox_raw.get("memory_limit", "256m"),
            timeout=sandbox_raw.get("timeout", 30),
        ),
        rbac=RBACConfig(
            roles=rbac_raw.get("roles", ["operator"]),
            allowed_actions=rbac_raw.get("allowed_actions", {}),
        ),
        hitl=HITLConfig(
            channels=hitl_raw.get("channels", [{"type": "dashboard"}]),
            default_timeout=float(hitl_raw.get("default_timeout", 300)),
            bypass_roles=hitl_raw.get("bypass_roles", []),
        ),
        governance=GovernanceConfig(
            enabled=governance_raw.get("enabled", False),
            toolkit=governance_raw.get("toolkit", "microsoft"),
            policy_endpoint=governance_raw.get("policy_endpoint", ""),
            policy_mode=governance_raw.get("policy_mode", "audit"),
            policy_file=governance_raw.get("policy_file", ""),
            blocked_patterns=governance_raw.get("blocked_patterns", []),
            max_tool_calls_per_task=governance_raw.get("max_tool_calls_per_task", 50),
        ),
        security_scan=SecurityScanConfig(
            mode=security_scan_raw.get("mode", "warn"),
            fail_open_if_no_scanner=security_scan_raw.get("fail_open_if_no_scanner", True),
        ),
        compliance=ComplianceConfig(
            # Default must match ComplianceConfig.mode's dataclass default
            # (see class docstring for rationale — 2026-04-24 flip).
            mode=compliance_raw.get("mode", "owasp_agentic"),
            prompt_injection=compliance_raw.get("prompt_injection", "detect"),
            max_tool_calls_per_task=int(compliance_raw.get("max_tool_calls_per_task", 50)),
            max_task_duration_seconds=int(compliance_raw.get("max_task_duration_seconds", 300)),
        ),
        observability=ObservabilityConfig(
            heartbeat_interval_seconds=_clamp_heartbeat(
                observability_raw.get("heartbeat_interval_seconds", 30)
            ),
            log_level=str(observability_raw.get("log_level", "INFO")).upper(),
        ),
        sub_workspaces=raw.get("sub_workspaces", []),
        effort=str(raw.get("effort", "")),
        task_budget=int(raw.get("task_budget", 0)),
    )
