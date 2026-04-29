"""Structural alignment tests — every adapter must agree with the registry.

The registry in workspace/platform_tools/registry.py is the single source
of truth for tool naming + docs. These tests fail if any consumer
(MCP server, LangChain @tool wrappers, doc generators) drifts.

If you add a tool: append a ToolSpec to registry.TOOLS, then add the
matching @tool wrapper in builtin_tools/. These tests catch the case
where the registry has a name that has no LangChain @tool counterpart
(or vice versa).

If you rename a tool: edit registry.TOOLS only. These tests fail loudly
if the LangChain @tool name or MCP TOOLS["name"] still has the old name.
"""

from __future__ import annotations

import pytest

from platform_tools.registry import TOOLS, a2a_tools, by_name, memory_tools, tool_names


def test_registry_names_are_unique():
    """Every ToolSpec must have a distinct name — duplicate is a typo."""
    names = tool_names()
    assert len(names) == len(set(names)), f"duplicate tool names: {names}"


def test_registry_a2a_and_memory_partition_is_complete():
    """Every tool belongs to exactly one section. No orphans."""
    a2a = {t.name for t in a2a_tools()}
    mem = {t.name for t in memory_tools()}
    all_names = set(tool_names())
    assert a2a | mem == all_names
    assert not (a2a & mem), f"tool in both sections: {a2a & mem}"


def test_by_name_lookup_works():
    spec = by_name("delegate_task")
    assert spec.name == "delegate_task"
    assert spec.section == "a2a"
    with pytest.raises(KeyError):
        by_name("nonexistent_tool")


def test_mcp_server_registers_every_registry_tool():
    """The MCP server's TOOLS list is built from the registry. Every
    spec must produce a corresponding entry — if not, the import-time
    list comprehension is broken or the registry has an entry the
    server isn't picking up.
    """
    from a2a_mcp_server import TOOLS as MCP_TOOLS

    mcp_names = {t["name"] for t in MCP_TOOLS}
    registry_names = set(tool_names())
    assert mcp_names == registry_names, (
        f"MCP and registry diverged. MCP-only: {mcp_names - registry_names}; "
        f"registry-only: {registry_names - mcp_names}"
    )


def test_mcp_tool_descriptions_match_registry_short():
    """Each MCP tool's description IS the registry's `short` field —
    the bullet-line description shown to the model. The deeper
    when_to_use guidance lives only in the system prompt.
    """
    from a2a_mcp_server import TOOLS as MCP_TOOLS

    by_mcp_name = {t["name"]: t for t in MCP_TOOLS}
    for spec in TOOLS:
        assert by_mcp_name[spec.name]["description"] == spec.short, (
            f"MCP description for {spec.name!r} drifted from registry.short. "
            f"Edit registry.py, not the MCP server's TOOLS list."
        )


def test_mcp_tool_input_schemas_match_registry():
    """Schemas must come from the registry, never duplicated in the server."""
    from a2a_mcp_server import TOOLS as MCP_TOOLS

    by_mcp_name = {t["name"]: t for t in MCP_TOOLS}
    for spec in TOOLS:
        assert by_mcp_name[spec.name]["inputSchema"] == spec.input_schema, (
            f"MCP inputSchema for {spec.name!r} drifted from registry."
        )


def test_a2a_instructions_text_includes_every_a2a_tool():
    """get_a2a_instructions must mention every a2a-section tool by name."""
    from executor_helpers import get_a2a_instructions

    instructions = get_a2a_instructions(mcp=True)
    for spec in a2a_tools():
        assert spec.name in instructions, (
            f"agent-facing A2A docs missing tool {spec.name!r} from registry"
        )


def test_hma_instructions_text_includes_every_memory_tool():
    """get_hma_instructions must mention every memory-section tool by name."""
    from executor_helpers import get_hma_instructions

    instructions = get_hma_instructions()
    for spec in memory_tools():
        assert spec.name in instructions, (
            f"agent-facing HMA docs missing tool {spec.name!r} from registry"
        )


def test_old_pre_rename_names_not_present_in_docs():
    """Pre-rename names (delegate_to_workspace, search_memory,
    check_delegation_status) must not leak back into the agent-facing
    docs. They're not in the registry; their absence is the canonical
    state.
    """
    from executor_helpers import get_a2a_instructions, get_hma_instructions

    blob = get_a2a_instructions(mcp=True) + get_hma_instructions()
    for stale in ("delegate_to_workspace", "search_memory", "check_delegation_status"):
        assert stale not in blob, (
            f"pre-rename name {stale!r} leaked into docs — registry "
            f"is the source of truth, not the doc generator."
        )
