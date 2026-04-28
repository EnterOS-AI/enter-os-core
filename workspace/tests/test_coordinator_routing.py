"""Tests for the coordinator routing policy path."""

import sys
from unittest.mock import AsyncMock, MagicMock

import pytest

import coordinator


@pytest.mark.asyncio
async def test_route_task_to_team_returns_policy_decision_when_no_children(monkeypatch):
    monkeypatch.setattr(coordinator, "get_children", AsyncMock(return_value=[]))

    result = await coordinator.route_task_to_team("Write docs")

    assert result == {
        "success": False,
        "error": "No team members available. Handle this task yourself.",
        "task": "Write docs",
        "members": [],
    }


@pytest.mark.asyncio
async def test_route_task_to_team_delegates_preferred_member(monkeypatch):
    monkeypatch.setattr(coordinator, "get_children", AsyncMock(return_value=[]))

    delegate = MagicMock()
    delegate.ainvoke = AsyncMock(return_value={"ok": True})
    monkeypatch.setattr(sys.modules["builtin_tools.delegation"], "delegate_task_async", delegate)

    result = await coordinator.route_task_to_team(
        "Do the thing",
        preferred_member_id="child-99",
    )

    assert result == {"ok": True}
    delegate.ainvoke.assert_awaited_once_with(
        {"workspace_id": "child-99", "task": "Do the thing"}
    )


def test_build_children_description_reuses_shared_renderer():
    children = [
        {
            "id": "child-1",
            "status": "online",
            "agent_card": {
                "name": "Alpha",
                "skills": [{"name": "research"}],
            },
        }
    ]

    description = coordinator.build_children_description(children)

    assert "## Your Team (sub-workspaces you coordinate)" in description
    assert "**Alpha** (id: `child-1`, status: online)" in description
    assert "Skills: research" in description
    assert "delegate_task_async" in description
