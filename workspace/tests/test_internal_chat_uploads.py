"""Unit + functional tests for /internal/chat/uploads/ingest.

Exercises the route via Starlette's TestClient so multipart parsing,
auth, and disk-write paths all run together.
"""
from __future__ import annotations

import os
from pathlib import Path

import pytest
from starlette.applications import Starlette
from starlette.routing import Route
from starlette.testclient import TestClient

import platform_inbound_auth
import internal_chat_uploads
from internal_chat_uploads import ingest_handler, sanitize_filename


@pytest.fixture
def configs_dir(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
    platform_inbound_auth.reset_cache()
    yield tmp_path
    platform_inbound_auth.reset_cache()


@pytest.fixture
def chat_uploads_dir(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Redirect CHAT_UPLOAD_DIR to a writable tmp path.

    The default /workspace/.molecule/chat-uploads requires real container
    filesystem; under pytest we point it at a tmpdir so the tests
    don't need root + container.
    """
    target = tmp_path / "chat-uploads"
    monkeypatch.setattr(internal_chat_uploads, "CHAT_UPLOAD_DIR", str(target))
    return target


@pytest.fixture
def client(configs_dir: Path, chat_uploads_dir: Path) -> TestClient:
    (configs_dir / ".platform_inbound_secret").write_text("test-secret")
    app = Starlette(routes=[
        Route("/internal/chat/uploads/ingest", ingest_handler, methods=["POST"]),
    ])
    return TestClient(app)


# ───────────── sanitize_filename ─────────────

@pytest.mark.parametrize("raw,expected", [
    ("foo.txt", "foo.txt"),
    ("hello world.txt", "hello_world.txt"),
    ("../../../etc/passwd", "passwd"),     # basename strips path; sanitize keeps the rest clean
    ("sneaky/../sneaky.png", "sneaky.png"),
    ("file with spaces & symbols!.png", "file_with_spaces___symbols_.png"),
    ("", "file"),                          # empty → safe default
    (".", "file"),
    ("..", "file"),
    ("名前.txt", "__.txt"),                  # Python operates on codepoints (2 CJK chars → 2 underscores); Go operated on bytes
])
def test_sanitize_filename(raw: str, expected: str):
    assert sanitize_filename(raw) == expected


def test_sanitize_filename_truncates_long_names():
    long = "a" * 200 + ".txt"
    out = sanitize_filename(long)
    assert len(out) <= 100
    assert out.endswith(".txt"), "extension preserved"


def test_sanitize_filename_drops_long_extension():
    """Extensions longer than 16 chars don't qualify as extensions; the
    truncation just chops the tail."""
    long = "a" * 110 + ".verylongextensionofdoom"
    out = sanitize_filename(long)
    assert len(out) == 100
    assert "." not in out[-16:], "no false-extension preserved"


# ───────────── auth ─────────────

def test_unauthorized_no_bearer(client: TestClient):
    r = client.post("/internal/chat/uploads/ingest", files={"files": ("a.txt", b"x")})
    assert r.status_code == 401
    assert r.json() == {"error": "unauthorized"}


def test_unauthorized_wrong_bearer(client: TestClient):
    r = client.post(
        "/internal/chat/uploads/ingest",
        files={"files": ("a.txt", b"x")},
        headers={"Authorization": "Bearer wrong"},
    )
    assert r.status_code == 401


def test_unauthorized_when_secret_file_missing(tmp_path: Path, chat_uploads_dir: Path, monkeypatch: pytest.MonkeyPatch):
    """Fail-closed: no secret file on disk → every request 401, even
    with an "Authorization: Bearer" header."""
    monkeypatch.setenv("CONFIGS_DIR", str(tmp_path))
    platform_inbound_auth.reset_cache()
    app = Starlette(routes=[
        Route("/internal/chat/uploads/ingest", ingest_handler, methods=["POST"]),
    ])
    client = TestClient(app)
    r = client.post(
        "/internal/chat/uploads/ingest",
        files={"files": ("a.txt", b"x")},
        headers={"Authorization": "Bearer anything"},
    )
    assert r.status_code == 401
    platform_inbound_auth.reset_cache()


# ───────────── happy paths ─────────────

def test_single_upload_writes_to_disk(client: TestClient, chat_uploads_dir: Path):
    payload = b"hello world"
    r = client.post(
        "/internal/chat/uploads/ingest",
        files={"files": ("greeting.txt", payload, "text/plain")},
        headers={"Authorization": "Bearer test-secret"},
    )
    assert r.status_code == 200, r.text
    body = r.json()
    assert "files" in body and len(body["files"]) == 1
    f = body["files"][0]
    assert f["name"] == "greeting.txt"
    assert f["mimeType"] == "text/plain"
    assert f["size"] == len(payload)
    # URI shape matches the Go handler's contract — canvas / agent code
    # that already resolves "workspace:..." paths keeps working.
    assert f["uri"].startswith("workspace:") and f["uri"].endswith("greeting.txt")
    # On-disk content matches.
    stored_path = f["uri"][len("workspace:"):]
    # In the test, CHAT_UPLOAD_DIR was redirected to chat_uploads_dir,
    # so stored_path's prefix is the redirected dir.
    assert stored_path.startswith(str(chat_uploads_dir))
    assert Path(stored_path).read_bytes() == payload


def test_multiple_uploads_in_one_batch(client: TestClient, chat_uploads_dir: Path):
    files = [
        ("files", ("a.txt", b"AAA", "text/plain")),
        ("files", ("b.png", b"BBBBBB", "image/png")),
    ]
    r = client.post(
        "/internal/chat/uploads/ingest",
        files=files,
        headers={"Authorization": "Bearer test-secret"},
    )
    assert r.status_code == 200, r.text
    items = r.json()["files"]
    assert len(items) == 2
    names = sorted(f["name"] for f in items)
    assert names == ["a.txt", "b.png"]
    sizes = sorted(f["size"] for f in items)
    assert sizes == [3, 6]


def test_uploads_get_unique_random_prefix(client: TestClient, chat_uploads_dir: Path):
    """Two uploads with the same filename land at distinct paths."""
    files = [
        ("files", ("dup.txt", b"first")),
        ("files", ("dup.txt", b"second")),
    ]
    r = client.post(
        "/internal/chat/uploads/ingest",
        files=files,
        headers={"Authorization": "Bearer test-secret"},
    )
    assert r.status_code == 200
    items = r.json()["files"]
    uri_a, uri_b = items[0]["uri"], items[1]["uri"]
    assert uri_a != uri_b, "uniqueness via random prefix"
    path_a = uri_a[len("workspace:"):]
    path_b = uri_b[len("workspace:"):]
    assert Path(path_a).read_bytes() == b"first"
    assert Path(path_b).read_bytes() == b"second"


def test_mime_type_falls_back_to_extension_guess(client: TestClient):
    """When the part doesn't carry a Content-Type header, guess from the
    extension. Matches the Go handler's precedence."""
    r = client.post(
        "/internal/chat/uploads/ingest",
        files={"files": ("doc.pdf", b"%PDF-")},
        headers={"Authorization": "Bearer test-secret"},
    )
    assert r.status_code == 200
    f = r.json()["files"][0]
    assert f["mimeType"].startswith("application/pdf"), f["mimeType"]


# ───────────── failure modes ─────────────

def test_no_files_field_returns_400(client: TestClient):
    """multipart with NO `files` part → 400, not 200 with empty list."""
    r = client.post(
        "/internal/chat/uploads/ingest",
        data={"unrelated": "field"},
        headers={"Authorization": "Bearer test-secret"},
    )
    assert r.status_code == 400


def test_per_file_oversize_returns_413(client: TestClient, monkeypatch: pytest.MonkeyPatch):
    """Per-file cap is enforced. Lower the cap for the test so we don't
    have to construct a real 25 MB body."""
    monkeypatch.setattr(internal_chat_uploads, "CHAT_UPLOAD_MAX_FILE_BYTES", 16)
    big = b"x" * 32  # > 16
    r = client.post(
        "/internal/chat/uploads/ingest",
        files={"files": ("big.bin", big)},
        headers={"Authorization": "Bearer test-secret"},
    )
    assert r.status_code == 413
    assert "exceeds per-file limit" in r.json()["error"]


# Pins the diagnostic shape of the 500 returned when the upload
# directory cannot be created. Prior to this fix, the response was
# {"error": "failed to prepare uploads dir"} only — opaque to the
# operator inspecting browser devtools, requiring SSM access to the
# workspace stderr to recover errno + actual path. Surfacing both in
# the response body makes the failure self-diagnosing the next time
# this class of bug recurs (e.g. EACCES on a root-owned `.molecule`
# subtree, ENOSPC on a full disk, EROFS on a read-only mount).
#
# Reproduces the failure by pointing CHAT_UPLOAD_DIR at a path whose
# parent the agent user can't write to. The exact errno in the test
# is 13 (EACCES) on a chmod-0 dir; values are not asserted exactly
# because they vary by OS / errno mapping. The PRESENCE of errno +
# path is what's pinned — drift on those keys breaks the operator
# diagnostic loop.
def test_mkdir_failure_returns_errno_and_path(client: TestClient, chat_uploads_dir: Path, monkeypatch: pytest.MonkeyPatch):
    # Plant a regular FILE where mkdir's parent should be — mkdir
    # raises FileExistsError / NotADirectoryError reliably across
    # platforms, exercising the OSError catch path.
    blocker = chat_uploads_dir.parent / "chat-uploads-blocker"
    blocker.write_text("not a dir")
    # Repoint CHAT_UPLOAD_DIR to a child path under the regular file
    # so mkdir(parents=True, exist_ok=True) raises NotADirectoryError.
    monkeypatch.setattr(internal_chat_uploads, "CHAT_UPLOAD_DIR", str(blocker / "child"))

    r = client.post(
        "/internal/chat/uploads/ingest",
        files={"files": ("a.txt", b"x")},
        headers={"Authorization": "Bearer test-secret"},
    )
    assert r.status_code == 500, r.text
    body = r.json()
    # Backwards-compatible top-level error keeps existing canvas /
    # external alert rules matching.
    assert body.get("error") == "failed to prepare uploads dir"
    # New diagnostic fields — operator can now see WHAT path failed
    # and WHY without SSM access.
    assert body.get("path") == str(blocker / "child")
    assert isinstance(body.get("errno"), int) and body["errno"] != 0
    assert "detail" in body and isinstance(body["detail"], str) and body["detail"]


def test_total_request_body_oversize_returns_413(client: TestClient, monkeypatch: pytest.MonkeyPatch):
    """Header-side total cap. Set the limit BELOW the actual body and
    confirm we reject before parsing multipart."""
    monkeypatch.setattr(internal_chat_uploads, "CHAT_UPLOAD_MAX_BYTES", 8)
    r = client.post(
        "/internal/chat/uploads/ingest",
        files={"files": ("a.txt", b"this is much more than 8 bytes")},
        headers={"Authorization": "Bearer test-secret"},
    )
    assert r.status_code == 413


def test_symlink_at_target_is_refused(client: TestClient, chat_uploads_dir: Path, monkeypatch: pytest.MonkeyPatch):
    """If a pre-existing symlink at the destination redirects writes to
    a sensitive path, the upload MUST refuse rather than follow.

    We force a deterministic prefix by patching pysecrets.token_hex so
    we know exactly which path to plant the symlink at.
    """
    chat_uploads_dir.mkdir(parents=True, exist_ok=True)
    # Plant a symlink pointing at a "secret" location.
    sentinel = chat_uploads_dir / "decoy-target"
    sentinel.write_bytes(b"original")
    monkeypatch.setattr(internal_chat_uploads.pysecrets, "token_hex", lambda n: "deadbeef" * (n // 4))
    target_path = chat_uploads_dir / ("deadbeef" * 4 + "-evil.txt")
    os.symlink(sentinel, target_path)

    r = client.post(
        "/internal/chat/uploads/ingest",
        files={"files": ("evil.txt", b"PWNED")},
        headers={"Authorization": "Bearer test-secret"},
    )
    assert r.status_code == 500, r.text
    # Sentinel content unchanged — the symlink wasn't followed.
    assert sentinel.read_bytes() == b"original"
