"""A2A protocol client — peer discovery, messaging, and workspace info.

Shared constants (WORKSPACE_ID, PLATFORM_URL) live here so that
a2a_tools and a2a_mcp_server can import them from a single place.
"""

import logging
import os
import uuid

import httpx

from platform_auth import auth_headers, self_source_headers

logger = logging.getLogger(__name__)

_WORKSPACE_ID_raw = os.environ.get("WORKSPACE_ID")
if not _WORKSPACE_ID_raw:
    raise RuntimeError("WORKSPACE_ID environment variable is required but not set")
WORKSPACE_ID = _WORKSPACE_ID_raw
if os.path.exists("/.dockerenv") or os.environ.get("DOCKER_VERSION"):
    PLATFORM_URL = os.environ.get("PLATFORM_URL", "http://host.docker.internal:8080")
else:
    PLATFORM_URL = os.environ.get("PLATFORM_URL", "http://localhost:8080")

# Cache workspace ID → name mappings (populated by list_peers calls)
_peer_names: dict[str, str] = {}

# Sentinel prefix for errors originating from send_a2a_message / child agents.
# Used by delegate_task to distinguish real errors from normal response text.
_A2A_ERROR_PREFIX = "[A2A_ERROR] "


async def discover_peer(target_id: str) -> dict | None:
    """Discover a peer workspace's URL via the platform registry."""
    async with httpx.AsyncClient(timeout=10.0) as client:
        try:
            resp = await client.get(
                f"{PLATFORM_URL}/registry/discover/{target_id}",
                headers={"X-Workspace-ID": WORKSPACE_ID, **auth_headers()},
            )
            if resp.status_code == 200:
                return resp.json()
            return None
        except Exception as e:
            logger.error(f"Discovery failed for {target_id}: {e}")
            return None


async def send_a2a_message(target_url: str, message: str) -> str:
    """Send an A2A message/send to a target workspace."""
    # Fix F (Cycle 5 / H2 — flagged 5 consecutive audits): timeout=None allowed
    # a hung upstream to block the agent indefinitely. Use a generous but bounded
    # timeout: 30s connect + 300s read (long enough for slow LLM responses).
    async with httpx.AsyncClient(
        timeout=httpx.Timeout(connect=30.0, read=300.0, write=30.0, pool=30.0)
    ) as client:
        try:
            # self_source_headers() includes X-Workspace-ID so the
            # platform's a2a_receive logger records source_id =
            # WORKSPACE_ID. Otherwise peer-A2A messages — including
            # the case where target_url resolves to this workspace's
            # own /a2a — get logged with source_id=NULL and surface
            # in the recipient's My Chat tab as user-typed input.
            resp = await client.post(
                target_url,
                headers=self_source_headers(WORKSPACE_ID),
                json={
                    "jsonrpc": "2.0",
                    "id": str(uuid.uuid4()),
                    "method": "message/send",
                    "params": {
                        "message": {
                            "role": "ROLE_USER",
                            "messageId": str(uuid.uuid4()),
                            "parts": [{"kind": "text", "text": message}],
                        }
                    },
                },
            )
            data = resp.json()
            if "result" in data:
                parts = data["result"].get("parts", [])
                text = parts[0].get("text", "") if parts else "(no response)"
                # Tag child-reported errors so the caller can detect them reliably
                if text.startswith("Agent error:"):
                    return f"{_A2A_ERROR_PREFIX}{text}"
                return text
            elif "error" in data:
                err = data["error"]
                msg = (err.get("message") or "").strip()
                code = err.get("code")
                if msg and code is not None:
                    detail = f"{msg} (code={code})"
                elif msg:
                    detail = msg
                elif code is not None:
                    detail = f"JSON-RPC error with no message (code={code})"
                else:
                    detail = "JSON-RPC error with no message"
                return f"{_A2A_ERROR_PREFIX}{detail} [target={target_url}]"
            return f"{_A2A_ERROR_PREFIX}unexpected response shape (no result, no error): {str(data)[:200]} [target={target_url}]"
        except Exception as e:
            # Some httpx exceptions stringify to empty (RemoteProtocolError,
            # ConnectionReset variants) — the canvas would then render
            # "[A2A_ERROR] " with no detail and the operator has no signal
            # to act on. Always include the exception class name and the
            # target URL so the activity log + Agent Comms panel have
            # actionable information without a trip through container logs.
            msg = str(e).strip()
            type_name = type(e).__name__
            if not msg:
                detail = f"{type_name} (no message — likely connection reset or silent timeout)"
            elif msg.startswith(f"{type_name}:") or msg.startswith(f"{type_name} "):
                # Already prefixed with the type — don't double-prefix.
                # Prefix-anchored check (not substring) so a message that
                # happens to mention some OTHER class name mid-string
                # (e.g. "got OSError on read") doesn't suppress our own
                # type prefix and lose the diagnostic signal.
                detail = msg
            else:
                detail = f"{type_name}: {msg}"
            return f"{_A2A_ERROR_PREFIX}{detail} [target={target_url}]"


async def get_peers() -> list[dict]:
    """Get this workspace's peers from the platform registry."""
    async with httpx.AsyncClient(timeout=10.0) as client:
        try:
            resp = await client.get(
                f"{PLATFORM_URL}/registry/{WORKSPACE_ID}/peers",
                headers={"X-Workspace-ID": WORKSPACE_ID, **auth_headers()},
            )
            if resp.status_code == 200:
                return resp.json()
            return []
        except Exception:
            return []


async def get_workspace_info() -> dict:
    """Get this workspace's info from the platform."""
    async with httpx.AsyncClient(timeout=10.0) as client:
        try:
            resp = await client.get(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}",
                headers=auth_headers(),
            )
            if resp.status_code == 200:
                return resp.json()
            return {"error": "not found"}
        except Exception as e:
            return {"error": str(e)}
