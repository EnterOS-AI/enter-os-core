"""Resolve the configs directory used by the workspace runtime.

The runtime persists per-workspace state to a single directory:
``.auth_token`` (platform_auth), ``.platform_inbound_secret``
(platform_inbound_auth), ``.mcp_inbox_cursor`` (inbox). Inside a
workspace EC2 container that directory is ``/configs`` — a tmpfs/EBS
mount owned by the agent user, populated by the provisioner before
runtime boot.

Outside a container — operators running ``molecule-mcp`` on a laptop
for the external-runtime path — ``/configs`` doesn't exist (or, if it
does, isn't writable by an unprivileged user). The default would
silently fail on the first heartbeat: ``.platform_inbound_secret``
write hits ``Read-only file system: '/configs'``, the heartbeat thread
logs and dies, the workspace flips offline within a minute. The
operator sees no actionable error.

This module is the single resolution point. Resolution order:

    1. ``CONFIGS_DIR`` env var, if set — explicit operator override.
    2. ``/configs`` — used iff the path exists AND is writable. This
       preserves the in-container default for every existing deployment.
    3. ``$HOME/.molecule-workspace`` — the non-container fallback,
       created with mode 0700 so per-file 0600 perms aren't undermined
       by a world-readable parent.

Not cached: callers (heartbeat thread, MCP tools) hit this at most a
few times per second; reading the env var + one ``stat()`` call is
cheap, and the existing call sites read ``os.environ`` live so tests
that monkeypatch ``CONFIGS_DIR`` between cases keep working.

Issue: Molecule-AI/molecule-core#2458.
"""
from __future__ import annotations

import os
from pathlib import Path


def resolve() -> Path:
    """Return the configs directory, creating the home fallback if needed."""
    explicit = os.environ.get("CONFIGS_DIR", "").strip()
    if explicit:
        path = Path(explicit)
        path.mkdir(parents=True, exist_ok=True)
        return path

    in_container = Path("/configs")
    if in_container.exists() and os.access(str(in_container), os.W_OK):
        return in_container

    home_path = Path.home() / ".molecule-workspace"
    home_path.mkdir(parents=True, exist_ok=True, mode=0o700)
    return home_path


def reset_cache() -> None:
    """No-op kept for API stability; this module is stateless. Tests
    that called reset_cache when the cached prototype was in tree
    keep working without modification."""
    return
