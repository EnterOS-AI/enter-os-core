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


# ---------------------------------------------------------------------------
# Snapshot / golden-file tests
#
# `_render_section` produces the LLM-visible system-prompt block. The
# structural tests above guarantee tool NAMES are present; these tests
# pin the SHAPE — bullet ordering, heading style, footer placement —
# so a future contributor who reorders fields in `_render_section` or
# rewrites a `when_to_use` paragraph sees the diff in CI.
#
# To regenerate after an intentional registry edit:
#   cd workspace && WORKSPACE_ID=test-snapshot PLATFORM_URL=http://localhost \
#     python3 -c "from executor_helpers import get_a2a_instructions, get_hma_instructions; \
#                 open('tests/snapshots/a2a_instructions_mcp.txt','w').write(get_a2a_instructions(mcp=True)); \
#                 open('tests/snapshots/a2a_instructions_cli.txt','w').write(get_a2a_instructions(mcp=False)); \
#                 open('tests/snapshots/hma_instructions.txt','w').write(get_hma_instructions())"
# ---------------------------------------------------------------------------

from pathlib import Path

_SNAPSHOTS = Path(__file__).parent / "snapshots"


def _read_snapshot(name: str) -> str:
    return (_SNAPSHOTS / name).read_text(encoding="utf-8")


def test_a2a_mcp_instructions_match_snapshot():
    """Pin the rendered MCP-variant A2A doc string against the golden file."""
    from executor_helpers import get_a2a_instructions

    actual = get_a2a_instructions(mcp=True)
    expected = _read_snapshot("a2a_instructions_mcp.txt")
    assert actual == expected, (
        "get_a2a_instructions(mcp=True) drifted from snapshot. If the change "
        "is intentional, regenerate with the command in the test-file header."
    )


def test_a2a_cli_instructions_match_snapshot():
    """Pin the rendered CLI-variant A2A doc string against the golden file."""
    from executor_helpers import get_a2a_instructions

    actual = get_a2a_instructions(mcp=False)
    expected = _read_snapshot("a2a_instructions_cli.txt")
    assert actual == expected, (
        "get_a2a_instructions(mcp=False) drifted from snapshot. If the change "
        "is intentional, regenerate with the command in the test-file header."
    )


def test_hma_instructions_match_snapshot():
    """Pin the rendered HMA persistent-memory doc string against the golden file."""
    from executor_helpers import get_hma_instructions

    actual = get_hma_instructions()
    expected = _read_snapshot("hma_instructions.txt")
    assert actual == expected, (
        "get_hma_instructions() drifted from snapshot. If the change is "
        "intentional, regenerate with the command in the test-file header."
    )


# ---------------------------------------------------------------------------
# CLI-block alignment tests
#
# Registry is the source of truth for MCP-capable runtimes; the CLI
# subprocess block (`_A2A_INSTRUCTIONS_CLI`) is a SEPARATE hand-maintained
# surface for ollama and other non-MCP adapters. The two diverged
# silently in the past — `send_message_to_user` was added to the
# registry but the CLI block was never updated. These tests close that
# gap by requiring a deliberate decision (subcommand keyword OR
# explicit `None`) for every a2a tool.
# ---------------------------------------------------------------------------


def test_cli_keyword_mapping_covers_every_a2a_tool():
    """Every a2a-section registry tool must have an entry in
    `_CLI_A2A_COMMAND_KEYWORDS` — either a subcommand keyword or an
    explicit `None`. Adding a new a2a tool without updating the
    mapping fails this test, forcing the contributor to decide
    whether the CLI subprocess interface should expose it.
    """
    from executor_helpers import _CLI_A2A_COMMAND_KEYWORDS

    a2a_names = {t.name for t in a2a_tools()}
    keyed_names = set(_CLI_A2A_COMMAND_KEYWORDS.keys())

    missing = a2a_names - keyed_names
    extra = keyed_names - a2a_names
    assert not missing, (
        f"a2a tools missing from _CLI_A2A_COMMAND_KEYWORDS: {missing}. "
        f"Add a key for each — set value to the CLI subcommand keyword "
        f"or None if the tool isn't exposed via the subprocess interface."
    )
    assert not extra, (
        f"_CLI_A2A_COMMAND_KEYWORDS has keys for tools no longer in the "
        f"registry: {extra}. Remove them."
    )


def test_cli_keyword_substrings_appear_in_cli_block():
    """Every non-None subcommand keyword in `_CLI_A2A_COMMAND_KEYWORDS`
    must literally appear in `_A2A_INSTRUCTIONS_CLI`. If a CLI
    subcommand is mapped here but missing from the doc block, agents
    on CLI-only runtimes don't see the invocation syntax.
    """
    from executor_helpers import _A2A_INSTRUCTIONS_CLI, _CLI_A2A_COMMAND_KEYWORDS

    for tool_name, keyword in _CLI_A2A_COMMAND_KEYWORDS.items():
        if keyword is None:
            continue
        assert keyword in _A2A_INSTRUCTIONS_CLI, (
            f"_CLI_A2A_COMMAND_KEYWORDS[{tool_name!r}] = {keyword!r} but "
            f"that substring is missing from _A2A_INSTRUCTIONS_CLI. Either "
            f"add the subcommand to the CLI doc block or change the "
            f"mapping value to None."
        )
