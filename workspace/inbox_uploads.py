"""Poll-mode chat-upload fetcher + URI cache for the standalone path.

Companion to ``inbox.py``. When the workspace's inbox poller sees an
``activity_logs`` row with ``method='chat_upload_receive'`` (written by
the platform's ``uploadPollMode`` handler — workspace-server
``internal/handlers/chat_files.go``), this module:

    1. Pulls the bytes from
       ``GET /workspaces/:id/pending-uploads/:file_id/content``.
    2. Writes them to ``/workspace/.molecule/chat-uploads/<prefix>-<name>``
       — same on-disk shape as the push-mode handler in
       ``internal_chat_uploads.py``, so anything downstream that already
       resolves ``workspace:/workspace/.molecule/chat-uploads/...`` URIs
       works unchanged.
    3. POSTs ``/workspaces/:id/pending-uploads/:file_id/ack`` so Phase 3
       sweep can clean up the platform-side ``pending_uploads`` row.
    4. Records a ``platform-pending:<wsid>/<file_id> →
       workspace:/workspace/.molecule/chat-uploads/...`` mapping in a
       process-local cache so the chat message that arrives later
       (referencing the platform-pending URI) gets rewritten before the
       agent sees it.

URI rewrite ordering — the chat message containing the
``platform-pending:`` URI is logged by the platform AFTER the
``chat_upload_receive`` row, so the inbox poller sees the upload-receive
row first (lower activity_logs.id) and stages the bytes before the chat
message arrives in the same poll batch (or a later one). The URI cache
is therefore populated before the message_from_activity path needs it.
A miss (network race, restart with stale cursor) is handled by keeping
the original ``platform-pending:`` URI in the rewritten body — the agent
will see something it can't open, which is preferable to silently
dropping the URI.

Auth — same Bearer token the inbox poller uses (``platform_auth.auth_headers``).
Both endpoints are on the wsAuth-gated route, so this module can never
read another tenant's bytes even if a token is misrouted.
"""
from __future__ import annotations

import concurrent.futures
import logging
import mimetypes
import os
import re
import secrets as pysecrets
import threading
from collections import OrderedDict
from pathlib import Path
from typing import Any

logger = logging.getLogger(__name__)

# Same on-disk root as internal_chat_uploads.CHAT_UPLOAD_DIR — keeping
# these decoupled would let drift sneak in. Imported here rather than
# from internal_chat_uploads to avoid pulling in starlette as a
# transitive dep (this module runs in the standalone MCP path which
# doesn't ship the in-container HTTP server).
CHAT_UPLOAD_DIR = "/workspace/.molecule/chat-uploads"

# Per-file safety net. The platform enforces 25 MB on the staging side,
# but a buggy or hostile platform response shouldn't be able to fill the
# workspace's disk — refuse to write more than this even if the response
# claims a larger Content-Length.
MAX_FILE_BYTES = 25 * 1024 * 1024

# Network deadline for the GET. Tuned for a 25 MB transfer over a
# reasonable consumer link (~5 Mbps gives ~40s for the full payload),
# plus headroom for TLS + platform auth. Aligned with inbox poller's
# 10s default for /activity calls — both are user-perceived latency.
DEFAULT_FETCH_TIMEOUT = 60.0

# Concurrency cap for ``BatchFetcher``. Four workers is enough headroom
# for the realistic "user dragged 3-4 files into chat at once" case
# while bounding the platform's per-workspace fan-out. The cap matters
# because the platform's /content endpoint reads bytea from Postgres in
# a single round-trip per request — N workers = N concurrent DB reads
# of up to 25 MB each, so a higher cap could pressure platform memory
# without much UX win (network bandwidth is the bottleneck once the
# bytes are buffered).
DEFAULT_BATCH_FETCH_WORKERS = 4

# Upper bound on how long ``BatchFetcher.wait_all`` blocks the inbox
# poll loop before giving up on still-in-flight fetches. Aligned with
# DEFAULT_FETCH_TIMEOUT so a single hung fetch can't stall the loop
# longer than its own deadline. A timeout fires only if a worker thread
# is stuck past the underlying httpx timeout — pathological case;
# normal completion is bounded by per-fetch timeout × ceil(N/W).
DEFAULT_BATCH_WAIT_TIMEOUT = DEFAULT_FETCH_TIMEOUT + 5.0

# Cap on the URI cache. A long-lived workspace handling thousands of
# uploads shouldn't grow without bound; an LRU cap of 1024 keeps the
# entries-needed-for-a-typical-conversation well within memory.
URI_CACHE_MAX_ENTRIES = 1024

# Same character class as internal_chat_uploads — kept duplicated rather
# than imported to avoid dragging starlette into the standalone path.
_UNSAFE_FILENAME_CHARS = re.compile(r"[^a-zA-Z0-9._\-]")


def sanitize_filename(name: str) -> str:
    """Reduce a user-supplied filename to a safe form.

    Mirrors ``internal_chat_uploads.sanitize_filename`` and the Go
    handler's ``SanitizeFilename`` — three-way parity is pinned by
    ``workspace-server/internal/handlers/sanitize_filename_test.go`` and
    ``workspace/tests/test_internal_chat_uploads.py`` so the URI shape
    is identical regardless of which path handles the upload.
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


# ---------------------------------------------------------------------------
# URI cache — maps platform-pending URIs to local workspace: URIs
# ---------------------------------------------------------------------------


class _URICache:
    """Thread-safe bounded LRU mapping of platform-pending → workspace URIs.

    Bounded so a workspace that runs for months and handles thousands of
    uploads doesn't accumulate entries forever. ``OrderedDict.move_to_end``
    promotes recently-used entries; eviction takes the oldest.

    The cache is intentionally per-process — there is no persistence
    across a workspace restart. A restart with a stale inbox cursor that
    re-poll an upload-receive row will re-fetch (the bytes are already
    on disk from the prior session — see ``stage_to_disk``'s O_EXCL
    handling) and re-register; a chat message that referenced the
    platform-pending URI BEFORE the restart and arrives AFTER would miss
    the rewrite and surface the platform-pending URI to the agent. That
    is preferable to a stale persisted mapping that points at a deleted
    file.
    """

    def __init__(self, max_entries: int = URI_CACHE_MAX_ENTRIES):
        self._max = max_entries
        self._lock = threading.Lock()
        self._entries: "OrderedDict[str, str]" = OrderedDict()

    def get(self, pending_uri: str) -> str | None:
        with self._lock:
            local = self._entries.get(pending_uri)
            if local is not None:
                self._entries.move_to_end(pending_uri)
            return local

    def set(self, pending_uri: str, local_uri: str) -> None:
        with self._lock:
            self._entries[pending_uri] = local_uri
            self._entries.move_to_end(pending_uri)
            while len(self._entries) > self._max:
                self._entries.popitem(last=False)

    def __len__(self) -> int:
        with self._lock:
            return len(self._entries)

    def clear(self) -> None:
        with self._lock:
            self._entries.clear()


_cache = _URICache()


def get_cache() -> _URICache:
    """Expose the module-singleton cache for tests and the rewrite path."""
    return _cache


def resolve_pending_uri(uri: str) -> str | None:
    """Return the local ``workspace:`` URI for a ``platform-pending:`` URI,
    or None if not yet staged. Convenience for callers that want to
    fall back to an on-demand fetch — pass the result through to
    ``executor_helpers.resolve_attachment_uri``.
    """
    return _cache.get(uri)


# ---------------------------------------------------------------------------
# On-disk staging
# ---------------------------------------------------------------------------


def _open_safe(path: str) -> int:
    """Open ``path`` for write with ``O_CREAT|O_EXCL|O_NOFOLLOW``.

    Same shape as ``internal_chat_uploads._open_safe`` — refuses to
    follow a pre-existing symlink at the target and refuses to overwrite
    an existing regular file. The 16-byte random prefix makes a name
    collision astronomical, but defense-in-depth costs nothing.
    """
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    return os.open(path, flags, 0o600)


def stage_to_disk(content: bytes, filename: str) -> str:
    """Write ``content`` under ``CHAT_UPLOAD_DIR`` and return the local URI.

    Returns ``workspace:/workspace/.molecule/chat-uploads/<prefix>-<sanitized>``.
    The 32-hex prefix makes the on-disk name unguessable to anything
    that didn't see the response, so even if a stale agent has a guess
    at the original filename it can't construct a URL to a sibling's
    upload.

    Raises:
        OSError: write failure (mkdir, open, or write). Caller is
            expected to log + skip; the activity row stays unacked so a
            future poll re-tries.
        ValueError: ``content`` exceeds ``MAX_FILE_BYTES``. Pre-staging
            guard belt-and-braces above the platform's same-side cap.
    """
    if len(content) > MAX_FILE_BYTES:
        raise ValueError(
            f"content size {len(content)} exceeds workspace cap {MAX_FILE_BYTES}"
        )

    Path(CHAT_UPLOAD_DIR).mkdir(parents=True, exist_ok=True)

    sanitized = sanitize_filename(filename)
    prefix = pysecrets.token_hex(16)
    stored = f"{prefix}-{sanitized}"
    target = os.path.join(CHAT_UPLOAD_DIR, stored)

    fd = _open_safe(target)
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(content)
    except OSError:
        # Best-effort cleanup — partial writes leave a stub file that
        # would mask a future retry's success otherwise.
        try:
            os.unlink(target)
        except OSError:
            pass
        raise

    return f"workspace:{CHAT_UPLOAD_DIR}/{stored}"


# ---------------------------------------------------------------------------
# Activity row → fetch/stage/ack flow
# ---------------------------------------------------------------------------


def _request_body_dict(row: dict[str, Any]) -> dict[str, Any] | None:
    """Coerce ``row['request_body']`` into a dict.

    The /activity API returns request_body as JSON (already-deserialized
    by httpx). Some legacy paths or mocked transports may emit a string;
    handle defensively rather than raising.
    """
    body = row.get("request_body")
    if isinstance(body, dict):
        return body
    if isinstance(body, str):
        import json
        try:
            decoded = json.loads(body)
        except (TypeError, ValueError):
            return None
        return decoded if isinstance(decoded, dict) else None
    return None


def is_chat_upload_row(row: dict[str, Any]) -> bool:
    """True if ``row`` is the platform's chat-upload-receive activity.

    Used by the inbox poller to fork the row off the regular A2A
    message handling path — this row is not a peer message; it's an
    instruction to fetch + stage bytes. Match on ``method`` only;
    ``activity_type`` is already filtered to ``a2a_receive`` upstream.
    """
    return row.get("method") == "chat_upload_receive"


def fetch_and_stage(
    row: dict[str, Any],
    *,
    platform_url: str,
    workspace_id: str,
    headers: dict[str, str],
    timeout_secs: float = DEFAULT_FETCH_TIMEOUT,
    client: Any = None,
) -> str | None:
    """Fetch the row's bytes, stage them under chat-uploads, and ack.

    Returns the local ``workspace:`` URI on success, or ``None`` if any
    step failed (logged with enough detail to triage). Failure leaves
    the platform-side row unacked, so a subsequent poll retries — the
    activity row stays in the cursor's window because we DO advance the
    cursor (the row is "handled" from the inbox's perspective even on
    fetch failure; otherwise a permanent network outage would stall the
    cursor and block real chat traffic).

    On success, the URI cache is updated so a subsequent chat message
    referencing the same ``platform-pending:`` URI is rewritten before
    the agent sees it.

    Pass ``client`` to reuse a shared ``httpx.Client`` for both GET and
    POST ack (saves one TLS handshake per row vs. constructing one
    per-call). ``BatchFetcher`` does this across an entire poll batch so
    N concurrent fetches share one connection pool.
    """
    body = _request_body_dict(row)
    if body is None:
        logger.warning(
            "inbox_uploads: row %s missing request_body; cannot fetch",
            row.get("id"),
        )
        return None

    file_id = body.get("file_id")
    if not isinstance(file_id, str) or not file_id:
        logger.warning(
            "inbox_uploads: row %s has no file_id in request_body",
            row.get("id"),
        )
        return None

    pending_uri = body.get("uri")
    if not isinstance(pending_uri, str) or not pending_uri:
        # Reconstruct what the platform would have written — defensive
        # against a row whose uri field got truncated. Same shape as the
        # Go handler's URI builder.
        pending_uri = f"platform-pending:{workspace_id}/{file_id}"

    filename = body.get("name") or "file"
    if not isinstance(filename, str):
        filename = "file"

    # Caller-supplied client: reuse for both GET + POST ack. Otherwise
    # build a one-shot client and close it on the way out. Lazy httpx
    # import keeps the standalone MCP path's optional dep optional.
    own_client = client is None
    if own_client:
        try:
            import httpx  # noqa: WPS433
        except ImportError:
            logger.error("inbox_uploads: httpx not installed; cannot fetch %s", file_id)
            return None
        client = httpx.Client(timeout=timeout_secs)

    try:
        return _fetch_and_stage_with_client(
            client,
            platform_url=platform_url,
            workspace_id=workspace_id,
            headers=headers,
            file_id=file_id,
            pending_uri=pending_uri,
            filename=filename,
            body=body,
        )
    finally:
        if own_client:
            try:
                client.close()
            except Exception:  # noqa: BLE001 — close should never crash the caller
                pass


def _fetch_and_stage_with_client(
    client: Any,
    *,
    platform_url: str,
    workspace_id: str,
    headers: dict[str, str],
    file_id: str,
    pending_uri: str,
    filename: str,
    body: dict[str, Any],
) -> str | None:
    """Inner body of fetch_and_stage. Always uses the supplied client for
    both GET and POST so the connection pool is shared across the call.
    """
    content_url = f"{platform_url}/workspaces/{workspace_id}/pending-uploads/{file_id}/content"
    ack_url = f"{platform_url}/workspaces/{workspace_id}/pending-uploads/{file_id}/ack"

    try:
        resp = client.get(content_url, headers=headers)
    except Exception as exc:  # noqa: BLE001
        logger.warning("inbox_uploads: GET %s failed: %s", content_url, exc)
        return None

    if resp.status_code == 404:
        # Row was swept or already acked by a previous poll race — nothing
        # to fetch. Don't ack again; the platform's GC handles it. This is
        # a soft-skip, not an error — log at INFO so triage isn't noisy.
        logger.info(
            "inbox_uploads: pending upload %s already gone (404); skipping",
            file_id,
        )
        return None
    if resp.status_code >= 400:
        logger.warning(
            "inbox_uploads: GET %s returned %d: %s",
            content_url,
            resp.status_code,
            (resp.text or "")[:200],
        )
        return None

    content = resp.content or b""
    if len(content) > MAX_FILE_BYTES:
        logger.warning(
            "inbox_uploads: refusing to stage %s — size %d exceeds cap %d",
            file_id,
            len(content),
            MAX_FILE_BYTES,
        )
        return None

    # Mimetype precedence: platform's Content-Type header → request_body
    # mimeType field → extension guess. Same precedence as the in-
    # container ingest handler.
    mime_header = resp.headers.get("content-type", "").split(";")[0].strip()
    mime = (
        mime_header
        or (body.get("mimeType") if isinstance(body.get("mimeType"), str) else "")
        or (mimetypes.guess_type(filename)[0] or "")
    )

    try:
        local_uri = stage_to_disk(content, filename)
    except (OSError, ValueError) as exc:
        logger.error(
            "inbox_uploads: failed to stage %s (%s) to disk: %s",
            file_id,
            filename,
            exc,
        )
        return None

    _cache.set(pending_uri, local_uri)
    logger.info(
        "inbox_uploads: staged file_id=%s name=%s size=%d mime=%s pending_uri=%s local_uri=%s",
        file_id,
        filename,
        len(content),
        mime,
        pending_uri,
        local_uri,
    )

    # Ack last so a write failure above leaves the row available for a
    # retry on the next poll. A failed ack is logged but doesn't roll
    # back the on-disk file — the platform's sweep will clean up
    # eventually.
    try:
        ack_resp = client.post(ack_url, headers=headers)
        if ack_resp.status_code >= 400:
            logger.warning(
                "inbox_uploads: ack %s returned %d: %s",
                ack_url,
                ack_resp.status_code,
                (ack_resp.text or "")[:200],
            )
    except Exception as exc:  # noqa: BLE001
        logger.warning("inbox_uploads: POST %s failed: %s", ack_url, exc)

    return local_uri


# ---------------------------------------------------------------------------
# BatchFetcher — concurrent fetch across a single poll batch
# ---------------------------------------------------------------------------


class BatchFetcher:
    """Fetch + stage + ack a batch of upload-receive rows concurrently.

    Why this exists: the inbox poll loop used to call ``fetch_and_stage``
    serially per row. With N upload rows in a batch (a user dragging
    multiple files into chat at once), the loop blocked for
    ``N × per_fetch_latency`` before processing the chat message that
    referenced them — a 4-file upload at 5s each = 20s of stall
    before the agent saw the user's prompt. ``BatchFetcher`` runs the
    fetches on a small thread pool (default 4 workers) so the stall is
    bounded by ``ceil(N/W) × per_fetch_latency`` instead.

    Connection reuse: one ``httpx.Client`` is shared across every fetch
    in the batch. httpx clients carry a connection pool, so a second
    fetch to the same platform host reuses the TCP+TLS handshake from
    the first — measurable win when fetches happen back-to-back.

    Correctness invariant the caller MUST preserve: the inbox loop is
    expected to call ``wait_all()`` before processing the chat-message
    activity row that REFERENCES one of these uploads. Without the
    barrier, the URI cache is empty when ``rewrite_request_body`` runs
    and the agent sees the un-rewritten ``platform-pending:`` URI. The
    caller-side test ``test_poll_once_waits_for_uploads_before_messages``
    pins this end-to-end.

    Use as a context manager so the executor + client are torn down
    even if the caller raises mid-batch.
    """

    def __init__(
        self,
        *,
        platform_url: str,
        workspace_id: str,
        headers: dict[str, str],
        timeout_secs: float = DEFAULT_FETCH_TIMEOUT,
        max_workers: int = DEFAULT_BATCH_FETCH_WORKERS,
        client: Any = None,
    ):
        self._platform_url = platform_url
        self._workspace_id = workspace_id
        self._headers = dict(headers)  # copy so caller mutations don't leak in
        self._timeout_secs = timeout_secs

        # Caller can inject a client (tests do this); production callers
        # let us build one. Track ownership so we only close ours.
        self._own_client = client is None
        if self._own_client:
            try:
                import httpx  # noqa: WPS433
            except ImportError:
                # Match fetch_and_stage's behavior: log + degrade rather
                # than raising at construction time. submit() will then
                # return None for every row.
                logger.error("inbox_uploads: httpx not installed; BatchFetcher inert")
                self._client: Any = None
            else:
                self._client = httpx.Client(timeout=timeout_secs)
        else:
            self._client = client

        self._executor = concurrent.futures.ThreadPoolExecutor(
            max_workers=max_workers,
            thread_name_prefix="upload-fetch",
        )
        self._futures: list[concurrent.futures.Future[Any]] = []
        self._closed = False
        # Flipped to True by wait_all when the timeout fires; close()
        # reads this to decide between drain-and-wait vs cancel-queued.
        self._timed_out = False

    def submit(self, row: dict[str, Any]) -> concurrent.futures.Future[Any] | None:
        """Submit ``row`` for fetch + stage + ack. Non-blocking — the
        worker thread runs ``fetch_and_stage`` with the shared client.

        Returns the Future so a caller that wants per-row outcome can
        await it; ``None`` if the BatchFetcher is in a degraded state
        (httpx missing).
        """
        if self._closed:
            raise RuntimeError("BatchFetcher: submit after close")
        if self._client is None:
            return None
        fut = self._executor.submit(
            fetch_and_stage,
            row,
            platform_url=self._platform_url,
            workspace_id=self._workspace_id,
            headers=self._headers,
            timeout_secs=self._timeout_secs,
            client=self._client,
        )
        self._futures.append(fut)
        return fut

    def wait_all(self, timeout: float | None = DEFAULT_BATCH_WAIT_TIMEOUT) -> None:
        """Block until every submitted future completes (or times out).

        Per-future exceptions are logged + swallowed — ``fetch_and_stage``
        already converts every error path to ``return None``, so a real
        exception propagating up to here is unexpected and we don't want
        one bad fetch to abort the whole batch.

        Timeouts are also logged + swallowed AND record the timed-out
        futures on ``self._timed_out`` so ``close`` can cancel them
        without paying their full latency. Without this hand-off,
        ``close()``'s ``shutdown(wait=True)`` would block on the leaked
        workers and undo the user-facing timeout — the inbox poll loop
        would stall indefinitely on a hung /content fetch.
        """
        if not self._futures:
            return
        try:
            done, not_done = concurrent.futures.wait(
                self._futures,
                timeout=timeout,
                return_when=concurrent.futures.ALL_COMPLETED,
            )
        except Exception as exc:  # noqa: BLE001 — concurrent.futures shouldn't raise here
            logger.warning("inbox_uploads: BatchFetcher.wait_all crashed: %s", exc)
            return
        for fut in done:
            exc = fut.exception()
            if exc is not None:
                logger.warning(
                    "inbox_uploads: BatchFetcher worker raised: %s", exc
                )
        if not_done:
            logger.warning(
                "inbox_uploads: BatchFetcher.wait_all left %d in-flight after %ss timeout",
                len(not_done),
                timeout,
            )
            # Mark these futures so close() knows to cancel-not-wait. We
            # cancel queued-but-not-started ones immediately; futures
            # already running can't be cancelled (Python's threading
            # model), but close() will pass cancel_futures=True so any
            # remaining queued items don't run.
            for fut in not_done:
                fut.cancel()
            self._timed_out = True

    def close(self) -> None:
        """Tear down the executor + (if owned) the httpx client.

        Idempotent. After close, ``submit`` raises and the BatchFetcher
        cannot be reused — construct a fresh one for the next poll.

        If ``wait_all`` reported a timeout, shutdown skips the
        ``wait=True`` drain and instead asks the executor to drop queued
        futures (``cancel_futures=True``). Currently-running workers
        can't be interrupted by Python's threading model, but the poll
        loop returns immediately rather than blocking on a hung fetch.
        """
        if self._closed:
            return
        self._closed = True
        timed_out = getattr(self, "_timed_out", False)
        try:
            if timed_out:
                # cancel_futures landed in Python 3.9 — guarded for older
                # interpreters via a TypeError fallback. Drop queued
                # tasks; running ones will exit when their httpx call
                # eventually returns or the daemon thread dies.
                try:
                    self._executor.shutdown(wait=False, cancel_futures=True)
                except TypeError:
                    self._executor.shutdown(wait=False)
            else:
                # Healthy path: wait for in-flight work so we don't
                # interrupt a fetch mid-write.
                self._executor.shutdown(wait=True)
        except Exception as exc:  # noqa: BLE001
            logger.warning("inbox_uploads: executor shutdown error: %s", exc)
        if self._own_client and self._client is not None:
            try:
                self._client.close()
            except Exception as exc:  # noqa: BLE001
                logger.warning("inbox_uploads: client close error: %s", exc)

    def __enter__(self) -> "BatchFetcher":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()


# ---------------------------------------------------------------------------
# URI rewrite for incoming chat messages
# ---------------------------------------------------------------------------
#
# The chat message that references a staged upload arrives as a
# SEPARATE activity_log row, with parts of kind=file containing
# platform-pending: URIs in the file.uri field. Walk the structure
# in-place and rewrite to the local workspace: URI when the cache has it.
# Unknown URIs pass through unchanged — the agent gets to choose how
# to react (most runtimes log + ignore an unresolvable URI).


def _rewrite_part(part: Any) -> None:
    """Mutate a single A2A Part dict to swap platform-pending: URIs."""
    if not isinstance(part, dict):
        return
    file_obj = part.get("file")
    if not isinstance(file_obj, dict):
        return
    uri = file_obj.get("uri")
    if not isinstance(uri, str) or not uri.startswith("platform-pending:"):
        return
    rewritten = _cache.get(uri)
    if rewritten:
        file_obj["uri"] = rewritten


def rewrite_request_body(body: Any) -> None:
    """Mutate ``body`` in-place, replacing platform-pending: URIs with
    the cached local equivalents.

    Walks the same shapes ``inbox._extract_text`` accepts:

      - ``body['parts']``
      - ``body['params']['parts']``
      - ``body['params']['message']['parts']``

    No-op for shapes that don't match — the message simply passes
    through to the agent as-is.
    """
    if not isinstance(body, dict):
        return
    candidates: list[Any] = []
    params = body.get("params") if isinstance(body.get("params"), dict) else None
    if params:
        message = params.get("message") if isinstance(params.get("message"), dict) else None
        if message:
            candidates.append(message.get("parts"))
        candidates.append(params.get("parts"))
    candidates.append(body.get("parts"))

    for parts in candidates:
        if isinstance(parts, list):
            for part in parts:
                _rewrite_part(part)
