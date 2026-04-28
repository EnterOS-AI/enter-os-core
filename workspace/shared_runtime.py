"""Shared runtime helpers for A2A-backed workspace executors."""

from __future__ import annotations

import json
from typing import Any

from a2a.server.agent_execution import RequestContext


def _extract_part_text(part) -> str:
    """Extract text from a message part, handling dicts and A2A objects."""
    if isinstance(part, dict):
        text = part.get("text", "")
        if text:
            return text
        root = part.get("root")
        if isinstance(root, dict):
            return root.get("text", "")
        return ""
    if hasattr(part, "text") and part.text:
        return part.text
    if hasattr(part, "root") and hasattr(part.root, "text") and part.root.text:
        return part.root.text
    return ""


def extract_message_text(context_or_parts) -> str:
    """Extract concatenated plain text from A2A message parts."""
    parts = getattr(getattr(context_or_parts, "message", None), "parts", None)
    if parts is None:
        parts = context_or_parts
    return " ".join(
        text for part in (parts or []) if (text := _extract_part_text(part))
    ).strip()


def extract_history(context: RequestContext) -> list[tuple[str, str]]:
    """Extract conversation history from A2A request metadata."""
    messages: list[tuple[str, str]] = []
    request = getattr(context, "request", None)
    metadata = getattr(request, "metadata", None) if request else None
    if not isinstance(metadata, dict):
        metadata = getattr(context, "metadata", None) or {}
    history = metadata.get("history", []) if isinstance(metadata, dict) else []
    if not isinstance(history, list):
        return messages

    for entry in history:
        if not isinstance(entry, dict):
            continue
        role = entry.get("role", "user")
        parts = entry.get("parts", [])
        text = " ".join(
            text for part in (parts or []) if (text := _extract_part_text(part))
        ).strip()
        if text:
            mapped_role = "human" if role == "user" else "ai"
            messages.append((mapped_role, text))
    return messages


def format_conversation_history(history: list[tuple[str, str]]) -> str:
    """Render `(role, text)` history into a stable human-readable transcript."""
    return "\n".join(
        f"{'User' if role == 'human' else 'Agent'}: {text}" for role, text in history
    )


def build_task_text(user_message: str, history: list[tuple[str, str]]) -> str:
    """Build a single task/request string with optional prepended conversation history."""
    if not history:
        return user_message
    transcript = format_conversation_history(history)
    return f"Conversation so far:\n{transcript}\n\nCurrent request: {user_message}"


def append_peer_guidance(
    base_text: str | None,
    peers_info: str,
    *,
    default_text: str,
    tool_name: str,
) -> str:
    """Append peer guidance text when peers are available."""
    text = (base_text or default_text).strip()
    if peers_info:
        text += f"\n\n## Peers\n{peers_info}\nUse {tool_name} to communicate with them."
    return text


def summarize_peer_cards(peers: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Return compact peer metadata for prompt rendering.

    Falls back to the registry row's `name` and `role` when `agent_card` is
    null or unparseable so peers stay visible to delegators even before
    their A2A discovery roundtrip has populated a card. Without this
    fallback a coordinator-tier workspace with N freshly-created worker
    peers would render an empty `## Your Peers` section and refuse to
    delegate (the regression behind the 2026-04-27 Design Director
    discovery bug).
    """
    summaries: list[dict[str, Any]] = []
    for peer in peers:
        agent_card = peer.get("agent_card")
        if isinstance(agent_card, str):
            try:
                agent_card = json.loads(agent_card)
            except Exception:
                agent_card = None
        if not isinstance(agent_card, dict):
            agent_card = None

        if agent_card:
            skills_raw = agent_card.get("skills") or []
            skills = [
                s.get("name", s.get("id", ""))
                for s in skills_raw
                if isinstance(s, dict)
            ]
            name = agent_card.get("name") or peer.get("name") or "Unknown"
        else:
            skills = []
            name = peer.get("name") or "Unknown"

        summaries.append(
            {
                "id": peer.get("id", "unknown"),
                "name": name,
                "role": peer.get("role") or "",
                "status": peer.get("status", "unknown"),
                "skills": skills,
            }
        )
    return summaries


def build_peer_section(
    peers: list[dict[str, Any]],
    *,
    heading: str = "## Your Peers (workspaces you can delegate to)",
    instruction: str = (
        "Use the `delegate_to_workspace` tool to send tasks to peers. "
        "Only delegate to peers listed above."
    ),
) -> str:
    """Render a stable peer section for system prompts."""
    summaries = summarize_peer_cards(peers)
    if not summaries:
        return ""

    parts = [heading, ""]
    for peer in summaries:
        parts.append(f"- **{peer['name']}** (id: `{peer['id']}`, status: {peer['status']})")
        if peer["skills"]:
            parts.append(f"  Skills: {', '.join(peer['skills'])}")
        elif peer.get("role"):
            parts.append(f"  Role: {peer['role']}")
        parts.append("")
    parts.append(instruction)
    return "\n".join(parts)


def brief_task(text: str, limit: int = 60) -> str:
    """Create a short human-readable task label for the heartbeat banner."""
    return text[:limit] + ("..." if len(text) > limit else "")


async def set_current_task(heartbeat: Any, task: str) -> None:
    """Update current task on heartbeat and push immediately to platform.

    Uses increment/decrement instead of binary 0/1 so agents can track
    multiple concurrent tasks (e.g. a cron running while an A2A delegation
    arrives). The counter never goes below 0.

    Pushes immediately on BOTH increment and decrement to avoid phantom-busy
    (#1372) where active_tasks=1 persisted in the platform DB indefinitely.
    """
    if heartbeat:
        if task:
            heartbeat.active_tasks = getattr(heartbeat, "active_tasks", 0) + 1
            heartbeat.current_task = task
        else:
            heartbeat.active_tasks = max(0, getattr(heartbeat, "active_tasks", 0) - 1)
            if heartbeat.active_tasks == 0:
                heartbeat.current_task = ""

    import os
    workspace_id = os.environ.get("WORKSPACE_ID", "")
    platform_url = os.environ.get("PLATFORM_URL", "")
    if workspace_id and platform_url:
        try:
            import httpx
            active = getattr(heartbeat, "active_tasks", 0) if heartbeat else (1 if task else 0)
            cur_task = getattr(heartbeat, "current_task", task or "") if heartbeat else (task or "")
            async with httpx.AsyncClient(timeout=3.0) as client:
                await client.post(
                    f"{platform_url}/registry/heartbeat",
                    json={
                        "workspace_id": workspace_id,
                        "current_task": cur_task,
                        "active_tasks": active,
                        "error_rate": 0,
                        "sample_error": "",
                        "uptime_seconds": 0,
                    },
                )
        except Exception:
            pass  # Best-effort
