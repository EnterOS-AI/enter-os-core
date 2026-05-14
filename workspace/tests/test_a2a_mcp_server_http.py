"""Tests for the HTTP/SSE transport of a2a_mcp_server.

Covers:
- _handle_http_mcp: JSON-RPC request parsing and routing
- Starlette app routes: POST /mcp, GET /mcp/stream, GET /health
- cli_main argparse: --transport and --port flags
"""

from __future__ import annotations

import asyncio
import json
import sys
import types
import uuid
from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


class _DummyRequest:
    """Minimal request duck-type for _handle_http_mcp."""

    def __init__(self, body_json: dict, headers: dict | None = None):
        self._body = body_json
        self.headers = headers or {}

    async def json(self) -> dict:
        return self._body


# ---------------------------------------------------------------------------
# _handle_http_mcp — unit tests (no I/O)
# ---------------------------------------------------------------------------


@pytest.mark.asyncio()
async def test_handle_http_mcp_initialize():
    """initialize method returns protocol version, capabilities, and server info."""
    from a2a_mcp_server import _handle_http_mcp

    req = _DummyRequest({"jsonrpc": "2.0", "id": 42, "method": "initialize", "params": {}})
    resp = await _handle_http_mcp(req)

    assert resp["jsonrpc"] == "2.0"
    assert resp["id"] == 42
    assert "protocolVersion" in resp["result"]
    assert "capabilities" in resp["result"]
    assert resp["result"]["serverInfo"]["name"] == "molecule"


@pytest.mark.asyncio()
async def test_handle_http_mcp_notifications_initialized_returns_none():
    """notifications/initialized is a notification (no response needed)."""
    from a2a_mcp_server import _handle_http_mcp

    req = _DummyRequest({"jsonrpc": "2.0", "method": "notifications/initialized"})
    resp = await _handle_http_mcp(req)

    assert resp is None


@pytest.mark.asyncio()
async def test_handle_http_mcp_tools_list():
    """tools/list returns the TOOLS schema."""
    from a2a_mcp_server import _handle_http_mcp

    req = _DummyRequest({"jsonrpc": "2.0", "id": 7, "method": "tools/list"})
    resp = await _handle_http_mcp(req)

    assert resp["jsonrpc"] == "2.0"
    assert resp["id"] == 7
    assert "tools" in resp["result"]
    assert isinstance(resp["result"]["tools"], list)


@pytest.mark.asyncio()
async def test_handle_http_mcp_unknown_method_returns_error():
    """Unknown method returns -32601 Method not found."""
    from a2a_mcp_server import _handle_http_mcp

    req = _DummyRequest({"jsonrpc": "2.0", "id": 3, "method": "foobar", "params": {}})
    resp = await _handle_http_mcp(req)

    assert resp["jsonrpc"] == "2.0"
    assert resp["id"] == 3
    assert resp["error"]["code"] == -32601
    assert "Method not found" in resp["error"]["message"]


@pytest.mark.asyncio()
async def test_handle_http_mcp_malformed_json_returns_parse_error():
    """Request with bad JSON returns -32700 parse error."""
    from a2a_mcp_server import _handle_http_mcp

    req = _DummyRequest.__new__(_DummyRequest)
    req.headers = {}
    req.json = AsyncMock(side_effect=ValueError("bad json"))

    resp = await _handle_http_mcp(req)

    assert resp["error"]["code"] == -32700


@pytest.mark.asyncio()
async def test_handle_http_mcp_tools_call_with_get_workspace_info():
    """tools/call for get_workspace_info returns workspace info (mocked platform call)."""
    from a2a_mcp_server import _handle_http_mcp

    with patch("a2a_mcp_server.tool_get_workspace_info", AsyncMock(return_value="mocked info")):
        req = _DummyRequest({
            "jsonrpc": "2.0",
            "id": 9,
            "method": "tools/call",
            "params": {"name": "get_workspace_info", "arguments": {}},
        })
        resp = await _handle_http_mcp(req)

    assert resp["jsonrpc"] == "2.0"
    assert resp["id"] == 9
    assert resp["result"]["content"][0]["text"] == "mocked info"


@pytest.mark.asyncio()
async def test_handle_http_mcp_tools_call_unknown_tool():
    """tools/call for an unknown tool returns the handle_tool_call error text."""
    from a2a_mcp_server import _handle_http_mcp

    req = _DummyRequest({
        "jsonrpc": "2.0",
        "id": 11,
        "method": "tools/call",
        "params": {"name": "not_a_real_tool", "arguments": {}},
    })
    resp = await _handle_http_mcp(req)

    assert resp["jsonrpc"] == "2.0"
    assert resp["id"] == 11
    assert "Unknown tool" in resp["result"]["content"][0]["text"]


# ---------------------------------------------------------------------------
# Starlette app — integration tests with TestClient
# ---------------------------------------------------------------------------


@pytest.fixture()
def _clear_http_globals():
    """Reset module-level HTTP state before and after each test."""
    import a2a_mcp_server

    # Save and restore globals
    saved_queues = a2a_mcp_server._http_connection_queues.copy()
    saved_lock = a2a_mcp_server._http_connection_lock
    a2a_mcp_server._http_connection_queues.clear()
    yield
    # Restore
    a2a_mcp_server._http_connection_queues = saved_queues





def _register_sse_queue():
    """Register a queue for SSE push delivery (synchronous — callable from tests)."""
    conn_id = str(uuid.uuid4())
    queue = asyncio.Queue(maxsize=100)
    import a2a_mcp_server
    a2a_mcp_server._http_connection_queues[conn_id] = queue
    return conn_id, queue


def _build_test_app(port: int = 9100):
    """Build the Starlette app for testing without starting a real server.

    Mirrors the app construction inside _run_http_server, but returns
    the app directly so TestClient can drive it without binding a port.
    """
    from starlette.applications import Starlette
    from starlette.routing import Route

    import a2a_mcp_server

    async def mcp_handler(request):
        conn_id = request.headers.get("x-mcp-conn-id", "default")
        response = await a2a_mcp_server._handle_http_mcp(request)
        if response is None:
            from starlette.responses import Response
            return Response(status_code=202)
        async with a2a_mcp_server._http_connection_lock:
            queue = a2a_mcp_server._http_connection_queues.get(conn_id)
        if queue is not None and not queue.full():
            await queue.put(response)
            from starlette.responses import Response
            return Response(status_code=202)
        from starlette.responses import JSONResponse
        return JSONResponse(response)

    async def sse_handler(request):
        conn_id, queue = _register_sse_queue()

        import asyncio as _asyncio

        async def event_stream():
            import json as _json
            yield f"event: connected\ndata: {_json.dumps({'conn_id': conn_id})}\n\n"
            try:
                while True:
                    response = await _asyncio.wait_for(queue.get(), timeout=300)
                    import json as _json
                    yield f"event: message\ndata: {_json.dumps(response)}\n\n"
                    if queue.empty():
                        yield "event: heartbeat\ndata: null\n\n"
            except _asyncio.TimeoutError:
                pass
            finally:
                async with a2a_mcp_server._http_connection_lock:
                    a2a_mcp_server._http_connection_queues.pop(conn_id, None)

        from starlette.responses import StreamingResponse
        return StreamingResponse(
            event_stream(),
            media_type="text/event-stream",
            headers={
                "Cache-Control": "no-cache",
                "Connection": "keep-alive",
                "X-Accel-Buffering": "no",
            },
        )

    async def health_handler(_request):
        from starlette.responses import JSONResponse
        return JSONResponse({"ok": True, "transport": "http+sse", "port": port})

    return Starlette(
        routes=[
            Route("/mcp", mcp_handler, methods=["POST"]),
            Route("/mcp/stream", sse_handler, methods=["GET"]),
            Route("/health", health_handler),
        ]
    )


class TestHTTPAppRoutes:
    """Integration tests using Starlette TestClient against the HTTP app.

    Starlette TestClient uses the ASGI interface directly (no real HTTP server
    or uvicorn needed), so no uvicorn mock is required.
    """

    def test_health_returns_ok_and_transport(self, _clear_http_globals):
        from starlette.testclient import TestClient

        app = _build_test_app(port=9100)
        with TestClient(app) as client:
            resp = client.get("/health")

        assert resp.status_code == 200
        data = resp.json()
        assert data["ok"] is True
        assert data["transport"] == "http+sse"
        assert data["port"] == 9100

    def test_health_accepts_different_port(self, _clear_http_globals):
        from starlette.testclient import TestClient

        app = _build_test_app(port=9999)
        with TestClient(app) as client:
            resp = client.get("/health")

        assert resp.json()["port"] == 9999

    def test_mcp_post_initialize(self, _clear_http_globals):
        from starlette.testclient import TestClient

        app = _build_test_app()
        with TestClient(app) as client:
            resp = client.post("/mcp", json={
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {},
            })

        assert resp.status_code == 200
        data = resp.json()
        assert data["id"] == 1
        assert "protocolVersion" in data["result"]

    def test_mcp_post_tools_list(self, _clear_http_globals):
        from starlette.testclient import TestClient

        app = _build_test_app()
        with TestClient(app) as client:
            resp = client.post("/mcp", json={
                "jsonrpc": "2.0",
                "id": 2,
                "method": "tools/list",
                "params": {},
            })

        assert resp.status_code == 200
        data = resp.json()
        assert "tools" in data["result"]
        assert len(data["result"]["tools"]) > 0

    def test_mcp_post_notifications_initialized_returns_202(self, _clear_http_globals):
        from starlette.testclient import TestClient

        app = _build_test_app()
        with TestClient(app) as client:
            resp = client.post("/mcp", json={
                "jsonrpc": "2.0",
                "method": "notifications/initialized",
            })

        # Notifications return 202 with no body
        assert resp.status_code == 202

    def test_mcp_post_unknown_method_returns_200_with_error(self, _clear_http_globals):
        from starlette.testclient import TestClient

        app = _build_test_app()
        with TestClient(app) as client:
            resp = client.post("/mcp", json={
                "jsonrpc": "2.0",
                "id": 5,
                "method": "no_such_method",
                "params": {},
            })

        assert resp.status_code == 200
        data = resp.json()
        assert data["error"]["code"] == -32601

    def test_mcp_post_malformed_json_returns_error(self, _clear_http_globals):
        """Malformed JSON body returns a JSON-RPC parse-error response (HTTP 200)."""
        from starlette.testclient import TestClient

        app = _build_test_app()
        with TestClient(app, raise_server_exceptions=False) as client:
            resp = client.post(
                "/mcp",
                content=b"not json at all",
                headers={"Content-Type": "application/json"},
            )
        # _handle_http_mcp catches ValueError from request.json() and returns
        # a JSON-RPC parse-error response with HTTP 200.
        assert resp.status_code == 200
        assert resp.json()["error"]["code"] == -32700
        assert "Parse error" in resp.json()["error"]["message"]

    @pytest.mark.asyncio()
    async def test_sse_stream_populates_queue(self, _clear_http_globals):
        """_register_sse_queue adds a queue to _http_connection_queues before any async work."""
        import a2a_mcp_server

        conn_id, queue = _register_sse_queue()

        # The queue is registered synchronously — no await needed, no cleanup ran yet.
        assert conn_id in a2a_mcp_server._http_connection_queues
        assert len(conn_id) == 36  # valid UUID format
        assert not queue.full()

    @pytest.mark.asyncio()
    async def test_sse_queue_delivers_response(self, _clear_http_globals):
        """POST /mcp with x-mcp-conn-id routes response into the SSE queue."""
        import uuid

        import a2a_mcp_server
        from starlette.testclient import TestClient

        # Pre-register an SSE queue to simulate an active SSE subscriber
        conn_id = str(uuid.uuid4())
        queue: asyncio.Queue = asyncio.Queue(maxsize=100)
        async with a2a_mcp_server._http_connection_lock:
            a2a_mcp_server._http_connection_queues[conn_id] = queue

        # POST a tools/call with the conn_id header
        with TestClient(_build_test_app()) as client:
            with patch("a2a_mcp_server.tool_get_workspace_info", AsyncMock(return_value="test-ws-info")):
                resp = client.post(
                    "/mcp",
                    headers={"x-mcp-conn-id": conn_id},
                    json={
                        "jsonrpc": "2.0",
                        "id": 99,
                        "method": "tools/call",
                        "params": {"name": "get_workspace_info", "arguments": {}},
                    },
                )

        # The handler returns 202 because the response was queued for SSE delivery
        assert resp.status_code == 202

        # Verify the response was placed in the SSE queue
        result = await asyncio.wait_for(queue.get(), timeout=2.0)
        assert result["id"] == 99
        assert result["result"]["content"][0]["text"] == "test-ws-info"


# ---------------------------------------------------------------------------
# handle_tool_call — remaining tool branches
# ---------------------------------------------------------------------------


@pytest.mark.asyncio()
async def test_handle_http_mcp_tools_call_send_message_to_user_with_mixed_attachments():
    """attachments with non-string elements are filtered; the list branch is exercised."""
    from a2a_mcp_server import _handle_http_mcp

    with patch("a2a_mcp_server.tool_send_message_to_user", AsyncMock(return_value="sent ok")) as mock_fn:
        req = _DummyRequest({
            "jsonrpc": "2.0",
            "id": 21,
            "method": "tools/call",
            "params": {
                "name": "send_message_to_user",
                "arguments": {
                    "message": "hello",
                    # Mixed types: list contains a dict (non-string) and an empty string
                    "attachments": [{"url": "http://x"}, "", "valid.zip", None],
                },
            },
        })
        resp = await _handle_http_mcp(req)

    assert resp["result"]["content"][0]["text"] == "sent ok"
    # Only string, non-empty values passed through
    mock_fn.assert_called_once()
    _, kwargs = mock_fn.call_args
    assert kwargs["attachments"] == ["valid.zip"]


@pytest.mark.asyncio()
async def test_handle_http_mcp_tools_call_wait_for_message():
    """wait_for_message is dispatched and returns the wrapped result."""
    from a2a_mcp_server import _handle_http_mcp

    with patch("a2a_mcp_server.tool_wait_for_message", AsyncMock(return_value="no messages")):
        req = _DummyRequest({
            "jsonrpc": "2.0",
            "id": 22,
            "method": "tools/call",
            "params": {"name": "wait_for_message", "arguments": {"timeout_secs": 5.0}},
        })
        resp = await _handle_http_mcp(req)

    assert resp["result"]["content"][0]["text"] == "no messages"


@pytest.mark.asyncio()
async def test_handle_http_mcp_tools_call_inbox_peek():
    """inbox_peek is dispatched with the limit argument."""
    from a2a_mcp_server import _handle_http_mcp

    with patch("a2a_mcp_server.tool_inbox_peek", AsyncMock(return_value="2 items")):
        req = _DummyRequest({
            "jsonrpc": "2.0",
            "id": 23,
            "method": "tools/call",
            "params": {"name": "inbox_peek", "arguments": {"limit": 5}},
        })
        resp = await _handle_http_mcp(req)

    assert resp["result"]["content"][0]["text"] == "2 items"


@pytest.mark.asyncio()
async def test_handle_http_mcp_tools_call_inbox_pop():
    """inbox_pop is dispatched with the activity_id argument."""
    from a2a_mcp_server import _handle_http_mcp

    with patch("a2a_mcp_server.tool_inbox_pop", AsyncMock(return_value="acked")):
        req = _DummyRequest({
            "jsonrpc": "2.0",
            "id": 24,
            "method": "tools/call",
            "params": {"name": "inbox_pop", "arguments": {"activity_id": "abc-123"}},
        })
        resp = await _handle_http_mcp(req)

    assert resp["result"]["content"][0]["text"] == "acked"


@pytest.mark.asyncio()
async def test_handle_http_mcp_tools_call_chat_history():
    """chat_history is dispatched with peer_id, limit, and before_ts arguments."""
    from a2a_mcp_server import _handle_http_mcp

    with patch("a2a_mcp_server.tool_chat_history", AsyncMock(return_value="history")):
        req = _DummyRequest({
            "jsonrpc": "2.0",
            "id": 25,
            "method": "tools/call",
            "params": {
                "name": "chat_history",
                "arguments": {"peer_id": "ws-peer-1", "limit": 10, "before_ts": ""},
            },
        })
        resp = await _handle_http_mcp(req)

    assert resp["result"]["content"][0]["text"] == "history"


# ---------------------------------------------------------------------------
# cli_main argparse — unit tests
# ---------------------------------------------------------------------------


def test_mcp_post_falls_back_to_json_when_sse_queue_is_full(_clear_http_globals):
    """When the SSE queue is full (>100 pending), the handler returns JSON directly."""
    import a2a_mcp_server
    from starlette.testclient import TestClient

    # Pre-register a queue and fill it to capacity
    conn_id = str(uuid.uuid4())
    queue: asyncio.Queue = asyncio.Queue(maxsize=2)  # small queue for testing

    async def _setup():
        async with a2a_mcp_server._http_connection_lock:
            a2a_mcp_server._http_connection_queues[conn_id] = queue
        queue.put_nowait({"id": 1})
        queue.put_nowait({"id": 2})

    _sync_run(_setup())
    assert queue.full()

    app = _build_test_app()
    with TestClient(app) as client:
        resp = client.post(
            "/mcp",
            headers={"x-mcp-conn-id": conn_id},
            json={"jsonrpc": "2.0", "id": 99, "method": "initialize", "params": {}},
        )

    # With a full queue, the handler returns the response as JSON (not 202)
    assert resp.status_code == 200
    assert resp.json()["id"] == 99
    assert "result" in resp.json()


def _sync_run(coro):
    """Run a coroutine synchronously for test isolation (no real event loop needed)."""
    try:
        loop = asyncio.new_event_loop()
        asyncio.set_event_loop(loop)
        try:
            return loop.run_until_complete(coro)
        finally:
            loop.close()
    except Exception:
        raise


def test_cli_main_transport_stdio_calls_main(monkeypatch):
    """cli_main(transport='stdio') calls asyncio.run(main) without HTTP."""
    import a2a_mcp_server

    run_calls: list = []

    async def fake_main():
        run_calls.append("called")

    monkeypatch.setattr(a2a_mcp_server, "main", fake_main)
    monkeypatch.setattr(a2a_mcp_server.asyncio, "run", _sync_run)
    monkeypatch.setattr(a2a_mcp_server, "_assert_stdio_is_pipe_compatible", lambda: None)

    a2a_mcp_server.cli_main(transport="stdio", port=9100)

    assert "called" in run_calls


def test_cli_main_transport_http_calls_run_http_server(monkeypatch):
    """cli_main(transport='http') calls _run_http_server without stdio."""
    import a2a_mcp_server

    run_http_calls = []

    async def fake_run_http(port):
        run_http_calls.append(port)

    # asyncio.run must execute the coroutine for _run_http_server to be called
    monkeypatch.setattr(a2a_mcp_server.asyncio, "run", _sync_run)
    monkeypatch.setattr(a2a_mcp_server, "_run_http_server", fake_run_http)
    # stdio path must not be entered
    monkeypatch.setattr(a2a_mcp_server, "_assert_stdio_is_pipe_compatible", lambda: None)

    a2a_mcp_server.cli_main(transport="http", port=9102)

    assert run_http_calls == [9102]


def test_cli_main_http_skips_stdio_check(monkeypatch):
    """When transport=http, _assert_stdio_is_pipe_compatible must NOT be called."""
    import a2a_mcp_server

    called = []

    def fake_assert():
        called.append("assert_called")

    # Patch on the module object directly
    monkeypatch.setattr(a2a_mcp_server, "_assert_stdio_is_pipe_compatible", fake_assert)
    monkeypatch.setattr(a2a_mcp_server.asyncio, "run", lambda fn: None)

    a2a_mcp_server.cli_main(transport="http", port=9100)

    assert "assert_called" not in called


def test_cli_main_default_transport_is_stdio(monkeypatch):
    """cli_main() with no args defaults to stdio transport."""
    import a2a_mcp_server

    called_as: list = []

    async def fake_main():
        called_as.append("called")

    monkeypatch.setattr(a2a_mcp_server, "main", fake_main)
    monkeypatch.setattr(a2a_mcp_server.asyncio, "run", _sync_run)
    monkeypatch.setattr(a2a_mcp_server, "_assert_stdio_is_pipe_compatible", lambda: None)

    a2a_mcp_server.cli_main()  # No args — defaults to stdio

    assert "called" in called_as


def test_cli_main_main_raises_propagates(monkeypatch):
    """If main() raises, cli_main() re-raises (doesn't swallow)."""
    import a2a_mcp_server

    async def fake_main():
        raise RuntimeError("boom")

    monkeypatch.setattr(a2a_mcp_server, "main", fake_main)
    monkeypatch.setattr(a2a_mcp_server.asyncio, "run", _sync_run)
    monkeypatch.setattr(a2a_mcp_server, "_assert_stdio_is_pipe_compatible", lambda: None)

    with pytest.raises(RuntimeError, match="boom"):
        a2a_mcp_server.cli_main(transport="stdio")


# ---------------------------------------------------------------------------
# uvicorn/starlette lazy-import
# ---------------------------------------------------------------------------


def test_run_http_server_is_coroutine_function():
    """_run_http_server is a coroutine function accepting a port argument."""
    import inspect
    from a2a_mcp_server import _run_http_server

    assert inspect.iscoroutinefunction(_run_http_server)


def test_run_http_server_signature_port_int():
    """_run_http_server accepts port as int."""
    import inspect
    from a2a_mcp_server import _run_http_server

    sig = inspect.signature(_run_http_server)
    assert "port" in sig.parameters
    assert sig.parameters["port"].annotation == int
