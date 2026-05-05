"""Tests for commit_memory and recall_memory in a2a_mcp_server.py."""

import asyncio
import importlib
import json
import os
import sys
from unittest.mock import AsyncMock, MagicMock, patch

import pytest


@pytest.fixture(autouse=True)
def env_setup(monkeypatch):
    monkeypatch.setenv("WORKSPACE_ID", "ws-test-123")
    monkeypatch.setenv("PLATFORM_URL", "http://platform.test:8080")


def _load_mcp():
    """Import the MCP server module (reload to pick up env changes)."""
    # Ensure all modules are reloaded with fresh env
    for mod in ("a2a_mcp_server", "a2a_tools", "a2a_client"):
        sys.modules.pop(mod, None)
    import a2a_mcp_server
    return a2a_mcp_server


class FakeResponse:
    def __init__(self, status_code, data):
        self.status_code = status_code
        self._data = data
        self.text = json.dumps(data)

    def json(self):
        return self._data


class FakeClient:
    def __init__(self, **kwargs):
        self.calls = []

    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        pass

    async def post(self, url, json=None, headers=None, **kwargs):
        self.calls.append(("POST", url, json))
        return FakeResponse(201, {"id": "mem-abc", "scope": json.get("scope", "LOCAL") if json else "LOCAL"})

    async def get(self, url, params=None, headers=None, **kwargs):
        self.calls.append(("GET", url, params))
        return FakeResponse(200, [
            {"id": "mem-1", "content": "Test memory", "scope": "LOCAL"},
            {"id": "mem-2", "content": "Team note", "scope": "TEAM"},
        ])


@pytest.mark.asyncio
async def test_commit_memory_success(monkeypatch):
    """commit_memory saves to platform memories API."""
    mcp = _load_mcp()

    client = FakeClient()
    monkeypatch.setattr("a2a_tools_memory.httpx.AsyncClient", lambda **kw: client)

    result = await mcp.handle_tool_call("commit_memory", {
        "content": "Architecture decision: use Go for backend",
        "scope": "LOCAL",
    })

    data = json.loads(result)
    assert data["success"] is True
    assert data["id"] == "mem-abc"
    assert data["scope"] == "LOCAL"
    assert len(client.calls) == 1
    assert "memories" in client.calls[0][1]


@pytest.mark.asyncio
async def test_commit_memory_empty_content():
    """commit_memory rejects empty content."""
    mcp = _load_mcp()
    result = await mcp.handle_tool_call("commit_memory", {"content": ""})
    assert "Error" in result


@pytest.mark.asyncio
async def test_commit_memory_default_scope(monkeypatch):
    """commit_memory defaults to LOCAL scope."""
    mcp = _load_mcp()

    client = FakeClient()
    monkeypatch.setattr("a2a_tools_memory.httpx.AsyncClient", lambda **kw: client)

    result = await mcp.handle_tool_call("commit_memory", {
        "content": "Some note",
    })

    data = json.loads(result)
    assert data["scope"] == "LOCAL"


@pytest.mark.asyncio
async def test_recall_memory_success(monkeypatch):
    """recall_memory returns formatted memories."""
    mcp = _load_mcp()

    client = FakeClient()
    monkeypatch.setattr("a2a_tools_memory.httpx.AsyncClient", lambda **kw: client)

    result = await mcp.handle_tool_call("recall_memory", {"query": "architecture"})

    assert "Test memory" in result
    assert "Team note" in result
    assert "[LOCAL]" in result
    assert "[TEAM]" in result


@pytest.mark.asyncio
async def test_recall_memory_empty(monkeypatch):
    """recall_memory returns message when no memories found."""
    mcp = _load_mcp()

    class EmptyClient(FakeClient):
        async def get(self, url, params=None, headers=None, **kwargs):
            return FakeResponse(200, [])

    monkeypatch.setattr("a2a_tools_memory.httpx.AsyncClient", lambda **kw: EmptyClient())

    result = await mcp.handle_tool_call("recall_memory", {})
    assert "No memories found" in result


@pytest.mark.asyncio
async def test_recall_memory_with_scope_filter(monkeypatch):
    """recall_memory passes scope parameter to API."""
    mcp = _load_mcp()

    client = FakeClient()
    monkeypatch.setattr("a2a_tools_memory.httpx.AsyncClient", lambda **kw: client)

    await mcp.handle_tool_call("recall_memory", {"scope": "TEAM"})

    assert len(client.calls) == 1
    _, url, params = client.calls[0]
    assert params["scope"] == "TEAM"


def test_memory_tools_in_tool_list():
    """commit_memory and recall_memory are listed in TOOLS."""
    mcp = _load_mcp()
    tool_names = [t["name"] for t in mcp.TOOLS]
    assert "commit_memory" in tool_names
    assert "recall_memory" in tool_names
