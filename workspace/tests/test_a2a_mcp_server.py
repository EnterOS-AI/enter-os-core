"""Tests for a2a_mcp_server.py — handle_tool_call dispatch."""

from unittest.mock import AsyncMock, patch

import pytest


async def test_handle_tool_call_delegate_task():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_delegate_task", new=AsyncMock(return_value="delegated")):
        result = await handle_tool_call("delegate_task", {"workspace_id": "ws1", "task": "do work"})
    assert result == "delegated"


async def test_handle_tool_call_delegate_task_async():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_delegate_task_async", new=AsyncMock(return_value='{"task_id":"t1"}')):
        result = await handle_tool_call("delegate_task_async", {"workspace_id": "ws1", "task": "do work"})
    assert "t1" in result


async def test_handle_tool_call_check_task_status():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_check_task_status", new=AsyncMock(return_value='{"status":"working"}')):
        result = await handle_tool_call("check_task_status", {"workspace_id": "ws1", "task_id": "t123"})
    assert "working" in result


async def test_handle_tool_call_send_message_to_user():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_send_message_to_user", new=AsyncMock(return_value="Message sent to user")):
        result = await handle_tool_call("send_message_to_user", {"message": "Hello!"})
    assert result == "Message sent to user"


async def test_handle_tool_call_list_peers():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_list_peers", new=AsyncMock(return_value="- peer1 (ID: ws1)")):
        result = await handle_tool_call("list_peers", {})
    assert "peer1" in result


async def test_handle_tool_call_get_workspace_info():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_get_workspace_info", new=AsyncMock(return_value='{"id":"ws1"}')):
        result = await handle_tool_call("get_workspace_info", {})
    assert "ws1" in result


async def test_handle_tool_call_commit_memory():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_commit_memory", new=AsyncMock(return_value='{"success":true}')):
        result = await handle_tool_call("commit_memory", {"content": "remember this", "scope": "LOCAL"})
    assert "true" in result


async def test_handle_tool_call_recall_memory():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_recall_memory", new=AsyncMock(return_value="[LOCAL] remember this")):
        result = await handle_tool_call("recall_memory", {"query": "remember", "scope": "LOCAL"})
    assert "remember" in result


async def test_handle_tool_call_unknown_tool():
    from a2a_mcp_server import handle_tool_call
    result = await handle_tool_call("nonexistent_tool", {})
    assert "Unknown tool" in result


async def test_handle_tool_call_missing_args_defaults():
    """Test that missing args default to empty strings (defensive)."""
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_delegate_task", new=AsyncMock(return_value="ok")):
        # No workspace_id or task in arguments — defaults to ""
        result = await handle_tool_call("delegate_task", {})
    assert result == "ok"


# ---------------------------------------------------------------------------
# Tool description steering — load-bearing prompts that train the LLM to
# use structured fields instead of pasting URLs in chat (task #118).
#
# Pin specific phrases so a future doc edit that softens or drops them
# fails this test. Production symptom of regression: agent pastes
# https://files.catbox.moe/... in the message body, canvas renders it as
# a plain text link the user can't click on a SaaS deployment where the
# external host is unreachable.
# ---------------------------------------------------------------------------


def _send_message_to_user_tool() -> dict:
    from a2a_mcp_server import TOOLS
    matches = [t for t in TOOLS if t["name"] == "send_message_to_user"]
    assert len(matches) == 1, "send_message_to_user not found in TOOLS"
    return matches[0]


def test_send_message_to_user_top_description_warns_against_pasting_urls():
    desc = _send_message_to_user_tool()["description"]
    # Combined: "NEVER paste file URLs in `message`" inside the tool-level
    # description. Without this the LLM frequently pastes URLs into the
    # message body and the canvas renders a plain markdown link.
    assert "NEVER paste file URLs" in desc, (
        "send_message_to_user top description must explicitly forbid pasting "
        "file URLs in `message`. Pre-#118 the description omitted this rule "
        "and agents routinely shipped catbox.moe / file:// links in chat."
    )


def test_message_param_description_says_DO_NOT_paste_URLs():
    desc = _send_message_to_user_tool()["inputSchema"]["properties"]["message"]["description"]
    # Caps lock matters — claude-code/hermes both responded better to the
    # all-caps version in informal testing during #118 prep. If a future
    # edit lowercases it, we lose that prompt-engineering signal.
    assert "DO NOT paste file URLs" in desc, (
        "`message` param description must include the all-caps DO NOT rule"
    )
    # SaaS reachability is the WHY — operators have asked for that
    # rationale to be explicit because external file hosts work in
    # self-hosted dev but break under SaaS where the user's browser
    # can't reach the agent's outbound network.
    assert "SaaS deployments" in desc, (
        "`message` param description must explain the SaaS reachability "
        "rationale, not just the rule"
    )


def test_attachments_param_description_emphasizes_REQUIRED():
    desc = _send_message_to_user_tool()["inputSchema"]["properties"]["attachments"]["description"]
    assert "REQUIRED for any file delivery" in desc, (
        "`attachments` description must lead with REQUIRED so the LLM picks "
        "this field instead of putting paths in `message`"
    )
    # Spell out the alternatives the agent should NOT use, so the LLM has
    # an explicit list of bad patterns to avoid (instead of relying on it
    # to infer).
    for forbidden in ("pasting URLs", "base64-encoding", "telling the user to look at a path"):
        assert forbidden in desc, (
            f"`attachments` description must call out {forbidden!r} as a wrong alternative"
        )
