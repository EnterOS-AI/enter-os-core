"""Auth gate for the /internal/* Starlette routes.

The platform calls into the workspace's HTTP server using a per-workspace
shared secret minted at provision time and stored in
``/configs/.platform_inbound_secret`` (see migration 044 + RFC #2312).
The workspace validates by string-equality against the file content —
the platform side stores the same plaintext in ``workspaces
.platform_inbound_secret`` and reads it back on every forward call.

Asymmetric to ``platform_auth.py``:

    platform_auth.py                platform_inbound_auth.py
    ────────────────                ────────────────────────
    workspace → platform            platform → workspace
    /configs/.auth_token            /configs/.platform_inbound_secret
    workspace presents bearer       workspace validates bearer

Fail-closed semantics (mirrors transcript_auth.py): if the secret file is
missing, empty, or unreadable, every request is rejected. The platform
will surface this as a structural error rather than silently sending
unauthenticated requests through.
"""
from __future__ import annotations

import logging
import os
from pathlib import Path

logger = logging.getLogger(__name__)

# In-process cache so we don't hit disk on every forward call. Same
# pattern as platform_auth._cached_token. The file is the durable copy;
# this var is the hot path.
_cached_secret: str | None = None


def _secret_file() -> Path:
    """Path to the on-disk inbound-secret file. Respects CONFIGS_DIR,
    falls back to /configs for the default container layout."""
    return Path(os.environ.get("CONFIGS_DIR", "/configs")) / ".platform_inbound_secret"


def get_inbound_secret() -> str | None:
    """Return the cached inbound secret, reading from disk on first call.

    Returns None if the file is missing, empty, or unreadable. Callers
    MUST treat None as an auth failure (fail-closed) — never substitute
    a default or skip-auth-on-missing semantics.
    """
    global _cached_secret
    if _cached_secret is not None:
        return _cached_secret
    path = _secret_file()
    if not path.exists():
        return None
    try:
        secret = path.read_text().strip()
    except OSError as exc:
        logger.warning("platform_inbound_auth: read %s failed: %s", path, exc)
        return None
    if not secret:
        return None
    _cached_secret = secret
    return secret


def reset_cache() -> None:
    """Drop the in-process cache. Used by tests + the rare runtime-side
    path that needs to re-read after the file is overwritten (e.g. a
    rotation flow lands in the future)."""
    global _cached_secret
    _cached_secret = None


def inbound_authorized(expected_secret: str | None, auth_header: str) -> bool:
    """Return True iff a /internal/* request should be served.

    Args:
        expected_secret: the workspace's stored inbound secret, or None
            if /configs/.platform_inbound_secret is absent / empty /
            unreadable.
        auth_header: raw Authorization request header value.

    Behavior:
        - None / empty expected → fail closed. A missing secret file
          is an auth failure, not a bypass.
        - Non-empty expected → strict string-equality against
          "Bearer <secret>". Bearer prefix is case-sensitive (matches
          the platform's wsauth.BearerTokenFromHeader contract).

    Constant-time comparison is used to avoid leaking the secret one
    byte at a time via timing analysis on a network-reachable endpoint.
    """
    if not expected_secret:
        return False
    expected = f"Bearer {expected_secret}"
    # hmac.compare_digest is the stdlib constant-time string compare.
    # Length mismatch is documented to short-circuit safely (returns
    # False without leaking length-difference timing).
    import hmac
    return hmac.compare_digest(auth_header, expected)
