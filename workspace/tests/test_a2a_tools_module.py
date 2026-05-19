"""Tests for tools/a2a_tools.py — framework-agnostic delegation helpers.

Uses importlib.util.spec_from_file_location to load the real module without
conftest interference (conftest installs a mock at tools.a2a_tools).
"""

import importlib.util
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
TOOLS_DIR = ROOT / "builtin_tools"


def _load_a2a_tools(monkeypatch, *, platform_url="http://platform.test", workspace_id="ws-test"):
    """Load the real tools/a2a_tools.py in isolation."""
    monkeypatch.setenv("PLATFORM_URL", platform_url)
    monkeypatch.setenv("WORKSPACE_ID", workspace_id)

    spec = importlib.util.spec_from_file_location(
        "_test_a2a_tools",
        TOOLS_DIR / "a2a_tools.py",
    )
    mod = importlib.util.module_from_spec(spec)
    # Do NOT register under tools.a2a_tools — keep it isolated
    spec.loader.exec_module(mod)
    # Patch module-level constants to match env
    mod.PLATFORM_URL = platform_url
    mod.WORKSPACE_ID = workspace_id
    return mod


class _FakeResponse:
    def __init__(self, status_code, payload):
        self.status_code = status_code
        self._payload = payload
        self.text = str(payload)

    def json(self):
        return self._payload


# ---------------------------------------------------------------------------
# list_peers
# ---------------------------------------------------------------------------

class TestListPeers:

    async def test_list_peers_200(self, monkeypatch):
        mod = _load_a2a_tools(monkeypatch)
        peers_data = [{"id": "ws-1", "name": "Peer One", "role": "worker", "status": "online"}]

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass
            async def get(self, url):
                assert url == "http://platform.test/registry/ws-test/peers"
                return _FakeResponse(200, peers_data)

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.list_peers()
        assert result == peers_data

    async def test_list_peers_non_200(self, monkeypatch):
        mod = _load_a2a_tools(monkeypatch)

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass
            async def get(self, url):
                return _FakeResponse(404, {"error": "not found"})

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.list_peers()
        assert result == []

    async def test_list_peers_exception(self, monkeypatch):
        mod = _load_a2a_tools(monkeypatch)

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass
            async def get(self, url):
                raise ConnectionError("network down")

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.list_peers()
        assert result == []


# ---------------------------------------------------------------------------
# delegate_task
# ---------------------------------------------------------------------------

class TestDelegateTask:

    async def test_delegate_task_success_with_parts(self, monkeypatch):
        """Full happy path: discover returns URL, A2A responds with result parts."""
        mod = _load_a2a_tools(monkeypatch)

        calls = []

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass

            async def get(self, url, headers=None):
                calls.append(("get", url, headers))
                return _FakeResponse(200, {"url": "http://target.test/a2a"})

            async def post(self, url, json=None, headers=None):
                calls.append(("post", url, headers))
                return _FakeResponse(200, {
                    "result": {
                        "parts": [{"kind": "text", "text": "Task done!"}]
                    }
                })

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.delegate_task("ws-target", "do something")
        assert result == "Task done!"
        assert any(c[0] == "get" for c in calls)
        post_calls = [c for c in calls if c[0] == "post"]
        assert post_calls, "delegate_task must POST to the target's /a2a endpoint"
        # Regression: peer A2A POSTs MUST include X-Workspace-ID so
        # the platform's a2a_receive logger writes source_id correctly
        # — without it the recipient's My Chat tab would render the
        # delegation as user-typed input. Same hazard fixed in
        # heartbeat.py / a2a_client.py / main.py initial+idle flows.
        post_headers = post_calls[0][2] or {}
        assert post_headers.get("X-Workspace-ID"), (
            f"delegate_task POST must include X-Workspace-ID; got headers={post_headers!r}"
        )

    async def test_delegate_task_success_empty_parts(self, monkeypatch):
        """Result with empty parts list falls back to str(result)."""
        mod = _load_a2a_tools(monkeypatch)

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass

            async def get(self, url, headers=None):
                return _FakeResponse(200, {"url": "http://target.test/a2a"})

            async def post(self, url, json=None, headers=None):
                return _FakeResponse(200, {"result": {"parts": []}})

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.delegate_task("ws-target", "do something")
        assert "parts" in result or result == str({"parts": []})

    async def test_delegate_task_discover_non_200(self, monkeypatch):
        """When discover returns non-200, returns error string."""
        mod = _load_a2a_tools(monkeypatch)

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass

            async def get(self, url, headers=None):
                return _FakeResponse(403, {"error": "forbidden"})

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.delegate_task("ws-target", "do something")
        assert "Error" in result
        assert "403" in result

    async def test_delegate_task_discover_no_url(self, monkeypatch):
        """When discover returns 200 but no url field, returns error string."""
        mod = _load_a2a_tools(monkeypatch)

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass

            async def get(self, url, headers=None):
                return _FakeResponse(200, {"url": ""})

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.delegate_task("ws-target", "do something")
        assert "Error" in result
        assert "no URL" in result

    async def test_delegate_task_discover_exception(self, monkeypatch):
        """When discover raises, returns error string."""
        mod = _load_a2a_tools(monkeypatch)

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass

            async def get(self, url, headers=None):
                raise ConnectionError("host unreachable")

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.delegate_task("ws-target", "do something")
        assert "Error discovering workspace" in result

    async def test_delegate_task_a2a_error_response(self, monkeypatch):
        """When A2A endpoint returns an error payload, returns error string."""
        mod = _load_a2a_tools(monkeypatch)

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass

            async def get(self, url, headers=None):
                return _FakeResponse(200, {"url": "http://target.test/a2a"})

            async def post(self, url, json=None, headers=None):
                return _FakeResponse(200, {
                    "error": {"code": -32603, "message": "Internal error"}
                })

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.delegate_task("ws-target", "do something")
        assert "Error" in result
        assert "Internal error" in result

    async def test_delegate_task_a2a_unknown_response(self, monkeypatch):
        """When A2A endpoint returns neither result nor error, returns str(data)."""
        mod = _load_a2a_tools(monkeypatch)

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass

            async def get(self, url, headers=None):
                return _FakeResponse(200, {"url": "http://target.test/a2a"})

            async def post(self, url, json=None, headers=None):
                return _FakeResponse(200, {"jsonrpc": "2.0", "id": "123"})

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.delegate_task("ws-target", "do something")
        assert "jsonrpc" in result

    async def test_delegate_task_a2a_exception(self, monkeypatch):
        """When A2A POST raises, returns error string."""
        mod = _load_a2a_tools(monkeypatch)

        call_count = {"n": 0}

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass

            async def get(self, url, headers=None):
                return _FakeResponse(200, {"url": "http://target.test/a2a"})

            async def post(self, url, json=None, headers=None):
                call_count["n"] += 1
                raise ConnectionError("target down")

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.delegate_task("ws-target", "do something")
        assert "Error sending A2A message" in result


# ---------------------------------------------------------------------------
# get_peers_summary
# ---------------------------------------------------------------------------

class TestGetPeersSummary:

    async def test_get_peers_summary_with_peers(self, monkeypatch):
        mod = _load_a2a_tools(monkeypatch)

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass
            async def get(self, url):
                return _FakeResponse(200, [
                    {"id": "ws-1", "name": "Alpha", "role": "worker", "status": "online"},
                    {"id": "ws-2", "name": "Beta", "role": "analyst", "status": "idle"},
                ])

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.get_peers_summary()
        assert "Available peers:" in result
        assert "Alpha" in result
        assert "ws-1" in result
        assert "worker" in result
        assert "online" in result
        assert "Beta" in result

    async def test_get_peers_summary_empty(self, monkeypatch):
        mod = _load_a2a_tools(monkeypatch)

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass
            async def get(self, url):
                return _FakeResponse(200, [])

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.get_peers_summary()
        assert result == "No peers available."


# ---------------------------------------------------------------------------
# Self-delegation guard (Task #190 / #193)
# ---------------------------------------------------------------------------

class TestSelfDelegationGuard:
    """delegate_task to your own workspace UUID must be rejected BEFORE any
    discovery / proxy hop. Otherwise the request round-trips back to us,
    deadlocks on the run lock, times out, and surfaces in the inbox as a
    peer_agent message from our own workspace (the documented #190 self-echo
    bug)."""

    async def test_delegate_task_rejects_self(self, monkeypatch):
        mod = _load_a2a_tools(monkeypatch, workspace_id="ws-self-abc")

        calls = []

        class TrappingClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass
            async def get(self, *a, **kw):
                calls.append(("get", a, kw))
                raise AssertionError("guard must reject before discover")
            async def post(self, *a, **kw):
                calls.append(("post", a, kw))
                raise AssertionError("guard must reject before proxy POST")

        monkeypatch.setattr(mod.httpx, "AsyncClient", TrappingClient)

        result = await mod.delegate_task("ws-self-abc", "do a thing")
        assert "self-delegation" in result.lower()
        assert not calls, "no HTTP call should be made for self-delegation"

    async def test_delegate_task_allows_real_peer(self, monkeypatch):
        """Guard is strictly equality on WORKSPACE_ID — a different target
        passes through to the normal discover/proxy path."""
        mod = _load_a2a_tools(monkeypatch, workspace_id="ws-self-abc")

        class FakeClient:
            def __init__(self, timeout): pass
            async def __aenter__(self): return self
            async def __aexit__(self, *a): pass
            async def get(self, url, headers=None):
                return _FakeResponse(200, {"url": "http://target.test/a2a"})
            async def post(self, url, json=None, headers=None):
                return _FakeResponse(200, {
                    "result": {"parts": [{"kind": "text", "text": "ok"}]}
                })

        monkeypatch.setattr(mod.httpx, "AsyncClient", FakeClient)

        result = await mod.delegate_task("ws-DIFFERENT-xyz", "do a thing")
        assert "self-delegation" not in result.lower()
