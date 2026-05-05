"""Drift gate + import-contract tests for ``a2a_tools_inbox`` (RFC #2873 iter 4e).

The full behavior matrix for the three inbox tool wrappers lives in
``test_a2a_tools_inbox_wrappers.py`` (kept on the public ``a2a_tools``
module so the same tests pin both the alias and the underlying impl).

This file pins:

  1. **Drift gate** — every previously-public symbol on ``a2a_tools``
     (``tool_inbox_peek``, ``tool_inbox_pop``, ``tool_wait_for_message``,
     ``_enrich_inbound_for_agent``, ``_INBOX_NOT_ENABLED_MSG``) is the
     EXACT same object as ``a2a_tools_inbox.foo``. Refactor wrapping
     silently loses existing test coverage; this gate makes that drift
     fail fast.
  2. **Import contract** — ``a2a_tools_inbox`` does NOT pull in
     ``a2a_tools`` at module-load time (the layered architecture: it
     depends only on stdlib + a lazy import of ``inbox`` + a lazy
     import of ``a2a_client``, never the kitchen-sink module that
     re-exports it).
  3. **_enrich_inbound_for_agent** branches that the wrapper tests
     can't easily reach: peer_id-empty (canvas_user) returns the
     dict unchanged; a2a_client unavailable degrades gracefully.
"""
from __future__ import annotations

import sys

import pytest


@pytest.fixture(autouse=True)
def _require_workspace_id(monkeypatch):
    monkeypatch.setenv("WORKSPACE_ID", "00000000-0000-0000-0000-000000000000")
    monkeypatch.setenv("PLATFORM_URL", "http://test.invalid")
    yield


# ============== Drift gate ==============

class TestBackCompatAliases:
    def test_tool_inbox_peek_alias(self):
        import a2a_tools
        import a2a_tools_inbox
        assert a2a_tools.tool_inbox_peek is a2a_tools_inbox.tool_inbox_peek

    def test_tool_inbox_pop_alias(self):
        import a2a_tools
        import a2a_tools_inbox
        assert a2a_tools.tool_inbox_pop is a2a_tools_inbox.tool_inbox_pop

    def test_tool_wait_for_message_alias(self):
        import a2a_tools
        import a2a_tools_inbox
        assert (
            a2a_tools.tool_wait_for_message is a2a_tools_inbox.tool_wait_for_message
        )

    def test_enrich_helper_alias(self):
        import a2a_tools
        import a2a_tools_inbox
        assert (
            a2a_tools._enrich_inbound_for_agent
            is a2a_tools_inbox._enrich_inbound_for_agent
        )

    def test_inbox_not_enabled_msg_alias(self):
        import a2a_tools
        import a2a_tools_inbox
        assert (
            a2a_tools._INBOX_NOT_ENABLED_MSG is a2a_tools_inbox._INBOX_NOT_ENABLED_MSG
        )


# ============== Import contract ==============

class TestImportContract:
    def test_inbox_module_does_not_import_a2a_tools_eagerly(self):
        # Force a fresh load of a2a_tools_inbox without a2a_tools in sight.
        for k in [k for k in list(sys.modules) if k in (
            "a2a_tools_inbox", "a2a_tools",
        )]:
            sys.modules.pop(k, None)
        import a2a_tools_inbox  # noqa: F401  — load only

        # a2a_tools_inbox MUST NOT have caused a2a_tools to load. The
        # extracted module sits BELOW the kitchen-sink in the layering;
        # the dependency arrow points the other direction.
        assert "a2a_tools" not in sys.modules, (
            "a2a_tools_inbox eagerly imported a2a_tools — the kitchen-sink "
            "module must not be a load-time dependency of its slices."
        )


# ============== _enrich_inbound_for_agent branches ==============

class TestEnrichInboundForAgent:
    def test_canvas_user_returns_dict_unchanged(self):
        # peer_id empty → canvas_user → no enrichment, no a2a_client touch.
        from a2a_tools_inbox import _enrich_inbound_for_agent

        msg = {"activity_id": "a-1", "kind": "canvas_user", "peer_id": ""}
        result = _enrich_inbound_for_agent(msg)
        assert result is msg  # same dict, mutated in place if at all
        assert "peer_name" not in result
        assert "peer_role" not in result
        assert "agent_card_url" not in result

    def test_missing_peer_id_key_returns_unchanged(self):
        from a2a_tools_inbox import _enrich_inbound_for_agent

        msg = {"activity_id": "a-2", "kind": "canvas_user"}  # no peer_id key
        result = _enrich_inbound_for_agent(msg)
        assert result is msg
        assert "agent_card_url" not in result

    def test_a2a_client_unavailable_degrades_gracefully(self, monkeypatch):
        # Simulate a2a_client import failing (test harness, partial
        # install). The helper must return the bare envelope, not raise.
        from a2a_tools_inbox import _enrich_inbound_for_agent

        # Force an ImportError by poisoning sys.modules.
        import builtins
        real_import = builtins.__import__

        def fake_import(name, *args, **kwargs):
            if name == "a2a_client":
                raise ImportError("simulated a2a_client unavailable")
            return real_import(name, *args, **kwargs)

        monkeypatch.setattr(builtins, "__import__", fake_import)

        msg = {"activity_id": "a-3", "kind": "peer_agent", "peer_id": "ws-x"}
        result = _enrich_inbound_for_agent(msg)
        # Bare envelope back — no peer_name, no agent_card_url. Crucially
        # the helper did NOT raise, so the inbox tool surfaces the message
        # to the agent even when the registry is unreachable.
        assert result is msg
        assert "peer_name" not in result
        assert "agent_card_url" not in result

    def test_registry_record_populates_peer_name_and_role(self, monkeypatch):
        from a2a_tools_inbox import _enrich_inbound_for_agent

        # Stub out the lazy-imported a2a_client functions.
        import sys
        import types
        fake_a2a_client = types.SimpleNamespace(
            _agent_card_url_for=lambda pid: f"http://test/agent/{pid}",
            enrich_peer_metadata_nonblocking=lambda pid: {
                "name": "PeerOne",
                "role": "worker",
            },
        )
        monkeypatch.setitem(sys.modules, "a2a_client", fake_a2a_client)

        msg = {"activity_id": "a-4", "kind": "peer_agent", "peer_id": "ws-1"}
        result = _enrich_inbound_for_agent(msg)
        assert result["peer_name"] == "PeerOne"
        assert result["peer_role"] == "worker"
        assert result["agent_card_url"] == "http://test/agent/ws-1"

    def test_registry_miss_keeps_agent_card_url(self, monkeypatch):
        # On registry cache miss the helper still surfaces agent_card_url
        # because it's constructable from peer_id alone — preserves the
        # contract that the receiving agent always has somewhere to
        # fetch the peer's full capability list.
        from a2a_tools_inbox import _enrich_inbound_for_agent

        import sys
        import types
        fake_a2a_client = types.SimpleNamespace(
            _agent_card_url_for=lambda pid: f"http://test/agent/{pid}",
            enrich_peer_metadata_nonblocking=lambda pid: None,  # cache miss
        )
        monkeypatch.setitem(sys.modules, "a2a_client", fake_a2a_client)

        msg = {"activity_id": "a-5", "kind": "peer_agent", "peer_id": "ws-2"}
        result = _enrich_inbound_for_agent(msg)
        assert "peer_name" not in result
        assert "peer_role" not in result
        assert result["agent_card_url"] == "http://test/agent/ws-2"
