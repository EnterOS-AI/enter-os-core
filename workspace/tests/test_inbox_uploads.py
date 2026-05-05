"""Tests for workspace/inbox_uploads.py — poll-mode chat-upload fetcher.

Covers the full activity-row → fetch → stage-on-disk → ack flow plus
the URI cache and the rewrite that swaps platform-pending: URIs to
local workspace: URIs in subsequent chat messages.
"""
from __future__ import annotations

import os
from typing import Any
from unittest.mock import MagicMock, patch

import pytest

import inbox_uploads


@pytest.fixture(autouse=True)
def _reset_cache_and_dir(tmp_path, monkeypatch):
    """Each test starts with an empty URI cache and a temp upload dir
    so on-disk artifacts from one test don't leak into the next."""
    inbox_uploads.get_cache().clear()
    monkeypatch.setattr(inbox_uploads, "CHAT_UPLOAD_DIR", str(tmp_path / "chat-uploads"))
    yield
    inbox_uploads.get_cache().clear()


# ---------------------------------------------------------------------------
# sanitize_filename — parity with internal_chat_uploads + Go SanitizeFilename
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "raw,want",
    [
        ("../../etc/passwd", "passwd"),
        ("/etc/passwd", "passwd"),
        ("hello world.pdf", "hello_world.pdf"),
        ("weird;chars!?.txt", "weird_chars__.txt"),
        ("中文.docx", "__.docx"),
        ("file (1).pdf", "file__1_.pdf"),
        ("report-2026.05.04_v2.pdf", "report-2026.05.04_v2.pdf"),
        ("", "file"),
        (".", "file"),
        ("..", "file"),
    ],
)
def test_sanitize_filename_parity_with_python_internal(raw, want):
    assert inbox_uploads.sanitize_filename(raw) == want


def test_sanitize_filename_caps_at_100_preserves_short_extension():
    long = "a" * 200 + ".pdf"
    got = inbox_uploads.sanitize_filename(long)
    assert len(got) == 100
    assert got.endswith(".pdf")


def test_sanitize_filename_drops_long_extension():
    long = "c" * 90 + ".thisisaverylongextensionnotpreserved"
    got = inbox_uploads.sanitize_filename(long)
    assert len(got) == 100
    assert ".thisisaverylongextensionnotpreserved" not in got


# ---------------------------------------------------------------------------
# _URICache — LRU semantics
# ---------------------------------------------------------------------------


def test_uricache_set_get_roundtrip():
    c = inbox_uploads._URICache(max_entries=10)
    c.set("platform-pending:ws/1", "workspace:/local/1")
    assert c.get("platform-pending:ws/1") == "workspace:/local/1"


def test_uricache_get_missing_returns_none():
    c = inbox_uploads._URICache(max_entries=10)
    assert c.get("platform-pending:ws/missing") is None


def test_uricache_evicts_oldest_at_capacity():
    c = inbox_uploads._URICache(max_entries=2)
    c.set("a", "A")
    c.set("b", "B")
    c.set("c", "C")  # evicts "a"
    assert c.get("a") is None
    assert c.get("b") == "B"
    assert c.get("c") == "C"
    assert len(c) == 2


def test_uricache_get_promotes_recently_used():
    c = inbox_uploads._URICache(max_entries=2)
    c.set("a", "A")
    c.set("b", "B")
    # Promote "a" by reading; next set should evict "b" instead of "a".
    assert c.get("a") == "A"
    c.set("c", "C")
    assert c.get("a") == "A"
    assert c.get("b") is None
    assert c.get("c") == "C"


def test_uricache_overwrite_updates_value():
    c = inbox_uploads._URICache(max_entries=10)
    c.set("k", "v1")
    c.set("k", "v2")
    assert c.get("k") == "v2"
    assert len(c) == 1


def test_uricache_clear():
    c = inbox_uploads._URICache(max_entries=10)
    c.set("a", "A")
    c.set("b", "B")
    c.clear()
    assert c.get("a") is None
    assert len(c) == 0


def test_resolve_pending_uri_uses_module_cache():
    inbox_uploads.get_cache().set("platform-pending:ws/x", "workspace:/local/x")
    assert inbox_uploads.resolve_pending_uri("platform-pending:ws/x") == "workspace:/local/x"
    assert inbox_uploads.resolve_pending_uri("platform-pending:ws/missing") is None


# ---------------------------------------------------------------------------
# stage_to_disk
# ---------------------------------------------------------------------------


def test_stage_to_disk_writes_file_and_returns_workspace_uri(tmp_path):
    uri = inbox_uploads.stage_to_disk(b"hello", "report.pdf")
    assert uri.startswith("workspace:")
    path = uri[len("workspace:"):]
    assert os.path.isfile(path)
    with open(path, "rb") as f:
        assert f.read() == b"hello"
    assert path.endswith("-report.pdf")
    # Prefix is 32 hex chars + "-" + name.
    name = os.path.basename(path)
    prefix, _, _ = name.partition("-")
    assert len(prefix) == 32


def test_stage_to_disk_sanitizes_filename():
    uri = inbox_uploads.stage_to_disk(b"x", "../../evil.txt")
    name = os.path.basename(uri)
    assert "/" not in name
    assert name.endswith("-evil.txt")


def test_stage_to_disk_rejects_oversize():
    with pytest.raises(ValueError):
        inbox_uploads.stage_to_disk(b"x" * (inbox_uploads.MAX_FILE_BYTES + 1), "big.bin")


def test_stage_to_disk_creates_directory_if_missing():
    # CHAT_UPLOAD_DIR is monkeypatched to a non-existent tmp path; the
    # call must mkdir -p it on first write.
    assert not os.path.exists(inbox_uploads.CHAT_UPLOAD_DIR)
    inbox_uploads.stage_to_disk(b"x", "a.txt")
    assert os.path.isdir(inbox_uploads.CHAT_UPLOAD_DIR)


def test_stage_to_disk_write_failure_cleans_partial_file(tmp_path, monkeypatch):
    # open() succeeds but write() fails — the partial file must be
    # removed so a retry can claim a fresh prefix without colliding.
    real_fdopen = os.fdopen
    written_paths: list[str] = []

    def boom_fdopen(fd, mode):
        # Wrap the real file with one whose write() raises.
        f = real_fdopen(fd, mode)
        # Track which path's fd we opened by inspecting the chat-upload dir.
        for entry in os.listdir(inbox_uploads.CHAT_UPLOAD_DIR):
            written_paths.append(os.path.join(inbox_uploads.CHAT_UPLOAD_DIR, entry))
        original_write = f.write

        def bad_write(b):
            original_write(b"")  # ensure file exists
            raise OSError(28, "no space")
        f.write = bad_write
        return f

    monkeypatch.setattr(os, "fdopen", boom_fdopen)
    with pytest.raises(OSError):
        inbox_uploads.stage_to_disk(b"data", "x.txt")
    # All staged files cleaned up.
    for p in written_paths:
        assert not os.path.exists(p)


def test_stage_to_disk_write_failure_unlink_failure_swallowed(monkeypatch):
    # open() succeeds, write() fails, unlink() ALSO fails — the unlink
    # error is swallowed and the original write error propagates.
    real_fdopen = os.fdopen

    def boom_fdopen(fd, mode):
        f = real_fdopen(fd, mode)

        def bad_write(_):
            raise OSError(28, "no space")
        f.write = bad_write
        return f

    def bad_unlink(_):
        raise OSError(13, "permission denied")

    monkeypatch.setattr(os, "fdopen", boom_fdopen)
    monkeypatch.setattr(os, "unlink", bad_unlink)
    with pytest.raises(OSError) as ei:
        inbox_uploads.stage_to_disk(b"data", "x.txt")
    # Original write error, not the unlink error.
    assert ei.value.errno == 28


def test_stage_to_disk_propagates_oserror_and_cleans_partial(tmp_path, monkeypatch):
    # Make the dir read-only AFTER mkdir succeeds, so open() fails. Skip
    # this on platforms where the dir's permissions don't restrict the
    # process owner (root in Docker, etc.).
    inbox_uploads.stage_to_disk(b"first", "a.txt")
    if os.geteuid() == 0:
        pytest.skip("root bypasses permission bits")
    os.chmod(inbox_uploads.CHAT_UPLOAD_DIR, 0o500)
    try:
        with pytest.raises(OSError):
            inbox_uploads.stage_to_disk(b"second", "b.txt")
    finally:
        os.chmod(inbox_uploads.CHAT_UPLOAD_DIR, 0o755)


# ---------------------------------------------------------------------------
# is_chat_upload_row + _request_body_dict
# ---------------------------------------------------------------------------


def test_is_chat_upload_row_true_on_method_match():
    assert inbox_uploads.is_chat_upload_row({"method": "chat_upload_receive"})


def test_is_chat_upload_row_false_on_other_methods():
    assert not inbox_uploads.is_chat_upload_row({"method": "message/send"})
    assert not inbox_uploads.is_chat_upload_row({"method": None})
    assert not inbox_uploads.is_chat_upload_row({})


def test_request_body_dict_passthrough():
    body = {"file_id": "x"}
    assert inbox_uploads._request_body_dict({"request_body": body}) is body


def test_request_body_dict_string_decoded():
    assert inbox_uploads._request_body_dict({"request_body": '{"a": 1}'}) == {"a": 1}


def test_request_body_dict_invalid_string_returns_none():
    assert inbox_uploads._request_body_dict({"request_body": "not json"}) is None


def test_request_body_dict_non_dict_after_decode_returns_none():
    assert inbox_uploads._request_body_dict({"request_body": "[1, 2]"}) is None


def test_request_body_dict_other_type_returns_none():
    assert inbox_uploads._request_body_dict({"request_body": 123}) is None


# ---------------------------------------------------------------------------
# fetch_and_stage — the full GET / write / ack flow
# ---------------------------------------------------------------------------


def _make_resp(status_code: int, content: bytes = b"", content_type: str = "", text: str = "") -> MagicMock:
    resp = MagicMock()
    resp.status_code = status_code
    resp.content = content
    headers: dict[str, str] = {}
    if content_type:
        headers["content-type"] = content_type
    resp.headers = headers
    resp.text = text
    return resp


def _patch_httpx_for_fetch(get_resp: MagicMock, ack_resp: MagicMock | None = None):
    """Patch httpx.Client so each new context-manager returns a client
    whose .get() returns get_resp and .post() returns ack_resp.
    """
    client = MagicMock()
    client.__enter__ = MagicMock(return_value=client)
    client.__exit__ = MagicMock(return_value=False)
    client.get = MagicMock(return_value=get_resp)
    client.post = MagicMock(return_value=ack_resp or _make_resp(200))
    return patch("httpx.Client", return_value=client), client


def _row(file_id: str = "file-1", uri: str | None = None, name: str = "report.pdf", body_extra: dict | None = None) -> dict:
    body: dict[str, Any] = {
        "file_id": file_id,
        "name": name,
        "mimeType": "application/pdf",
        "size": 9,
    }
    if uri is not None:
        body["uri"] = uri
    if body_extra:
        body.update(body_extra)
    return {
        "id": "act-100",
        "source_id": None,
        "method": "chat_upload_receive",
        "summary": "chat_upload_receive: report.pdf",
        "request_body": body,
        "created_at": "2026-05-04T10:00:00Z",
    }


def test_fetch_and_stage_happy_path_writes_file_acks_and_caches():
    pending_uri = "platform-pending:ws-1/file-1"
    row = _row(uri=pending_uri)
    get_resp = _make_resp(200, content=b"PDF-bytes", content_type="application/pdf")
    p, client = _patch_httpx_for_fetch(get_resp)
    with p:
        local_uri = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={"Authorization": "Bearer t"}
        )
    assert local_uri is not None
    assert local_uri.startswith("workspace:")
    # On-disk file content matches.
    path = local_uri[len("workspace:"):]
    with open(path, "rb") as f:
        assert f.read() == b"PDF-bytes"
    # Cache populated.
    assert inbox_uploads.get_cache().get(pending_uri) == local_uri
    # Ack POSTed to the right URL.
    client.post.assert_called_once()
    args, kwargs = client.post.call_args
    assert "/pending-uploads/file-1/ack" in args[0]
    assert kwargs["headers"]["Authorization"] == "Bearer t"


def test_fetch_and_stage_reconstructs_uri_when_missing_in_body():
    row = _row(uri=None)  # request_body has no 'uri'
    get_resp = _make_resp(200, content=b"x", content_type="text/plain")
    p, _ = _patch_httpx_for_fetch(get_resp)
    with p:
        inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    # Cache key reconstructed from workspace_id + file_id.
    assert inbox_uploads.get_cache().get("platform-pending:ws-1/file-1") is not None


def test_fetch_and_stage_returns_none_on_missing_request_body():
    row = {"id": "act-100", "method": "chat_upload_receive"}
    # No httpx call should happen, but we patch defensively.
    p, client = _patch_httpx_for_fetch(_make_resp(200))
    with p:
        result = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert result is None
    client.get.assert_not_called()


def test_fetch_and_stage_returns_none_on_missing_file_id():
    row = {"id": "act-100", "method": "chat_upload_receive", "request_body": {"name": "x.pdf"}}
    p, client = _patch_httpx_for_fetch(_make_resp(200))
    with p:
        result = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert result is None
    client.get.assert_not_called()


def test_fetch_and_stage_handles_nonstring_file_id():
    row = {"id": "act-100", "method": "chat_upload_receive", "request_body": {"file_id": 123}}
    p, client = _patch_httpx_for_fetch(_make_resp(200))
    with p:
        result = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert result is None
    client.get.assert_not_called()


def test_fetch_and_stage_404_returns_none_no_ack():
    row = _row()
    get_resp = _make_resp(404, text="gone")
    ack_resp = _make_resp(200)
    p, client = _patch_httpx_for_fetch(get_resp, ack_resp)
    with p:
        result = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert result is None
    # No ack — the row is already gone.
    client.post.assert_not_called()


def test_fetch_and_stage_500_returns_none_no_ack():
    row = _row()
    p, client = _patch_httpx_for_fetch(_make_resp(500, text="boom"))
    with p:
        result = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert result is None
    client.post.assert_not_called()


def test_fetch_and_stage_network_error_returns_none():
    row = _row()
    client = MagicMock()
    client.__enter__ = MagicMock(return_value=client)
    client.__exit__ = MagicMock(return_value=False)
    client.get = MagicMock(side_effect=RuntimeError("connection refused"))
    with patch("httpx.Client", return_value=client):
        result = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert result is None


def test_fetch_and_stage_oversize_response_refused():
    row = _row()
    big = b"x" * (inbox_uploads.MAX_FILE_BYTES + 1)
    p, client = _patch_httpx_for_fetch(_make_resp(200, content=big, content_type="application/octet-stream"))
    with p:
        result = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert result is None
    client.post.assert_not_called()


def test_fetch_and_stage_ack_failure_does_not_invalidate_local_uri():
    row = _row(uri="platform-pending:ws-1/file-1")
    get_resp = _make_resp(200, content=b"data", content_type="text/plain")
    ack_resp = _make_resp(500, text="ack failed")
    p, _ = _patch_httpx_for_fetch(get_resp, ack_resp)
    with p:
        local_uri = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    # On-disk staging succeeded; ack failure is logged but doesn't
    # roll back the cache.
    assert local_uri is not None
    assert inbox_uploads.get_cache().get("platform-pending:ws-1/file-1") == local_uri


def test_fetch_and_stage_ack_network_error_swallowed():
    row = _row(uri="platform-pending:ws-1/file-1")
    client = MagicMock()
    client.__enter__ = MagicMock(return_value=client)
    client.__exit__ = MagicMock(return_value=False)
    client.get = MagicMock(return_value=_make_resp(200, content=b"data", content_type="text/plain"))
    client.post = MagicMock(side_effect=RuntimeError("ack network error"))
    with patch("httpx.Client", return_value=client):
        result = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert result is not None  # GET succeeded → URI returned even if ack blew up


def test_fetch_and_stage_uses_response_content_type_when_present():
    row = _row(name="thing.bin", body_extra={"mimeType": "application/x-bogus"})
    # Response says image/png; should win over body's mimeType.
    get_resp = _make_resp(200, content=b"PNG", content_type="image/png; charset=binary")
    p, _ = _patch_httpx_for_fetch(get_resp)
    with p:
        # We don't assert on returned mime (not part of the contract);
        # the test just verifies the happy path runs without trying to
        # parse the trailing parameter.
        result = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert result is not None


def test_fetch_and_stage_nonstring_filename_falls_back_to_file():
    # body['name'] is a non-string (e.g. truncated to None or a number);
    # filename must default to "file" so sanitize_filename has something
    # to work with.
    row = _row(body_extra={"name": 12345})
    p, _ = _patch_httpx_for_fetch(_make_resp(200, content=b"x", content_type="text/plain"))
    with p:
        local_uri = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert local_uri is not None
    assert local_uri.endswith("-file")


def test_fetch_and_stage_default_filename_when_missing():
    row = {
        "id": "act",
        "method": "chat_upload_receive",
        "request_body": {"file_id": "file-1"},
    }
    p, _ = _patch_httpx_for_fetch(_make_resp(200, content=b"data", content_type="text/plain"))
    with p:
        local_uri = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert local_uri is not None
    assert local_uri.endswith("-file")  # default filename


def test_fetch_and_stage_disk_write_failure_returns_none(monkeypatch):
    row = _row()
    p, client = _patch_httpx_for_fetch(_make_resp(200, content=b"x", content_type="text/plain"))

    def bad_stage(*args, **kwargs):
        raise OSError(28, "no space left")
    monkeypatch.setattr(inbox_uploads, "stage_to_disk", bad_stage)

    with p:
        result = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert result is None
    client.post.assert_not_called()


def test_fetch_and_stage_disk_value_error_returns_none(monkeypatch):
    row = _row()
    p, client = _patch_httpx_for_fetch(_make_resp(200, content=b"x", content_type="text/plain"))

    def bad_stage(*args, **kwargs):
        raise ValueError("oversize after sanity check")
    monkeypatch.setattr(inbox_uploads, "stage_to_disk", bad_stage)

    with p:
        result = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert result is None
    client.post.assert_not_called()


def test_fetch_and_stage_httpx_missing_returns_none(monkeypatch):
    row = _row()
    # Simulate httpx not installed by making the import fail.
    import sys
    real_httpx = sys.modules.pop("httpx", None)
    monkeypatch.setitem(sys.modules, "httpx", None)
    try:
        result = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    finally:
        if real_httpx is not None:
            sys.modules["httpx"] = real_httpx
        else:
            sys.modules.pop("httpx", None)
    assert result is None


def test_fetch_and_stage_falls_back_to_extension_mime(monkeypatch):
    row = _row(name="snap.png", body_extra={"mimeType": ""})  # no mimeType in body
    # Response also has no content-type so it falls through to mimetypes.guess_type.
    get_resp = _make_resp(200, content=b"PNG", content_type="")
    p, _ = _patch_httpx_for_fetch(get_resp)
    with p:
        result = inbox_uploads.fetch_and_stage(
            row, platform_url="http://plat", workspace_id="ws-1", headers={}
        )
    assert result is not None


# ---------------------------------------------------------------------------
# rewrite_request_body — URI swap in chat-message bodies
# ---------------------------------------------------------------------------


def test_rewrite_request_body_swaps_pending_uri_in_message_parts():
    inbox_uploads.get_cache().set("platform-pending:ws/1", "workspace:/local/1")
    body = {
        "method": "message/send",
        "params": {
            "message": {
                "parts": [
                    {"kind": "text", "text": "see this"},
                    {"kind": "file", "file": {"uri": "platform-pending:ws/1", "name": "a.pdf"}},
                ]
            }
        },
    }
    inbox_uploads.rewrite_request_body(body)
    assert body["params"]["message"]["parts"][1]["file"]["uri"] == "workspace:/local/1"


def test_rewrite_request_body_swaps_in_params_parts():
    inbox_uploads.get_cache().set("platform-pending:ws/2", "workspace:/local/2")
    body = {
        "params": {
            "parts": [
                {"kind": "file", "file": {"uri": "platform-pending:ws/2"}},
            ]
        }
    }
    inbox_uploads.rewrite_request_body(body)
    assert body["params"]["parts"][0]["file"]["uri"] == "workspace:/local/2"


def test_rewrite_request_body_swaps_in_top_level_parts():
    inbox_uploads.get_cache().set("platform-pending:ws/3", "workspace:/local/3")
    body = {
        "parts": [{"kind": "file", "file": {"uri": "platform-pending:ws/3"}}]
    }
    inbox_uploads.rewrite_request_body(body)
    assert body["parts"][0]["file"]["uri"] == "workspace:/local/3"


def test_rewrite_request_body_leaves_unmatched_uri_unchanged():
    # No cache entry → URI stays as-is. Agent surfaces the unresolvable
    # URI rather than the inbox silently dropping the part.
    body = {
        "parts": [{"kind": "file", "file": {"uri": "platform-pending:ws/missing"}}]
    }
    inbox_uploads.rewrite_request_body(body)
    assert body["parts"][0]["file"]["uri"] == "platform-pending:ws/missing"


def test_rewrite_request_body_leaves_non_pending_uri_unchanged():
    inbox_uploads.get_cache().set("platform-pending:ws/3", "workspace:/local/3")
    body = {
        "parts": [
            {"kind": "file", "file": {"uri": "workspace:/already-local.pdf"}},
            {"kind": "file", "file": {"uri": "https://example.com/x.pdf"}},
        ]
    }
    inbox_uploads.rewrite_request_body(body)
    assert body["parts"][0]["file"]["uri"] == "workspace:/already-local.pdf"
    assert body["parts"][1]["file"]["uri"] == "https://example.com/x.pdf"


def test_rewrite_request_body_skips_non_dict_parts():
    body = {"parts": ["not a dict", 42, None]}
    inbox_uploads.rewrite_request_body(body)  # must not raise
    assert body["parts"] == ["not a dict", 42, None]


def test_rewrite_request_body_skips_text_parts():
    body = {
        "parts": [{"kind": "text", "text": "platform-pending:ws/should-not-rewrite"}]
    }
    inbox_uploads.rewrite_request_body(body)
    # Text content not touched — only file.uri fields are URIs.
    assert body["parts"][0]["text"] == "platform-pending:ws/should-not-rewrite"


def test_rewrite_request_body_skips_part_without_file_dict():
    body = {"parts": [{"kind": "file"}]}  # no file key
    inbox_uploads.rewrite_request_body(body)
    assert body["parts"] == [{"kind": "file"}]


def test_rewrite_request_body_skips_file_without_uri():
    body = {"parts": [{"kind": "file", "file": {"name": "x.pdf"}}]}
    inbox_uploads.rewrite_request_body(body)
    assert body["parts"][0]["file"] == {"name": "x.pdf"}


def test_rewrite_request_body_skips_nonstring_uri():
    body = {"parts": [{"kind": "file", "file": {"uri": None}}]}
    inbox_uploads.rewrite_request_body(body)  # must not raise


def test_rewrite_request_body_handles_non_dict_body():
    inbox_uploads.rewrite_request_body(None)  # no-op
    inbox_uploads.rewrite_request_body("string body")  # no-op
    inbox_uploads.rewrite_request_body([1, 2, 3])  # no-op


def test_rewrite_request_body_handles_non_dict_params():
    body = {"params": "not a dict", "parts": []}
    inbox_uploads.rewrite_request_body(body)  # must not raise


def test_rewrite_request_body_handles_non_dict_message():
    body = {"params": {"message": "not a dict"}}
    inbox_uploads.rewrite_request_body(body)  # must not raise


def test_rewrite_request_body_handles_non_list_parts():
    body = {"parts": "not a list"}
    inbox_uploads.rewrite_request_body(body)  # must not raise


def test_rewrite_request_body_handles_non_dict_file():
    body = {"parts": [{"kind": "file", "file": "not a dict"}]}
    inbox_uploads.rewrite_request_body(body)  # must not raise


# ---------------------------------------------------------------------------
# fetch_and_stage with shared client — Phase 5b client-reuse contract
# ---------------------------------------------------------------------------
#
# When a caller passes ``client=`` to fetch_and_stage, that client must be
# used for BOTH the GET /content and the POST /ack — no fresh
# ``httpx.Client(...)`` constructions should happen. The pre-Phase-5b
# implementation made one new client for GET and another for ack; the new
# shape lets BatchFetcher share one connection pool across an entire batch.


def test_fetch_and_stage_with_supplied_client_does_not_construct_new_client(monkeypatch):
    row = _row(uri="platform-pending:ws-1/file-1")
    get_resp = _make_resp(200, content=b"PDF", content_type="application/pdf")
    ack_resp = _make_resp(200)
    supplied = MagicMock()
    supplied.get = MagicMock(return_value=get_resp)
    supplied.post = MagicMock(return_value=ack_resp)
    # Sentinel: any code path that constructs httpx.Client when one was
    # already supplied is a regression — count constructions.
    constructed: list[Any] = []

    class _ShouldNotBeCalled:
        def __init__(self, *a, **kw):
            constructed.append((a, kw))

    monkeypatch.setattr("httpx.Client", _ShouldNotBeCalled)

    local_uri = inbox_uploads.fetch_and_stage(
        row,
        platform_url="http://plat",
        workspace_id="ws-1",
        headers={"Authorization": "Bearer t"},
        client=supplied,
    )
    assert local_uri is not None
    assert constructed == [], "supplied client must be reused; no new Client should be constructed"
    # GET + POST ack both went through the supplied client.
    supplied.get.assert_called_once()
    supplied.post.assert_called_once()
    # Caller-owned client must NOT be closed by fetch_and_stage; the
    # batch fetcher (or test) closes it once the whole batch is done.
    supplied.close.assert_not_called()


def test_fetch_and_stage_without_supplied_client_constructs_and_closes_one(monkeypatch):
    row = _row(uri="platform-pending:ws-1/file-1")
    get_resp = _make_resp(200, content=b"PDF", content_type="application/pdf")
    ack_resp = _make_resp(200)
    built: list[MagicMock] = []

    def _factory(*args, **kwargs):
        c = MagicMock()
        c.get = MagicMock(return_value=get_resp)
        c.post = MagicMock(return_value=ack_resp)
        built.append(c)
        return c

    monkeypatch.setattr("httpx.Client", _factory)

    local_uri = inbox_uploads.fetch_and_stage(
        row, platform_url="http://plat", workspace_id="ws-1", headers={}
    )
    assert local_uri is not None
    # Pre-Phase-5b built TWO clients (one for GET, one for ack); now exactly one.
    assert len(built) == 1, f"expected 1 httpx.Client construction, got {len(built)}"
    # Same client must serve BOTH calls.
    built[0].get.assert_called_once()
    built[0].post.assert_called_once()
    # Owned client must be closed by fetch_and_stage on the way out.
    built[0].close.assert_called_once()


def test_fetch_and_stage_with_supplied_client_does_not_close_caller_client():
    # Even on failure the supplied client must not be closed — the
    # BatchFetcher owns the lifecycle for the whole batch.
    row = _row(uri="platform-pending:ws-1/file-1")
    supplied = MagicMock()
    supplied.get = MagicMock(side_effect=RuntimeError("network down"))
    supplied.post = MagicMock()  # should not be reached on GET failure
    inbox_uploads.fetch_and_stage(
        row,
        platform_url="http://plat",
        workspace_id="ws-1",
        headers={},
        client=supplied,
    )
    supplied.close.assert_not_called()
    supplied.post.assert_not_called()


# ---------------------------------------------------------------------------
# BatchFetcher — concurrent fetch + URI cache barrier
# ---------------------------------------------------------------------------


def _row_with_id(act_id: str, file_id: str) -> dict:
    """Helper: an upload-receive row with a distinct activity id + file id."""
    return {
        "id": act_id,
        "method": "chat_upload_receive",
        "request_body": {
            "file_id": file_id,
            "name": f"{file_id}.pdf",
            "uri": f"platform-pending:ws-1/{file_id}",
            "mimeType": "application/pdf",
            "size": 1,
        },
    }


def _stub_client_for_batch(get_responses: dict[str, MagicMock]) -> MagicMock:
    """Build one MagicMock client that returns per-file_id responses
    based on the file_id segment of the URL.
    """
    client = MagicMock()

    def _get(url: str, headers: dict[str, str] | None = None) -> MagicMock:
        for fid, resp in get_responses.items():
            if f"/pending-uploads/{fid}/content" in url:
                return resp
        return _make_resp(404)

    def _post(url: str, headers: dict[str, str] | None = None) -> MagicMock:
        return _make_resp(200)

    client.get = MagicMock(side_effect=_get)
    client.post = MagicMock(side_effect=_post)
    return client


def test_batch_fetcher_runs_submitted_rows_concurrently():
    # Three rows whose .get() blocks for ~120ms each. With 4 workers the
    # batch should complete in ~120ms (parallel), not ~360ms (serial).
    # The 250ms ceiling accommodates CI scheduler jitter while still
    # discriminating concurrent (~120ms) from serial (~360ms).
    import time

    barrier_start = [0.0]

    def _slow_get(url: str, headers: dict[str, str] | None = None) -> MagicMock:
        time.sleep(0.12)
        for fid in ("a", "b", "c"):
            if f"/pending-uploads/{fid}/content" in url:
                return _make_resp(200, content=b"X", content_type="text/plain")
        return _make_resp(404)

    client = MagicMock()
    client.get = MagicMock(side_effect=_slow_get)
    client.post = MagicMock(return_value=_make_resp(200))

    bf = inbox_uploads.BatchFetcher(
        platform_url="http://plat",
        workspace_id="ws-1",
        headers={},
        client=client,
        max_workers=4,
    )
    barrier_start[0] = time.time()
    for fid in ("a", "b", "c"):
        bf.submit(_row_with_id(f"act-{fid}", fid))
    bf.wait_all()
    elapsed = time.time() - barrier_start[0]
    bf.close()

    assert elapsed < 0.25, (
        f"3 rows × 120ms with 4 workers should finish in <250ms; got {elapsed:.3f}s "
        "(suggests serial execution — Phase 5b regression)"
    )
    assert client.get.call_count == 3
    assert client.post.call_count == 3


def test_batch_fetcher_wait_all_blocks_until_uri_cache_populated():
    """Pin the correctness invariant: when wait_all returns, the URI
    cache is hot for every submitted row. Without this barrier the
    inbox loop would process the chat-message row before its uploads
    were staged, and rewrite_request_body would surface the un-rewritten
    platform-pending: URI to the agent.
    """
    import time

    def _slow_get(url: str, headers: dict[str, str] | None = None) -> MagicMock:
        time.sleep(0.05)
        return _make_resp(200, content=b"data", content_type="text/plain")

    client = MagicMock()
    client.get = MagicMock(side_effect=_slow_get)
    client.post = MagicMock(return_value=_make_resp(200))

    inbox_uploads.get_cache().clear()
    with inbox_uploads.BatchFetcher(
        platform_url="http://plat", workspace_id="ws-1", headers={}, client=client
    ) as bf:
        bf.submit(_row_with_id("act-a", "a"))
        bf.submit(_row_with_id("act-b", "b"))
        bf.wait_all()
        # Cache must be hot for BOTH rows by the time wait_all returns.
        assert inbox_uploads.get_cache().get("platform-pending:ws-1/a") is not None
        assert inbox_uploads.get_cache().get("platform-pending:ws-1/b") is not None


def test_batch_fetcher_isolates_per_row_failure():
    """One failing fetch must not abort siblings. Sibling rows complete,
    URI cache populates for them; the bad row's cache entry stays absent.
    """
    def _get(url: str, headers: dict[str, str] | None = None) -> MagicMock:
        if "/pending-uploads/bad/content" in url:
            return _make_resp(500, text="upstream broken")
        return _make_resp(200, content=b"ok", content_type="text/plain")

    client = MagicMock()
    client.get = MagicMock(side_effect=_get)
    client.post = MagicMock(return_value=_make_resp(200))

    inbox_uploads.get_cache().clear()
    with inbox_uploads.BatchFetcher(
        platform_url="http://plat", workspace_id="ws-1", headers={}, client=client
    ) as bf:
        bf.submit(_row_with_id("act-1", "good1"))
        bf.submit(_row_with_id("act-2", "bad"))
        bf.submit(_row_with_id("act-3", "good2"))
        bf.wait_all()

    cache = inbox_uploads.get_cache()
    assert cache.get("platform-pending:ws-1/good1") is not None
    assert cache.get("platform-pending:ws-1/good2") is not None
    assert cache.get("platform-pending:ws-1/bad") is None


def test_batch_fetcher_reuses_one_client_across_all_submits():
    """Every row in the batch must share the same client instance. This
    is the connection-pool-reuse leg of the perf win: a second fetch
    to the same host reuses the TCP+TLS handshake from the first.
    """
    client = MagicMock()
    client.get = MagicMock(return_value=_make_resp(200, content=b"x", content_type="text/plain"))
    client.post = MagicMock(return_value=_make_resp(200))

    with inbox_uploads.BatchFetcher(
        platform_url="http://plat", workspace_id="ws-1", headers={}, client=client
    ) as bf:
        for fid in ("a", "b", "c"):
            bf.submit(_row_with_id(f"act-{fid}", fid))
        bf.wait_all()

    # 3 GETs + 3 POST acks all on the same client — no per-row Client
    # construction.
    assert client.get.call_count == 3
    assert client.post.call_count == 3


def test_batch_fetcher_close_idempotent():
    client = MagicMock()
    bf = inbox_uploads.BatchFetcher(
        platform_url="http://plat", workspace_id="ws-1", headers={}, client=client
    )
    bf.close()
    bf.close()  # second call must not raise


def test_batch_fetcher_submit_after_close_raises():
    client = MagicMock()
    bf = inbox_uploads.BatchFetcher(
        platform_url="http://plat", workspace_id="ws-1", headers={}, client=client
    )
    bf.close()
    with pytest.raises(RuntimeError, match="submit after close"):
        bf.submit(_row_with_id("act-x", "x"))


def test_batch_fetcher_owns_client_when_not_supplied(monkeypatch):
    built: list[MagicMock] = []

    def _factory(*args, **kwargs):
        c = MagicMock()
        c.get = MagicMock(return_value=_make_resp(200, content=b"x", content_type="text/plain"))
        c.post = MagicMock(return_value=_make_resp(200))
        built.append(c)
        return c

    monkeypatch.setattr("httpx.Client", _factory)

    bf = inbox_uploads.BatchFetcher(
        platform_url="http://plat", workspace_id="ws-1", headers={}
    )
    bf.submit(_row_with_id("act-a", "a"))
    bf.wait_all()
    bf.close()

    assert len(built) == 1, "expected one owned client per BatchFetcher"
    built[0].close.assert_called_once()


def test_batch_fetcher_does_not_close_supplied_client():
    client = MagicMock()
    client.get = MagicMock(return_value=_make_resp(200, content=b"x", content_type="text/plain"))
    client.post = MagicMock(return_value=_make_resp(200))
    with inbox_uploads.BatchFetcher(
        platform_url="http://plat", workspace_id="ws-1", headers={}, client=client
    ) as bf:
        bf.submit(_row_with_id("act-a", "a"))
        bf.wait_all()
    # Supplied client survives the BatchFetcher's close — caller's lifecycle.
    client.close.assert_not_called()


def test_batch_fetcher_wait_all_no_op_on_empty_batch():
    client = MagicMock()
    with inbox_uploads.BatchFetcher(
        platform_url="http://plat", workspace_id="ws-1", headers={}, client=client
    ) as bf:
        bf.wait_all()  # nothing submitted; must not block, must not raise
    client.get.assert_not_called()
    client.post.assert_not_called()


def test_batch_fetcher_httpx_missing_makes_submit_a_noop(monkeypatch):
    # No client supplied + httpx import fails → BatchFetcher degrades
    # gracefully: submit() returns None and the row is silently skipped.
    import sys

    real_httpx = sys.modules.pop("httpx", None)
    monkeypatch.setitem(sys.modules, "httpx", None)
    try:
        bf = inbox_uploads.BatchFetcher(
            platform_url="http://plat", workspace_id="ws-1", headers={}
        )
        result = bf.submit(_row_with_id("act-a", "a"))
        bf.wait_all()
        bf.close()
    finally:
        if real_httpx is not None:
            sys.modules["httpx"] = real_httpx
        else:
            sys.modules.pop("httpx", None)
    assert result is None


def test_batch_fetcher_close_after_timeout_does_not_block_on_running_workers():
    """The deadline contract: when wait_all times out, close() must NOT
    block waiting for the leaked worker threads. Otherwise the inbox
    poll loop stalls indefinitely on a hung /content fetch — undoing
    the user-facing timeout.

    Strategy: build a client whose .get() blocks on a threading.Event
    that the test never sets. Submit a row, wait_all with a tiny
    timeout, then time close(). If close() drained-and-waited it would
    block until we set the event (i.e., forever in this test).
    """
    import threading
    import time

    blocker = threading.Event()  # never set — workers stay running

    def _hang_get(url, headers=None):
        # Wait at most ~5s so a buggy implementation eventually unblocks
        # the test instead of timing out the whole pytest run, but
        # nothing legitimate should reach this fallback.
        blocker.wait(timeout=5.0)
        return _make_resp(200, content=b"x", content_type="text/plain")

    client = MagicMock()
    client.get = MagicMock(side_effect=_hang_get)
    client.post = MagicMock(return_value=_make_resp(200))

    bf = inbox_uploads.BatchFetcher(
        platform_url="http://plat",
        workspace_id="ws-1",
        headers={},
        client=client,
        max_workers=1,  # serialize so submitting 1 keeps the worker busy
    )
    bf.submit(_row_with_id("act-a", "a"))
    # Tiny timeout — wait_all must report the future as not_done.
    bf.wait_all(timeout=0.05)
    t0 = time.time()
    bf.close()
    elapsed = time.time() - t0
    # Unblock the lingering worker so it doesn't pollute later tests.
    blocker.set()

    # Without the cancel-on-timeout fix, close() would block until
    # blocker.set() — i.e., the full ~5s. With the fix it returns
    # immediately because shutdown(wait=False) doesn't drain.
    assert elapsed < 1.0, (
        f"close() blocked for {elapsed:.2f}s after wait_all timeout — "
        "cancel-on-timeout regression: close() is draining instead of bailing"
    )


def test_batch_fetcher_close_without_timeout_still_drains():
    """Negative leg of the timeout contract: when wait_all completes
    cleanly (no timeout), close() must KEEP its drain-and-wait
    behavior so a still-queued ack POST isn't dropped mid-write.
    """
    import time

    def _slow_get(url, headers=None):
        time.sleep(0.05)
        return _make_resp(200, content=b"x", content_type="text/plain")

    client = MagicMock()
    client.get = MagicMock(side_effect=_slow_get)
    client.post = MagicMock(return_value=_make_resp(200))

    bf = inbox_uploads.BatchFetcher(
        platform_url="http://plat",
        workspace_id="ws-1",
        headers={},
        client=client,
        max_workers=2,
    )
    bf.submit(_row_with_id("act-a", "a"))
    bf.submit(_row_with_id("act-b", "b"))
    bf.wait_all()  # generous default timeout — should not fire
    bf.close()

    # All 2 GETs + 2 ACK POSTs ran to completion via drain-and-wait.
    assert client.get.call_count == 2
    assert client.post.call_count == 2
