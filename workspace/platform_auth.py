"""Workspace auth-token store (Phase 30.1).

Single source of truth for this workspace's authentication token. The
token is issued by the platform on the first successful
``POST /registry/register`` call and travels with every subsequent
heartbeat / update-card / (later) secrets-pull / A2A request.

The token is persisted to ``<configs>/.auth_token`` so it survives
restarts — we only expect to receive it once from the platform, since
``/registry/register`` no-ops token issuance for workspaces that already
have one on file.

Storage:
    ${CONFIGS_DIR}/.auth_token        # 0600, one line, no trailing newline

Callers interact with three functions:
    :func:`get_token`   — returns the cached token or None
    :func:`save_token`  — persists a freshly-issued token
    :func:`auth_headers`— builds the Authorization header dict for httpx
"""
from __future__ import annotations

import logging
import os
from pathlib import Path

import configs_dir

logger = logging.getLogger(__name__)

# In-process cache so we don't hit disk on every heartbeat. The heartbeat
# loop fires on a short interval and reading a tiny file 10x per minute
# is wasteful. The file is the durable copy; this var is the hot path.
_cached_token: str | None = None


def _token_file() -> Path:
    """Path to the on-disk token file. Resolved via configs_dir so
    in-container (/configs) and external-runtime (~/.molecule-workspace)
    operators land on a writable location automatically. Explicit
    CONFIGS_DIR env var still wins."""
    return configs_dir.resolve() / ".auth_token"


def get_token() -> str | None:
    """Return the cached token, reading it from disk on first call.

    Resolution order:
        1. In-process cache (hot path)
        2. ``${CONFIGS_DIR}/.auth_token`` file (in-container default —
           the platform writes this on provision and rotates it on
           restart)
        3. ``MOLECULE_WORKSPACE_TOKEN`` env var (external-runtime path —
           operators running the universal MCP server outside a
           container have no /configs volume to populate, so they pass
           the token via env)

    File-first preserves in-container behavior unchanged: containers
    always have /configs/.auth_token on disk, env-var fallback only
    fires when there's no file. This is additive — no existing caller
    sees a behavior change.
    """
    global _cached_token
    if _cached_token is not None:
        return _cached_token
    path = _token_file()
    if path.exists():
        try:
            tok = path.read_text().strip()
        except OSError as exc:
            logger.warning("platform_auth: failed to read %s: %s", path, exc)
            tok = ""
        if tok:
            _cached_token = tok
            return tok
    # File missing or empty — fall back to env (external-runtime path).
    env_tok = os.environ.get("MOLECULE_WORKSPACE_TOKEN", "").strip()
    if env_tok:
        _cached_token = env_tok
        return env_tok
    return None


def save_token(token: str) -> None:
    """Persist a newly-issued token. Creates the file with 0600 mode atomically.

    Uses ``os.open(O_CREAT, 0o600)`` so the file is never world-readable,
    even transiently. The previous ``write_text()`` + ``chmod()`` approach
    had a TOCTOU window where a concurrent reader could access the token
    between the two syscalls (M4 — flagged in security audit cycle 10).

    Idempotent — if an identical token is already on disk we skip the
    write so we don't churn the file's mtime or trigger spurious
    filesystem watchers."""
    global _cached_token
    token = token.strip()
    if not token:
        raise ValueError("platform_auth: refusing to save empty token")
    if get_token() == token:
        return
    path = _token_file()
    path.parent.mkdir(parents=True, exist_ok=True)
    # O_CREAT | O_WRONLY | O_TRUNC with mode=0o600 atomically creates (or
    # truncates) the file with restricted permissions in a single syscall,
    # eliminating the TOCTOU window.
    fd = os.open(str(path), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.write(fd, token.encode())
    finally:
        os.close(fd)
    _cached_token = token


def auth_headers() -> dict[str, str]:
    """Return a header dict to merge into httpx calls. Empty if no token
    is available yet — callers send the request as-is and the platform's
    heartbeat handler grandfathers pre-token workspaces through until
    their next /registry/register issues one.

    Always sets ``Origin`` to ``PLATFORM_URL`` when that env var is set.
    On hosted SaaS deployments the tenant's edge WAF requires a same-
    origin header — without it ``/workspaces/*`` and ``/registry/*/peers``
    requests get silently rewritten to the canvas Next.js app, which has
    no such routes and returns an empty 404. Inside-container calls are
    unaffected (Docker-internal PLATFORM_URLs aren't behind the WAF).
    Discovered while smoke-testing the molecule-mcp external-runtime
    path against a live tenant — every tool call returned "not found"
    because the WAF was eating them.
    """
    headers: dict[str, str] = {}
    platform_url = os.environ.get("PLATFORM_URL", "").strip()
    if platform_url:
        headers["Origin"] = platform_url
    tok = get_token()
    if tok:
        headers["Authorization"] = f"Bearer {tok}"
    return headers


def self_source_headers(workspace_id: str) -> dict[str, str]:
    """Return auth headers PLUS X-Workspace-ID identifying this workspace
    as the source of the request.

    Use this for any POST the workspace's own runtime fires against the
    platform's A2A endpoints — heartbeat self-messages, initial_prompt,
    idle-loop fires, peer-to-peer A2A from runtime tools. Without the
    X-Workspace-ID header the platform's a2a_receive logger writes
    source_id=NULL, which the canvas's My Chat tab interprets as a
    user-typed message and renders the internal prompt to the user.
    See workspace-server/internal/handlers/a2a_proxy.go:184 for the
    server-side classification rule.

    Centralised here so adding a new system header (e.g. a per-fire
    correlation ID) only touches one place — and so that any
    workspace→A2A POST that doesn't use this helper stands out in
    review as a probable bug."""
    return {**auth_headers(), "X-Workspace-ID": workspace_id}


def clear_cache() -> None:
    """Reset the in-memory cache. Used by tests that write fresh token
    files between cases."""
    global _cached_token
    _cached_token = None


def refresh_cache() -> str | None:
    """Force re-read of the token from disk, discarding the in-process cache.

    Use this when a 401 response suggests the cached token is stale —
    e.g. after the platform rotates tokens during a restart (issue #1877).
    Returns the (new) token value or None if not found/error."""
    global _cached_token
    _cached_token = None
    return get_token()
