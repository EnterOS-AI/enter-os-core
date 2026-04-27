"""A2A communication tools — framework-agnostic delegation and peer discovery.

These are plain async functions that any adapter can wrap in its native tool format.
The LangChain @tool versions are in tools/delegation.py.
"""

import os
import uuid

import httpx

PLATFORM_URL = os.environ.get("PLATFORM_URL", "http://host.docker.internal:8080")
WORKSPACE_ID = os.environ.get("WORKSPACE_ID", "")


async def list_peers() -> list[dict]:
    """Get this workspace's peers from the platform registry."""
    async with httpx.AsyncClient(timeout=10.0) as client:
        try:
            resp = await client.get(f"{PLATFORM_URL}/registry/{WORKSPACE_ID}/peers")
            if resp.status_code == 200:
                return resp.json()
            return []
        except Exception:
            return []


async def delegate_task(workspace_id: str, task: str) -> str:
    """Send a task to a peer workspace via A2A and return the response text."""
    async with httpx.AsyncClient(timeout=120.0) as client:
        # Discover target URL
        try:
            resp = await client.get(
                f"{PLATFORM_URL}/registry/discover/{workspace_id}",
                headers={"X-Workspace-ID": WORKSPACE_ID},
            )
            if resp.status_code != 200:
                return f"Error: cannot reach workspace {workspace_id} (status {resp.status_code})"
            target_url = resp.json().get("url", "")
            if not target_url:
                return f"Error: workspace {workspace_id} has no URL"
        except Exception as e:
            return f"Error discovering workspace: {e}"

        # Send A2A message. X-Workspace-ID identifies us as the source —
        # without it the platform's a2a_receive logger writes
        # source_id=NULL and the recipient's My Chat tab renders the
        # delegation as if a human user typed it. Same hazard fixed
        # in heartbeat.py / a2a_client.py / main.py initial+idle flows.
        try:
            a2a_resp = await client.post(
                target_url,
                headers={"X-Workspace-ID": WORKSPACE_ID},
                json={
                    "jsonrpc": "2.0",
                    "id": str(uuid.uuid4()),
                    "method": "message/send",
                    "params": {
                        "message": {
                            "role": "ROLE_USER",
                            "messageId": str(uuid.uuid4()),
                            "parts": [{"kind": "text", "text": task}],
                        },
                    },
                },
            )
            data = a2a_resp.json()
            if "result" in data:
                parts = data["result"].get("parts", [])
                return parts[0].get("text", "(no text)") if parts else str(data["result"])
            elif "error" in data:
                return f"Error: {data['error'].get('message', str(data['error']))}"
            return str(data)
        except Exception as e:
            return f"Error sending A2A message: {e}"


async def get_peers_summary() -> str:
    """Return a formatted string of available peers for system prompts."""
    peers = await list_peers()
    if not peers:
        return "No peers available."
    lines = []
    for p in peers:
        name = p.get("name", "Unknown")
        pid = p.get("id", "")
        role = p.get("role", "")
        status = p.get("status", "")
        lines.append(f"- {name} (ID: {pid}) — {role} [{status}]")
    return "Available peers:\n" + "\n".join(lines)
