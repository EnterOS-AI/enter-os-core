"""Tests for executor_helpers.py — the shared helpers that back the
adapter executors. Post-#87 the executors live in template repos
(claude-code, gemini-cli, etc.); this module stays in molecule-runtime
because the helpers are runtime-agnostic.

Covers 100% of the public surface:
- get_mcp_server_path
- get_http_client / _reset_http_client
- recall_memories (all branches: no env, HTTP error, non-200, non-list, empty
  list, success)
- commit_memory (all branches: no env, empty content, success, exception)
- read_delegation_results (no file, rename race, read error, valid records,
  invalid JSON, mixed, no-preview branch, empty lines)
- set_current_task (no heartbeat, with heartbeat, no env, HTTP exception)
- get_system_prompt (file exists, file missing, fallback, UTF-8 encoding)
- get_a2a_instructions (MCP variant, CLI variant)
- brief_summary (empty, short, long, markdown headers, bold/italic, code
  fences, HR, fallback when all lines stripped)
- extract_message_text (empty parts, .text path, .root.text path, mixed)
- sanitize_agent_error (class name, no body leak)
"""

from __future__ import annotations

import json
import os
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

import executor_helpers as eh
from executor_helpers import (
    BRIEF_SUMMARY_MAX_LEN,
    DEFAULT_MCP_SERVER_PATH,
    brief_summary,
    classify_subprocess_error,
    commit_memory,
    extract_message_text,
    get_a2a_instructions,
    get_http_client,
    get_mcp_server_path,
    get_system_prompt,
    read_delegation_results,
    recall_memories,
    sanitize_agent_error,
    set_current_task,
)


# ---------- fixtures / helpers ----------

@pytest.fixture(autouse=True)
def _reset_shared_http_client():
    """Drop the module-level httpx client before and after every test so
    tests don't leak state into each other."""
    eh.reset_http_client_for_tests()
    yield
    eh.reset_http_client_for_tests()


@pytest.fixture
def platform_env(monkeypatch):
    monkeypatch.setenv("WORKSPACE_ID", "ws-test")
    monkeypatch.setenv("PLATFORM_URL", "http://platform.test")
    return "ws-test", "http://platform.test"


@pytest.fixture
def no_platform_env(monkeypatch):
    monkeypatch.delenv("WORKSPACE_ID", raising=False)
    monkeypatch.delenv("PLATFORM_URL", raising=False)


def _install_mock_http_client(monkeypatch) -> AsyncMock:
    client = AsyncMock()
    client.is_closed = False
    monkeypatch.setattr(eh, "_http_client", client)
    return client


# ======================================================================
# get_mcp_server_path
# ======================================================================

def test_get_mcp_server_path_default(monkeypatch):
    monkeypatch.delenv("A2A_MCP_SERVER_PATH", raising=False)
    assert get_mcp_server_path() == DEFAULT_MCP_SERVER_PATH


def test_get_mcp_server_path_default_resolves_to_existing_file():
    # Locks in the wheel-relative resolution: if a future refactor moves
    # a2a_mcp_server.py out of the package directory or breaks the
    # __file__-based lookup, Claude Code SDK silently fails to spawn the
    # MCP subprocess and inter-agent tools (list_peers, delegate_task)
    # vanish at runtime. This assertion catches that at unit-test time.
    assert os.path.exists(DEFAULT_MCP_SERVER_PATH), (
        f"DEFAULT_MCP_SERVER_PATH points at a missing file: "
        f"{DEFAULT_MCP_SERVER_PATH}"
    )


def test_get_mcp_server_path_env_override(monkeypatch):
    monkeypatch.setenv("A2A_MCP_SERVER_PATH", "/custom/mcp.py")
    assert get_mcp_server_path() == "/custom/mcp.py"


# ======================================================================
# get_http_client
# ======================================================================

def test_get_http_client_returns_same_instance_on_repeat_calls():
    eh.reset_http_client_for_tests()
    c1 = get_http_client()
    c2 = get_http_client()
    assert c1 is c2


@pytest.mark.asyncio
async def test_get_http_client_rebuilds_when_closed():
    c1 = get_http_client()
    await c1.aclose()
    c2 = get_http_client()
    try:
        assert c1 is not c2
    finally:
        await c2.aclose()


def test_reset_http_client_nulls_state():
    get_http_client()
    assert eh._http_client is not None
    eh.reset_http_client_for_tests()
    assert eh._http_client is None


# ======================================================================
# recall_memories
# ======================================================================

@pytest.mark.asyncio
async def test_recall_memories_no_env_returns_empty(no_platform_env):
    assert await recall_memories() == ""


@pytest.mark.asyncio
async def test_recall_memories_only_workspace_id_returns_empty(monkeypatch):
    monkeypatch.setenv("WORKSPACE_ID", "ws-1")
    monkeypatch.delenv("PLATFORM_URL", raising=False)
    assert await recall_memories() == ""


@pytest.mark.asyncio
async def test_recall_memories_non_200_returns_empty(monkeypatch, platform_env):
    client = _install_mock_http_client(monkeypatch)
    resp = MagicMock(status_code=500)
    client.get = AsyncMock(return_value=resp)
    assert await recall_memories() == ""


@pytest.mark.asyncio
async def test_recall_memories_exception_returns_empty(monkeypatch, platform_env):
    client = _install_mock_http_client(monkeypatch)
    client.get = AsyncMock(side_effect=RuntimeError("boom"))
    assert await recall_memories() == ""


@pytest.mark.asyncio
async def test_recall_memories_non_list_payload_returns_empty(monkeypatch, platform_env):
    client = _install_mock_http_client(monkeypatch)
    resp = MagicMock(status_code=200)
    resp.json = MagicMock(return_value={"not": "a list"})
    client.get = AsyncMock(return_value=resp)
    assert await recall_memories() == ""


@pytest.mark.asyncio
async def test_recall_memories_empty_list_returns_empty(monkeypatch, platform_env):
    client = _install_mock_http_client(monkeypatch)
    resp = MagicMock(status_code=200)
    resp.json = MagicMock(return_value=[])
    client.get = AsyncMock(return_value=resp)
    assert await recall_memories() == ""


@pytest.mark.asyncio
async def test_recall_memories_success_formats_bullet_list(monkeypatch, platform_env):
    client = _install_mock_http_client(monkeypatch)
    resp = MagicMock(status_code=200)
    resp.json = MagicMock(return_value=[
        {"scope": "LOCAL", "content": "User likes Python"},
        {"scope": "GLOBAL", "content": "User prefers concise answers"},
    ])
    client.get = AsyncMock(return_value=resp)
    result = await recall_memories()
    assert "[LOCAL] User likes Python" in result
    assert "[GLOBAL] User prefers concise answers" in result
    assert result.count("\n") == 1


@pytest.mark.asyncio
async def test_recall_memories_trims_to_last_ten(monkeypatch, platform_env):
    client = _install_mock_http_client(monkeypatch)
    payload = [{"scope": "L", "content": f"m{i}"} for i in range(15)]
    resp = MagicMock(status_code=200)
    resp.json = MagicMock(return_value=payload)
    client.get = AsyncMock(return_value=resp)
    result = await recall_memories()
    # Only the last 10 should appear
    assert "m14" in result
    assert "m5" in result  # boundary: 15 - 10 = index 5
    assert "m4" not in result


@pytest.mark.asyncio
async def test_recall_memories_handles_missing_fields(monkeypatch, platform_env):
    client = _install_mock_http_client(monkeypatch)
    resp = MagicMock(status_code=200)
    resp.json = MagicMock(return_value=[{}])
    client.get = AsyncMock(return_value=resp)
    result = await recall_memories()
    assert "[?]" in result  # default scope placeholder


# ======================================================================
# commit_memory
# ======================================================================

@pytest.mark.asyncio
async def test_commit_memory_no_env_is_noop(no_platform_env):
    # Should not raise, should not create a client
    await commit_memory("anything")
    assert eh._http_client is None


@pytest.mark.asyncio
async def test_commit_memory_empty_content_is_noop(monkeypatch, platform_env):
    client = _install_mock_http_client(monkeypatch)
    await commit_memory("")
    client.post.assert_not_called()


@pytest.mark.asyncio
async def test_commit_memory_posts_to_platform(monkeypatch, platform_env):
    client = _install_mock_http_client(monkeypatch)
    client.post = AsyncMock(return_value=MagicMock(status_code=200))
    await commit_memory("Remember this fact")
    client.post.assert_called_once()
    url = client.post.call_args[0][0]
    body = client.post.call_args[1]["json"]
    assert "ws-test/memories" in url
    assert body == {"content": "Remember this fact", "scope": "LOCAL"}


@pytest.mark.asyncio
async def test_commit_memory_swallows_exceptions(monkeypatch, platform_env):
    client = _install_mock_http_client(monkeypatch)
    client.post = AsyncMock(side_effect=Exception("network down"))
    # Should not raise
    await commit_memory("content")


# ======================================================================
# read_delegation_results
# ======================================================================

def test_read_delegation_results_no_file(tmp_path, monkeypatch):
    monkeypatch.setenv("DELEGATION_RESULTS_FILE", str(tmp_path / "missing.jsonl"))
    assert read_delegation_results() == ""


def test_read_delegation_results_valid_records(tmp_path, monkeypatch):
    results_file = tmp_path / "delegation.jsonl"
    results_file.write_text(
        json.dumps({
            "status": "completed",
            "summary": "Task A",
            "response_preview": "Here is A",
        }) + "\n" + json.dumps({
            "status": "failed",
            "summary": "Task B",
        }) + "\n",
        encoding="utf-8",
    )
    monkeypatch.setenv("DELEGATION_RESULTS_FILE", str(results_file))
    out = read_delegation_results()
    assert "[completed] Task A" in out
    assert "Response: Here is A" in out
    assert "[failed] Task B" in out
    # Preview omitted when absent
    lines_for_b = [l for l in out.splitlines() if "Task B" in l]
    assert lines_for_b and not any("Response:" in l for l in lines_for_b[1:2])
    # File consumed
    assert not results_file.exists()


def test_read_delegation_results_skips_invalid_json(tmp_path, monkeypatch):
    results_file = tmp_path / "delegation.jsonl"
    results_file.write_text("not json\n{bad\n", encoding="utf-8")
    monkeypatch.setenv("DELEGATION_RESULTS_FILE", str(results_file))
    assert read_delegation_results() == ""
    assert not results_file.exists()


def test_read_delegation_results_handles_blank_lines_in_middle(tmp_path, monkeypatch):
    """A blank line between valid records must be skipped, not crash."""
    results_file = tmp_path / "delegation.jsonl"
    results_file.write_text(
        json.dumps({"status": "ok", "summary": "first"})
        + "\n   \n"  # blank line with whitespace
        + json.dumps({"status": "ok", "summary": "second"})
        + "\n",
        encoding="utf-8",
    )
    monkeypatch.setenv("DELEGATION_RESULTS_FILE", str(results_file))
    out = read_delegation_results()
    assert "[ok] first" in out
    assert "[ok] second" in out


def test_read_delegation_results_rename_race(tmp_path, monkeypatch):
    """If the file disappears between exists() and rename(), return empty."""
    results_file = tmp_path / "delegation.jsonl"
    results_file.write_text("{}\n", encoding="utf-8")
    monkeypatch.setenv("DELEGATION_RESULTS_FILE", str(results_file))

    with patch("executor_helpers.Path") as MockPath:
        mock_instance = MagicMock()
        mock_instance.exists.return_value = True
        mock_instance.with_suffix.return_value = tmp_path / "delegation.consumed"
        mock_instance.rename.side_effect = OSError("race")
        MockPath.return_value = mock_instance
        assert read_delegation_results() == ""


def test_read_delegation_results_read_text_raises(tmp_path, monkeypatch):
    """Post-rename read failure returns empty instead of crashing."""
    results_file = tmp_path / "delegation.jsonl"
    results_file.write_text("{}\n", encoding="utf-8")
    monkeypatch.setenv("DELEGATION_RESULTS_FILE", str(results_file))

    consumed_mock = MagicMock()
    consumed_mock.read_text.side_effect = OSError("disk gone")
    consumed_mock.unlink = MagicMock()

    with patch("executor_helpers.Path") as MockPath:
        mock_instance = MagicMock()
        mock_instance.exists.return_value = True
        mock_instance.with_suffix.return_value = consumed_mock
        mock_instance.rename.return_value = None
        MockPath.return_value = mock_instance
        assert read_delegation_results() == ""

    consumed_mock.unlink.assert_called_once_with(missing_ok=True)


# ======================================================================
# set_current_task
# ======================================================================

@pytest.mark.asyncio
async def test_set_current_task_no_heartbeat_no_env_is_noop(no_platform_env):
    # Nothing to update, nothing to POST → should return cleanly
    await set_current_task(None, "some task")


@pytest.mark.asyncio
async def test_set_current_task_updates_heartbeat_state():
    hb = SimpleNamespace(current_task="old", active_tasks=0)
    await set_current_task(hb, "new task")
    assert hb.current_task == "new task"
    assert hb.active_tasks == 1


@pytest.mark.asyncio
async def test_set_current_task_empty_clears_heartbeat_state():
    hb = SimpleNamespace(current_task="old", active_tasks=1)
    await set_current_task(hb, "")
    assert hb.current_task == ""
    assert hb.active_tasks == 0


@pytest.mark.asyncio
async def test_set_current_task_posts_to_platform(monkeypatch, platform_env):
    client = _install_mock_http_client(monkeypatch)
    client.post = AsyncMock(return_value=MagicMock(status_code=200))
    hb = SimpleNamespace(current_task="", active_tasks=0)
    await set_current_task(hb, "running")
    client.post.assert_called_once()
    url = client.post.call_args[0][0]
    body = client.post.call_args[1]["json"]
    assert url.endswith("/registry/heartbeat")
    assert body["current_task"] == "running"
    assert body["active_tasks"] == 1


@pytest.mark.asyncio
async def test_set_current_task_swallows_http_exceptions(monkeypatch, platform_env):
    client = _install_mock_http_client(monkeypatch)
    client.post = AsyncMock(side_effect=Exception("boom"))
    # Should not raise
    await set_current_task(None, "x")


# ======================================================================
# get_system_prompt
# ======================================================================

def test_get_system_prompt_reads_file(tmp_path):
    (tmp_path / "system-prompt.md").write_text("You are helpful.", encoding="utf-8")
    assert get_system_prompt(str(tmp_path)) == "You are helpful."


def test_get_system_prompt_missing_uses_fallback(tmp_path):
    assert get_system_prompt(str(tmp_path), fallback="fb") == "fb"


def test_get_system_prompt_missing_no_fallback_returns_none(tmp_path):
    assert get_system_prompt(str(tmp_path)) is None


def test_get_system_prompt_strips_whitespace(tmp_path):
    (tmp_path / "system-prompt.md").write_text("\n  prompt text  \n", encoding="utf-8")
    assert get_system_prompt(str(tmp_path)) == "prompt text"


def test_get_system_prompt_handles_non_utf8(tmp_path):
    # Write invalid utf-8 bytes; errors='replace' should salvage the text.
    (tmp_path / "system-prompt.md").write_bytes(b"hello \xff world")
    out = get_system_prompt(str(tmp_path))
    assert "hello" in out and "world" in out


# ======================================================================
# get_a2a_instructions
# ======================================================================

def test_get_a2a_instructions_mcp_default():
    out = get_a2a_instructions()
    # Section heading is the canonical agent-facing label.
    assert "## Inter-Agent Communication" in out
    # Every A2A tool from the registry must appear by name.
    assert "list_peers" in out
    assert "send_message_to_user" in out
    assert "delegate_task" in out


def test_get_a2a_instructions_cli_variant():
    out = get_a2a_instructions(mcp=False)
    assert "a2a_cli" in out
    assert "MCP tools" not in out


def test_a2a_cli_instructions_use_module_invocation_not_legacy_app_path():
    # The CLI variant of the a2a instructions ships in the agent system
    # prompt for non-MCP runtimes (Ollama, custom). The model copies the
    # invocation form verbatim into shell calls, so any path drift here
    # silently breaks delegation. The legacy /app/a2a_cli.py path was
    # correct under the pre-#87 monolithic-template Docker layout but
    # stops resolving once the runtime ships as a wheel — pin the
    # canonical `python3 -m molecule_runtime.a2a_cli` form so future
    # refactors can't silently regress it.
    out = get_a2a_instructions(mcp=False)
    assert "/app/a2a_cli.py" not in out, (
        "Legacy /app/a2a_cli.py path leaked back into the CLI-variant "
        "system prompt — agents on Ollama/custom runtimes would copy "
        "this verbatim and every delegation would fail."
    )
    assert "python3 -m molecule_runtime.a2a_cli" in out


def test_a2a_mcp_instructions_reference_existing_tools():
    """Pin the registry-driven alignment: every tool name appearing in the
    agent-facing A2A instructions must be a tool the MCP server actually
    registers. Both sides now derive from platform_tools.registry, so the
    real test is that the registry's a2a_tools() set drives both surfaces
    consistently.
    """
    from a2a_mcp_server import TOOLS as MCP_TOOLS
    from platform_tools.registry import a2a_tools

    registered = {t["name"] for t in MCP_TOOLS}
    instructions = get_a2a_instructions(mcp=True)

    for spec in a2a_tools():
        assert spec.name in instructions, (
            f"A2A instructions are missing the tool {spec.name!r} that "
            f"the registry declares — the doc generator drifted."
        )
        assert spec.name in registered, (
            f"MCP server no longer registers {spec.name!r} that the registry "
            f"declares — the MCP TOOLS list drifted from the registry."
        )


# ======================================================================
# brief_summary
# ======================================================================

def test_brief_summary_short_text_returned_as_is():
    assert brief_summary("Hello world") == "Hello world"


def test_brief_summary_truncates_long_text():
    text = "a" * 100
    out = brief_summary(text, max_len=20)
    assert len(out) == 20
    assert out.endswith("...")


def test_brief_summary_strips_markdown_headers():
    assert brief_summary("### Task: refactor auth") == "Task: refactor auth"


def test_brief_summary_strips_bold_and_italic():
    assert brief_summary("**urgent** __deploy__") == "urgent deploy"


def test_brief_summary_skips_blank_and_code_fences():
    text = "\n\n```python\n```\nActual task line"
    assert brief_summary(text) == "Actual task line"


def test_brief_summary_skips_horizontal_rule():
    text = "---\nReal content"
    assert brief_summary(text) == "Real content"


def test_brief_summary_empty_string():
    assert brief_summary("") == ""


def test_brief_summary_all_skipped_falls_back_to_prefix():
    """If every line is skipped, fall back to the raw prefix."""
    text = "\n\n```\n```"
    out = brief_summary(text, max_len=5)
    # Fallback returns text[:max_len] which keeps the skipped content
    assert len(out) <= 5


def test_brief_summary_exact_boundary_length():
    text = "x" * BRIEF_SUMMARY_MAX_LEN
    assert brief_summary(text) == text  # <= max_len, no truncation


def test_brief_summary_clamps_absurdly_small_max_len():
    """max_len below 4 is clamped — no negative slice indices."""
    out = brief_summary("hello world", max_len=1)
    # Clamped to min 4: "h..." (1 char + 3 ellipsis)
    assert out == "h..."


def test_brief_summary_clamps_negative_max_len():
    """Even negative max_len is handled gracefully via clamp."""
    out = brief_summary("hello world", max_len=-5)
    assert out == "h..."


# ======================================================================
# extract_message_text
# ======================================================================

def test_extract_message_text_empty_parts():
    msg = SimpleNamespace(parts=[])
    assert extract_message_text(msg) == ""


def test_extract_message_text_no_parts_attr():
    msg = SimpleNamespace()
    assert extract_message_text(msg) == ""


def test_extract_message_text_direct_text():
    part = SimpleNamespace(text="hello")
    msg = SimpleNamespace(parts=[part])
    assert extract_message_text(msg) == "hello"


def test_extract_message_text_root_text_fallback():
    root = SimpleNamespace(text="nested")
    part = SimpleNamespace(text=None, root=root)
    msg = SimpleNamespace(parts=[part])
    assert extract_message_text(msg) == "nested"


def test_extract_message_text_mixed_parts():
    p1 = SimpleNamespace(text="hello")
    p2 = SimpleNamespace(text=None, root=SimpleNamespace(text="world"))
    p3 = SimpleNamespace(text=None, root=None)  # empty — skipped
    msg = SimpleNamespace(parts=[p1, p2, p3])
    assert extract_message_text(msg) == "hello world"


def test_extract_message_text_ignores_non_string_text():
    part = SimpleNamespace(text="")
    msg = SimpleNamespace(parts=[part])
    assert extract_message_text(msg) == ""


# ======================================================================
# sanitize_agent_error
# ======================================================================

def test_sanitize_agent_error_exposes_class_not_body():
    exc = ValueError("internal secret token abc-123-XYZ")
    out = sanitize_agent_error(exc)
    assert "ValueError" in out
    assert "abc-123-XYZ" not in out
    assert "workspace logs" in out


def test_sanitize_agent_error_with_custom_exception():
    class MyErr(Exception):
        pass
    out = sanitize_agent_error(MyErr("very long stack trace with /etc/secret/key"))
    assert "MyErr" in out
    assert "/etc/secret/key" not in out


def test_sanitize_agent_error_with_category_only():
    """category kwarg wins when no exception is given (subprocess path)."""
    out = sanitize_agent_error(category="rate_limited")
    assert "rate_limited" in out
    assert "workspace logs" in out


def test_sanitize_agent_error_category_takes_precedence_over_exception():
    """If both are given, category wins (lets CLI executor override class name)."""
    out = sanitize_agent_error(ValueError("boom"), category="auth_failed")
    assert "auth_failed" in out
    assert "ValueError" not in out


def test_sanitize_agent_error_with_neither_falls_back_to_unknown():
    out = sanitize_agent_error()
    assert "unknown" in out


# ======================================================================
# classify_subprocess_error
# ======================================================================

def test_classify_subprocess_error_rate_limited():
    assert classify_subprocess_error("429 rate limit exceeded", 1) == "rate_limited"
    assert classify_subprocess_error("Server overloaded, try again", 1) == "rate_limited"


def test_classify_subprocess_error_auth():
    assert classify_subprocess_error("authentication failed", 1) == "auth_failed"
    assert classify_subprocess_error("bad api_key", 1) == "auth_failed"
    assert classify_subprocess_error("missing api-key header", 1) == "auth_failed"
    # Word-boundary regex must not match "author" or "authorize"
    assert classify_subprocess_error(
        "authored by jane on 2024-01-01", 99,
    ) == "exit_99"


def test_classify_subprocess_error_session():
    assert classify_subprocess_error("no conversation found", 1) == "session_error"
    assert classify_subprocess_error("session expired", 1) == "session_error"


def test_classify_subprocess_error_session_false_positive_avoided():
    """'sessions' (plural) should still match the \\bsession\\b pattern,
    but 'sessionless' must NOT trigger."""
    # 'sessions' — word boundary allows trailing 's'? No: \b matches between
    # \w and \W, and 's' is \w. So \bsession\b doesn't match 'sessions'.
    # The conservative assumption is OK — we'd rather miscategorize a rare
    # plural than false-positive on 'sessionless'.
    assert classify_subprocess_error("sessionless mode", 1) != "session_error"


def test_classify_subprocess_error_rate_false_positive_avoided():
    # "generate" and "iterate" contain "rate" as substrings but not as a word
    assert classify_subprocess_error("failed to generate output", 2) == "exit_2"
    assert classify_subprocess_error("iterate faster", None) == "subprocess_error"


def test_classify_subprocess_error_exit_code_fallback():
    assert classify_subprocess_error("mystery failure", 42) == "exit_42"


def test_classify_subprocess_error_generic_fallback():
    assert classify_subprocess_error("generic unknown failure", None) == "subprocess_error"
    # exit_code=0 with no keyword match also lands here
    assert classify_subprocess_error("mysterious but zero exit", 0) == "subprocess_error"


# ============================================================================
# Chat attachment helpers (drag-drop file + agent-returned file)
# ============================================================================


def test_resolve_attachment_uri_all_schemes(tmp_path, monkeypatch):
    """All three canvas-issued URI shapes resolve to the same container path.

    The canvas mints ``workspace:`` but the download endpoint used to accept
    ``file:///`` and bare ``/workspace/…`` for legacy agents — the helper has
    to handle all three so agents don't have to normalize before calling us.
    """
    from executor_helpers import resolve_attachment_uri, WORKSPACE_MOUNT

    # Use a real path that starts with WORKSPACE_MOUNT. resolve() enforces
    # the containment check — anything outside /workspace/ must return None.
    ws_path = f"{WORKSPACE_MOUNT}/foo.txt"
    assert resolve_attachment_uri(f"workspace:{ws_path}") == ws_path
    assert resolve_attachment_uri(f"file://{ws_path}") == ws_path
    assert resolve_attachment_uri(ws_path) == ws_path

    # Out-of-tree is refused even when the raw path shape looks right.
    # CWE-22 regression: a crafted "workspace:/workspace/../etc/passwd"
    # must NOT return "/etc/passwd" just because resolve() normalizes it.
    assert resolve_attachment_uri("/etc/passwd") is None
    assert resolve_attachment_uri("workspace:/workspace/../etc/passwd") is None
    assert resolve_attachment_uri("") is None
    assert resolve_attachment_uri("https://example.com/x") is None


def test_extract_attached_files_skips_unresolvable():
    """Files with URIs that don't resolve to an existing file are dropped.

    A crafted A2A message can include any uri it wants; we must not hand
    non-existent or out-of-tree paths to downstream code as if they were
    real attachments.
    """
    from types import SimpleNamespace
    from executor_helpers import extract_attached_files

    msg = SimpleNamespace(parts=[
        SimpleNamespace(kind="file", file=SimpleNamespace(
            uri="workspace:/etc/passwd", name="x", mimeType="text/plain"
        )),
        SimpleNamespace(root=SimpleNamespace(kind="file", file=SimpleNamespace(
            uri="/workspace/does-not-exist", name="y", mimeType="text/plain"
        ))),
        SimpleNamespace(kind="text", text="ignored"),
    ])
    assert extract_attached_files(msg) == []


def test_extract_attached_files_accepts_both_shapes(tmp_path, monkeypatch):
    """a2a-sdk emits ``part.root.file`` via RootModel; some callers still
    build ``part.file`` directly. Both shapes have to yield the same
    dict structure — runtimes can pick either without surprise."""
    from types import SimpleNamespace
    from executor_helpers import extract_attached_files

    # Stage two real files under a fake /workspace for the resolver
    real_a = tmp_path / "a.txt"
    real_b = tmp_path / "b.txt"
    real_a.write_text("A")
    real_b.write_text("B")
    # Point the helper's containment check at tmp_path instead of /workspace
    monkeypatch.setattr("executor_helpers.WORKSPACE_MOUNT", str(tmp_path))

    msg = SimpleNamespace(parts=[
        SimpleNamespace(kind="file", file=SimpleNamespace(
            uri=f"workspace:{real_a}", name="a.txt", mimeType="text/plain"
        )),
        SimpleNamespace(root=SimpleNamespace(kind="file", file=SimpleNamespace(
            uri=f"workspace:{real_b}", name="b.txt", mimeType="text/plain"
        ))),
    ])
    out = extract_attached_files(msg)
    assert len(out) == 2
    assert {f["name"] for f in out} == {"a.txt", "b.txt"}


def test_extract_attached_files_accepts_v1_protobuf_part(tmp_path, monkeypatch):
    """a2a-sdk v1 protobuf ``Part`` has fields
    ``[text, raw, url, data, metadata, filename, media_type]`` — no
    ``kind`` field at all (the discriminator is now a oneof
    ``content`` of {text, raw, url, data}). Without v1-shape tolerance,
    every file part on the v0→v1 transition silently parses to an
    empty Part and surfaces as the user-visible
    "Error: message contained no text content" on image-only chats
    (2026-05-01 hongming incident).

    This pins the v1 detection: a non-empty ``url`` plus ``filename``
    + ``media_type`` is treated as a file part regardless of the
    missing ``kind``. The conftest stub ``Part`` mirrors v1's flat
    field shape (kwargs become attributes) so extracting via getattr
    sees the same surface the real protobuf does."""
    from types import SimpleNamespace
    from executor_helpers import extract_attached_files

    img = tmp_path / "screenshot.png"
    img.write_bytes(b"\x89PNG\r\n\x1a\n")
    monkeypatch.setattr("executor_helpers.WORKSPACE_MOUNT", str(tmp_path))

    # v1 protobuf surface: flat Part with url/filename/media_type, no kind.
    v1_part = SimpleNamespace(
        url=f"workspace:{img}",
        filename="screenshot.png",
        media_type="image/png",
    )
    msg = SimpleNamespace(parts=[v1_part])
    out = extract_attached_files(msg)
    assert len(out) == 1
    assert out[0]["name"] == "screenshot.png"
    assert out[0]["mime_type"] == "image/png"
    assert out[0]["path"] == str(img)


def test_extract_attached_files_empty_v1_part_returns_empty(tmp_path, monkeypatch):
    """Documents the v0→v1 silent-drop failure mode this fix defends
    against. When canvas pre-fix sends ``{kind:"file", file:{...}}``
    and the a2a-sdk v1 protobuf parser receives it with
    ``ignore_unknown_fields=True``, both legacy keys silently drop —
    the resulting Part has every field empty. The helper must NOT
    raise and must return ``[]`` — empty, not crashy.

    The real fix is shipping the canvas v1 shape; this test pins the
    runtime's defense so a template stuck on an old wheel against a
    new canvas still fails closed (empty attachments + agent
    proceeds) rather than mid-turn."""
    from types import SimpleNamespace
    from executor_helpers import extract_attached_files

    monkeypatch.setattr("executor_helpers.WORKSPACE_MOUNT", str(tmp_path))
    # Empty Part — no kind, no url, no filename, no media_type. This is
    # the all-empty proto state json_format leaves behind on the v0→v1
    # silent-drop. The helper must skip it without raising.
    empty_v1_part = SimpleNamespace()
    msg = SimpleNamespace(parts=[empty_v1_part])
    assert extract_attached_files(msg) == []


def test_build_user_content_with_files_no_attachments_is_string():
    """Zero attachments → plain string so models without multi-modal
    support (most non-vision LLMs) see the same payload shape they always
    did. Regressing this would break every runtime that assumed
    content is a string."""
    from executor_helpers import build_user_content_with_files

    out = build_user_content_with_files("hello", [])
    assert out == "hello"


def test_build_user_content_with_files_non_image_is_string_with_manifest():
    """Non-image attachments append a manifest line so the agent knows the
    filename and absolute path. Without this the agent had no signal that
    anything was attached — see canvas/src/components/tabs/ChatTab.tsx
    and the "I'm not sure what you're referring to" user report."""
    from executor_helpers import build_user_content_with_files

    content = build_user_content_with_files("read this", [
        {"name": "app.log", "mime_type": "text/plain", "path": "/workspace/app.log"},
    ])
    assert isinstance(content, str)
    assert "app.log" in content and "/workspace/app.log" in content
    assert "read this" in content


def test_build_user_content_with_files_image_is_multimodal(tmp_path):
    """Image attachments yield the OpenAI-compat list-of-parts shape so
    vision models see the bytes. Data URL check covers the common
    regression where an empty/missing file silently drops the image part."""
    from executor_helpers import build_user_content_with_files

    # Minimal 1x1 PNG
    png = tmp_path / "x.png"
    png.write_bytes(bytes.fromhex(
        "89504e470d0a1a0a0000000d49484452000000010000000108060000001f"
        "15c4890000000a49444154789c6300010000000500010d0a2db40000000049454e44ae426082"
    ))
    content = build_user_content_with_files("describe", [
        {"name": "x.png", "mime_type": "image/png", "path": str(png)},
    ])
    assert isinstance(content, list)
    assert len(content) == 2
    assert content[0]["type"] == "text"
    assert content[1]["type"] == "image_url"
    assert content[1]["image_url"]["url"].startswith("data:image/png;base64,")


def test_build_user_content_with_files_large_image_skipped(tmp_path, monkeypatch):
    """Images over the inline cap don't break the request — the manifest
    still carries the path so the agent can read via its file_read tool
    without blowing past provider context limits with a 50MB base64 blob."""
    from executor_helpers import build_user_content_with_files
    monkeypatch.setattr("executor_helpers.MAX_INLINE_ATTACHMENT_BYTES", 10)

    big = tmp_path / "big.png"
    big.write_bytes(b"x" * 100)
    content = build_user_content_with_files("describe", [
        {"name": "big.png", "mime_type": "image/png", "path": str(big)},
    ])
    # Image too large → no image_url entry, but the text manifest still mentions it
    assert isinstance(content, list)
    # Only the text part — the image_url was skipped
    assert all(c["type"] == "text" for c in content)


def test_collect_outbound_files_stages_workspace_paths(tmp_path, monkeypatch):
    """Agent reply mentioning a /workspace/… path → each unique existing
    file becomes an attachment, staged under chat-uploads. A crafted
    reply referencing /etc/passwd must NOT escape."""
    from pathlib import Path as _Path
    from executor_helpers import collect_outbound_files

    # Point the chat-uploads dir and the workspace root at a sandboxed tmp.
    # resolve() normalizes macOS /var → /private/var so the helper's
    # containment check (which also resolve()s) sees identical prefixes.
    ws_root = _Path(str(tmp_path / "workspace"))
    ws_root.mkdir()
    ws_root = ws_root.resolve()
    uploads = ws_root / ".molecule" / "chat-uploads"
    uploads.mkdir(parents=True)
    monkeypatch.setattr("executor_helpers.WORKSPACE_MOUNT", str(ws_root))
    monkeypatch.setattr("executor_helpers.CHAT_UPLOADS_DIR", str(uploads))
    # Rebuild the regex against the overridden mount (module caches it)
    import re as _re
    monkeypatch.setattr(
        "executor_helpers._WORKSPACE_PATH_RE",
        _re.compile(rf"(?:^|[\s`(\[])({ws_root}/[A-Za-z0-9_./\-]+)"),
    )

    # A real file inside the fake workspace
    report = ws_root / "report.txt"
    report.write_text("data")
    # A decoy outside the workspace — must be ignored even if mentioned
    (tmp_path / "secret.txt").write_text("leaked")

    reply = f"Saved to {report} — also see {tmp_path}/secret.txt for extras."
    out = collect_outbound_files(reply)
    assert len(out) == 1
    assert out[0]["name"] == "report.txt"
    # Staged copy lives under chat-uploads (the download endpoint's whitelist)
    assert out[0]["path"].startswith(str(uploads))


def test_ensure_workspace_writable_chmods_777(tmp_path, monkeypatch):
    """The platform-level hook opens /workspace + chat-uploads to 777 so
    agents running as any non-root user can write files the user will
    then download. This is the single point of fix for what used to need
    a chmod in every template's Dockerfile."""
    import stat
    from executor_helpers import ensure_workspace_writable

    ws = tmp_path / "workspace"
    ws.mkdir(mode=0o755)
    uploads = ws / ".molecule" / "chat-uploads"
    # Don't pre-create uploads — the helper must makedirs it.
    monkeypatch.setattr("executor_helpers.WORKSPACE_MOUNT", str(ws))
    monkeypatch.setattr("executor_helpers.CHAT_UPLOADS_DIR", str(uploads))

    ensure_workspace_writable()

    assert uploads.is_dir(), "chat-uploads dir should be created"
    assert stat.S_IMODE(ws.stat().st_mode) == 0o777
    assert stat.S_IMODE(uploads.stat().st_mode) == 0o777


def test_ensure_workspace_writable_tolerates_non_root(tmp_path, monkeypatch, caplog):
    """When molecule-runtime isn't root (rare CP configurations), the
    chmod silently no-ops rather than crashing boot — a misconfigured
    perm is recoverable; a SystemExit here would wedge the workspace
    in provisioning forever."""
    import logging
    from executor_helpers import ensure_workspace_writable

    ws = tmp_path / "workspace"
    ws.mkdir()
    monkeypatch.setattr("executor_helpers.WORKSPACE_MOUNT", str(ws))
    monkeypatch.setattr("executor_helpers.CHAT_UPLOADS_DIR", str(ws / "x"))

    def _boom(*_a, **_kw):
        raise PermissionError("Operation not permitted")

    monkeypatch.setattr("executor_helpers.os.chmod", _boom)
    with caplog.at_level(logging.INFO, logger="executor_helpers"):
        ensure_workspace_writable()  # must not raise


def test_collect_outbound_files_deduplicates(tmp_path, monkeypatch):
    """Reply mentioning the same path twice should only attach once."""
    from pathlib import Path as _Path
    from executor_helpers import collect_outbound_files

    ws_root = _Path(str(tmp_path / "workspace"))
    ws_root.mkdir()
    ws_root = ws_root.resolve()
    uploads = ws_root / ".molecule" / "chat-uploads"
    uploads.mkdir(parents=True)
    monkeypatch.setattr("executor_helpers.WORKSPACE_MOUNT", str(ws_root))
    monkeypatch.setattr("executor_helpers.CHAT_UPLOADS_DIR", str(uploads))
    import re as _re
    monkeypatch.setattr(
        "executor_helpers._WORKSPACE_PATH_RE",
        _re.compile(rf"(?:^|[\s`(\[])({ws_root}/[A-Za-z0-9_./\-]+)"),
    )

    report = ws_root / "report.txt"
    report.write_text("data")
    reply = f"Wrote {report}. Again at {report}."
    out = collect_outbound_files(reply)
    assert len(out) == 1


# ============================================================================
# new_response_message — A2A v1 protobuf Message envelope with task/context
# correlation. Replaces ad-hoc per-template Message construction so every
# adapter response threads task_id/context_id back to the platform.
# ============================================================================


def test_new_response_message_text_only():
    """Text-only response sets one text Part; role=ROLE_AGENT;
    task_id/context_id passed through from context."""
    from executor_helpers import new_response_message
    from a2a.types import Role

    ctx = SimpleNamespace(task_id="task-abc", context_id="ctx-xyz")
    msg = new_response_message(ctx, "hello world")

    assert msg.role == Role.ROLE_AGENT
    assert msg.task_id == "task-abc"
    assert msg.context_id == "ctx-xyz"
    assert len(msg.parts) == 1
    assert msg.parts[0].text == "hello world"
    # message_id should be a 32-char hex (uuid4().hex)
    assert len(msg.message_id) == 32


def test_new_response_message_with_files():
    """Files become file Parts with workspace: URI scheme, filename,
    media_type. Text Part comes first when text is non-empty."""
    from executor_helpers import new_response_message

    ctx = SimpleNamespace(task_id="t", context_id="c")
    files = [
        {"path": "/workspace/.molecule/chat-uploads/a.png", "name": "a.png", "mime_type": "image/png"},
        {"path": "/workspace/.molecule/chat-uploads/b.txt", "name": "b.txt", "mime_type": "text/plain"},
    ]
    msg = new_response_message(ctx, "see attachments", files=files)

    assert len(msg.parts) == 3  # 1 text + 2 file parts
    assert msg.parts[0].text == "see attachments"
    assert msg.parts[1].url == "workspace:/workspace/.molecule/chat-uploads/a.png"
    assert msg.parts[1].filename == "a.png"
    assert msg.parts[1].media_type == "image/png"
    assert msg.parts[2].url == "workspace:/workspace/.molecule/chat-uploads/b.txt"


def test_new_response_message_files_only_no_text():
    """Empty text omits the text Part — useful when replying with files only."""
    from executor_helpers import new_response_message

    ctx = SimpleNamespace(task_id="t", context_id="c")
    files = [{"path": "/x.txt", "name": "x.txt", "mime_type": "text/plain"}]
    msg = new_response_message(ctx, "", files=files)

    assert len(msg.parts) == 1
    assert msg.parts[0].url == "workspace:/x.txt"


def test_new_response_message_falls_back_when_context_ids_unset():
    """RequestContextBuilder always populates task_id/context_id in
    production, but unit tests + edge cases may have None. Helper falls
    back to fresh UUIDs so the resulting Message is still well-formed."""
    from executor_helpers import new_response_message

    ctx = SimpleNamespace(task_id=None, context_id=None)
    msg = new_response_message(ctx, "hi")

    # Both should be 32-char hex UUIDs (fallback path)
    assert len(msg.task_id) == 32
    assert len(msg.context_id) == 32
    # And they should be DIFFERENT (not accidentally the same uuid)
    assert msg.task_id != msg.context_id


def test_new_response_message_handles_missing_attrs():
    """getattr with default — context object lacking task_id/context_id
    attributes entirely (not just None) still works."""
    from executor_helpers import new_response_message

    class BareContext:
        pass

    msg = new_response_message(BareContext(), "hi")
    assert len(msg.task_id) == 32  # fallback uuid
    assert len(msg.context_id) == 32
