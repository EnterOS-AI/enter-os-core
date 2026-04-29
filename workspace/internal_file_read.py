"""GET /internal/file/read?path=<abs path> — workspace-side file read sink.

Companion to /internal/chat/uploads/ingest (RFC #2312 PR-B). Replaces the
docker-cp tar-stream extraction the platform-side workspace-server used
in chat_files.go::Download. Same path-safety contract as the legacy Go
handler:

  * absolute path required
  * must canonicalise to itself (no `..` segments, no double-slashes)
  * must land under one of {/configs, /workspace, /home, /plugins}
  * must be a regular file (not a directory, symlink, device, etc.)

Why a single broad "/internal/file/read" instead of a chat-specific path:

  Today's chat_files.go::Download already accepts paths under any of the
  four allowed roots — it's not strictly chat. Future PR-G/H will migrate
  /files/* template-config reads to the same forward pattern; reusing
  the same endpoint avoids three near-identical handlers (one per domain)
  with duplicated path-safety logic.

Auth: Bearer <platform_inbound_secret>; fail-closed when missing.

Response shape (matches Go contract for byte-for-byte compatibility):

  Content-Type: <mime.guess from extension or application/octet-stream>
  Content-Length: <stat size>
  Content-Disposition: attachment; filename="<basename>"; filename*=UTF-8''<encoded>
  body: raw file bytes (binary-safe — no JSON wrapping)
"""
from __future__ import annotations

import logging
import mimetypes
import os
import urllib.parse
from pathlib import Path

from starlette.requests import Request
from starlette.responses import FileResponse, JSONResponse

from platform_inbound_auth import get_inbound_secret, inbound_authorized

logger = logging.getLogger(__name__)

# Mirror chat_files.go's allowedRoots set. A request whose `path` doesn't
# fall under one of these — by exact-match or prefix-with-trailing-slash
# — is rejected at the gate, regardless of how many `..` segments
# canonicalised away.
_ALLOWED_ROOTS = ("/configs", "/workspace", "/home", "/plugins")


def _content_disposition_attachment(name: str) -> str:
    """Mirror chat_files.go::contentDispositionAttachment.

    Quotes, CR, and LF stripped/escaped per RFC 6266 / RFC 5987.
    Drop control chars, escape backslash and double-quote in the
    quoted-string. Emit percent-encoded filename* so non-ASCII names
    survive in clients that prefer the modern form.
    """
    safe_q: list[str] = []
    for ch in name:
        if ch in ("\r", "\n"):
            continue  # would terminate the header
        if ch in ('"', "\\"):
            safe_q.append("\\")
            safe_q.append(ch)
            continue
        if ord(ch) < 0x20 or ord(ch) == 0x7f:
            continue  # other control chars
        safe_q.append(ch)
    ascii_safe = "".join(safe_q)
    encoded = urllib.parse.quote(name, safe="")  # full RFC 3986 unreserved-only
    return f'attachment; filename="{ascii_safe}"; filename*=UTF-8\'\'{encoded}'


def _validate_path(path: str) -> tuple[bool, str]:
    """Return (ok, error_msg). Mirrors Go's chat_files.go::Download
    validation in the same order so error shapes stay identical."""
    if not path:
        return False, "path query required"
    if not os.path.isabs(path):
        return False, "path must be absolute"
    rooted = False
    for root in _ALLOWED_ROOTS:
        if path == root or path.startswith(root + "/"):
            rooted = True
            break
    if not rooted:
        return False, "path must be under /configs, /workspace, /home, or /plugins"
    # Reject anything that canonicalises differently or contains a
    # traversal segment. Defence-in-depth on top of the prefix check.
    if os.path.normpath(path) != path or ".." in path:
        return False, "invalid path"
    return True, ""


async def file_read_handler(request: Request):
    """GET /internal/file/read — Starlette route handler."""
    if not inbound_authorized(get_inbound_secret(), request.headers.get("Authorization", "")):
        return JSONResponse({"error": "unauthorized"}, status_code=401)

    path = request.query_params.get("path", "")
    ok, err = _validate_path(path)
    if not ok:
        return JSONResponse({"error": err}, status_code=400)

    # lstat (not stat) so a symlink at the path doesn't pretend to be the
    # file it points at — we want to know "is this LITERALLY a regular
    # file at the validated path." A symlink could redirect to /etc/*
    # or another mount.
    try:
        st = os.lstat(path)
    except FileNotFoundError:
        return JSONResponse({"error": "file not found"}, status_code=404)
    except OSError as exc:
        logger.warning("internal_file_read: lstat %s failed: %s", path, exc)
        return JSONResponse({"error": "stat failed"}, status_code=500)

    import stat as _stat
    if not _stat.S_ISREG(st.st_mode):
        return JSONResponse({"error": "path is not a regular file"}, status_code=400)

    name = os.path.basename(path)
    mime_type, _ = mimetypes.guess_type(name)
    if not mime_type:
        mime_type = "application/octet-stream"

    return FileResponse(
        path,
        media_type=mime_type,
        headers={
            "Content-Disposition": _content_disposition_attachment(name),
        },
    )
