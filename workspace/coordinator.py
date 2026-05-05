"""Coordinator pattern for team workspaces.

When a workspace is expanded into a team, the parent agent becomes a
coordinator that routes incoming tasks to the appropriate child workspace
based on the task content and children's capabilities.

The coordinator:
1. Fetches its children's Agent Cards (skills, capabilities)
2. Analyzes each incoming task to determine which child is best suited
3. Delegates to the chosen child via the delegation tool
4. Aggregates responses if a task requires multiple children
5. Falls back to handling the task itself if no child is appropriate
"""

import logging
import os

import httpx
from langchain_core.tools import tool
from shared_runtime import build_peer_section
from policies.routing import build_team_routing_payload

logger = logging.getLogger(__name__)

if os.path.exists("/.dockerenv") or os.environ.get("DOCKER_VERSION"):
    PLATFORM_URL = os.environ.get("PLATFORM_URL", "http://host.docker.internal:8080")
else:
    PLATFORM_URL = os.environ.get("PLATFORM_URL", "http://localhost:8080")
_WORKSPACE_ID_raw = os.environ.get("WORKSPACE_ID")
if not _WORKSPACE_ID_raw:
    raise RuntimeError("WORKSPACE_ID environment variable is required but not set")
WORKSPACE_ID = _WORKSPACE_ID_raw


async def get_children() -> list[dict]:
    """Fetch this workspace's children from the platform."""
    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.get(
                f"{PLATFORM_URL}/registry/{WORKSPACE_ID}/peers",
                headers={"X-Workspace-ID": WORKSPACE_ID},
            )
            if resp.status_code == 200:
                peers = resp.json()
                # Filter to only children (parent_id == our ID)
                return [p for p in peers if p.get("parent_id") == WORKSPACE_ID]
    except Exception as e:
        logger.warning("Failed to fetch children: %s", e)
    return []


def build_children_description(children: list[dict]) -> str:
    """Build a description of children's capabilities for the coordinator prompt."""
    if not children:
        return ""

    team_section = build_peer_section(
        children,
        heading="## Your Team (sub-workspaces you coordinate)",
        instruction=(
            "Use the `delegate_task_async` tool to send tasks to the chosen member. "
            "Only delegate to members listed above."
        ),
    )

    return "\n".join(
        [
            team_section,
            "",
            "### Coordination Rules — MANDATORY",
            "1. You are a COORDINATOR. Your ONLY job is to delegate and synthesize. NEVER do the work yourself.",
            "2. For EVERY task, use `delegate_task_async` to send it to the appropriate team member(s). "
            "Do this BEFORE writing any analysis, code, or research yourself.",
            "3. If a task spans multiple members, delegate to ALL of them in parallel and aggregate results.",
            "4. If ALL members are offline/paused, tell the caller which members are unavailable. "
            "Do NOT attempt the work yourself — you lack the specialist context.",
            "5. If a delegation FAILS (error, timeout): try another member first. "
            "Only provide your own brief summary if NO member can respond. Never forward raw errors.",
            "6. Your response should be a SYNTHESIS of your team's work, not your own analysis.",
            "7. Always respond in the same language the caller uses.",
        ]
    )


@tool
async def route_task_to_team(
    task: str,
    preferred_member_id: str = "",
) -> dict:
    """Route a task to the most appropriate team member.

    As the team coordinator, analyze the task and delegate to the best-suited
    child workspace. If preferred_member_id is provided, delegate directly to
    that member.

    Args:
        task: The task description to route.
        preferred_member_id: Optional — directly delegate to this member.
    """
    import time
    from builtin_tools.delegation import delegate_task_async as delegate

    # RFC #2251 V1.0 reproduction-harness instrumentation. Phase-tagged log
    # lines correlate with scripts/measure-coordinator-task-bounds.sh's
    # external timing trace, so an operator running the harness against
    # staging can answer "what phase was the coordinator in at minute 7?".
    # `grep rfc2251_phase` on the workspace's container logs is the query.
    # Strip when V1.0 ships and the phase data lands in the structured
    # heartbeat payload instead.
    _phase_t0 = time.monotonic()
    logger.info(
        "rfc2251_phase=route_start task_chars=%d preferred_member_id=%s",
        len(task), preferred_member_id or "none",
    )

    children = await get_children()
    logger.info(
        "rfc2251_phase=children_fetched count=%d elapsed_ms=%d",
        len(children), int((time.monotonic() - _phase_t0) * 1000),
    )

    decision = build_team_routing_payload(
        children,
        task=task,
        preferred_member_id=preferred_member_id,
    )
    logger.info(
        "rfc2251_phase=routing_decided action=%s elapsed_ms=%d",
        decision.get("action", "unknown"), int((time.monotonic() - _phase_t0) * 1000),
    )

    if decision.get("action") == "delegate_to_preferred_member":
        # Async delegation — returns immediately with task_id
        target = decision["preferred_member_id"]
        logger.info(
            "rfc2251_phase=delegate_invoked target=%s elapsed_ms=%d",
            target, int((time.monotonic() - _phase_t0) * 1000),
        )
        result = await delegate.ainvoke(
            {"workspace_id": target, "task": task}
        )
        logger.info(
            "rfc2251_phase=delegate_returned target=%s task_id=%s elapsed_ms=%d",
            target, result.get("task_id", "n/a"), int((time.monotonic() - _phase_t0) * 1000),
        )
        return result

    logger.info(
        "rfc2251_phase=route_returning_decision_only elapsed_ms=%d",
        int((time.monotonic() - _phase_t0) * 1000),
    )
    return decision
