"""Base adapter interface for agent infrastructure providers."""

import logging
import os
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from typing import Any

from a2a.server.agent_execution import AgentExecutor

logger = logging.getLogger(__name__)


@dataclass
class SetupResult:
    """Result from the shared _common_setup() pipeline."""
    system_prompt: str
    loaded_skills: list          # LoadedSkill instances
    langchain_tools: list        # LangChain BaseTool instances
    is_coordinator: bool
    children: list               # child workspace dicts


@dataclass
class AdapterConfig:
    """Standardized config passed to every adapter."""
    model: str                              # e.g. "anthropic:claude-sonnet-4-6" or "openrouter:google/gemini-2.5-flash"
    system_prompt: str | None = None        # Assembled system prompt text
    tools: list[str] = field(default_factory=list)  # Tool names from config.yaml
    runtime_config: dict[str, Any] = field(default_factory=dict)  # Raw runtime_config block
    config_path: str = "/configs"           # Path to configs directory
    workspace_id: str = ""                  # Workspace identifier
    prompt_files: list[str] = field(default_factory=list)  # Ordered prompt file names
    a2a_port: int = 8000                    # Port for A2A server
    heartbeat: Any = None                   # HeartbeatLoop instance


@dataclass(frozen=True)
class RuntimeCapabilities:
    """Adapter-declared ownership of cross-cutting platform capabilities.

    The platform provides FALLBACK implementations of heartbeat, cron,
    durable session, etc. When a runtime SDK provides one of these
    natively (e.g. claude-code's streaming session model, hermes-agent's
    sidecar lifecycle), the adapter sets the corresponding flag to True.
    The platform reads these flags and skips its fallback for that
    capability — the adapter is responsible instead.

    Observability is NEVER skipped: A2A protocol, activity_logs, and the
    broadcaster always run regardless of who owns the capability. These
    flags only switch WHO IMPLEMENTS the behavior, not whether the
    platform sees it.

    All defaults are False so introducing this dataclass is a no-op:
    every existing adapter inherits BaseAdapter.capabilities() which
    returns RuntimeCapabilities() with everything off, matching today's
    "platform does it all" behavior. Each capability gets a platform-
    side consumer in a follow-up PR; this class is the foundation.

    See project memory `project_runtime_native_pluggable.md` for the
    architecture principle these flags encode.
    """
    # Heartbeat — adapter sends its own keep-alive signal to the platform's
    # broadcaster instead of relying on workspace/heartbeat.py's 30s loop.
    # Set True when the SDK already maintains a long-lived session that
    # produces natural progress events (e.g. claude-code streaming).
    provides_native_heartbeat: bool = False

    # Cron / schedule — adapter handles scheduled triggers internally
    # (Temporal workflows, Durable Functions, sidecar daemons). Platform
    # scheduler skips polling workspace_schedules for this workspace,
    # avoiding double-fire on restart.
    provides_native_scheduler: bool = False

    # Durable session — adapter persists in-flight session state across
    # restarts and exposes it via pre_stop_state/restore_state. When True,
    # the platform's a2a_queue does not need to enqueue mid-session
    # requests; the adapter handles QUEUED-state on its own.
    provides_native_session: bool = False

    # Status lifecycle — adapter reports its own ready/degraded/failed
    # state (e.g. via heartbeat metadata). Platform respects the adapter
    # report instead of inferring status from heartbeat error rate.
    provides_native_status_mgmt: bool = False

    # Retry — adapter handles transient errors (rate limits, 5xx) with
    # its own backoff. Platform stops re-dispatching A2A requests that
    # the adapter explicitly marked as "retrying internally".
    provides_native_retry: bool = False

    # Activity log decoration — adapter contributes runtime-specific
    # fields (model, token_count, latency breakdown) into activity_log
    # rows alongside the platform-defined columns.
    provides_activity_decoration: bool = False

    # Channel dispatch — adapter sends to external channels (Slack,
    # Lark, etc.) directly instead of routing through platform channels
    # manager. Used when the SDK has built-in channel integrations.
    provides_channel_dispatch: bool = False

    def to_dict(self) -> dict[str, bool]:
        """Serializable shape for the heartbeat payload + /capabilities
        endpoint. Plain dict avoids leaking dataclass internals to Go."""
        return {
            "heartbeat": self.provides_native_heartbeat,
            "scheduler": self.provides_native_scheduler,
            "session": self.provides_native_session,
            "status_mgmt": self.provides_native_status_mgmt,
            "retry": self.provides_native_retry,
            "activity_decoration": self.provides_activity_decoration,
            "channel_dispatch": self.provides_channel_dispatch,
        }


class BaseAdapter(ABC):
    """Interface every agent infrastructure adapter must implement.

    To add a new agent infra:
    1. Create a standalone template repo (molecule-ai-workspace-template-<infra>)
    2. Implement adapter.py with a class extending BaseAdapter
    3. Add requirements.txt with your infra's dependencies + molecule-runtime
    4. Set ADAPTER_MODULE in the Dockerfile to your adapter module path

    Cross-cutting capabilities your adapter can opt into:
    - capabilities() — declare native ownership of heartbeat, scheduler,
      session, status mgmt, etc. (see RuntimeCapabilities above)
    - idle_timeout_override() — extend the platform's per-dispatch
      silence window for SDKs with long synth turns
    - runtime_wedge.mark_wedged() / clear_wedge() — flip the workspace
      to `degraded` + auto-recover when your SDK hits a non-recoverable
      error class. Import directly from `runtime_wedge`; the heartbeat
      forwards the state to the platform automatically. See the
      runtime_wedge module docstring for the integration recipe.
    """

    @staticmethod
    @abstractmethod
    def name() -> str:  # pragma: no cover
        """Return the runtime identifier (e.g. 'langgraph', 'crewai').
        This must match the 'runtime' field in config.yaml."""
        ...

    @staticmethod
    @abstractmethod
    def display_name() -> str:  # pragma: no cover
        """Human-readable name for UI display."""
        ...

    @staticmethod
    @abstractmethod
    def description() -> str:  # pragma: no cover
        """Short description of what this adapter provides."""
        ...

    @staticmethod
    def get_config_schema() -> dict:
        """Return JSON Schema for runtime_config fields this adapter supports.
        Used by the Config tab UI to render the right form fields.
        Override in subclasses for adapter-specific settings."""
        return {}

    def capabilities(self) -> "RuntimeCapabilities":
        """Declare which cross-cutting capabilities this adapter owns
        natively vs delegates to platform fallback.

        Default returns RuntimeCapabilities() — every flag False, meaning
        the platform owns everything (today's behavior). Adapters override
        to declare native ownership; e.g. claude-code's adapter returns
        RuntimeCapabilities(provides_native_heartbeat=True,
                             provides_native_session=True).

        Subsequent platform-side consumers (idle-timeout override,
        scheduler skip, etc.) read this and route accordingly. See
        project memory `project_runtime_native_pluggable.md`."""
        return RuntimeCapabilities()

    def idle_timeout_override(self) -> int | None:
        """Per-A2A-dispatch silence window override, in SECONDS.

        Return None to use the platform default (env var
        A2A_IDLE_TIMEOUT_SECONDS, falling back to 5 minutes — see
        a2a_proxy.go:defaultIdleTimeoutDuration). Override when this
        runtime's SDK can legitimately go silent longer than the
        default before the dispatch should be considered wedged.

        Why this is per-adapter, not just env: the env value is a
        cluster-wide knob set by ops. Different SDKs have different
        latency profiles — claude-code synthesis on Opus + tool use
        legitimately runs 8-10 min between broadcasts; hermes synth
        with custom providers can be even slower. Hardcoding 5min for
        everyone either cancels real work (claude-code synth) or
        leaves wedged runtimes (langgraph) hanging too long.

        Platform reads this from the heartbeat payload and stashes
        it per-workspace; dispatchA2A consults it before applying the
        idle timer. None / unset / zero falls through to the global
        default — same behavior as before this hook landed."""
        return None

    # ------------------------------------------------------------------
    # Plugin install hooks
    # ------------------------------------------------------------------
    # New pipeline: each plugin ships per-runtime adaptors resolved via
    # `plugins_registry.resolve()`. Adapters expose hooks below that
    # adaptors call to wire plugin content into the runtime.
    #
    # Default implementations are filesystem-only (write to /configs,
    # append to CLAUDE.md). Runtimes with a dynamic tool registry
    # (e.g. DeepAgents sub-agents) override the hooks to also register
    # in-process state.

    def memory_filename(self) -> str:
        """File under /configs that the runtime treats as long-lived memory.

        Both Claude Code and DeepAgents read CLAUDE.md natively, so this is
        the sensible default. Override only if a runtime expects a different
        filename.
        """
        return "CLAUDE.md"

    def register_tool_hook(self, name: str, fn) -> None:
        """Default no-op. Override on runtimes with a dynamic tool registry.

        Runtimes that pick tools up at startup via filesystem scan (Claude
        Code reads /configs/skills, LangGraph globs **/*.py) don't need to
        do anything here — the adaptor's file-write step is enough.
        """
        return None

    async def transcript_lines(self, since: int = 0, limit: int = 100) -> dict:
        """Return live transcript entries for the most-recent agent session.

        Default implementation returns ``supported: False`` for runtimes
        that don't expose a per-session log on disk. Override in subclasses
        that DO (Claude Code reads ``~/.claude/projects/<cwd>/<session>.jsonl``).

        This is the "look over the agent's shoulder" feature — lets canvas /
        operators see live tool calls + AI thinking instead of waiting for
        the high-level activity log to flush.

        Args:
            since: line offset to skip — caller's last cursor (0 = from start)
            limit: max lines to return (caller-side cap, default 100, max 1000)

        Returns:
            ``{runtime, supported, lines, cursor, more, source}`` where
            ``cursor`` is the new offset to pass on the next poll, ``more``
            is True if additional lines remain past ``limit``, and ``source``
            is the file path lines were read from (useful for debugging).
        """
        return {
            "runtime": self.name(),
            "supported": False,
            "lines": [],
            "cursor": since,
            "more": False,
            "source": None,
        }

    def pre_stop_state(self) -> dict:
        """Capture in-memory state for pause/resume serialization.

        Called by main.py's shutdown handler just before the container exits.
        Returns a dict that will be scrubbed (via lib.snapshot_scrub) and
        written to /configs/.agent_snapshot.json.

        Default implementation:
        1. Attempts to read ``self._executor._session_id`` (set by
           create_executor) and includes it as ``session_id``.
        2. Includes up to 200 recent transcript lines via transcript_lines().

        Override in adapters that hold additional in-memory state that
        should survive a container stop.

        Returns:
            A JSON-serializable dict. All string values are scrubbed before
            persisting, so it is safe to include raw content from the
            agent's context.
        """
        from lib.pre_stop import MAX_TRANSCRIPT_LINES

        state: dict = {}

        # Session handle — critical for resuming the Claude Code session.
        executor = getattr(self, "_executor", None)
        if executor is not None:
            session_id = getattr(executor, "_session_id", None)
            if session_id:
                state["session_id"] = session_id

        # Recent conversation log — captures where the agent left off.
        # transcript_lines() may be async; call it synchronously if possible,
        # otherwise let async adapters override pre_stop_state entirely.
        try:
            import inspect as _inspect
            transcript_fn = self.transcript_lines
            if _inspect.iscoroutinefunction(transcript_fn):
                # Async adapter — override pre_stop_state() for transcript access.
                # The base impl still captures session_id above.
                pass
            else:
                transcript = transcript_fn(since=0, limit=MAX_TRANSCRIPT_LINES)
                if transcript.get("supported"):
                    state["transcript_lines"] = transcript.get("lines", [])
        except Exception:
            # Best-effort: never let transcript capture failure block serialization.
            pass

        return state

    def restore_state(self, snapshot: dict) -> None:
        """Restore in-memory state from a pause/resume snapshot.

        Called by main.py on first boot when /configs/.agent_snapshot.json
        exists. Gives the adapter a chance to restore session handles,
        conversation context, or any other in-memory state before the A2A
        server starts accepting requests.

        Default implementation stores ``snapshot["session_id"]`` and
        ``snapshot["transcript_lines"]`` as ``self._snapshot_session_id``
        and ``self._snapshot_transcript`` so that ``create_executor()`` or
        the executor itself can pick them up.

        Args:
            snapshot: The scrubbed snapshot dict previously written by
                     pre_stop_state(). All secrets have already been redacted.
        """
        self._snapshot_session_id: str | None = snapshot.get("session_id")
        self._snapshot_transcript: list | None = snapshot.get("transcript_lines")

    def register_subagent_hook(self, name: str, spec: dict) -> None:
        """Default no-op. DeepAgents overrides to register a sub-agent."""
        return None

    def append_to_memory_hook(self, config: AdapterConfig, filename: str, content: str) -> None:
        """Append text to /configs/<filename> if the marker isn't already present.

        Idempotent: looks for the first line of `content` as a marker so a
        re-install doesn't duplicate the block. Adaptors should pass content
        beginning with a unique header (e.g. ``# Plugin: molecule-dev-conventions``).
        """
        import os
        target = os.path.join(config.config_path, filename)
        marker = content.splitlines()[0].strip() if content else ""
        existing = ""
        if os.path.exists(target):
            with open(target) as f:
                existing = f.read()
            if marker and marker in existing:
                logger.info("append_to_memory: %s already contains %r — skipping", filename, marker)
                return
        os.makedirs(os.path.dirname(target) or ".", exist_ok=True)
        with open(target, "a") as f:
            if existing and not existing.endswith("\n"):
                f.write("\n")
            f.write(content if content.endswith("\n") else content + "\n")
        logger.info("append_to_memory: appended %d chars to %s", len(content), filename)

    async def install_plugins_via_registry(
        self,
        config: AdapterConfig,
        plugins,
    ) -> list:
        """Drive the new per-runtime adaptor pipeline for every loaded plugin.

        For each plugin in `plugins.plugins`, resolve the adaptor for this
        runtime (via :func:`plugins_registry.resolve`) and invoke
        ``install(ctx)``. Returns the list of :class:`InstallResult` so
        callers can surface warnings (e.g. raw-drop fallback hits).

        Adapters whose runtime supports the new pipeline call this from
        ``setup()`` instead of the legacy ``inject_plugins()``.
        """
        from pathlib import Path
        from plugins_registry import InstallContext, resolve

        results = []
        runtime = self.name().replace("-", "_")  # e.g. "claude-code" -> "claude_code"

        for plugin in plugins.plugins:
            adaptor, source = resolve(plugin.name, runtime, Path(plugin.path))
            ctx = InstallContext(
                configs_dir=Path(config.config_path),
                workspace_id=config.workspace_id,
                runtime=runtime,
                plugin_root=Path(plugin.path),
                memory_filename=self.memory_filename(),
                register_tool=self.register_tool_hook,
                register_subagent=self.register_subagent_hook,
                append_to_memory=lambda fn, c, _cfg=config: self.append_to_memory_hook(_cfg, fn, c),
            )
            try:
                result = await adaptor.install(ctx)
                results.append(result)
                logger.info(
                    "Plugin %s installed via %s adaptor (warnings: %d)",
                    plugin.name, source, len(result.warnings),
                )
            except Exception as exc:
                logger.exception("Plugin %s install via %s failed: %s", plugin.name, source, exc)

        return results

    async def inject_plugins(self, config: AdapterConfig, plugins) -> None:
        """Legacy hook — kept for backwards compatibility during migration.

        Default: drive the new per-runtime adaptor pipeline. Adapters not yet
        migrated may still override this with their own logic.
        """
        await self.install_plugins_via_registry(config, plugins)

    async def _common_setup(self, config: AdapterConfig) -> SetupResult:
        """Shared setup pipeline — loads plugins, skills, tools, coordinator, and builds system prompt.

        All adapters can call this to get the full platform feature set.
        Returns a SetupResult with LangChain BaseTool instances that adapters
        convert to their native format if needed.
        """
        from plugins import load_plugins
        from skill_loader.loader import load_skills
        from coordinator import get_children, get_parent_context, build_children_description
        from prompt import build_system_prompt, get_peer_capabilities, get_platform_instructions
        from builtin_tools.approval import request_approval
        from builtin_tools.delegation import delegate_to_workspace, check_delegation_status
        from builtin_tools.memory import commit_memory, search_memory
        from builtin_tools.sandbox import run_code

        platform_url = os.environ.get("PLATFORM_URL", "http://host.docker.internal:8080")

        # Load plugins from per-workspace dir first, then shared fallback
        workspace_plugins_dir = os.path.join(config.config_path, "plugins")
        plugins = load_plugins(
            workspace_plugins_dir=workspace_plugins_dir,
            shared_plugins_dir=os.environ.get("PLUGINS_DIR", "/plugins"),
        )
        await self.inject_plugins(config, plugins)
        if plugins.plugin_names:
            logger.info(f"Plugins: {', '.join(plugins.plugin_names)}")

        # Load skills (workspace + plugin skills, deduped). Pass the runtime
        # name so SKILL.md frontmatter `runtime: [...]` can opt skills out
        # of incompatible adapters (hermes won't load claude-code-only
        # skills, etc.).
        runtime_name = type(self).name()
        loaded_skills = load_skills(config.config_path, config.tools, current_runtime=runtime_name)
        seen_skill_ids = {s.metadata.id for s in loaded_skills}
        for plugin_skills_dir in plugins.skill_dirs:
            plugin_skill_names = [
                d for d in os.listdir(plugin_skills_dir)
                if os.path.isdir(os.path.join(plugin_skills_dir, d))
            ]
            for skill in load_skills(plugin_skills_dir, plugin_skill_names, current_runtime=runtime_name):
                if skill.metadata.id not in seen_skill_ids:
                    loaded_skills.append(skill)
                    seen_skill_ids.add(skill.metadata.id)
        logger.info(f"Loaded {len(loaded_skills)} skills: {[s.metadata.id for s in loaded_skills]}")

        # Assemble tools: 6 core + skill tools
        all_tools = [delegate_to_workspace, check_delegation_status, request_approval, commit_memory, search_memory, run_code]
        for skill in loaded_skills:
            all_tools.extend(skill.tools)

        # Coordinator mode: detect children and add routing tool
        children = await get_children()
        is_coordinator = len(children) > 0
        if is_coordinator:
            from coordinator import route_task_to_team
            logger.info(f"Coordinator mode: {len(children)} children")
            all_tools.append(route_task_to_team)

        # Parent context (if this is a child workspace)
        parent_context = await get_parent_context()

        # Build system prompt with all context
        peers = await get_peer_capabilities(platform_url, config.workspace_id)
        platform_instructions = await get_platform_instructions(platform_url, config.workspace_id)
        coordinator_prompt = build_children_description(children) if is_coordinator else ""
        extra_prompts = list(plugins.prompt_fragments)
        if coordinator_prompt:
            extra_prompts.append(coordinator_prompt)

        system_prompt = build_system_prompt(
            config.config_path, config.workspace_id, loaded_skills, peers,
            prompt_files=config.prompt_files,
            plugin_rules=plugins.rules,
            plugin_prompts=extra_prompts,
            parent_context=parent_context,
            platform_instructions=platform_instructions,
        )

        return SetupResult(
            system_prompt=system_prompt,
            loaded_skills=loaded_skills,
            langchain_tools=all_tools,
            is_coordinator=is_coordinator,
            children=children,
        )

    @abstractmethod
    async def setup(self, config: AdapterConfig) -> None:
        """One-time setup: validate config, prepare internal state.
        Called after deps are installed but before create_executor().
        Raise RuntimeError if setup fails (missing deps, bad config, etc.)."""
        ...  # pragma: no cover

    @abstractmethod
    async def create_executor(self, config: AdapterConfig) -> AgentExecutor:
        """Create and return an AgentExecutor ready for A2A integration.
        The returned executor's execute() method will be called by the
        A2A server's DefaultRequestHandler.

        Subclasses should also store the returned executor as ``self._executor``
        so ``pre_stop_state()`` can access it for serialization.
        """
        ...  # pragma: no cover
