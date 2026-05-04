"""Integration tests for boot_routes.build_routes — pin the contract that
PR #2756's card-vs-setup decoupling depends on.

Why these matter (issue #2761): main.py is ``# pragma: no cover``. The
inline if/else that mounted ``DefaultRequestHandler`` vs the
not-configured handler had no pytest coverage; a future refactor that
re-coupled card and setup() would have shipped the original "stuck
booting forever" UX again. Extracting to ``boot_routes.build_routes``
+ these tests make the contract regression-proof.

Each test exercises a real Starlette TestClient against the routes —
no uvicorn, no socket, but every assertion is the same one canvas's
TranscriptHandler / a2a_proxy would make in production.
"""
from __future__ import annotations

import sys
from pathlib import Path
from unittest.mock import MagicMock

import pytest

# Make workspace/ importable in test isolation — same pattern as the
# adjacent tests (test_not_configured_handler.py, test_card_helpers.py).
WORKSPACE_DIR = Path(__file__).resolve().parents[1]
if str(WORKSPACE_DIR) not in sys.path:
    sys.path.insert(0, str(WORKSPACE_DIR))


@pytest.fixture
def agent_card():
    """Build a minimal AgentCard the way main.py does at boot."""
    from a2a.types import (
        AgentCard,
        AgentCapabilities,
        AgentInterface,
        AgentSkill,
    )

    return AgentCard(
        name="test-agent",
        description="test-agent",
        version="0.0.0",
        supported_interfaces=[
            AgentInterface(protocol_binding="https://a2a.g/v1", url="http://test:8000")
        ],
        capabilities=AgentCapabilities(streaming=True, push_notifications=False),
        skills=[
            AgentSkill(id="echo", name="echo", description="echo", tags=[], examples=[])
        ],
        default_input_modes=["text/plain"],
        default_output_modes=["text/plain"],
    )


# ---- card route always mounted, regardless of adapter state -------------


def test_card_route_serves_200_when_adapter_ready(agent_card):
    """Adapter setup OK → card serves 200, the canonical happy path."""
    from starlette.applications import Starlette
    from starlette.testclient import TestClient

    from boot_routes import build_routes

    fake_executor = MagicMock()
    app = Starlette(routes=build_routes(agent_card, fake_executor, None))
    client = TestClient(app)
    resp = client.get("/.well-known/agent-card.json")
    assert resp.status_code == 200
    body = resp.json()
    assert body["name"] == "test-agent"


def test_card_route_serves_200_when_adapter_failed(agent_card):
    """Adapter setup raised → card route is STILL mounted with the same
    static skills. This is the entire point of PR #2756: a misconfigured
    workspace stays REACHABLE so canvas can show the user a clear error
    instead of silently looking dead."""
    from starlette.applications import Starlette
    from starlette.testclient import TestClient

    from boot_routes import build_routes

    app = Starlette(
        routes=build_routes(
            agent_card, executor=None, adapter_error="MISSING_API_KEY"
        )
    )
    client = TestClient(app)
    resp = client.get("/.well-known/agent-card.json")
    assert resp.status_code == 200
    body = resp.json()
    assert body["name"] == "test-agent"
    # Skill stubs survive even though setup() didn't run.
    assert any(s.get("id") == "echo" for s in body.get("skills", []))


# ---- JSON-RPC route swaps based on executor presence -------------------


def test_jsonrpc_returns_503_when_no_executor(agent_card):
    """The not-configured branch: POST / returns 503 with JSON-RPC -32603
    and the adapter_error in error.data. This is what canvas sees when a
    user tries to message a workspace whose setup() failed — turns a
    "stuck silent" workspace into "agent not configured: <reason>"."""
    from starlette.applications import Starlette
    from starlette.testclient import TestClient

    from boot_routes import build_routes

    app = Starlette(
        routes=build_routes(
            agent_card,
            executor=None,
            adapter_error="RuntimeError: Neither OPENAI_API_KEY nor MINIMAX_API_KEY is set",
        )
    )
    client = TestClient(app)
    resp = client.post(
        "/",
        json={"jsonrpc": "2.0", "id": 42, "method": "message/send"},
    )
    assert resp.status_code == 503
    body = resp.json()
    assert body["jsonrpc"] == "2.0"
    assert body["id"] == 42  # echoed
    assert body["error"]["code"] == -32603
    assert "MINIMAX_API_KEY" in body["error"]["data"]


def test_jsonrpc_returns_503_with_generic_when_no_error_string(agent_card):
    """Defensive: if main.py reached this branch without a captured
    error string (shouldn't happen in practice but the helper is
    defensive), the handler still returns -32603 with a generic
    fallback so the operator gets a useful response shape."""
    from starlette.applications import Starlette
    from starlette.testclient import TestClient

    from boot_routes import build_routes

    app = Starlette(
        routes=build_routes(agent_card, executor=None, adapter_error=None)
    )
    client = TestClient(app)
    resp = client.post(
        "/", json={"jsonrpc": "2.0", "id": 1, "method": "message/send"}
    )
    assert resp.status_code == 503
    assert resp.json()["error"]["code"] == -32603
    # Falls back to generic "adapter.setup() failed".
    assert "setup() failed" in resp.json()["error"]["data"]


# ---- Specific regression: re-coupling card to setup would break this ---


def test_card_route_does_not_depend_on_executor(agent_card):
    """Direct regression test for PR #2756. If a future refactor moved
    create_agent_card_routes into the executor-only branch, this test
    would catch it: the card MUST be served from a code path that runs
    even when executor is None."""
    from boot_routes import build_routes

    routes_with_executor = build_routes(agent_card, MagicMock(), None)
    routes_without_executor = build_routes(agent_card, None, "err")

    # Both branches mount /.well-known/agent-card.json. Find by path.
    def has_card_route(routes):
        for r in routes:
            for attr in ("path", "path_format"):
                p = getattr(r, attr, None)
                if p and "agent-card.json" in p:
                    return True
        return False

    assert has_card_route(routes_with_executor), (
        "card route MUST be mounted on the executor-present path"
    )
    assert has_card_route(routes_without_executor), (
        "card route MUST be mounted on the executor-missing path "
        "(this is the PR #2756 contract — re-coupling here breaks tenant readiness)"
    )


def test_executor_present_does_not_mount_not_configured_handler(agent_card):
    """Sanity: when executor is present, the not-configured handler
    must NOT be mounted at /. Otherwise a healthy workspace would
    return -32603 to every JSON-RPC call.

    We call POST / with a malformed JSON-RPC body and assert the
    response is NOT the -32603 not-configured envelope. (The real
    DefaultRequestHandler may return its own error for the malformed
    payload, but it won't have ``data: "adapter.setup() failed"``.)"""
    from starlette.applications import Starlette
    from starlette.testclient import TestClient

    from boot_routes import build_routes

    fake_executor = MagicMock()
    app = Starlette(routes=build_routes(agent_card, fake_executor, None))
    client = TestClient(app)
    resp = client.post(
        "/", json={"jsonrpc": "2.0", "id": 1, "method": "message/send"}
    )
    body = resp.json() if resp.headers.get("content-type", "").startswith("application/json") else {}
    # Whatever DefaultRequestHandler does, it isn't the not-configured
    # envelope. The cheap discriminator: error.data won't say "setup() failed".
    err = body.get("error") or {}
    data = err.get("data") if isinstance(err, dict) else ""
    assert "setup() failed" not in (data or ""), (
        "executor-present branch must not mount the not-configured handler"
    )
