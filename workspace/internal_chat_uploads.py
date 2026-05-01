"""POST /internal/chat/uploads/ingest — workspace-side chat upload sink.

Replaces the Docker-exec / tar-copy path the platform-side workspace-server
used historically (see RFC #2312). The platform forwards the multipart
request to this handler with a Bearer header carrying the workspace's
inbound secret; this handler validates, writes each file under
``/workspace/.molecule/chat-uploads/<random>-<sanitized-name>``, and
returns the same ``ChatUploadedFile`` shape the platform Go handler
returned previously, so callers (canvas, molecli, A2A tools) see no
contract change.

Why no platform-side Docker-exec equivalent here:
    The handler runs INSIDE the workspace container, which already has
    direct filesystem access to /workspace. mkdir + open + write is
    enough — no archive ceremony, no remote-exec round-trip, no
    docker socket dependency. Same code path on local Docker and SaaS
    EC2; the bug behind #2308 (platform's findContainer is nil in
    SaaS) cannot exist here by construction.

Path safety:
    sanitize_filename strips everything outside [A-Za-z0-9._-], collapses
    spaces, refuses ``""``/`"."`/`".."`, and caps length at 100 chars
    (preserving extension if ≤16 chars). Files are written with
    O_CREAT|O_EXCL|O_NOFOLLOW so a pre-existing symlink at the target
    cannot redirect the write to /etc/* or any sensitive location, and
    a colliding name fails fast (the random prefix already makes
    collisions astronomical, but defense-in-depth costs nothing).

Limits (matches the Go contract from chat_files.go):
    - 50 MB total request body
    - 25 MB per file
    - filename truncated to 100 chars

Response shape:
    {"files": [
        {"uri": "workspace:/workspace/.molecule/chat-uploads/<id>-<name>",
         "name": "<sanitized name>",
         "mimeType": "<content-type or guessed>",
         "size": <bytes>}
    ]}
"""
from __future__ import annotations

import logging
import mimetypes
import os
import re
import secrets as pysecrets
from pathlib import Path

from starlette.requests import Request
from starlette.responses import JSONResponse

from platform_inbound_auth import get_inbound_secret, inbound_authorized

logger = logging.getLogger(__name__)

# In-container destination — must match the platform-side Go constant
# `chatUploadDir` so the URI scheme stays identical and existing canvas
# / agent code that resolves "workspace:/workspace/.molecule/chat-uploads/*"
# keeps working unchanged.
CHAT_UPLOAD_DIR = "/workspace/.molecule/chat-uploads"

# Total-request body cap. multipart/form-data with multiple parts can
# add ~100 bytes of framing per file; the cap is the bytes hitting the
# socket, including framing.
CHAT_UPLOAD_MAX_BYTES = 50 * 1024 * 1024  # 50 MB

# Per-file cap. Keeping per-file under total lets a user attach, say,
# a 5 MB PDF + 10 small screenshots in a single batch.
CHAT_UPLOAD_MAX_FILE_BYTES = 25 * 1024 * 1024  # 25 MB

# Conservative {alnum, dot, underscore, dash} character class — anything
# outside gets rewritten so embedded paths, control chars, newlines,
# quotes, and shell metachars never reach the filesystem.
_UNSAFE_FILENAME_CHARS = re.compile(r"[^a-zA-Z0-9._\-]")


def sanitize_filename(name: str) -> str:
    """Reduce a user-supplied filename to a safe form.

    Mirrors workspace-server/internal/handlers/chat_files.go::sanitizeFilename
    so canvas-emitted URIs stay identical regardless of which path
    handles the upload.
    """
    base = os.path.basename(name)
    base = base.replace(" ", "_")
    base = _UNSAFE_FILENAME_CHARS.sub("_", base)
    if len(base) > 100:
        ext = ""
        dot = base.rfind(".")
        if dot >= 0 and len(base) - dot <= 16:
            ext = base[dot:]
        base = base[: 100 - len(ext)] + ext
    if base in ("", ".", ".."):
        return "file"
    return base


def _open_safe(path: str) -> int:
    """Open `path` for write with O_CREAT|O_EXCL|O_NOFOLLOW.

    Refuses to follow a pre-existing symlink at the target, and refuses
    to overwrite an existing regular file. Both protections close the
    same class of attack: a process inside the workspace container that
    raced to create a symlink at the destination before the upload landed.
    The random 16-byte prefix on the stored name makes the race
    effectively impossible, but defense-in-depth costs nothing here.
    """
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    # O_NOFOLLOW is POSIX; refuses to open if the path is a symlink.
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    return os.open(path, flags, 0o600)


async def ingest_handler(request: Request) -> JSONResponse:
    """POST /internal/chat/uploads/ingest — Starlette route handler.

    Auth: Bearer <platform_inbound_secret>; fail-closed when the secret
    file is missing or empty.

    Body: multipart/form-data with one or more `files` parts.

    Returns 200 with the list of stored URIs on success, or one of:
        401 unauthorized — bad / missing bearer
        400 bad request — malformed multipart, no files field, etc.
        413 payload too large — total body or per-file over cap
        500 internal — disk write failed
    """
    if not inbound_authorized(get_inbound_secret(), request.headers.get("Authorization", "")):
        return JSONResponse({"error": "unauthorized"}, status_code=401)

    # Total-body guard. Starlette won't enforce this for us; we read
    # Content-Length first and reject early to avoid streaming a 5 GB
    # request through the multipart parser only to bail at the end.
    cl_str = request.headers.get("Content-Length", "")
    if cl_str:
        try:
            cl = int(cl_str)
        except ValueError:
            cl = -1
        if cl > CHAT_UPLOAD_MAX_BYTES:
            return JSONResponse(
                {"error": f"request body exceeds total limit ({CHAT_UPLOAD_MAX_BYTES // (1024*1024)} MB)"},
                status_code=413,
            )

    try:
        form = await request.form(max_files=64, max_fields=32)
    except Exception as exc:  # multipart parse error
        logger.warning("internal_chat_uploads: multipart parse failed: %s", exc)
        return JSONResponse({"error": "failed to parse multipart form"}, status_code=400)

    # Starlette's FormData allows multiple values per key — `files` may
    # appear multiple times for batched uploads. getlist returns them
    # in order.
    parts = form.getlist("files")
    if not parts:
        return JSONResponse({"error": "expected at least one 'files' field"}, status_code=400)

    # Filter out non-file entries defensively. Starlette's UploadFile
    # has a .filename attribute; plain string fields don't.
    uploads = [p for p in parts if hasattr(p, "filename") and hasattr(p, "read")]
    if not uploads:
        return JSONResponse({"error": "expected at least one 'files' field"}, status_code=400)

    # mkdir -p is idempotent. Fired every call so a container restart
    # that wipes /workspace/.molecule doesn't surprise us.
    try:
        Path(CHAT_UPLOAD_DIR).mkdir(parents=True, exist_ok=True)
    except OSError as exc:
        # Surface errno + path in the response so a fresh-tenant
        # "failed to prepare uploads dir" 500 self-diagnoses without
        # requiring SSM access to the workspace stderr. Prior incident
        # 2026-05-01: hongming.moleculesai.app hit EACCES on the
        # /workspace volume's `.molecule` subtree (root-owned race
        # window between Docker volume create and entrypoint's chown,
        # fixed via molecule-ai-workspace-template-claude-code#23).
        # The errno + path are not security-sensitive — both are
        # well-known to anyone with workspace access.
        logger.error("internal_chat_uploads: mkdir %s failed: %s", CHAT_UPLOAD_DIR, exc)
        return JSONResponse(
            {
                "error": "failed to prepare uploads dir",
                "path": CHAT_UPLOAD_DIR,
                "errno": exc.errno,
                "detail": str(exc),
            },
            status_code=500,
        )

    response_files: list[dict] = []
    total_bytes = 0
    for upload in uploads:
        # Read into memory with a hard cap. Files larger than the cap
        # surface as 413; we don't truncate silently.
        data = await upload.read(CHAT_UPLOAD_MAX_FILE_BYTES + 1)
        if len(data) > CHAT_UPLOAD_MAX_FILE_BYTES:
            return JSONResponse(
                {"error": f"{upload.filename} exceeds per-file limit ({CHAT_UPLOAD_MAX_FILE_BYTES // (1024*1024)} MB)"},
                status_code=413,
            )
        total_bytes += len(data)
        if total_bytes > CHAT_UPLOAD_MAX_BYTES:
            return JSONResponse(
                {"error": f"total request body exceeds limit ({CHAT_UPLOAD_MAX_BYTES // (1024*1024)} MB)"},
                status_code=413,
            )

        sanitized = sanitize_filename(upload.filename or "file")
        # 16-byte random prefix → 32-hex-char + sanitized name. Same
        # shape as the Go handler's `hex.EncodeToString(rand 16) + "-" + name`.
        prefix = pysecrets.token_hex(16)
        stored = f"{prefix}-{sanitized}"
        target = os.path.join(CHAT_UPLOAD_DIR, stored)

        try:
            fd = _open_safe(target)
        except FileExistsError:
            # 32 hex chars of entropy → 128 bits → re-collision is
            # astronomical. If we hit it anyway, surface as 500 rather
            # than overwriting; the next retry will pick a fresh prefix.
            logger.error("internal_chat_uploads: collision at %s — refusing overwrite", target)
            return JSONResponse({"error": "internal collision; retry"}, status_code=500)
        except OSError as exc:
            logger.error("internal_chat_uploads: open %s failed: %s", target, exc)
            return JSONResponse({"error": "failed to write file"}, status_code=500)

        try:
            with os.fdopen(fd, "wb") as f:
                f.write(data)
        except OSError as exc:
            logger.error("internal_chat_uploads: write %s failed: %s", target, exc)
            # Best-effort cleanup of the partial file. unlink can fail
            # if the file was never created (open succeeded but write
            # failed before any bytes hit disk) or if the dir was
            # concurrently torn down — neither case warrants surfacing.
            try:
                os.unlink(target)
            except OSError as unlink_exc:
                logger.debug("internal_chat_uploads: unlink %s after write fail: %s", target, unlink_exc)
            return JSONResponse({"error": "failed to write file"}, status_code=500)

        # Mime type: prefer the part's Content-Type header, fall back to
        # extension-based guess. matches the Go handler's precedence.
        mime_type = upload.headers.get("content-type") if hasattr(upload, "headers") else None
        if not mime_type:
            mime_type, _ = mimetypes.guess_type(sanitized)

        response_files.append({
            "uri": f"workspace:{CHAT_UPLOAD_DIR}/{stored}",
            "name": sanitized,
            "mimeType": mime_type or "",
            "size": len(data),
        })

    return JSONResponse({"files": response_files}, status_code=200)
