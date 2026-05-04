"""Tests for ``not_configured_handler`` — the JSON-RPC -32603 fallback the
runtime mounts when ``adapter.setup()`` fails.

Tests the behavior end-to-end via Starlette's TestClient so the JSON-RPC
wire shape (status 503, code -32603, id-echo) is exercised the same way
canvas would see it.
"""
from __future__ import annotations

import sys
from pathlib import Path

# Make workspace/ importable in test isolation — same pattern as the
# adjacent tests (test_smoke_mode.py, test_heartbeat.py).
WORKSPACE_DIR = Path(__file__).resolve().parents[1]
if str(WORKSPACE_DIR) not in sys.path:
    sys.path.insert(0, str(WORKSPACE_DIR))

from starlette.applications import Starlette
from starlette.routing import Route
from starlette.testclient import TestClient

from not_configured_handler import make_not_configured_handler


def _build_app(reason: str | None) -> TestClient:
    handler = make_not_configured_handler(reason)
    app = Starlette(routes=[Route("/", handler, methods=["POST"])])
    return TestClient(app)


def test_returns_503_with_jsonrpc_error_envelope():
    """Status 503; body is a valid JSON-RPC 2.0 error envelope."""
    client = _build_app("MINIMAX_API_KEY not set")
    resp = client.post("/", json={"jsonrpc": "2.0", "id": 7, "method": "message/send"})
    assert resp.status_code == 503
    body = resp.json()
    assert body["jsonrpc"] == "2.0"
    assert body["error"]["code"] == -32603
    assert body["error"]["message"] == "Internal error: agent not configured"


def test_echoes_request_id_when_present():
    """JSON-RPC clients correlate replies via id; the handler must echo it."""
    client = _build_app("reason")
    resp = client.post("/", json={"jsonrpc": "2.0", "id": "abc-123", "method": "x"})
    assert resp.json()["id"] == "abc-123"


def test_id_is_null_when_body_malformed():
    """Per JSON-RPC 2.0: id MUST be null when it can't be determined from
    the request. Malformed bodies (non-JSON, empty, non-object) all map
    to id=null."""
    client = _build_app("reason")
    resp = client.post("/", content=b"not json at all", headers={"content-type": "application/json"})
    assert resp.status_code == 503
    assert resp.json()["id"] is None


def test_reason_surfaces_in_error_data():
    """Operators read ``error.data`` to figure out what to fix. The
    setup() exception string lands there verbatim."""
    client = _build_app("RuntimeError: Neither OPENAI_API_KEY nor MINIMAX_API_KEY is set")
    resp = client.post("/", json={"jsonrpc": "2.0", "id": 1, "method": "x"})
    assert resp.json()["error"]["data"] == (
        "RuntimeError: Neither OPENAI_API_KEY nor MINIMAX_API_KEY is set"
    )


def test_none_reason_falls_back_to_generic_message():
    """If the adapter raised but we couldn't capture a reason, give the
    operator a hint where to look (still better than a stuck-booting
    workspace with no log line)."""
    client = _build_app(None)
    resp = client.post("/", json={"jsonrpc": "2.0", "id": 1, "method": "x"})
    assert resp.json()["error"]["data"] == "adapter.setup() failed"


def test_array_body_does_not_crash_id_extraction():
    """JSON-RPC supports batch (array) requests. We don't currently
    support batch in the runtime, but the handler shouldn't crash on a
    batch body — it should just respond with id=null and the same -32603
    so the client sees a clear error instead of a 500."""
    client = _build_app("reason")
    resp = client.post("/", json=[{"jsonrpc": "2.0", "id": 1, "method": "x"}])
    assert resp.status_code == 503
    assert resp.json()["id"] is None
