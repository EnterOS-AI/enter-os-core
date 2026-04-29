"""Tests for prompt.py — system prompt construction."""

from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from skill_loader.loader import LoadedSkill, SkillMetadata
from prompt import build_system_prompt, get_peer_capabilities


def test_build_system_prompt_with_prompt_files(tmp_path):
    """Prompt files are loaded in order and concatenated."""
    (tmp_path / "SOUL.md").write_text("You are a helpful agent.")
    (tmp_path / "TOOLS.md").write_text("You have these tools.")

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
        prompt_files=["SOUL.md", "TOOLS.md"],
    )

    assert "You are a helpful agent." in result
    assert "You have these tools." in result
    # SOUL.md should appear before TOOLS.md
    assert result.index("helpful agent") < result.index("these tools")


def test_build_system_prompt_default_fallback(tmp_path):
    """Without prompt_files, falls back to system-prompt.md."""
    (tmp_path / "system-prompt.md").write_text("Default system prompt content.")

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
    )

    assert "Default system prompt content." in result


def test_build_system_prompt_auto_includes_memory_snapshot(tmp_path):
    """Memory snapshot files are auto-included when present."""
    (tmp_path / "system-prompt.md").write_text("Base prompt.")
    (tmp_path / "MEMORY.md").write_text("Known workspace facts.")
    (tmp_path / "USER.md").write_text("User prefers concise answers.")

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
    )

    assert "Base prompt." in result
    assert "Known workspace facts." in result
    assert "User prefers concise answers." in result
    assert result.index("Base prompt.") < result.index("Known workspace facts.")
    assert result.index("Known workspace facts.") < result.index("User prefers concise answers.")


def test_build_system_prompt_deduplicates_explicit_memory_files(tmp_path):
    """Explicit snapshot files are not loaded twice."""
    (tmp_path / "system-prompt.md").write_text("Base prompt.")
    (tmp_path / "MEMORY.md").write_text("Known workspace facts.")
    (tmp_path / "USER.md").write_text("User prefers concise answers.")

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
        prompt_files=["system-prompt.md", "MEMORY.md"],
    )

    assert result.count("Known workspace facts.") == 1
    assert result.count("User prefers concise answers.") == 1


def test_build_system_prompt_missing_file(tmp_path):
    """Missing prompt files are skipped with a warning (no crash)."""
    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
        prompt_files=["nonexistent.md"],
    )

    # Should still contain the delegation failure section
    assert "Handling delegation failures" in result


def test_plugin_rules_injection(tmp_path):
    """Plugin rules are injected under '## Platform Rules'."""
    (tmp_path / "system-prompt.md").write_text("Base prompt.")

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
        plugin_rules=["Always be concise.", "Never reveal secrets."],
    )

    assert "## Platform Rules" in result
    assert "Always be concise." in result
    assert "Never reveal secrets." in result


def test_plugin_prompts_injection(tmp_path):
    """Plugin prompts are injected under '## Platform Guidelines'."""
    (tmp_path / "system-prompt.md").write_text("Base prompt.")

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
        plugin_prompts=["Use markdown formatting."],
    )

    assert "## Platform Guidelines" in result
    assert "Use markdown formatting." in result


def test_skills_listing(tmp_path):
    """Loaded skills appear with name, description, and instructions."""
    (tmp_path / "system-prompt.md").write_text("Base.")

    skills = [
        LoadedSkill(
            metadata=SkillMetadata(
                id="seo",
                name="SEO Optimization",
                description="Optimize content for search engines.",
                tags=["seo"],
                examples=["Optimize this blog post"],
            ),
            instructions="1. Analyze keywords\n2. Optimize headings",
        ),
        LoadedSkill(
            metadata=SkillMetadata(
                id="writing",
                name="Creative Writing",
                description="",
            ),
            instructions="Write creatively.",
        ),
    ]

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=skills,
        peers=[],
    )

    assert "## Your Skills" in result
    assert "### SEO Optimization" in result
    assert "Optimize content for search engines." in result
    assert "1. Analyze keywords" in result
    assert "### Creative Writing" in result
    assert "Write creatively." in result


def test_peer_capabilities_format(tmp_path):
    """Peers appear with name, id, status, and skills."""
    (tmp_path / "system-prompt.md").write_text("Base.")

    peers = [
        {
            "id": "peer-1",
            "name": "Echo Agent",
            "status": "online",
            "agent_card": {
                "name": "Echo Agent",
                "skills": [
                    {"name": "echo", "id": "echo"},
                    {"name": "repeat", "id": "repeat"},
                ],
            },
        },
        {
            "id": "peer-2",
            "name": "Silent Agent",
            "status": "offline",
            "agent_card": None,
        },
    ]

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=peers,
    )

    assert "## Your Peers" in result
    assert "**Echo Agent** (id: `peer-1`, status: online)" in result
    assert "Skills: echo, repeat" in result
    assert "delegate_task_async" in result
    # peer-2 has no agent_card but DOES have a DB name + status — must
    # still render so coordinators can delegate to freshly-created peers
    # whose A2A discovery hasn't populated a card yet (regression of the
    # 2026-04-27 Design Director discovery bug).
    assert "**Silent Agent** (id: `peer-2`, status: offline)" in result


def test_peer_with_json_string_agent_card(tmp_path):
    """agent_card as a JSON string is parsed correctly."""
    import json

    (tmp_path / "system-prompt.md").write_text("Base.")

    peers = [
        {
            "id": "peer-3",
            "name": "JSON Peer",
            "status": "online",
            "agent_card": json.dumps({
                "name": "JSON Peer",
                "skills": [{"name": "parse"}],
            }),
        },
    ]

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=peers,
    )

    assert "**JSON Peer** (id: `peer-3`, status: online)" in result
    assert "Skills: parse" in result


def test_delegation_failure_section_always_present(tmp_path):
    """The delegation failure handling section is always appended."""
    (tmp_path / "system-prompt.md").write_text("Base.")

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
    )

    assert "## Handling delegation failures" in result
    assert "Retry transient failures" in result


def test_parent_context_injection(tmp_path):
    """parent_context creates a '## Parent Context' section with file contents."""
    (tmp_path / "system-prompt.md").write_text("Base.")

    parent_context = [
        {"path": "guidelines.md", "content": "Always use type hints."},
        {"path": "architecture.md", "content": "We use hexagonal architecture."},
    ]

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
        parent_context=parent_context,
    )

    assert "## Parent Context" in result
    assert "shared by your parent workspace" in result
    assert "### guidelines.md" in result
    assert "Always use type hints." in result
    assert "### architecture.md" in result
    assert "We use hexagonal architecture." in result


def test_parent_context_empty(tmp_path):
    """No '## Parent Context' section when parent_context is an empty list."""
    (tmp_path / "system-prompt.md").write_text("Base.")

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
        parent_context=[],
    )

    assert "## Parent Context" not in result


def test_parent_context_none(tmp_path):
    """No '## Parent Context' section when parent_context is None."""
    (tmp_path / "system-prompt.md").write_text("Base.")

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
        parent_context=None,
    )

    assert "## Parent Context" not in result


def test_parent_context_skips_empty_content(tmp_path):
    """Files with empty/whitespace-only content are skipped."""
    (tmp_path / "system-prompt.md").write_text("Base.")

    parent_context = [
        {"path": "empty.md", "content": ""},
        {"path": "whitespace.md", "content": "   \n  "},
        {"path": "real.md", "content": "Real content here."},
    ]

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
        parent_context=parent_context,
    )

    assert "## Parent Context" in result
    assert "### empty.md" not in result
    assert "### whitespace.md" not in result
    assert "### real.md" in result
    assert "Real content here." in result


# ---------------------------------------------------------------------------
# get_peer_capabilities() tests
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_get_peer_capabilities_success():
    """get_peer_capabilities() returns the list from a 200 response."""
    peers = [
        {"id": "peer-1", "name": "Alpha"},
        {"id": "peer-2", "name": "Beta"},
    ]

    mock_resp = MagicMock()
    mock_resp.status_code = 200
    mock_resp.json.return_value = peers

    mock_client = AsyncMock()
    mock_client.__aenter__ = AsyncMock(return_value=mock_client)
    mock_client.__aexit__ = AsyncMock(return_value=False)
    mock_client.get = AsyncMock(return_value=mock_resp)

    # httpx is imported lazily inside get_peer_capabilities(), so patch at module level
    with patch("httpx.AsyncClient", return_value=mock_client):
        result = await get_peer_capabilities("http://platform:8080", "ws-abc")

    assert result == peers
    mock_client.get.assert_called_once_with(
        "http://platform:8080/registry/ws-abc/peers",
        headers={"X-Workspace-ID": "ws-abc"},
    )


@pytest.mark.asyncio
async def test_get_peer_capabilities_non_200():
    """get_peer_capabilities() returns [] when response status is not 200."""
    mock_resp = MagicMock()
    mock_resp.status_code = 404

    mock_client = AsyncMock()
    mock_client.__aenter__ = AsyncMock(return_value=mock_client)
    mock_client.__aexit__ = AsyncMock(return_value=False)
    mock_client.get = AsyncMock(return_value=mock_resp)

    with patch("httpx.AsyncClient", return_value=mock_client):
        result = await get_peer_capabilities("http://platform:8080", "ws-abc")

    assert result == []


@pytest.mark.asyncio
async def test_get_peer_capabilities_exception():
    """get_peer_capabilities() returns [] when httpx raises an exception."""
    mock_client = AsyncMock()
    mock_client.__aenter__ = AsyncMock(return_value=mock_client)
    mock_client.__aexit__ = AsyncMock(return_value=False)
    mock_client.get = AsyncMock(side_effect=Exception("Network unreachable"))

    with patch("httpx.AsyncClient", return_value=mock_client):
        result = await get_peer_capabilities("http://platform:8080", "ws-abc")

    assert result == []


# Regression tests for the A2A + HMA tool-instruction injection. Pre-fix,
# get_a2a_instructions() and get_hma_instructions() were defined in
# executor_helpers.py but never called from build_system_prompt — workers
# saw the platform's delegate_task / commit_memory tools registered but
# had no documentation telling them how to use them.

def test_a2a_instructions_injected_default_mcp(tmp_path):
    """build_system_prompt embeds A2A MCP-variant instructions by default."""
    (tmp_path / "system-prompt.md").write_text("Base.")

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
    )

    assert "## Inter-Agent Communication" in result
    assert "delegate_task" in result
    assert "list_peers" in result
    assert "send_message_to_user" in result


def test_a2a_instructions_cli_variant_when_disabled(tmp_path):
    """a2a_mcp=False emits the CLI subprocess variant for non-MCP runtimes."""
    (tmp_path / "system-prompt.md").write_text("Base.")

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
        a2a_mcp=False,
    )

    assert "## Inter-Agent Communication" in result
    assert "molecule_runtime.a2a_cli" in result
    # MCP-only details must NOT leak into the CLI variant.
    assert "send_message_to_user" not in result


def test_hma_instructions_injected(tmp_path):
    """build_system_prompt embeds HMA persistent-memory instructions."""
    (tmp_path / "system-prompt.md").write_text("Base.")

    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=[],
    )

    assert "## Hierarchical Memory (HMA)" in result
    assert "commit_memory" in result
    assert "recall_memory" in result


def test_tool_instructions_precede_peer_section(tmp_path):
    """A2A docs must precede the peer list — peer IDs are operands of A2A tools."""
    (tmp_path / "system-prompt.md").write_text("Base.")

    peers = [{"id": "p1", "name": "Worker", "status": "active", "agent_card": None}]
    result = build_system_prompt(
        config_path=str(tmp_path),
        workspace_id="ws-1",
        loaded_skills=[],
        peers=peers,
    )

    a2a_idx = result.index("## Inter-Agent Communication")
    peers_idx = result.index("## Your Peers")
    assert a2a_idx < peers_idx, "A2A instructions must come before the peer list"
