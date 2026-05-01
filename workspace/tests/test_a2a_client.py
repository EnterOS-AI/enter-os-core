"""Comprehensive tests for a2a_client.py — 100% statement coverage.

Tests every async function:  discover_peer, send_a2a_message, get_peers,
get_workspace_info.  Each test covers exactly one execution path so failures
are easy to diagnose.
"""

import sys
import os
import importlib
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _make_mock_client(*, get_resp=None, post_resp=None, get_exc=None, post_exc=None):
    """Build a reusable AsyncClient context-manager mock."""
    mock_client = AsyncMock()
    mock_client.__aenter__ = AsyncMock(return_value=mock_client)
    mock_client.__aexit__ = AsyncMock(return_value=False)

    if get_exc is not None:
        mock_client.get = AsyncMock(side_effect=get_exc)
    elif get_resp is not None:
        mock_client.get = AsyncMock(return_value=get_resp)

    if post_exc is not None:
        mock_client.post = AsyncMock(side_effect=post_exc)
    elif post_resp is not None:
        mock_client.post = AsyncMock(return_value=post_resp)

    return mock_client


def _make_response(status_code, json_data):
    resp = MagicMock()
    resp.status_code = status_code
    resp.json = MagicMock(return_value=json_data)
    return resp


# Canonical UUID used wherever a test needs a peer_id. send_a2a_message and
# discover_peer reject non-UUID strings at the trust boundary (see
# a2a_client._validate_peer_id), so test inputs must be valid UUIDs.
_TEST_PEER_ID = "11111111-1111-1111-1111-111111111111"


# ---------------------------------------------------------------------------
# Module-level constants (just ensure they exist and have sensible types)
# ---------------------------------------------------------------------------

def test_constants_exist():
    import a2a_client
    assert isinstance(a2a_client.PLATFORM_URL, str)
    assert isinstance(a2a_client.WORKSPACE_ID, str)
    assert isinstance(a2a_client._A2A_ERROR_PREFIX, str)
    assert isinstance(a2a_client._peer_names, dict)


# ---------------------------------------------------------------------------
# discover_peer
# ---------------------------------------------------------------------------

class TestDiscoverPeer:

    async def test_success_returns_json_on_200(self):
        """200 response → returns the JSON body."""
        import a2a_client

        peer_data = {"id": _TEST_PEER_ID, "url": "http://ws-abc.svc", "name": "Alpha"}
        resp = _make_response(200, peer_data)
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.discover_peer(_TEST_PEER_ID)

        assert result == peer_data

    async def test_non_200_returns_none(self):
        """Non-200 response → returns None."""
        import a2a_client

        resp = _make_response(404, {"detail": "not found"})
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.discover_peer(_TEST_PEER_ID)

        assert result is None

    async def test_403_returns_none(self):
        """403 forbidden → returns None (any non-200 code)."""
        import a2a_client

        resp = _make_response(403, {"detail": "forbidden"})
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.discover_peer(_TEST_PEER_ID)

        assert result is None

    async def test_exception_returns_none(self):
        """Network exception → returns None (exception swallowed)."""
        import a2a_client

        mock_client = _make_mock_client(get_exc=ConnectionError("host unreachable"))

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.discover_peer(_TEST_PEER_ID)

        assert result is None

    async def test_invalid_peer_id_returns_none_without_http(self):
        """Malformed peer_id is rejected at the trust boundary — no HTTP call.

        Path-traversal-shaped input ("../admin"), free-form labels
        ("ws-abc"), and empty strings all return None and don't reach
        the platform. Closes the URL-interpolation class of bug.
        """
        import a2a_client

        mock_client = _make_mock_client(get_resp=_make_response(200, {}))
        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            for bad in ("", "ws-abc", "../admin", "not-a-uuid", "8dad3e29"):
                assert await a2a_client.discover_peer(bad) is None
        # No GET should have been issued for any of those.
        mock_client.get.assert_not_called()

    async def test_request_uses_correct_url_and_header(self):
        """GET is called with the right URL and X-Workspace-ID header."""
        import a2a_client

        resp = _make_response(200, {"url": "http://target"})
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            await a2a_client.discover_peer(_TEST_PEER_ID)

        mock_client.get.assert_called_once()
        positional_url = mock_client.get.call_args.args[0]
        assert _TEST_PEER_ID in positional_url
        # X-Workspace-ID must be present; bearer token also merged in when available
        headers_sent = mock_client.get.call_args.kwargs.get("headers", {})
        assert headers_sent.get("X-Workspace-ID") == a2a_client.WORKSPACE_ID


# ---------------------------------------------------------------------------
# send_a2a_message
# ---------------------------------------------------------------------------

class TestSendA2AMessage:

    async def test_result_with_text_part_returns_text(self):
        """'result' key with text parts → returns the text."""
        import a2a_client

        resp = _make_response(200, {
            "result": {"parts": [{"kind": "text", "text": "Hello!"}]}
        })
        mock_client = _make_mock_client(post_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "ping")

        assert result == "Hello!"

    async def test_result_with_empty_parts_returns_no_response(self):
        """'result' key with empty parts list → returns '(no response)'."""
        import a2a_client

        resp = _make_response(200, {"result": {"parts": []}})
        mock_client = _make_mock_client(post_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "ping")

        assert result == "(no response)"

    async def test_result_text_starts_with_agent_error_gets_prefix(self):
        """Text starting with 'Agent error:' gets the _A2A_ERROR_PREFIX prepended."""
        import a2a_client

        resp = _make_response(200, {
            "result": {"parts": [{"kind": "text", "text": "Agent error: something bad"}]}
        })
        mock_client = _make_mock_client(post_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "task")

        assert result.startswith(a2a_client._A2A_ERROR_PREFIX)
        assert "Agent error: something bad" in result

    async def test_error_key_returns_error_prefix_and_message(self):
        """'error' key in response → returns _A2A_ERROR_PREFIX + error message."""
        import a2a_client

        resp = _make_response(200, {
            "error": {"code": -32603, "message": "Internal error occurred"}
        })
        mock_client = _make_mock_client(post_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "task")

        assert result.startswith(a2a_client._A2A_ERROR_PREFIX)
        assert "Internal error occurred" in result

    async def test_error_key_missing_message_returns_unknown(self):
        """'error' key without 'message' → falls back to 'unknown'."""
        import a2a_client

        resp = _make_response(200, {"error": {"code": -32600}})
        mock_client = _make_mock_client(post_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "task")

        assert result.startswith(a2a_client._A2A_ERROR_PREFIX)
        # The error includes the JSON-RPC code so the operator can look it
        # up; "no message" surfaces the missing-message condition explicitly
        # instead of the previous opaque "unknown".
        assert "code=-32600" in result
        assert "no message" in result.lower()
        # Target URL is included so chained delegations are traceable.
        # Target URL now constructed internally — assert it contains the peer_id
        # and the proxy path, not the old hand-passed URL.
        assert _TEST_PEER_ID in result
        assert "/workspaces/" in result and "/a2a" in result

    async def test_jsonrpc_error_with_code_zero_includes_code_in_detail(self):
        """JSON-RPC error code=0 is technically not valid in the spec,
        but a malformed peer can still send it — make sure the code is
        preserved in the detail rather than collapsing into the
        no-code path. Locks in the `code is not None` semantics over
        the truthy-check shortcut."""
        import a2a_client

        resp = _make_response(200, {"error": {"code": 0, "message": "weird"}})
        mock_client = _make_mock_client(post_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "task")

        assert result.startswith(a2a_client._A2A_ERROR_PREFIX)
        assert "code=0" in result
        assert "weird" in result

    async def test_neither_result_nor_error_returns_a2a_error_with_payload(self):
        """Response with neither 'result' nor 'error' → A2A_ERROR + payload context."""
        import a2a_client

        payload = {"jsonrpc": "2.0", "id": "abc123"}
        resp = _make_response(200, payload)
        mock_client = _make_mock_client(post_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "task")

        # Pre-fix this returned bare str(payload) which the canvas
        # rendered as a confusing "looks like a successful response"
        # block. Now it's tagged so downstream UI / delegate_task
        # routes it through the error path.
        assert result.startswith(a2a_client._A2A_ERROR_PREFIX)
        assert "unexpected response shape" in result
        assert "abc123" in result  # snippet of payload included for context
        # Target URL now constructed internally — assert it contains the peer_id
        # and the proxy path, not the old hand-passed URL.
        assert _TEST_PEER_ID in result
        assert "/workspaces/" in result and "/a2a" in result

    async def test_exception_returns_error_prefix_and_message(self):
        """Network exception → returns _A2A_ERROR_PREFIX + exception text."""
        import a2a_client

        mock_client = _make_mock_client(post_exc=ConnectionError("connection refused"))

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "task")

        assert result.startswith(a2a_client._A2A_ERROR_PREFIX)
        assert "connection refused" in result
        # Exception class name is prepended when the message doesn't
        # already include it — gives the operator a typed handle to
        # search for in container logs.
        assert "ConnectionError" in result
        # Target URL now constructed internally — assert it contains the peer_id
        # and the proxy path, not the old hand-passed URL.
        assert _TEST_PEER_ID in result
        assert "/workspaces/" in result and "/a2a" in result

    async def test_empty_stringifying_exception_falls_back_to_class_name(self):
        """The user's reported bug: httpx.RemoteProtocolError and similar
        exceptions can stringify to "" — pre-fix the canvas rendered
        "[A2A_ERROR] " with no detail. Verify the empty path now
        produces an actionable message including the exception type
        and the target URL."""
        import a2a_client

        # Subclass Exception with __str__ → "" to simulate the
        # silent-exception variants without depending on a specific
        # httpx version's behavior.
        class _SilentRemoteProtocolError(Exception):
            def __str__(self) -> str:
                return ""

        mock_client = _make_mock_client(post_exc=_SilentRemoteProtocolError())

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "task")

        # Must NOT be just the bare prefix — that's the regression.
        assert result != a2a_client._A2A_ERROR_PREFIX.strip()
        assert result != f"{a2a_client._A2A_ERROR_PREFIX}"
        # Must include the class name + something explanatory.
        assert "_SilentRemoteProtocolError" in result
        assert "no message" in result.lower()
        # Target URL now constructed internally — assert it contains the peer_id
        # and the proxy path, not the old hand-passed URL.
        assert _TEST_PEER_ID in result
        assert "/workspaces/" in result and "/a2a" in result

    async def test_result_text_part_missing_text_key_returns_empty(self):
        """Part dict without 'text' key → falls back to '' (empty string returned)."""
        import a2a_client

        resp = _make_response(200, {
            "result": {"parts": [{"kind": "text"}]}  # no "text" key
        })
        mock_client = _make_mock_client(post_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "task")

        # Returns "" (empty string — does not start with _A2A_ERROR_PREFIX)
        assert result == ""

    async def test_invalid_peer_id_short_circuits_without_http(self):
        """Malformed peer_id is rejected at the trust boundary — no POST.

        Symmetric coverage with discover_peer's validation gate. Path-traversal
        ("../admin"), free-form labels ("ws-abc"), and empty strings all
        return an _A2A_ERROR_PREFIX message identifying the bad input and
        never reach the platform.
        """
        import a2a_client

        mock_client = _make_mock_client(post_resp=_make_response(200, {}))
        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            for bad in ("", "ws-abc", "../admin", "not-a-uuid", "8dad3e29"):
                result = await a2a_client.send_a2a_message(bad, "ping")
                assert result.startswith(a2a_client._A2A_ERROR_PREFIX)
                assert "invalid peer_id" in result
        # No POST should have been issued for any of those.
        mock_client.post.assert_not_called()


# ---------------------------------------------------------------------------
# send_a2a_message — transient-error retry behaviour
# ---------------------------------------------------------------------------

def _make_seq_mock_client(post_side_effect):
    """Build an AsyncClient mock whose .post() returns a different result
    on each successive call (matching httpx.AsyncClient's per-request
    semantics — each AsyncClient context-manager opens fresh in the
    retry loop, so the sequence is observed across attempts).

    A new AsyncClient context is opened for every retry attempt in the
    SUT, so we route AsyncClient(...) to a single mock that hands back
    the same client on every __aenter__ but the .post side-effect list
    is shared and consumed sequentially across attempts.
    """
    mock_client = AsyncMock()
    mock_client.__aenter__ = AsyncMock(return_value=mock_client)
    mock_client.__aexit__ = AsyncMock(return_value=False)
    mock_client.post = AsyncMock(side_effect=post_side_effect)
    return mock_client


class TestSendA2AMessageRetry:
    """Verify auto-retry on transient transport errors (RemoteProtocolError,
    ConnectError, ReadTimeout, etc.) up to _DELEGATE_MAX_ATTEMPTS times.
    Application-level errors (HTTP-status errors, JSON-RPC error in
    response body) MUST NOT be retried — they're deterministic and
    re-trying just wastes wall-clock.

    asyncio.sleep is patched to a no-op so tests don't actually wait
    out the exponential backoff.
    """

    async def test_retry_succeeds_after_two_remote_protocol_errors(self):
        """Two RemoteProtocolErrors followed by a 200 → returns the 200's text."""
        import a2a_client
        import httpx

        success = _make_response(200, {"result": {"parts": [{"kind": "text", "text": "OK"}]}})
        side_effects = [
            httpx.RemoteProtocolError("Server disconnected"),
            httpx.RemoteProtocolError("Server disconnected"),
            success,
        ]
        mock_client = _make_seq_mock_client(side_effects)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client), \
             patch("a2a_client.asyncio.sleep", new=AsyncMock()):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "ping")

        assert result == "OK"
        assert mock_client.post.await_count == 3

    async def test_retry_succeeds_after_connect_error(self):
        """Single ConnectError then 200 → returns the 200's text."""
        import a2a_client
        import httpx

        success = _make_response(200, {"result": {"parts": [{"kind": "text", "text": "OK"}]}})
        side_effects = [
            httpx.ConnectError("connection refused"),
            success,
        ]
        mock_client = _make_seq_mock_client(side_effects)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client), \
             patch("a2a_client.asyncio.sleep", new=AsyncMock()):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "ping")

        assert result == "OK"
        assert mock_client.post.await_count == 2

    async def test_all_attempts_fail_returns_last_error(self):
        """5 RemoteProtocolErrors → returns the last error formatted with target URL."""
        import a2a_client
        import httpx

        side_effects = [httpx.RemoteProtocolError("Server disconnected")] * 5
        mock_client = _make_seq_mock_client(side_effects)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client), \
             patch("a2a_client.asyncio.sleep", new=AsyncMock()):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "ping")

        assert mock_client.post.await_count == 5  # _DELEGATE_MAX_ATTEMPTS
        assert result.startswith(a2a_client._A2A_ERROR_PREFIX)
        assert "RemoteProtocolError" in result
        # Target URL now constructed internally — assert it contains the peer_id
        # and the proxy path, not the old hand-passed URL.
        assert _TEST_PEER_ID in result
        assert "/workspaces/" in result and "/a2a" in result

    async def test_caps_at_max_attempts(self):
        """If transient errors keep coming, we MUST stop at _DELEGATE_MAX_ATTEMPTS,
        not retry forever. Pin the exact attempt count so a future tweak to
        the constant has to update this test in lockstep."""
        import a2a_client
        import httpx

        side_effects = [httpx.ReadTimeout("timeout")] * 20  # way more than max
        mock_client = _make_seq_mock_client(side_effects)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client), \
             patch("a2a_client.asyncio.sleep", new=AsyncMock()):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "ping")

        assert mock_client.post.await_count == a2a_client._DELEGATE_MAX_ATTEMPTS
        assert mock_client.post.await_count == 5
        assert result.startswith(a2a_client._A2A_ERROR_PREFIX)

    async def test_application_error_not_retried(self):
        """JSON-RPC error response (application-level) is deterministic —
        retrying just wastes wall-clock. Must return on the first attempt."""
        import a2a_client

        resp = _make_response(200, {
            "error": {"code": -32603, "message": "Internal error"}
        })
        mock_client = _make_seq_mock_client([resp, resp, resp])

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client), \
             patch("a2a_client.asyncio.sleep", new=AsyncMock()):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "ping")

        assert mock_client.post.await_count == 1  # NO retry
        assert "Internal error" in result

    async def test_non_transient_exception_not_retried(self):
        """A non-httpx exception (programmer bug, JSON parse, etc.) must
        not trigger retry — surface immediately so the bug is loud."""
        import a2a_client

        # A plain ValueError isn't in _TRANSIENT_HTTP_ERRORS.
        side_effects = [ValueError("malformed something")] * 3
        mock_client = _make_seq_mock_client(side_effects)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client), \
             patch("a2a_client.asyncio.sleep", new=AsyncMock()):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "ping")

        assert mock_client.post.await_count == 1  # NO retry
        assert result.startswith(a2a_client._A2A_ERROR_PREFIX)
        assert "ValueError" in result

    async def test_total_budget_caps_retry_loop(self, monkeypatch):
        """Total wall-clock budget caps the retry loop even if attempts
        remain — protects against a string of 5×300s ReadTimeouts.
        Simulate elapsed time advancing past the budget on attempt 2."""
        import a2a_client
        import httpx

        side_effects = [httpx.ReadTimeout("timeout")] * 5
        mock_client = _make_seq_mock_client(side_effects)

        # Make time.monotonic() jump forward past the budget after the
        # second attempt — the retry loop should detect the deadline
        # and stop, even though _DELEGATE_MAX_ATTEMPTS is 5.
        call_count = {"n": 0}
        original_budget = a2a_client._DELEGATE_TOTAL_BUDGET_S

        def fake_monotonic():
            call_count["n"] += 1
            # First call (deadline computation) → 0
            # Subsequent calls → 0 until attempt 3, then jump past budget
            if call_count["n"] <= 4:
                return 0.0
            return original_budget + 1.0

        monkeypatch.setattr(a2a_client.time, "monotonic", fake_monotonic)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client), \
             patch("a2a_client.asyncio.sleep", new=AsyncMock()):
            result = await a2a_client.send_a2a_message(_TEST_PEER_ID, "ping")

        # Stopped before exhausting all 5 attempts.
        assert mock_client.post.await_count < 5
        assert result.startswith(a2a_client._A2A_ERROR_PREFIX)


def test_delegate_backoff_seconds_grows_exponentially_with_jitter():
    """Schedule: ~1s, ~2s, ~4s, ~8s, then capped at 16s. ±25% jitter
    means each delay falls in [base*0.75, base*1.25]."""
    import a2a_client

    # Run a bunch to sample the jitter distribution; assert each value
    # falls in the expected window.
    for attempt, base in [(0, 1.0), (1, 2.0), (2, 4.0), (3, 8.0), (4, 16.0), (10, 16.0)]:
        for _ in range(20):
            d = a2a_client._delegate_backoff_seconds(attempt)
            assert d >= base * 0.75 - 1e-9, f"attempt {attempt}: {d} < lower"
            assert d <= base * 1.25 + 1e-9, f"attempt {attempt}: {d} > upper"


# ---------------------------------------------------------------------------
# get_peers
# ---------------------------------------------------------------------------

class TestGetPeers:

    async def test_success_returns_list_on_200(self):
        """200 response → returns the JSON list."""
        import a2a_client

        peers = [{"id": "ws-1", "name": "Alpha"}, {"id": "ws-2", "name": "Beta"}]
        resp = _make_response(200, peers)
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.get_peers()

        assert result == peers

    async def test_non_200_returns_empty_list(self):
        """Non-200 response → returns []."""
        import a2a_client

        resp = _make_response(503, {"detail": "service unavailable"})
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.get_peers()

        assert result == []

    async def test_404_returns_empty_list(self):
        """404 response → returns []."""
        import a2a_client

        resp = _make_response(404, {"detail": "not found"})
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.get_peers()

        assert result == []

    async def test_exception_returns_empty_list(self):
        """Network exception → returns [] (exception swallowed)."""
        import a2a_client

        mock_client = _make_mock_client(get_exc=TimeoutError("timed out"))

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.get_peers()

        assert result == []

    async def test_request_url_includes_workspace_id(self):
        """GET URL contains the WORKSPACE_ID."""
        import a2a_client

        resp = _make_response(200, [])
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            await a2a_client.get_peers()

        url = mock_client.get.call_args.args[0]
        assert "peers" in url

    async def test_request_sends_workspace_id_header(self):
        """GET /registry/:id/peers must send X-Workspace-ID header (Phase 30.6)."""
        import a2a_client

        resp = _make_response(200, [])
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            await a2a_client.get_peers()

        headers_sent = mock_client.get.call_args.kwargs.get("headers", {})
        assert headers_sent.get("X-Workspace-ID") == a2a_client.WORKSPACE_ID


# ---------------------------------------------------------------------------
# get_peers_with_diagnostic — issue #2397
#
# Pin: an empty peer list MUST come with an actionable diagnostic on every
# non-200 + every transport failure. The bug was that get_peers swallowed
# every failure mode behind `return []`, leaving the agent's tool wrapper
# with no way to distinguish "you have no peers" from "auth broke" / "404
# from registry" / "platform 5xx" / "network timeout". Each of these
# requires a different operator action.
# ---------------------------------------------------------------------------

class TestGetPeersWithDiagnostic:

    async def test_200_returns_peers_and_no_diagnostic(self):
        """200 with valid list → (peers, None). diagnostic stays None on success."""
        import a2a_client

        peers = [{"id": "ws-1", "name": "Alpha"}]
        resp = _make_response(200, peers)
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result, diag = await a2a_client.get_peers_with_diagnostic()

        assert result == peers
        assert diag is None

    async def test_200_empty_list_returns_no_diagnostic(self):
        """200 with [] → (peers=[], diag=None). Truly no peers is success, not error."""
        import a2a_client

        resp = _make_response(200, [])
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result, diag = await a2a_client.get_peers_with_diagnostic()

        assert result == []
        assert diag is None

    async def test_401_returns_auth_diagnostic(self):
        """401 → diagnostic mentions auth + restart hint."""
        import a2a_client

        resp = _make_response(401, {"detail": "unauthorized"})
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result, diag = await a2a_client.get_peers_with_diagnostic()

        assert result == []
        assert diag is not None
        assert "401" in diag
        assert "Authentication" in diag or "authentication" in diag.lower()

    async def test_403_returns_auth_diagnostic(self):
        """403 → same auth-failure diagnostic shape as 401."""
        import a2a_client

        resp = _make_response(403, {"detail": "forbidden"})
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result, diag = await a2a_client.get_peers_with_diagnostic()

        assert result == []
        assert diag is not None
        assert "403" in diag

    async def test_404_returns_registration_diagnostic(self):
        """404 → diagnostic tells operator the workspace ID is missing from the registry."""
        import a2a_client

        resp = _make_response(404, {"detail": "not found"})
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result, diag = await a2a_client.get_peers_with_diagnostic()

        assert result == []
        assert diag is not None
        assert "404" in diag
        assert "registered" in diag.lower() or "registration" in diag.lower()

    async def test_500_returns_platform_error_diagnostic(self):
        """5xx → 'Platform error: HTTP <code>.'"""
        import a2a_client

        resp = _make_response(503, {"detail": "service unavailable"})
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result, diag = await a2a_client.get_peers_with_diagnostic()

        assert result == []
        assert diag is not None
        assert "503" in diag
        assert "Platform error" in diag or "platform error" in diag.lower()

    async def test_network_exception_returns_unreachable_diagnostic(self):
        """httpx exception → diagnostic mentions PLATFORM_URL + the underlying error."""
        import a2a_client

        mock_client = _make_mock_client(get_exc=TimeoutError("connection timed out"))

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result, diag = await a2a_client.get_peers_with_diagnostic()

        assert result == []
        assert diag is not None
        assert "Cannot reach platform" in diag or "cannot reach" in diag.lower()
        assert "timed out" in diag

    async def test_200_with_non_list_body_returns_diagnostic(self):
        """200 but body is a dict → diagnostic flags shape mismatch (regression guard)."""
        import a2a_client

        resp = _make_response(200, {"oops": "should have been a list"})
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result, diag = await a2a_client.get_peers_with_diagnostic()

        assert result == []
        assert diag is not None
        assert "list" in diag.lower()

    async def test_get_peers_shim_preserves_bare_list_contract(self):
        """get_peers() still returns just list[dict] — no API break for non-tool callers."""
        import a2a_client

        peers = [{"id": "ws-1", "name": "Alpha"}]
        resp = _make_response(200, peers)
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.get_peers()

        # Must be a list, not a tuple — bare-list shim contract.
        assert isinstance(result, list)
        assert result == peers


# ---------------------------------------------------------------------------
# get_workspace_info
# ---------------------------------------------------------------------------

class TestGetWorkspaceInfo:

    async def test_success_returns_dict_on_200(self):
        """200 response → returns the JSON dict."""
        import a2a_client

        info = {"id": "ws-test", "name": "Test Workspace", "status": "online"}
        resp = _make_response(200, info)
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.get_workspace_info()

        assert result == info

    async def test_non_200_returns_error_dict(self):
        """Non-200 response → returns {'error': 'not found'}."""
        import a2a_client

        resp = _make_response(404, {"detail": "no such workspace"})
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.get_workspace_info()

        assert result == {"error": "not found"}

    async def test_500_returns_error_dict(self):
        """500 response → returns {'error': 'not found'}."""
        import a2a_client

        resp = _make_response(500, {"detail": "server error"})
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.get_workspace_info()

        assert result == {"error": "not found"}

    async def test_410_returns_removed_with_hint(self):
        """410 Gone (#2429) → distinct error 'removed' so callers can
        prompt re-onboard instead of falling through to 'not found'.
        Body shape passes through removed_at + the platform hint."""
        import a2a_client

        body = {
            "error": "workspace removed",
            "id": "ws-deleted-uuid",
            "removed_at": "2026-04-30T12:00:00Z",
            "hint": "Regenerate workspace + token from the canvas → Tokens tab",
        }
        resp = _make_response(410, body)
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.get_workspace_info()

        assert result["error"] == "removed"
        assert result["id"] == "ws-deleted-uuid"
        assert result["removed_at"] == "2026-04-30T12:00:00Z"
        assert "Regenerate" in result["hint"]

    async def test_410_with_unparseable_body_falls_back_to_default_hint(self):
        """If the platform's 410 body isn't JSON for some reason, the
        default hint still surfaces — the actionable signal must not
        depend on body shape parity with the platform."""
        import a2a_client

        resp = MagicMock()
        resp.status_code = 410
        resp.json = MagicMock(side_effect=ValueError("not json"))
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.get_workspace_info()

        assert result["error"] == "removed"
        assert result["id"] == a2a_client.WORKSPACE_ID
        assert result["removed_at"] is None
        assert "Regenerate" in result["hint"]

    async def test_exception_returns_error_dict_with_message(self):
        """Network exception → returns {'error': '<exception message>'}."""
        import a2a_client

        exc = RuntimeError("network failure")
        mock_client = _make_mock_client(get_exc=exc)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            result = await a2a_client.get_workspace_info()

        assert "error" in result
        assert "network failure" in result["error"]

    async def test_request_url_includes_workspaces_path(self):
        """GET URL contains /workspaces/."""
        import a2a_client

        resp = _make_response(200, {})
        mock_client = _make_mock_client(get_resp=resp)

        with patch("a2a_client.httpx.AsyncClient", return_value=mock_client):
            await a2a_client.get_workspace_info()

        url = mock_client.get.call_args.args[0]
        assert "/workspaces/" in url
