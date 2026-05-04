"""Tests for cross-workspace A2A delegation + peer aggregation (PR-2 of
the multi-workspace MCP feature).

PR-1 made the auth registry per-workspace. PR-2 threads
``source_workspace_id`` through the A2A client + tool surface so an
external agent registered against multiple workspaces can:

  - List peers across every registered workspace in one call.
  - Delegate from a specific source workspace (or auto-route via the
    peer→source cache populated by list_peers).
  - The legacy single-workspace path (no MOLECULE_WORKSPACES) is
    untouched — falls back to the module-level WORKSPACE_ID exactly as
    before.
"""
from __future__ import annotations

import sys
from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest

_THIS = Path(__file__).resolve()
sys.path.insert(0, str(_THIS.parent.parent))


@pytest.fixture(autouse=True)
def _isolate_env(monkeypatch):
    """Ensure WORKSPACE_ID + PLATFORM_URL are predictable across tests
    and the per-workspace token registry doesn't leak between cases."""
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000001")
    monkeypatch.setenv("PLATFORM_URL", "http://test-platform")

    import platform_auth
    platform_auth.clear_cache()

    import a2a_client
    a2a_client._peer_to_source.clear()
    a2a_client._peer_names.clear()

    yield

    platform_auth.clear_cache()
    a2a_client._peer_to_source.clear()
    a2a_client._peer_names.clear()


# ---------------------------------------------------------------------------
# Lower-layer helpers — discover_peer / send_a2a_message /
# get_peers_with_diagnostic — should route via source_workspace_id when
# set, fall back to module-level WORKSPACE_ID otherwise.
# ---------------------------------------------------------------------------


class TestDiscoverPeerSourceRouting:
    @pytest.mark.asyncio
    async def test_routes_through_source_workspace_id_when_set(self, monkeypatch):
        """source_workspace_id drives the X-Workspace-ID header AND the
        bearer token (via auth_headers(src))."""
        import platform_auth, a2a_client

        platform_auth.register_workspace_token("aaaa1111-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "token-A")

        captured: dict = {}

        class _Resp:
            status_code = 200
            def json(self):
                return {"id": "bbbb2222-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "name": "peer-of-A"}

        class _Client:
            async def __aenter__(self):
                return self
            async def __aexit__(self, *a):
                return None
            async def get(self, url, headers):
                captured["url"] = url
                captured["headers"] = headers
                return _Resp()

        monkeypatch.setattr(a2a_client.httpx, "AsyncClient", lambda timeout: _Client())

        result = await a2a_client.discover_peer(
            "bbbb2222-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
            source_workspace_id="aaaa1111-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
        )
        assert result == {"id": "bbbb2222-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "name": "peer-of-A"}
        assert captured["headers"]["X-Workspace-ID"] == "aaaa1111-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
        assert captured["headers"]["Authorization"] == "Bearer token-A"

    @pytest.mark.asyncio
    async def test_falls_back_to_module_workspace_id(self, monkeypatch):
        """No source_workspace_id → uses module-level WORKSPACE_ID."""
        import a2a_client

        captured: dict = {}

        class _Resp:
            status_code = 200
            def json(self):
                return {"id": "x", "name": "y"}

        class _Client:
            async def __aenter__(self):
                return self
            async def __aexit__(self, *a):
                return None
            async def get(self, url, headers):
                captured["headers"] = headers
                return _Resp()

        monkeypatch.setattr(a2a_client.httpx, "AsyncClient", lambda timeout: _Client())

        await a2a_client.discover_peer("11111111-1111-1111-1111-111111111111")
        # WORKSPACE_ID is captured at a2a_client import time; assert
        # against the module attribute rather than a hardcoded UUID so
        # the test is portable across CI environments that pre-set
        # WORKSPACE_ID before pytest runs.
        assert captured["headers"]["X-Workspace-ID"] == a2a_client.WORKSPACE_ID

    @pytest.mark.asyncio
    async def test_invalid_target_id_returns_none_without_routing(self, monkeypatch):
        """Validation runs before routing — short-circuits without an
        outbound HTTP attempt regardless of source."""
        import a2a_client

        called = {"hit": False}

        class _Client:
            async def __aenter__(self):
                called["hit"] = True
                return self
            async def __aexit__(self, *a):
                return None
            async def get(self, *a, **kw):
                called["hit"] = True

        monkeypatch.setattr(a2a_client.httpx, "AsyncClient", lambda timeout: _Client())

        result = await a2a_client.discover_peer("not-a-uuid", source_workspace_id="anything")
        assert result is None
        assert not called["hit"]


class TestSendA2AMessageSourceRouting:
    @pytest.mark.asyncio
    async def test_self_source_headers_built_from_source_arg(self, monkeypatch):
        """The X-Workspace-ID source header must reflect the SENDING
        workspace, not the module-level WORKSPACE_ID. Otherwise
        cross-workspace delegations land in the wrong tenant's audit log."""
        import platform_auth, a2a_client

        platform_auth.register_workspace_token("cccc3333-cccc-cccc-cccc-cccccccccccc", "token-C")

        captured: dict = {}

        class _Resp:
            status_code = 200
            def json(self):
                return {"jsonrpc": "2.0", "result": {"parts": [{"text": "PONG"}]}}

        class _Client:
            async def __aenter__(self):
                return self
            async def __aexit__(self, *a):
                return None
            async def post(self, url, headers, json):
                captured["url"] = url
                captured["headers"] = headers
                return _Resp()

        monkeypatch.setattr(a2a_client.httpx, "AsyncClient", lambda timeout: _Client())

        result = await a2a_client.send_a2a_message(
            "dddd4444-dddd-dddd-dddd-dddddddddddd",
            "ping",
            source_workspace_id="cccc3333-cccc-cccc-cccc-cccccccccccc",
        )
        assert result == "PONG"
        assert captured["headers"]["X-Workspace-ID"] == "cccc3333-cccc-cccc-cccc-cccccccccccc"
        assert captured["headers"]["Authorization"] == "Bearer token-C"


class TestGetPeersSourceRouting:
    @pytest.mark.asyncio
    async def test_url_and_headers_use_source_workspace_id(self, monkeypatch):
        import platform_auth, a2a_client

        platform_auth.register_workspace_token("eeee5555-eeee-eeee-eeee-eeeeeeeeeeee", "token-E")

        captured: dict = {}

        class _Resp:
            status_code = 200
            def json(self):
                return [{"id": "x", "name": "peer-x", "status": "online"}]

        class _Client:
            async def __aenter__(self):
                return self
            async def __aexit__(self, *a):
                return None
            async def get(self, url, headers):
                captured["url"] = url
                captured["headers"] = headers
                return _Resp()

        monkeypatch.setattr(a2a_client.httpx, "AsyncClient", lambda timeout: _Client())

        peers, diag = await a2a_client.get_peers_with_diagnostic(
            source_workspace_id="eeee5555-eeee-eeee-eeee-eeeeeeeeeeee",
        )
        assert diag is None
        assert peers == [{"id": "x", "name": "peer-x", "status": "online"}]
        assert "/registry/eeee5555-eeee-eeee-eeee-eeeeeeeeeeee/peers" in captured["url"]
        assert captured["headers"]["X-Workspace-ID"] == "eeee5555-eeee-eeee-eeee-eeeeeeeeeeee"
        assert captured["headers"]["Authorization"] == "Bearer token-E"


# ---------------------------------------------------------------------------
# Tool surface — tool_list_peers aggregation + tool_delegate_task
# auto-routing via the peer→source cache.
# ---------------------------------------------------------------------------


class TestToolListPeersAggregation:
    @pytest.mark.asyncio
    async def test_aggregates_across_registered_workspaces(self, monkeypatch):
        """Multi-workspace mode (>1 registered) → list_peers aggregates."""
        import platform_auth, a2a_tools, a2a_client

        ws_a = "aaaa1111-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
        ws_b = "bbbb2222-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
        platform_auth.register_workspace_token(ws_a, "token-A")
        platform_auth.register_workspace_token(ws_b, "token-B")

        async def fake_get_peers(source_workspace_id=None):
            if source_workspace_id == ws_a:
                return [{"id": "1111aaaa-1111-1111-1111-111111111111", "name": "alice", "status": "online", "role": "ops"}], None
            if source_workspace_id == ws_b:
                return [{"id": "2222bbbb-2222-2222-2222-222222222222", "name": "bob", "status": "online", "role": "dev"}], None
            return [], None

        with patch("a2a_tools.get_peers_with_diagnostic", side_effect=fake_get_peers):
            output = await a2a_tools.tool_list_peers()

        assert "alice" in output
        assert "bob" in output
        assert f"via: {ws_a[:8]}" in output
        assert f"via: {ws_b[:8]}" in output

        # Side-effect: peer→source map populated for downstream auto-routing.
        assert a2a_client._peer_to_source["1111aaaa-1111-1111-1111-111111111111"] == ws_a
        assert a2a_client._peer_to_source["2222bbbb-2222-2222-2222-222222222222"] == ws_b

    @pytest.mark.asyncio
    async def test_single_workspace_unchanged(self, monkeypatch):
        """Legacy path: no MOLECULE_WORKSPACES → module WORKSPACE_ID,
        no `via:` annotation, no aggregation."""
        import a2a_tools, a2a_client

        async def fake_get_peers(source_workspace_id=None):
            assert source_workspace_id == a2a_client.WORKSPACE_ID
            return [{"id": "1111aaaa-1111-1111-1111-111111111111", "name": "alice", "status": "online", "role": "ops"}], None

        with patch("a2a_tools.get_peers_with_diagnostic", side_effect=fake_get_peers):
            output = await a2a_tools.tool_list_peers()

        assert "alice" in output
        assert "via:" not in output

    @pytest.mark.asyncio
    async def test_explicit_source_workspace_id_overrides(self, monkeypatch):
        """Explicit source_workspace_id arg → query that workspace only,
        not aggregated."""
        import platform_auth, a2a_tools

        ws_a = "aaaa1111-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
        ws_b = "bbbb2222-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
        platform_auth.register_workspace_token(ws_a, "token-A")
        platform_auth.register_workspace_token(ws_b, "token-B")

        seen = []

        async def fake_get_peers(source_workspace_id=None):
            seen.append(source_workspace_id)
            return [{"id": "1111aaaa-1111-1111-1111-111111111111", "name": "alice", "status": "online", "role": "ops"}], None

        with patch("a2a_tools.get_peers_with_diagnostic", side_effect=fake_get_peers):
            output = await a2a_tools.tool_list_peers(source_workspace_id=ws_a)

        assert seen == [ws_a]
        # Aggregate annotation not applied when scoped to one source.
        assert "via:" not in output

    @pytest.mark.asyncio
    async def test_aggregated_diagnostic_per_source(self):
        """When all workspaces return empty-with-diagnostic, the message
        prefixes each diagnostic with its source workspace's short id."""
        import platform_auth, a2a_tools

        ws_a = "aaaa1111-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
        ws_b = "bbbb2222-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
        platform_auth.register_workspace_token(ws_a, "token-A")
        platform_auth.register_workspace_token(ws_b, "token-B")

        async def fake_get_peers(source_workspace_id=None):
            if source_workspace_id == ws_a:
                return [], "auth failed"
            return [], "platform 5xx"

        with patch("a2a_tools.get_peers_with_diagnostic", side_effect=fake_get_peers):
            out = await a2a_tools.tool_list_peers()

        assert "[aaaa1111] auth failed" in out
        assert "[bbbb2222] platform 5xx" in out


class TestToolDelegateTaskAutoRouting:
    @pytest.mark.asyncio
    async def test_uses_cached_source_when_available(self, monkeypatch):
        """When the peer is in the _peer_to_source cache (populated by a
        prior list_peers), delegate_task auto-routes through that
        source without the agent specifying source_workspace_id."""
        import a2a_tools, a2a_client

        ws_a = "aaaa1111-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
        peer_id = "1111aaaa-1111-1111-1111-111111111111"
        a2a_client._peer_to_source[peer_id] = ws_a

        seen_discover_src = {}
        seen_send_src = {}

        async def fake_discover(target_id, source_workspace_id=None):
            seen_discover_src["src"] = source_workspace_id
            return {"id": target_id, "name": "alice", "status": "online"}

        async def fake_send(passed_peer_id, message, source_workspace_id=None):
            seen_send_src["src"] = source_workspace_id
            return "ok"

        with patch("a2a_tools.discover_peer", side_effect=fake_discover), \
             patch("a2a_tools.send_a2a_message", side_effect=fake_send), \
             patch("a2a_tools.report_activity", new=AsyncMock()):
            await a2a_tools.tool_delegate_task(peer_id, "do thing")

        assert seen_discover_src["src"] == ws_a
        assert seen_send_src["src"] == ws_a

    @pytest.mark.asyncio
    async def test_explicit_source_overrides_cache(self):
        """Explicit source_workspace_id beats the auto-routing cache."""
        import a2a_tools, a2a_client

        peer_id = "1111aaaa-1111-1111-1111-111111111111"
        ws_cached = "aaaa1111-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
        ws_explicit = "cccc3333-cccc-cccc-cccc-cccccccccccc"
        a2a_client._peer_to_source[peer_id] = ws_cached

        seen = {}

        async def fake_discover(target_id, source_workspace_id=None):
            seen["discover"] = source_workspace_id
            return {"id": target_id, "name": "alice", "status": "online"}

        async def fake_send(passed_peer_id, message, source_workspace_id=None):
            seen["send"] = source_workspace_id
            return "ok"

        with patch("a2a_tools.discover_peer", side_effect=fake_discover), \
             patch("a2a_tools.send_a2a_message", side_effect=fake_send), \
             patch("a2a_tools.report_activity", new=AsyncMock()):
            await a2a_tools.tool_delegate_task(
                peer_id, "do thing", source_workspace_id=ws_explicit,
            )

        assert seen["discover"] == ws_explicit
        assert seen["send"] == ws_explicit

    @pytest.mark.asyncio
    async def test_no_cache_no_explicit_falls_back_to_module(self):
        """Single-workspace operators see no behavior change — when the
        peer isn't cached and no source is passed, source_workspace_id
        stays None and the lower layer falls back to WORKSPACE_ID."""
        import a2a_tools

        peer_id = "1111aaaa-1111-1111-1111-111111111111"
        seen = {}

        async def fake_discover(target_id, source_workspace_id=None):
            seen["discover"] = source_workspace_id
            return {"id": target_id, "name": "alice", "status": "online"}

        async def fake_send(passed_peer_id, message, source_workspace_id=None):
            seen["send"] = source_workspace_id
            return "ok"

        with patch("a2a_tools.discover_peer", side_effect=fake_discover), \
             patch("a2a_tools.send_a2a_message", side_effect=fake_send), \
             patch("a2a_tools.report_activity", new=AsyncMock()):
            await a2a_tools.tool_delegate_task(peer_id, "do thing")

        assert seen["discover"] is None
        assert seen["send"] is None


# ---------------------------------------------------------------------------
# platform_auth registry helper exposed to the tool layer.
# ---------------------------------------------------------------------------


class TestListRegisteredWorkspaces:
    def test_empty_when_no_registrations(self):
        import platform_auth
        assert platform_auth.list_registered_workspaces() == []

    def test_returns_registered_ids(self):
        import platform_auth
        platform_auth.register_workspace_token("ws-1", "tok-1")
        platform_auth.register_workspace_token("ws-2", "tok-2")
        result = sorted(platform_auth.list_registered_workspaces())
        assert result == ["ws-1", "ws-2"]

    def test_clear_cache_empties_registry(self):
        import platform_auth
        platform_auth.register_workspace_token("ws-1", "tok-1")
        platform_auth.clear_cache()
        assert platform_auth.list_registered_workspaces() == []
