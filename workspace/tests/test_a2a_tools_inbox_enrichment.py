"""Tests for `_enrich_inbound_for_agent` — the poll-path companion to
the push-path enrichment in `a2a_mcp_server._build_channel_notification`.

The MCP poll path (inbox_peek / wait_for_message) returns
`InboxMessage.to_dict()`, which has `activity_id, text, peer_id, kind,
method, created_at` but NOT the registry-resolved `peer_name`,
`peer_role`, or `agent_card_url`. The receiving agent then sees a
plain message and can't tell who's writing — breaking the universal
contract documented in `a2a_mcp_server.py:303-345` ("In both paths
the same fields apply").

The enrichment helper closes that gap. These tests pin:
  - canvas_user (peer_id="") passes through unchanged
  - peer_agent with cache hit gets peer_name + peer_role + agent_card_url
  - peer_agent with cache miss still gets agent_card_url (constructable
    from peer_id alone)
  - a2a_client unavailable (test harness without registry) degrades
    gracefully — agent still gets the bare envelope
"""

from __future__ import annotations

import os

# a2a_client.py reads WORKSPACE_ID at import time and raises if it's
# unset. Stamp a stub before any test pulls in a2a_tools (which transitively
# imports a2a_client). conftest.py mocks the SDK but not this env var.
os.environ.setdefault("WORKSPACE_ID", "00000000-0000-0000-0000-000000000001")

import sys
import types
from unittest.mock import patch


PEER_UUID = "11111111-2222-3333-4444-555555555555"


def test_canvas_user_passes_through_unchanged():
    from a2a_tools import _enrich_inbound_for_agent

    base = {
        "activity_id": "act-1",
        "text": "hello from canvas",
        "peer_id": "",
        "kind": "canvas_user",
        "method": "message/send",
        "created_at": "2026-05-05T11:00:00Z",
    }

    out = _enrich_inbound_for_agent(dict(base))

    # Plain pass-through — no enrichment fields added for canvas_user.
    assert out == base
    assert "peer_name" not in out
    assert "peer_role" not in out
    assert "agent_card_url" not in out


def test_peer_agent_cache_hit_adds_name_role_and_card_url():
    from a2a_tools import _enrich_inbound_for_agent

    record = {"name": "ops-agent", "role": "sre"}
    card_url = f"https://platform.example/registry/{PEER_UUID}/agent-card"

    with patch(
        "a2a_client.enrich_peer_metadata_nonblocking",
        return_value=record,
    ), patch(
        "a2a_client._agent_card_url_for",
        return_value=card_url,
    ):
        out = _enrich_inbound_for_agent({
            "activity_id": "act-2",
            "text": "ping",
            "peer_id": PEER_UUID,
            "kind": "peer_agent",
            "method": "message/send",
            "created_at": "2026-05-05T11:01:00Z",
        })

    assert out["peer_name"] == "ops-agent"
    assert out["peer_role"] == "sre"
    assert out["agent_card_url"] == card_url


def test_peer_agent_cache_miss_still_gets_agent_card_url():
    """agent_card_url is constructable from peer_id alone — surface it
    even when registry enrichment misses, so the receiving agent has a
    single endpoint to hit for the peer's full capability list."""
    from a2a_tools import _enrich_inbound_for_agent

    card_url = f"https://platform.example/registry/{PEER_UUID}/agent-card"

    with patch(
        "a2a_client.enrich_peer_metadata_nonblocking",
        return_value=None,  # cache miss
    ), patch(
        "a2a_client._agent_card_url_for",
        return_value=card_url,
    ):
        out = _enrich_inbound_for_agent({
            "activity_id": "act-3",
            "text": "ping",
            "peer_id": PEER_UUID,
            "kind": "peer_agent",
            "method": "message/send",
            "created_at": "2026-05-05T11:02:00Z",
        })

    assert "peer_name" not in out
    assert "peer_role" not in out
    assert out["agent_card_url"] == card_url


def test_peer_agent_a2a_client_unavailable_degrades_gracefully(monkeypatch):
    """If a2a_client can't be imported (test harness, partial install),
    return the bare envelope — agent still gets text + peer_id + kind +
    activity_id, just without the friendly identity."""
    from a2a_tools import _enrich_inbound_for_agent

    # Stub a2a_client import to fail.
    real_module = sys.modules.pop("a2a_client", None)
    fake = types.ModuleType("a2a_client")
    # Deliberately omit enrich_peer_metadata_nonblocking and
    # _agent_card_url_for so the helper's fallback path fires.
    sys.modules["a2a_client"] = fake

    try:
        out = _enrich_inbound_for_agent({
            "activity_id": "act-4",
            "text": "ping",
            "peer_id": PEER_UUID,
            "kind": "peer_agent",
            "method": "message/send",
            "created_at": "2026-05-05T11:03:00Z",
        })
    finally:
        if real_module is not None:
            sys.modules["a2a_client"] = real_module
        else:
            sys.modules.pop("a2a_client", None)

    # Bare envelope passes through — receiving agent still has enough
    # to act, even if the friendly identity is missing.
    assert out["peer_id"] == PEER_UUID
    assert out["text"] == "ping"
    assert out["kind"] == "peer_agent"
    assert "peer_name" not in out
    assert "peer_role" not in out
    assert "agent_card_url" not in out
