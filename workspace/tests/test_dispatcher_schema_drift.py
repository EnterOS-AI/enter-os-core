"""Drift gate: every property declared in a tool's ``input_schema`` MUST
be read by the matching dispatch arm in ``a2a_mcp_server.handle_tool_call``.

Why this exists (issue #2790):
    PR #2766 added ``source_workspace_id`` to four tools' ``input_schema``
    and tool implementations, but the dispatcher in ``a2a_mcp_server.py``
    silently dropped the kwarg for ``commit_memory`` / ``recall_memory``
    / ``chat_history`` / ``get_workspace_info``. The schema lied: the LLM
    saw the parameter as valid, populated it correctly, and every call
    fell back to ``WORKSPACE_ID`` defeating multi-tenant isolation.
    Existing dispatcher tests asserted return-value substrings instead
    of kwarg flow (``"working" in result``), so the bug shipped to main.

What this test catches:
    For every ``ToolSpec`` registered in ``platform_tools.registry``
    whose ``input_schema`` declares a property ``X``, the matching
    ``elif name == "<tool_name>"`` arm in ``handle_tool_call`` must
    contain a literal string ``"X"`` passed to ``arguments.get(...)``.
    A future PR that adds a new property to the schema but forgets the
    dispatcher will fail this gate at CI time, before the bad code hits
    main.

Why an AST check, not a runtime invocation:
    The dispatcher is a long if/elif chain. Runtime invocation would
    need to mock every inner tool, then call the dispatcher with each
    name and assert the kwargs were forwarded. That's exactly what
    ``test_a2a_mcp_server.py::test_dispatch_*_forwards_source_workspace_id``
    already does for the four tools we explicitly tested. This gate is
    cheaper (~1ms) and catches the structural drift before someone has
    to remember to write the runtime test for each new property.
"""
from __future__ import annotations

import ast
from pathlib import Path

import pytest


_DISPATCHER_PATH = (
    Path(__file__).resolve().parents[1] / "a2a_mcp_server.py"
)


def _load_dispatch_arms() -> dict[str, ast.If]:
    """Parse ``a2a_mcp_server.py`` and return a mapping of tool name
    → the AST node for its ``elif name == "<tool_name>"`` arm.

    Walks the body of ``handle_tool_call`` and matches each If/elif
    branch whose test compares ``name`` against a string literal.
    """
    source = _DISPATCHER_PATH.read_text()
    tree = ast.parse(source)

    # Find handle_tool_call (sync def doesn't matter — same shape).
    handle_fn: ast.AsyncFunctionDef | None = None
    for node in ast.walk(tree):
        if isinstance(node, (ast.AsyncFunctionDef, ast.FunctionDef)) and node.name == "handle_tool_call":
            handle_fn = node  # type: ignore[assignment]
            break
    assert handle_fn is not None, "handle_tool_call not found in a2a_mcp_server.py"

    arms: dict[str, ast.If] = {}

    def _walk_if_chain(if_node: ast.If) -> None:
        # Each If has a `test` like `name == "delegate_task"` and may
        # carry an `orelse` that is either another If (elif) or a final
        # else block.
        test = if_node.test
        if (
            isinstance(test, ast.Compare)
            and len(test.ops) == 1
            and isinstance(test.ops[0], ast.Eq)
            and isinstance(test.left, ast.Name)
            and test.left.id == "name"
            and len(test.comparators) == 1
            and isinstance(test.comparators[0], ast.Constant)
            and isinstance(test.comparators[0].value, str)
        ):
            arms[test.comparators[0].value] = if_node

        if len(if_node.orelse) == 1 and isinstance(if_node.orelse[0], ast.If):
            _walk_if_chain(if_node.orelse[0])

    for stmt in handle_fn.body:
        if isinstance(stmt, ast.If):
            _walk_if_chain(stmt)
            break  # Only the top-level if/elif chain matters.

    return arms


def _extract_arguments_get_keys(arm: ast.If) -> set[str]:
    """Return every string literal passed as the first positional arg to
    a call shaped like ``arguments.get("X", ...)`` inside this arm's body.

    These represent the schema-property names this dispatch arm reads.
    A property declared in ``input_schema`` but NOT pulled by an
    ``arguments.get(...)`` call here is the drift the gate catches.
    """
    keys: set[str] = set()

    class _Visitor(ast.NodeVisitor):
        def visit_Call(self, node: ast.Call) -> None:
            # arguments.get("foo", ...) / arguments.get("foo")
            func = node.func
            if (
                isinstance(func, ast.Attribute)
                and func.attr == "get"
                and isinstance(func.value, ast.Name)
                and func.value.id == "arguments"
                and node.args
                and isinstance(node.args[0], ast.Constant)
                and isinstance(node.args[0].value, str)
            ):
                keys.add(node.args[0].value)
            self.generic_visit(node)

    visitor = _Visitor()
    # Walk only the body (not the test or orelse) so nested elifs don't
    # bleed their keys upward.
    for stmt in arm.body:
        visitor.visit(stmt)
    return keys


def _registry_tool_schemas() -> dict[str, dict]:
    """Return a mapping of ToolSpec.name → ``input_schema.properties``
    dict. Imports the registry module so this gate stays in sync with
    whatever the registry exposes (no manual list to update)."""
    from platform_tools import registry

    out: dict[str, dict] = {}
    for spec in registry.TOOLS:
        schema = spec.input_schema or {}
        props = schema.get("properties") or {}
        out[spec.name] = props
    return out


# ---------------------------------------------------------------------------
# The actual gate
# ---------------------------------------------------------------------------


def test_every_dispatch_arm_reads_every_schema_property():
    """Schema↔dispatcher drift gate. PR #2766 → PR #2771 cycle protection.

    Walks every ToolSpec in the registry, finds its dispatch arm in
    ``a2a_mcp_server.handle_tool_call``, and asserts that every property
    name declared in ``input_schema.properties`` is read by an
    ``arguments.get("<name>", ...)`` call inside that arm.

    Failure mode the gate prevents: a new schema property advertised to
    the LLM but silently dropped by the dispatcher (the exact PR #2766
    bug — schema said ``source_workspace_id`` was a valid param,
    dispatcher ignored it, every call fell back to ``WORKSPACE_ID``).
    """
    arms = _load_dispatch_arms()
    schemas = _registry_tool_schemas()

    failures: list[str] = []

    for tool_name, props in schemas.items():
        if tool_name not in arms:
            # Tool registered but not dispatched — the registry's
            # ``ALL_SPECS`` is the canonical list of MCP-exposed tools,
            # so a missing arm IS a bug. Surface it clearly.
            failures.append(
                f"Tool {tool_name!r} is registered in platform_tools.registry "
                f"but has no dispatch arm in a2a_mcp_server.handle_tool_call. "
                f"LLM clients will receive 'Unknown tool' for every call."
            )
            continue

        arm = arms[tool_name]
        read_keys = _extract_arguments_get_keys(arm)
        declared_keys = set(props.keys())
        missing = declared_keys - read_keys
        if missing:
            failures.append(
                f"Tool {tool_name!r} declares schema properties "
                f"{sorted(missing)} that the dispatch arm in "
                f"a2a_mcp_server.handle_tool_call does NOT read via "
                f"arguments.get(). The schema is lying — LLMs will pass "
                f"these parameters and the dispatcher will silently drop "
                f"them. (See PR #2766 → PR #2771 for the prior incident.)"
            )

    if failures:
        pytest.fail("\n\n".join(failures))


def test_dispatch_arms_reach_every_registered_tool():
    """Inverse direction: every dispatched tool name corresponds to a
    registered ToolSpec. Catches a dispatch arm for a tool that was
    removed from the registry (would still serve, but the schema /
    docs / wrappers wouldn't know about it).
    """
    arms = _load_dispatch_arms()
    schemas = _registry_tool_schemas()

    orphan_arms = set(arms.keys()) - set(schemas.keys())
    if orphan_arms:
        pytest.fail(
            f"Dispatch arms for {sorted(orphan_arms)} have no matching "
            f"ToolSpec in platform_tools.registry. Either remove the arm "
            f"or re-register the ToolSpec — keeping a dispatched-but-"
            f"unregistered tool means the schema, docs, and LangChain "
            f"wrappers all silently disagree with what the MCP server "
            f"actually exposes."
        )


def test_drift_gate_self_check_finds_known_arms():
    """Sanity: if the AST parsing is wrong (e.g. handle_tool_call
    refactored into a dict-dispatch), this test catches it. Pin the
    minimum-known set of dispatch arms — at least the 9 workspace-
    scoped tools shipped through PR #2766 and #2771 must be present.
    Without this, a refactor that breaks _load_dispatch_arms returns
    {} silently, and the main gate vacuously passes.
    """
    arms = _load_dispatch_arms()
    expected_minimum = {
        "delegate_task",
        "delegate_task_async",
        "check_task_status",
        "send_message_to_user",
        "list_peers",
        "get_workspace_info",
        "commit_memory",
        "recall_memory",
        "chat_history",
        "wait_for_message",
        "inbox_peek",
        "inbox_pop",
    }
    missing = expected_minimum - set(arms.keys())
    assert not missing, (
        f"AST gate failed self-check: dispatch arms {sorted(missing)} "
        f"weren't recognised by _load_dispatch_arms. Likely cause: "
        f"handle_tool_call was refactored into a different shape (dict "
        f"dispatch, registry-driven, etc.). Update this test's parser "
        f"so the main schema-drift gate still works."
    )
