# Platform tool registry

Single source of truth for every tool the platform exposes to agents
(A2A delegation, hierarchical memory, broadcast, introspection).

## Why this exists

Pre-#2240, three places independently declared each tool:

1. **MCP server** (`workspace/a2a_mcp_server.py`) — the `TOOLS` JSON list
2. **LangChain `@tool` wrappers** (`workspace/builtin_tools/{delegation,memory}.py`)
3. **Agent-facing system-prompt docs** (`workspace/executor_helpers.py`)

Adding a tool to one and forgetting the others happened repeatedly. The
canonical case: `send_message_to_user` was registered in MCP TOOLS but
the executor_helpers doc string never mentioned it, so agents saw the
tool as available but had no usage guidance — a silent capability
regression.

## What the registry does

`registry.py` defines each tool ONCE as a frozen `ToolSpec`:

```python
ToolSpec(
    name="delegate_task",
    short="Delegate a task to a peer workspace via A2A and WAIT for the response.",
    when_to_use="Use for QUICK questions and small sub-tasks where you can afford to wait inline...",
    input_schema={...},          # JSON Schema, consumed by MCP server
    impl=tool_delegate_task,     # the actual coroutine
    section="a2a",               # which prompt section it belongs to
)
```

Adapters consume specs; no hardcoded names anywhere else:

- **MCP server** builds its `TOOLS` list from `_PLATFORM_TOOL_SPECS` at import time
- **LangChain `@tool` wrappers** read `name=spec.name` from the registry
- **Doc generator** (`executor_helpers._render_section()`) produces the
  system-prompt block from `spec.short` (bullet) + `spec.when_to_use`
  (heading + paragraph)

## CLI subprocess block — special case

Non-MCP runtimes (ollama, custom subprocess adapters) use a separate
hand-maintained block in `executor_helpers._A2A_INSTRUCTIONS_CLI` because
the CLI subcommand vocabulary (`peers`, `delegate`, `status`, `info`)
differs from the MCP tool names (`list_peers`, `delegate_task`, etc.).
Auto-generation would lose the readable invocation syntax.

Alignment is enforced via `_CLI_A2A_COMMAND_KEYWORDS` (in
`executor_helpers.py`): every a2a-section spec must be keyed there with
either a CLI subcommand keyword OR an explicit `None` if the tool is
intentionally not exposed via subprocess (e.g.
`send_message_to_user` because its structured `attachments` field
doesn't survive positional-arg shell invocation).

## Tests that catch drift

`workspace/tests/test_platform_tools.py`:

| Test | What it catches |
|---|---|
| `test_mcp_server_registers_every_registry_tool` | MCP TOOLS list out of sync with registry |
| `test_mcp_tool_descriptions_match_registry_short` | hand-edited MCP description that drifted |
| `test_mcp_tool_input_schemas_match_registry` | schema duplicated in server file |
| `test_a2a_instructions_text_includes_every_a2a_tool` | doc generator missed a tool |
| `test_old_pre_rename_names_not_present_in_docs` | stale name leaked back in |
| `test_a2a_mcp_instructions_match_snapshot` | rendered shape (bullet ordering, headings, footers) drifted |
| `test_a2a_cli_instructions_match_snapshot` | CLI block edited in a way that changes shape |
| `test_hma_instructions_match_snapshot` | HMA section drifted |
| `test_cli_keyword_mapping_covers_every_a2a_tool` | tool added to registry without a CLI mapping decision |
| `test_cli_keyword_substrings_appear_in_cli_block` | CLI keyword in the mapping but missing from the doc block |

The snapshot files at `workspace/tests/snapshots/*.txt` are LF-pinned
in `.gitattributes` so a Windows contributor with `core.autocrlf=true`
doesn't get mysterious test failures.

## Adding a new tool

1. Append a `ToolSpec(...)` to `TOOLS` in `registry.py`.
2. Add the LangChain `@tool` wrapper in `workspace/builtin_tools/`
   (the wrapper body just calls `spec.impl`).
3. Update `_CLI_A2A_COMMAND_KEYWORDS` in `executor_helpers.py` — set the
   value to the CLI subcommand keyword, or to `None` if the tool isn't
   exposed via the subprocess interface.
4. Regenerate snapshots — see the comment block at the top of
   `workspace/tests/test_platform_tools.py` for the one-liner.
5. Run `pytest workspace/tests/test_platform_tools.py --no-cov`.

## Renaming a tool

Edit `name` in `registry.py` only. Then:

1. The MCP TOOLS list rebuilds automatically.
2. The doc generator regenerates automatically (snapshots will fail
   the diff — regenerate them).
3. Search `workspace/` for the old literal in case a non-adapter
   consumer (tests, plugin code) hardcoded the old name; update those.
4. Update any `_CLI_A2A_COMMAND_KEYWORDS` key + the literal substring
   in `_A2A_INSTRUCTIONS_CLI` if applicable.

## Removing a tool

Delete the `ToolSpec` and the `_CLI_A2A_COMMAND_KEYWORDS` key. Adapters
and doc generators stop registering it automatically; the structural
tests prevent stale references from surviving.
