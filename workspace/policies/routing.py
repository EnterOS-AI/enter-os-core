"""Explicit routing policy for coordinator workspaces."""

from __future__ import annotations

import json
from typing import Any


def _load_agent_card(agent_card: Any) -> dict[str, Any]:
    if isinstance(agent_card, str):
        try:
            loaded = json.loads(agent_card)
        except json.JSONDecodeError:
            return {}
        return loaded if isinstance(loaded, dict) else {}
    return agent_card if isinstance(agent_card, dict) else {}


def summarize_children(children: list[dict]) -> list[dict[str, Any]]:
    """Return the minimal child summary needed for routing and prompts."""
    members: list[dict[str, Any]] = []
    for child in children:
        card = _load_agent_card(child.get("agent_card", {}))
        members.append(
            {
                "id": child.get("id"),
                "name": child.get("name"),
                "status": child.get("status"),
                "skills": [
                    s.get("name", s.get("id", ""))
                    for s in card.get("skills", [])
                    if isinstance(s, dict)
                ],
            }
        )
    return members


def build_team_routing_payload(
    children: list[dict],
    task: str,
    preferred_member_id: str = "",
) -> dict[str, Any]:
    """Return the deterministic routing payload for coordinator tasks."""
    if preferred_member_id:
        return {
            "success": True,
            "action": "delegate_to_preferred_member",
            "preferred_member_id": preferred_member_id,
            "task": task,
        }

    members = summarize_children(children)
    if not members:
        return {
            "success": False,
            "error": "No team members available. Handle this task yourself.",
            "task": task,
            "members": [],
        }

    return {
        "success": True,
        "action": "choose_member",
        "message": (
            f"You have {len(members)} team members. "
            "Choose the best one for this task and call delegate_task_async with their ID."
        ),
        "task": task,
        "members": members,
    }


def decide_team_route(
    children: list[dict],
    *,
    task: str,
    preferred_member_id: str = "",
) -> dict[str, Any]:
    """Compatibility wrapper for older callers."""
    return build_team_routing_payload(
        children,
        task=task,
        preferred_member_id=preferred_member_id,
    )


def build_team_route_decision(
    children: list[dict],
    task: str,
    preferred_member_id: str = "",
) -> dict[str, Any]:
    """Compatibility wrapper for tests and older imports."""
    return build_team_routing_payload(
        children,
        task=task,
        preferred_member_id=preferred_member_id,
    )
