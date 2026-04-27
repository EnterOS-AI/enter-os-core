#!/usr/bin/env python3
"""Update workspace task status on the canvas.

Usage (from any script, cron job, or shell inside the container):

  # Set current task (shows on canvas card)
  python3 -m molecule_runtime.molecule_ai_status "Running weekly SEO audit..."

  # Clear task (removes banner from canvas)
  python3 -m molecule_runtime.molecule_ai_status ""

The status appears as an amber banner on the workspace card in the canvas,
visible to the project owner in real-time.
"""

import os
import sys

import httpx

_WORKSPACE_ID_raw = os.environ.get("WORKSPACE_ID")
if not _WORKSPACE_ID_raw:
    raise RuntimeError("WORKSPACE_ID environment variable is required but not set")
WORKSPACE_ID = _WORKSPACE_ID_raw
PLATFORM_URL = os.environ.get("PLATFORM_URL", "http://host.docker.internal:8080")


def set_status(task: str):
    """Push current_task to platform via heartbeat."""
    try:
        try:
            from platform_auth import auth_headers as _auth
            _headers = _auth()
        except Exception:
            _headers = {}
        httpx.post(
            f"{PLATFORM_URL}/registry/heartbeat",
            json={
                "workspace_id": WORKSPACE_ID,
                "current_task": task,
                "active_tasks": 1 if task else 0,
                "error_rate": 0,
                "sample_error": "",
                "uptime_seconds": 0,
            },
            headers=_headers,
            timeout=5.0,
        )
        if task:
            # Also log as activity for traceability
            httpx.post(
                f"{PLATFORM_URL}/workspaces/{WORKSPACE_ID}/activity",
                json={
                    "activity_type": "task_update",
                    "source_id": WORKSPACE_ID,
                    "summary": task,
                    "status": "ok",
                },
                timeout=5.0,
            )
    except Exception as e:
        print(f"molecule_ai_status: failed to update: {e}", file=sys.stderr)


if __name__ == "__main__":  # pragma: no cover
    if len(sys.argv) < 2:
        print("Usage: python3 -m molecule_runtime.molecule_ai_status 'task description'")
        print("       python3 -m molecule_runtime.molecule_ai_status ''  # clear")
        sys.exit(1)

    set_status(sys.argv[1])
