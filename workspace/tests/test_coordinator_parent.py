"""Tests for coordinator.get_children() and build_children_description().

shared_context / get_parent_context was removed: parent→child knowledge
sharing now flows through memory v2's team:<id> namespace via recall_memory
on demand, not through file paths injected at boot.
"""

from unittest.mock import AsyncMock, patch, MagicMock

import pytest

from coordinator import get_children, build_children_description


# ---------------------------------------------------------------------------
# get_children() tests
# ---------------------------------------------------------------------------

@pytest.mark.asyncio
async def test_get_children_success(monkeypatch):
    """get_children() returns only peers whose parent_id matches WORKSPACE_ID."""
    import coordinator
    monkeypatch.setattr(coordinator, "PLATFORM_URL", "http://localhost:8080")
    monkeypatch.setattr(coordinator, "WORKSPACE_ID", "parent-ws")

    mock_resp = MagicMock()
    mock_resp.status_code = 200
    mock_resp.json.return_value = [
        {"id": "child-1", "parent_id": "parent-ws"},
        {"id": "peer-2", "parent_id": "other-ws"},
        {"id": "child-3", "parent_id": "parent-ws"},
    ]

    mock_client = AsyncMock()
    mock_client.__aenter__ = AsyncMock(return_value=mock_client)
    mock_client.__aexit__ = AsyncMock(return_value=False)
    mock_client.get = AsyncMock(return_value=mock_resp)

    with patch("coordinator.httpx.AsyncClient", return_value=mock_client):
        result = await get_children()

    assert len(result) == 2
    assert result[0]["id"] == "child-1"
    assert result[1]["id"] == "child-3"


@pytest.mark.asyncio
async def test_get_children_non_200(monkeypatch):
    """get_children() returns [] when the response status is not 200."""
    import coordinator
    monkeypatch.setattr(coordinator, "PLATFORM_URL", "http://localhost:8080")
    monkeypatch.setattr(coordinator, "WORKSPACE_ID", "parent-ws")

    mock_resp = MagicMock()
    mock_resp.status_code = 503

    mock_client = AsyncMock()
    mock_client.__aenter__ = AsyncMock(return_value=mock_client)
    mock_client.__aexit__ = AsyncMock(return_value=False)
    mock_client.get = AsyncMock(return_value=mock_resp)

    with patch("coordinator.httpx.AsyncClient", return_value=mock_client):
        result = await get_children()

    assert result == []


@pytest.mark.asyncio
async def test_get_children_exception(monkeypatch):
    """get_children() returns [] when httpx raises an exception."""
    import coordinator
    monkeypatch.setattr(coordinator, "PLATFORM_URL", "http://localhost:8080")
    monkeypatch.setattr(coordinator, "WORKSPACE_ID", "parent-ws")

    mock_client = AsyncMock()
    mock_client.__aenter__ = AsyncMock(return_value=mock_client)
    mock_client.__aexit__ = AsyncMock(return_value=False)
    mock_client.get = AsyncMock(side_effect=Exception("Network error"))

    with patch("coordinator.httpx.AsyncClient", return_value=mock_client):
        result = await get_children()

    assert result == []


def test_build_children_description_empty_returns_empty_string():
    """build_children_description() with empty list returns '' (covers line 72)."""
    result = build_children_description([])
    assert result == ""


def test_build_children_description_with_children():
    """build_children_description() formats children correctly."""
    children = [
        {"id": "child-1", "name": "Worker A", "description": "Does work A"},
        {"id": "child-2", "name": "Worker B"},
    ]
    result = build_children_description(children)
    assert result != ""
    assert "Coordination Rules" in result
