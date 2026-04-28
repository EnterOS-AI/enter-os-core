"""Build the system prompt for the workspace agent."""

import logging
import os
from pathlib import Path

from executor_helpers import get_a2a_instructions, get_hma_instructions
from skill_loader.loader import LoadedSkill
from shared_runtime import build_peer_section

logger = logging.getLogger(__name__)

DEFAULT_MEMORY_SNAPSHOT_FILES = ("MEMORY.md", "USER.md")


async def get_peer_capabilities(platform_url: str, workspace_id: str) -> list[dict]:
    """Fetch peer workspace capabilities from the platform."""
    try:
        import httpx

        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.get(
                f"{platform_url}/registry/{workspace_id}/peers",
                headers={"X-Workspace-ID": workspace_id},
            )
            if resp.status_code == 200:
                return resp.json()
    except Exception as e:
        print(f"Warning: could not fetch peers: {e}")
    return []


async def get_platform_instructions(platform_url: str, workspace_id: str) -> str:
    """Fetch resolved platform instructions (global + workspace scope).

    Endpoint is gated by WorkspaceAuth — the workspace token (read from env)
    is sent as a bearer header. Fails open (returns "") on any error so a
    platform outage doesn't block agent startup. Short timeout (3s) because
    this runs in the boot hot path.
    """
    try:
        import httpx

        token = os.environ.get("MOLECULE_WORKSPACE_TOKEN", "")
        headers = {"X-Workspace-ID": workspace_id}
        if token:
            headers["Authorization"] = f"Bearer {token}"

        async with httpx.AsyncClient(timeout=3.0) as client:
            resp = await client.get(
                f"{platform_url}/workspaces/{workspace_id}/instructions/resolve",
                headers=headers,
            )
            if resp.status_code == 200:
                data = resp.json()
                return data.get("instructions", "")
    except Exception as e:
        logger.warning("could not fetch platform instructions: %s", e)
    return ""


def build_system_prompt(
    config_path: str,
    workspace_id: str,
    loaded_skills: list[LoadedSkill],
    peers: list[dict],
    prompt_files: list[str] | None = None,
    plugin_rules: list[str] | None = None,
    plugin_prompts: list[str] | None = None,
    parent_context: list[dict] | None = None,
    platform_instructions: str = "",
    a2a_mcp: bool = True,
) -> str:
    """Build the complete system prompt.

    Loads prompt files in order from config_path. If prompt_files is specified
    in config.yaml, those files are loaded in order. Otherwise falls back to
    system-prompt.md for backwards compatibility.
    If MEMORY.md or USER.md exist alongside the config, they are appended as a
    frozen memory snapshot without needing to list them explicitly.

    This allows different agent frameworks to use their own file structures:
    - OpenClaw: SOUL.md, BOOTSTRAP.md, AGENTS.md, HEARTBEAT.md, TOOLS.md, USER.md
    - Claude Code: CLAUDE.md
    - Default: system-prompt.md
    """
    parts = []

    # Platform instructions (global → team → workspace scope) go first so
    # they take highest precedence in the context window.
    if platform_instructions:
        parts.append("# Platform Instructions\n")
        parts.append(platform_instructions)

    # Load prompt files in order
    files_to_load = list(prompt_files or [])
    if not files_to_load:
        # Backwards compatible: fall back to system-prompt.md
        files_to_load = ["system-prompt.md"]

    seen_files = set(files_to_load)

    for filename in files_to_load:
        file_path = Path(config_path) / filename
        if file_path.exists():
            content = file_path.read_text().strip()
            if content:
                parts.append(content)
        else:
            print(f"Warning: prompt file not found: {file_path}")

    # Hermes-style memory snapshot files: load automatically when present.
    # These stay as thin markdown files so the runtime does not need a new storage layer.
    for filename in DEFAULT_MEMORY_SNAPSHOT_FILES:
        if filename in seen_files:
            continue
        file_path = Path(config_path) / filename
        if file_path.exists():
            content = file_path.read_text().strip()
            if content:
                parts.append(content)

    # Inject parent's shared context (if this workspace is a child)
    if parent_context:
        parts.append("\n## Parent Context\n")
        parts.append("The following context was shared by your parent workspace:\n")
        for ctx_file in parent_context:
            path = ctx_file.get("path", "unknown")
            content = ctx_file.get("content", "")
            if content.strip():
                parts.append(f"### {path}")
                parts.append(content.strip())
                parts.append("")

    # Inject plugin rules (always-on guidelines from ECC, Superpowers, etc.)
    if plugin_rules:
        parts.append("\n## Platform Rules\n")
        for rule in plugin_rules:
            parts.append(rule)
            parts.append("")

    # Inject plugin prompt fragments
    if plugin_prompts:
        parts.append("\n## Platform Guidelines\n")
        for fragment in plugin_prompts:
            parts.append(fragment)
            parts.append("")

    # Add skill instructions
    if loaded_skills:
        parts.append("\n## Your Skills\n")
        for skill in loaded_skills:
            parts.append(f"### {skill.metadata.name}")
            if skill.metadata.description:
                parts.append(skill.metadata.description)
            parts.append(skill.instructions)
            parts.append("")

    # Platform tool instructions: A2A (inter-agent communication) and HMA
    # (persistent memory). These document how to call delegate_task,
    # commit_memory, etc — without them, agents see the tools registered
    # but have no instructions on when/how to use them. Placed between
    # Skills and Peers so the A2A docs precede the peer list (which is
    # the data shape the A2A tools operate over).
    #
    # a2a_mcp=True: MCP tool variant (claude-code, hermes, langchain,
    # crewai). a2a_mcp=False: CLI subprocess variant (ollama, custom
    # runtimes that don't speak MCP). Default True matches the
    # MCP-capable majority; CLI-only adapters override at the call site.
    parts.append(get_a2a_instructions(mcp=a2a_mcp))
    parts.append(get_hma_instructions())

    # Add peer capabilities with a single shared renderer.
    peer_section = build_peer_section(peers)
    if peer_section:
        parts.append(peer_section)

    # Add delegation failure handling
    parts.append("""
## Handling delegation failures
If a delegation fails:
1. Check if the task is blocking — if not, continue other work
2. Retry transient failures (connection errors) after 30 seconds
3. For persistent failures, report to the caller with context
4. Never silently drop a failed task
""")

    return "\n".join(parts)
