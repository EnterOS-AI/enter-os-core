"""Tests for a2a_mcp_server.py — handle_tool_call dispatch."""

import asyncio
import json
import os

from unittest.mock import AsyncMock, MagicMock, patch

import pytest


async def test_handle_tool_call_delegate_task():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_delegate_task", new=AsyncMock(return_value="delegated")):
        result = await handle_tool_call("delegate_task", {"workspace_id": "ws1", "task": "do work"})
    assert result == "delegated"


async def test_handle_tool_call_delegate_task_async():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_delegate_task_async", new=AsyncMock(return_value='{"task_id":"t1"}')):
        result = await handle_tool_call("delegate_task_async", {"workspace_id": "ws1", "task": "do work"})
    assert "t1" in result


async def test_handle_tool_call_check_task_status():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_check_task_status", new=AsyncMock(return_value='{"status":"working"}')):
        result = await handle_tool_call("check_task_status", {"workspace_id": "ws1", "task_id": "t123"})
    assert "working" in result


async def test_handle_tool_call_send_message_to_user():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_send_message_to_user", new=AsyncMock(return_value="Message sent to user")):
        result = await handle_tool_call("send_message_to_user", {"message": "Hello!"})
    assert result == "Message sent to user"


async def test_handle_tool_call_list_peers():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_list_peers", new=AsyncMock(return_value="- peer1 (ID: ws1)")):
        result = await handle_tool_call("list_peers", {})
    assert "peer1" in result


async def test_handle_tool_call_get_workspace_info():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_get_workspace_info", new=AsyncMock(return_value='{"id":"ws1"}')):
        result = await handle_tool_call("get_workspace_info", {})
    assert "ws1" in result


async def test_handle_tool_call_commit_memory():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_commit_memory", new=AsyncMock(return_value='{"success":true}')):
        result = await handle_tool_call("commit_memory", {"content": "remember this", "scope": "LOCAL"})
    assert "true" in result


async def test_handle_tool_call_recall_memory():
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_recall_memory", new=AsyncMock(return_value="[LOCAL] remember this")):
        result = await handle_tool_call("recall_memory", {"query": "remember", "scope": "LOCAL"})
    assert "remember" in result


async def test_handle_tool_call_unknown_tool():
    from a2a_mcp_server import handle_tool_call
    result = await handle_tool_call("nonexistent_tool", {})
    assert "Unknown tool" in result


async def test_handle_tool_call_missing_args_defaults():
    """Test that missing args default to empty strings (defensive)."""
    from a2a_mcp_server import handle_tool_call
    with patch("a2a_mcp_server.tool_delegate_task", new=AsyncMock(return_value="ok")):
        # No workspace_id or task in arguments — defaults to ""
        result = await handle_tool_call("delegate_task", {})
    assert result == "ok"


# ---------------------------------------------------------------------------
# Tool description steering — load-bearing prompts that train the LLM to
# use structured fields instead of pasting URLs in chat (task #118).
#
# Pin specific phrases so a future doc edit that softens or drops them
# fails this test. Production symptom of regression: agent pastes
# https://files.catbox.moe/... in the message body, canvas renders it as
# a plain text link the user can't click on a SaaS deployment where the
# external host is unreachable.
# ---------------------------------------------------------------------------


def _send_message_to_user_tool() -> dict:
    from a2a_mcp_server import TOOLS
    matches = [t for t in TOOLS if t["name"] == "send_message_to_user"]
    assert len(matches) == 1, "send_message_to_user not found in TOOLS"
    return matches[0]


def test_send_message_to_user_top_description_warns_against_pasting_urls():
    desc = _send_message_to_user_tool()["description"]
    # Combined: "NEVER paste file URLs in `message`" inside the tool-level
    # description. Without this the LLM frequently pastes URLs into the
    # message body and the canvas renders a plain markdown link.
    assert "NEVER paste file URLs" in desc, (
        "send_message_to_user top description must explicitly forbid pasting "
        "file URLs in `message`. Pre-#118 the description omitted this rule "
        "and agents routinely shipped catbox.moe / file:// links in chat."
    )


def test_message_param_description_says_DO_NOT_paste_URLs():
    desc = _send_message_to_user_tool()["inputSchema"]["properties"]["message"]["description"]
    # Caps lock matters — claude-code/hermes both responded better to the
    # all-caps version in informal testing during #118 prep. If a future
    # edit lowercases it, we lose that prompt-engineering signal.
    assert "DO NOT paste file URLs" in desc, (
        "`message` param description must include the all-caps DO NOT rule"
    )
    # SaaS reachability is the WHY — operators have asked for that
    # rationale to be explicit because external file hosts work in
    # self-hosted dev but break under SaaS where the user's browser
    # can't reach the agent's outbound network.
    assert "SaaS deployments" in desc, (
        "`message` param description must explain the SaaS reachability "
        "rationale, not just the rule"
    )


def test_attachments_param_description_emphasizes_REQUIRED():
    desc = _send_message_to_user_tool()["inputSchema"]["properties"]["attachments"]["description"]
    assert "REQUIRED for any file delivery" in desc, (
        "`attachments` description must lead with REQUIRED so the LLM picks "
        "this field instead of putting paths in `message`"
    )
    # Spell out the alternatives the agent should NOT use, so the LLM has
    # an explicit list of bad patterns to avoid (instead of relying on it
    # to infer).
    for forbidden in ("pasting URLs", "base64-encoding", "telling the user to look at a path"):
        assert forbidden in desc, (
            f"`attachments` description must call out {forbidden!r} as a wrong alternative"
        )


# ============== Inbox → MCP notification bridge (2026-05-01) ==============
# Notification-capable hosts (Claude Code) get push UX when a new inbound
# message lands; pollers (wait_for_message/inbox_peek) keep working.
# `_build_channel_notification` is the pure shape transformer — wire-up
# in main() composes it with asyncio.run_coroutine_threadsafe.


def test_build_channel_notification_method_matches_claude_contract():
    """Method MUST be `notifications/claude/channel` exactly — that's
    what Claude Code's MCP runtime listens for as a conversation
    interrupt. Same string as the bun channel bridge sends
    (server.ts:509) so this is a drop-in replacement."""
    from a2a_mcp_server import _build_channel_notification

    payload = _build_channel_notification({
        "activity_id": "act-1",
        "text": "hello",
        "peer_id": "",
        "kind": "canvas_user",
        "method": "message/send",
        "created_at": "2026-05-01T00:00:00Z",
    })

    assert payload["method"] == "notifications/claude/channel"
    assert payload["jsonrpc"] == "2.0"


def test_build_channel_notification_content_is_message_text():
    """`content` is what becomes the agent conversation turn —
    pulled directly from the inbox message text."""
    from a2a_mcp_server import _build_channel_notification

    payload = _build_channel_notification({
        "activity_id": "act-1",
        "text": "hello from canvas",
        "peer_id": "",
        "kind": "canvas_user",
        "method": "message/send",
        "created_at": "2026-05-01T00:00:00Z",
    })

    assert payload["params"]["content"] == "hello from canvas"


def test_build_channel_notification_meta_carries_routing_fields():
    """Meta must include kind, peer_id, method, activity_id, ts —
    fields the agent or downstream tooling needs to route a reply
    (canvas_user → /notify, peer_agent → /a2a) and to acknowledge
    via inbox_pop."""
    from a2a_mcp_server import _build_channel_notification

    payload = _build_channel_notification({
        "activity_id": "act-7",
        "text": "ping",
        "peer_id": "11111111-2222-3333-4444-555555555555",
        "kind": "peer_agent",
        "method": "message/send",
        "created_at": "2026-05-01T01:23:45Z",
    })
    meta = payload["params"]["meta"]

    assert meta["source"] == "molecule"
    assert meta["kind"] == "peer_agent"
    assert meta["peer_id"] == "11111111-2222-3333-4444-555555555555"
    assert meta["method"] == "message/send"
    assert meta["activity_id"] == "act-7"
    assert meta["ts"] == "2026-05-01T01:23:45Z"


def test_build_channel_notification_no_id_field():
    """Notifications MUST NOT carry a JSON-RPC `id` field — that's
    what distinguishes them from requests. A notification with `id`
    would be mis-interpreted as a request and clients would wait
    for a response that never comes."""
    from a2a_mcp_server import _build_channel_notification

    payload = _build_channel_notification({"text": "x"})

    assert "id" not in payload, (
        "notifications must omit `id` per JSON-RPC 2.0 spec — "
        "presence would make MCP clients await a phantom response"
    )


def test_build_channel_notification_handles_missing_fields_gracefully():
    """Some fields may be absent on edge-case messages (e.g. cursor
    bootstrapping with no created_at yet). Default to empty strings
    so the wire shape stays valid JSON instead of crashing."""
    from a2a_mcp_server import _build_channel_notification

    payload = _build_channel_notification({})

    assert payload["params"]["content"] == ""
    meta = payload["params"]["meta"]
    assert meta["activity_id"] == ""
    assert meta["peer_id"] == ""
    assert meta["kind"] == ""


# ----- Channel envelope enrichment (peer_name / peer_role / agent_card_url) ---
#
# The bare envelope only carries `peer_id` for peer_agent inbound, so the
# receiving agent has to round-trip to /registry to find out who's
# talking. Enrichment surfaces the sender's display name, role, and an
# agent-card URL alongside the routing fields so the agent can render
# "ops-agent (sre): hi" in one shot. Cache-backed and TTL'd so a busy
# multi-peer chat doesn't hit the registry on every push.
#
# Tests pin: cache hit, cache miss + registry hit, registry miss
# (graceful degrade), TTL expiry, canvas_user (no enrichment), and the
# agent_card_url surfaces even when the registry is reachable but
# returns nothing usable.


_PEER_UUID = "11111111-2222-3333-4444-555555555555"


@pytest.fixture()
def _reset_peer_metadata_cache(monkeypatch):
    """Each test starts with a clean ``_peer_metadata`` cache so an
    earlier test's hit doesn't satisfy a later test's miss. Mutates the
    module-level dict in place rather than reassigning so other modules
    that imported the dict by reference still see the same instance."""
    import a2a_client
    a2a_client._peer_metadata.clear()
    yield
    a2a_client._peer_metadata.clear()


def _make_httpx_response(status_code: int, json_body: object) -> MagicMock:
    resp = MagicMock()
    resp.status_code = status_code
    resp.json.return_value = json_body
    return resp


def _patch_httpx_client(returning: MagicMock):
    """Replace httpx.Client with a context-manager mock returning
    ``returning`` from .get(). Mirrors the inbox tests' pattern so a
    future refactor of the registry GET path can be re-tested with the
    same harness."""
    client = MagicMock()
    client.__enter__ = MagicMock(return_value=client)
    client.__exit__ = MagicMock(return_value=False)
    client.get = MagicMock(return_value=returning)
    return patch("httpx.Client", return_value=client), client


def test_envelope_enrichment_canvas_user_has_no_peer_fields(_reset_peer_metadata_cache):
    """canvas_user pushes have no peer (peer_id=''). The enrichment
    block must short-circuit so we don't fire a wasted registry GET +
    don't add empty peer_name/role/agent_card_url to the meta dict."""
    from a2a_mcp_server import _build_channel_notification

    payload = _build_channel_notification({
        "activity_id": "act-1",
        "text": "hello from canvas",
        "peer_id": "",
        "kind": "canvas_user",
        "method": "message/send",
        "created_at": "2026-05-01T00:00:00Z",
    })
    meta = payload["params"]["meta"]
    assert "peer_name" not in meta
    assert "peer_role" not in meta
    assert "agent_card_url" not in meta


def test_envelope_enrichment_uses_cache_when_present(_reset_peer_metadata_cache):
    """Cache hit: registry NOT called, meta carries the cached fields.
    This is the hot path on a busy multi-peer chat — every cache hit
    saves a 2-second timeout-bounded registry GET."""
    import a2a_client
    from a2a_mcp_server import _build_channel_notification
    import time as _time

    a2a_client._peer_metadata[_PEER_UUID] = (
        _time.monotonic(),
        {"id": _PEER_UUID, "name": "ops-agent", "role": "sre", "status": "online"},
    )

    p, client = _patch_httpx_client(_make_httpx_response(200, {}))
    with p:
        payload = _build_channel_notification({
            "activity_id": "act-2",
            "text": "ping",
            "peer_id": _PEER_UUID,
            "kind": "peer_agent",
            "method": "message/send",
            "created_at": "2026-05-01T01:23:45Z",
        })

    assert client.get.call_count == 0, "cache hit must not fire a registry GET"
    meta = payload["params"]["meta"]
    assert meta["peer_id"] == _PEER_UUID
    assert meta["peer_name"] == "ops-agent"
    assert meta["peer_role"] == "sre"
    assert meta["agent_card_url"].endswith(f"/registry/discover/{_PEER_UUID}")


def test_envelope_enrichment_fetches_on_cache_miss(_reset_peer_metadata_cache):
    """Cache miss + registry hit: GET fires, response cached, meta
    carries fetched fields. Subsequent build for the same peer must
    NOT re-fetch (cache populated by first call)."""
    import a2a_client
    from a2a_mcp_server import _build_channel_notification

    p, client = _patch_httpx_client(
        _make_httpx_response(
            200,
            {"id": _PEER_UUID, "name": "fetched-name", "role": "router", "status": "online"},
        )
    )
    with p:
        payload1 = _build_channel_notification({
            "peer_id": _PEER_UUID, "kind": "peer_agent", "text": "first",
        })
        payload2 = _build_channel_notification({
            "peer_id": _PEER_UUID, "kind": "peer_agent", "text": "second",
        })

    assert client.get.call_count == 1, (
        f"second push for same peer must use cache, got {client.get.call_count} GETs"
    )
    assert payload1["params"]["meta"]["peer_name"] == "fetched-name"
    assert payload2["params"]["meta"]["peer_name"] == "fetched-name"


def test_envelope_enrichment_degrades_on_registry_failure(_reset_peer_metadata_cache):
    """Registry returns 500 (or 4xx, or network error): enrichment
    silently degrades to bare peer_id. The push must not crash, the
    push must not block, and the agent_card_url must still surface
    because it's constructable from peer_id alone."""
    from a2a_mcp_server import _build_channel_notification

    p, _ = _patch_httpx_client(_make_httpx_response(500, {}))
    with p:
        payload = _build_channel_notification({
            "activity_id": "act-3",
            "text": "ping",
            "peer_id": _PEER_UUID,
            "kind": "peer_agent",
            "method": "message/send",
            "created_at": "2026-05-01T00:00:00Z",
        })

    meta = payload["params"]["meta"]
    assert meta["peer_id"] == _PEER_UUID
    assert "peer_name" not in meta
    assert "peer_role" not in meta
    assert meta["agent_card_url"].endswith(f"/registry/discover/{_PEER_UUID}"), (
        "agent_card_url must be present even on registry failure — "
        "it's deterministic from peer_id and gives the agent a single "
        "endpoint to retry against"
    )


def test_envelope_enrichment_negative_caches_registry_failure(_reset_peer_metadata_cache):
    """Registry failure must be cached for the TTL window. Without
    this, a peer with a flaky or missing registry record re-fires the
    2s-bounded GET on EVERY push — the cache becomes a no-op for the
    exact scenarios it most needs to defend against, and the poller
    thread stalls 2s per push for that peer until the registry comes
    back. Pin: two pushes from a 5xx-returning peer fire exactly one
    GET, not two."""
    from a2a_mcp_server import _build_channel_notification

    p, client = _patch_httpx_client(_make_httpx_response(500, {}))
    with p:
        payload1 = _build_channel_notification({
            "peer_id": _PEER_UUID, "kind": "peer_agent", "text": "first",
        })
        payload2 = _build_channel_notification({
            "peer_id": _PEER_UUID, "kind": "peer_agent", "text": "second",
        })

    assert client.get.call_count == 1, (
        f"second push from a 5xx-returning peer must use the negative "
        f"cache, got {client.get.call_count} GETs"
    )
    # Both pushes deliver without enrichment (peer_name/role absent),
    # but agent_card_url surfaces unconditionally.
    for payload in (payload1, payload2):
        meta = payload["params"]["meta"]
        assert "peer_name" not in meta
        assert "peer_role" not in meta
        assert meta["agent_card_url"].endswith(f"/registry/discover/{_PEER_UUID}")


def test_envelope_enrichment_negative_caches_network_exception(_reset_peer_metadata_cache):
    """Same negative-caching contract for network exceptions —
    httpx.ConnectError, DNS failure, registry pod restart all
    surface as exceptions from client.get(). Without negative
    caching, a temporary network blip turns into a 2s stall on
    every push for the duration."""
    import a2a_client
    from a2a_mcp_server import _build_channel_notification

    client = MagicMock()
    client.__enter__ = MagicMock(return_value=client)
    client.__exit__ = MagicMock(return_value=False)
    # Important: simulate the exception INSIDE the with-block (which
    # is where the real httpx.Client raises) by making get() raise.
    import httpx as _httpx
    client.get = MagicMock(side_effect=_httpx.ConnectError("dns down"))
    with patch("httpx.Client", return_value=client):
        _build_channel_notification({"peer_id": _PEER_UUID, "kind": "peer_agent"})
        _build_channel_notification({"peer_id": _PEER_UUID, "kind": "peer_agent"})

    assert client.get.call_count == 1, (
        f"network exceptions must be negative-cached, got "
        f"{client.get.call_count} GETs"
    )
    # Sanity: the cache entry exists and carries None as the record.
    cached = a2a_client._peer_metadata[_PEER_UUID]
    assert cached[1] is None


def test_envelope_enrichment_negative_caches_non_json_200(_reset_peer_metadata_cache):
    """HTTP 200 but the body isn't JSON (registry returns HTML, an empty
    string, or a partial response): ``response.json()`` raises. The
    enrichment block must absorb the exception, write the negative-cache
    entry, and never re-fetch this peer until TTL elapses.

    Without this contract a registry that mistakenly returns a non-JSON
    200 (proxy injecting an HTML error page; partial response from a
    flapping pod) would re-fire the 2s-bounded GET on every push for
    that peer — same DoS-on-self pattern the 5xx negative-cache test
    pins. #2483.
    """
    import json as _json

    import a2a_client
    from a2a_mcp_server import _build_channel_notification

    # 200 OK shape but .json() raises. side_effect overrides the
    # _make_httpx_response default of `return_value` so the helper can
    # stay shape-stable for callers that DO want a JSON body.
    resp = _make_httpx_response(200, {})
    resp.json.side_effect = _json.JSONDecodeError("not json", "<html>", 0)
    p, client = _patch_httpx_client(resp)
    with p:
        _build_channel_notification({"peer_id": _PEER_UUID, "kind": "peer_agent", "text": "first"})
        _build_channel_notification({"peer_id": _PEER_UUID, "kind": "peer_agent", "text": "second"})

    assert client.get.call_count == 1, (
        f"non-JSON 200 must be negative-cached, got {client.get.call_count} GETs"
    )
    cached = a2a_client._peer_metadata[_PEER_UUID]
    assert cached[1] is None, "negative cache stores None as the record"


def test_envelope_enrichment_negative_caches_non_dict_json_200(_reset_peer_metadata_cache):
    """HTTP 200, valid JSON, but the body is a list / string / number /
    null instead of the expected dict. ``isinstance(record, dict)``
    skips enrichment but the call must still write to the negative
    cache so a second push doesn't re-fetch.

    Pins behaviour for a registry that mistakenly returns
    ``[{"id": ...}, ...]`` (collection shape) or just ``null`` (no-record
    sentinel) — both should land at the same negative-cache outcome as a
    5xx or a non-JSON 200. #2483.
    """
    import a2a_client
    from a2a_mcp_server import _build_channel_notification

    p, client = _patch_httpx_client(
        _make_httpx_response(200, ["not", "a", "dict"]),
    )
    with p:
        _build_channel_notification({"peer_id": _PEER_UUID, "kind": "peer_agent", "text": "first"})
        _build_channel_notification({"peer_id": _PEER_UUID, "kind": "peer_agent", "text": "second"})

    assert client.get.call_count == 1, (
        f"non-dict JSON 200 must be negative-cached, got {client.get.call_count} GETs"
    )
    cached = a2a_client._peer_metadata[_PEER_UUID]
    assert cached[1] is None, "negative cache stores None as the record"


def test_envelope_enrichment_re_fetches_after_ttl(_reset_peer_metadata_cache):
    """Cached entry past TTL: registry is hit again. Pin the TTL
    behaviour so a future caller bumping ``_PEER_METADATA_TTL_SECONDS``
    doesn't accidentally make the cache permanent."""
    import time

    import a2a_client
    from a2a_mcp_server import _build_channel_notification

    # Stale entry: anchored to *current* monotonic time minus TTL+slack
    # so the entry is unambiguously past the freshness window. A naked
    # `0.0` looked stale relative to wall-clock but `time.monotonic()`
    # starts at process uptime — when this test ran early in the pytest
    # run, current was <300s and the entry was treated as fresh,
    # silently skipping the re-fetch the assertion expects.
    a2a_client._peer_metadata[_PEER_UUID] = (
        time.monotonic() - a2a_client._PEER_METADATA_TTL_SECONDS - 60.0,
        {"id": _PEER_UUID, "name": "stale-name", "role": "old"},
    )

    p, client = _patch_httpx_client(
        _make_httpx_response(
            200,
            {"id": _PEER_UUID, "name": "fresh-name", "role": "new", "status": "online"},
        )
    )
    with p:
        payload = _build_channel_notification({
            "peer_id": _PEER_UUID, "kind": "peer_agent", "text": "ping",
        })

    assert client.get.call_count == 1, "stale cache must trigger a re-fetch"
    assert payload["params"]["meta"]["peer_name"] == "fresh-name"
    assert payload["params"]["meta"]["peer_role"] == "new"


def test_envelope_enrichment_invalid_peer_id_skips_lookup(_reset_peer_metadata_cache):
    """Defensive: a malformed peer_id (not a UUID) must not crash the
    push path, must not fire a registry GET against an unsanitised URL,
    and must not reflect the raw input back into either the envelope
    `peer_id` field or the `agent_card_url`. UUID validation is a hard
    trust boundary — the envelope's job is to surface metadata about
    *trusted* peers, never to launder attacker-controlled bytes through
    the JSON-RPC notification into the agent's rendered context."""
    from a2a_mcp_server import _build_channel_notification

    p, client = _patch_httpx_client(_make_httpx_response(200, {}))
    with p:
        payload = _build_channel_notification({
            "peer_id": "not-a-uuid",
            "kind": "peer_agent",
            "text": "evil",
        })

    assert client.get.call_count == 0, (
        "invalid peer_id must not reach a network call — UUID validation "
        "guards the URL-construction surface"
    )
    meta = payload["params"]["meta"]
    # peer_id echo is canonicalised to empty-string on validation failure,
    # so attacker bytes never reach the agent's <channel peer_id="..."> attr.
    assert meta["peer_id"] == ""
    assert "peer_name" not in meta
    assert "peer_role" not in meta
    # agent_card_url is omitted entirely rather than constructed against
    # the unsanitised id — receiving agent gracefully degrades to
    # inbox_pop without any URL to hit.
    assert "agent_card_url" not in meta


def test_envelope_enrichment_strips_path_traversal_peer_id(_reset_peer_metadata_cache):
    """Hard regression for the trust-boundary issue surfaced in code review:
    a peer_id containing path-traversal characters MUST NOT be interpolated
    into the registry URL or echoed into the envelope. ``_agent_card_url_for``
    builds against ``${PLATFORM_URL}/registry/discover/<peer_id>`` — without
    the UUID guard, an upstream row with peer_id=``../../foo`` produces an
    agent-visible URL pointing at a sibling path, and the receiving agent
    would fetch from the wrong endpoint or the operator's reverse proxy
    would normalise it into something unintended."""
    from a2a_mcp_server import _build_channel_notification

    p, client = _patch_httpx_client(_make_httpx_response(200, {}))
    with p:
        payload = _build_channel_notification({
            "peer_id": "../../foo",
            "kind": "peer_agent",
            "text": "redirect-attempt",
        })

    assert client.get.call_count == 0
    meta = payload["params"]["meta"]
    assert meta["peer_id"] == ""
    assert "agent_card_url" not in meta, (
        "path-traversal peer_id leaked into agent_card_url — "
        "_agent_card_url_for must call _validate_peer_id"
    )


# ============== initialize handshake — capability declaration ==============
# Without `experimental.claude/channel`, Claude Code's MCP client drops
# our notifications/claude/channel emissions instead of routing them as
# inline conversation interrupts. Anticipated as a failure mode in
# molecule-core#2444 ("notification arrives but Claude Code doesn't
# surface it"). Pin the declaration here so a refactor of
# _build_initialize_result can't silently strip the flag.


def test_initialize_declares_experimental_claude_channel_capability():
    """Without this capability the push-UX bridge ships, the
    notifications fire, and nothing happens in the host — silent. This
    is the contract that flips Claude Code's routing on."""
    from a2a_mcp_server import _build_initialize_result

    result = _build_initialize_result()
    experimental = result["capabilities"].get("experimental", {})

    assert "claude/channel" in experimental, (
        "experimental.claude/channel capability is required for Claude "
        "Code to surface our notifications/claude/channel emissions as "
        "conversation interrupts (issue #2444 §2). Removing this would "
        "regress live push UX while leaving every unit test green."
    )


def test_initialize_keeps_tools_capability():
    """Pin the tools capability too — losing it would break tools/list."""
    from a2a_mcp_server import _build_initialize_result

    assert "tools" in _build_initialize_result()["capabilities"]


def test_initialize_protocol_version_is_pinned():
    """MCP protocol version is part of the handshake contract; bumping
    it changes what fields the host expects."""
    from a2a_mcp_server import _build_initialize_result

    assert _build_initialize_result()["protocolVersion"] == "2024-11-05"


def test_initialize_declares_instructions():
    """Per code.claude.com/docs/en/channels-reference, the
    `instructions` field is required for Claude Code to actually surface
    `<channel>` tags. Capability declaration alone is not enough — the
    agent has to know what the tag means and how to reply. Without
    instructions the channel is registered but unusable."""
    from a2a_mcp_server import _build_initialize_result

    instructions = _build_initialize_result().get("instructions", "")
    assert instructions, (
        "instructions field must be non-empty for the channel to be "
        "usable (channels-reference.md). Empty string ships the wire "
        "shape without the agent knowing what to do with the tag."
    )


def test_initialize_instructions_documents_reply_tools():
    """The instructions string is what the agent reads to decide which
    tool to call when a <channel> tag arrives. Pin the routing rules
    so a copy-edit can't silently break them."""
    from a2a_mcp_server import _build_initialize_result

    instructions = _build_initialize_result()["instructions"]

    assert "send_message_to_user" in instructions, (
        "canvas_user → send_message_to_user is the documented reply "
        "path; instructions must name the tool"
    )
    assert "delegate_task" in instructions, (
        "peer_agent → delegate_task is the documented reply path; "
        "instructions must name the tool"
    )
    assert "inbox_pop" in instructions, (
        "instructions must tell the agent to ack via inbox_pop or "
        "duplicate-poll deliveries are a footgun"
    )


def test_initialize_instructions_documents_meta_attributes():
    """The instructions must explain what the meta-derived tag
    attributes mean — kind, peer_id, activity_id — so the agent can
    correctly route the reply."""
    from a2a_mcp_server import _build_initialize_result

    instructions = _build_initialize_result()["instructions"]

    for required_attr in ("kind", "peer_id", "activity_id"):
        assert required_attr in instructions, (
            f"instructions must document the `{required_attr}` tag "
            f"attribute for the agent to act on it"
        )


def test_initialize_instructions_documents_universal_poll_path():
    """The polling contract is what makes inbound delivery universal —
    every spec-compliant MCP client surfaces ``instructions`` to the
    agent, so an instruction telling the agent to call
    ``wait_for_message`` at every turn reaches Claude Code, Cursor,
    Cline, opencode, hermes-agent, and codex alike.

    Without this clause the wheel silently regresses to push-only
    delivery, which only works on Claude Code with the dev-channels
    flag — exactly the failure mode that bit live use 2026-05-01
    (canvas message stuck in inbox, never reached the agent).

    Pin the tool name AND the timeout-secs param so a copy-edit that
    drops one half can't keep the surface but break the contract.
    """
    from a2a_mcp_server import _build_initialize_result

    instructions = _build_initialize_result()["instructions"]

    assert "wait_for_message" in instructions, (
        "instructions must name `wait_for_message` as the universal "
        "poll path so non-Claude-Code clients (Cursor, Cline, "
        "opencode, hermes-agent, codex) and unflagged Claude Code "
        "actually receive inbound messages instead of silently "
        "stalling"
    )
    assert "timeout_secs" in instructions, (
        "instructions must reference the timeout_secs parameter so "
        "the agent calls wait_for_message with the operator-tunable "
        "blocking window — without it the agent might pass 0 and "
        "polling becomes a no-op"
    )


def test_initialize_instructions_calls_out_dual_paths():
    """Push and poll co-exist intentionally (push promotes to
    zero-stall delivery on capable hosts; poll is the universal
    floor). Pin both labels so a future "simplification" that picks
    one path can't ship green — that change must reach review."""
    from a2a_mcp_server import _build_initialize_result

    instructions = _build_initialize_result()["instructions"]
    upper = instructions.upper()

    assert "PUSH PATH" in upper, (
        "instructions must explicitly label the PUSH PATH — Claude "
        "Code channel users need to know <channel> tags are how "
        "messages reach them, distinct from the poll path"
    )
    assert "POLL PATH" in upper, (
        "instructions must explicitly label the POLL PATH — every "
        "non-Claude-Code client (and unflagged Claude Code) reads "
        "this section to know wait_for_message is the universal "
        "delivery mechanism"
    )


def test_poll_timeout_resolution_clamps_and_falls_back():
    """The env knob must accept positive ints, fall back gracefully
    on bad input, and clamp to a sane upper bound — operator config
    should never break the initialize handshake."""
    import os

    from a2a_mcp_server import _DEFAULT_POLL_TIMEOUT_SECS, _poll_timeout_secs

    saved = os.environ.pop("MOLECULE_MCP_POLL_TIMEOUT_SECS", None)
    try:
        # Default when unset
        assert _poll_timeout_secs() == _DEFAULT_POLL_TIMEOUT_SECS

        # Operator override
        os.environ["MOLECULE_MCP_POLL_TIMEOUT_SECS"] = "5"
        assert _poll_timeout_secs() == 5

        # 0 disables polling (push-only mode for flagged Claude Code)
        os.environ["MOLECULE_MCP_POLL_TIMEOUT_SECS"] = "0"
        assert _poll_timeout_secs() == 0

        # Garbage falls back to default
        os.environ["MOLECULE_MCP_POLL_TIMEOUT_SECS"] = "not-a-number"
        assert _poll_timeout_secs() == _DEFAULT_POLL_TIMEOUT_SECS

        # Negative falls back (treated as malformed)
        os.environ["MOLECULE_MCP_POLL_TIMEOUT_SECS"] = "-3"
        assert _poll_timeout_secs() == _DEFAULT_POLL_TIMEOUT_SECS

        # Above 60 clamps to 60 — protects against an operator
        # accidentally turning every agent turn into a 5-minute stall
        os.environ["MOLECULE_MCP_POLL_TIMEOUT_SECS"] = "300"
        assert _poll_timeout_secs() == 60
    finally:
        os.environ.pop("MOLECULE_MCP_POLL_TIMEOUT_SECS", None)
        if saved is not None:
            os.environ["MOLECULE_MCP_POLL_TIMEOUT_SECS"] = saved


def test_instructions_substitute_operator_timeout():
    """When the operator sets MOLECULE_MCP_POLL_TIMEOUT_SECS, the
    value reaches the agent — instructions are built per-call so a
    relaunch with new env is enough; no wheel rebuild needed."""
    import os

    from a2a_mcp_server import _build_initialize_result

    saved = os.environ.pop("MOLECULE_MCP_POLL_TIMEOUT_SECS", None)
    try:
        os.environ["MOLECULE_MCP_POLL_TIMEOUT_SECS"] = "7"
        instructions = _build_initialize_result()["instructions"]
        assert "timeout_secs=7" in instructions, (
            "operator override of MOLECULE_MCP_POLL_TIMEOUT_SECS must "
            "appear in the instructions string — otherwise the agent "
            "polls with a stale value and the env knob does nothing"
        )
    finally:
        os.environ.pop("MOLECULE_MCP_POLL_TIMEOUT_SECS", None)
        if saved is not None:
            os.environ["MOLECULE_MCP_POLL_TIMEOUT_SECS"] = saved


def test_instructions_zero_timeout_means_push_only_mode():
    """Setting MOLECULE_MCP_POLL_TIMEOUT_SECS=0 is the explicit
    operator gesture for "I'm running flagged Claude Code; don't
    waste cycles polling." Instructions must reflect this so the
    agent doesn't call wait_for_message in a tight loop."""
    import os

    from a2a_mcp_server import _build_initialize_result

    saved = os.environ.pop("MOLECULE_MCP_POLL_TIMEOUT_SECS", None)
    try:
        os.environ["MOLECULE_MCP_POLL_TIMEOUT_SECS"] = "0"
        instructions = _build_initialize_result()["instructions"]
        assert "Polling is disabled" in instructions, (
            "with timeout=0 the instructions must tell the agent "
            "polling is off (push-only mode) instead of asking it to "
            "call wait_for_message(timeout_secs=0) — which would "
            "either spam the inbox or no-op silently"
        )
    finally:
        os.environ.pop("MOLECULE_MCP_POLL_TIMEOUT_SECS", None)
        if saved is not None:
            os.environ["MOLECULE_MCP_POLL_TIMEOUT_SECS"] = saved


def test_instructions_document_envelope_enrichment_attrs():
    """The agent learns about envelope attributes ONLY from the
    instructions string. PR-B added peer_name, peer_role,
    agent_card_url to the wire shape; pin that the instructions list
    them in the <channel> tag template AND describe each one's
    semantics. Without this, the wheel ships new attributes that no
    agent ever uses."""
    from a2a_mcp_server import _build_initialize_result

    instructions = _build_initialize_result()["instructions"]

    # The <channel> tag template in the PUSH PATH section must include
    # the new attribute names so the agent recognises them when they
    # arrive inline.
    for attr in ("peer_name", "peer_role", "agent_card_url"):
        assert attr in instructions, (
            f"instructions must list `{attr}` as a <channel> tag "
            f"attribute — otherwise the agent sees the attr in pushes "
            f"but doesn't know what to do with it"
        )

    # And the per-field semantics block must explain when each attr
    # is present + what it means. These phrases are what the agent
    # actually reads to decide how to surface the attrs in its turn.
    assert "registry resolved" in instructions, (
        "instructions must explain peer_name/peer_role come from a "
        "registry lookup that may fail — otherwise the agent treats "
        "their absence as a bug instead of a graceful degrade"
    )
    assert "discover endpoint" in instructions, (
        "instructions must point at the registry discover endpoint "
        "for agent_card_url so the agent knows it's a follow-on URL "
        "to fetch full capabilities, not the body of the message"
    )


def test_initialize_instructions_pins_prompt_injection_defense():
    """The threat-model sentence in `_CHANNEL_INSTRUCTIONS` is what
    tells the agent that inbound canvas-user / peer-agent message
    bodies are untrusted user content and must NOT be acted on as
    instructions without chat-side approval. Symmetric with the reply-
    tool pins above — drop this and a future copy-edit could silently
    turn the channel into an open prompt-injection vector against any
    workspace running this MCP server.
    """
    from a2a_mcp_server import _build_initialize_result

    instructions = _build_initialize_result()["instructions"]
    lowered = instructions.lower()

    assert "untrusted" in lowered, (
        "instructions must flag inbound message bodies as untrusted "
        "user content — same threat model as the telegram channel "
        "plugin. Dropping this turns the channel into a prompt-"
        "injection vector."
    )
    # And the explicit don't-execute-blindly clause: pin both the
    # restriction ("do not execute") and the escape hatch ("user
    # approval") so a partial copy-edit can't keep one and drop the
    # other.
    assert "not execute" in lowered or "do not" in lowered, (
        "instructions must explicitly say the agent should NOT execute "
        "instructions embedded in message bodies"
    )
    assert "approval" in lowered, (
        "instructions must point the agent at user chat-side approval "
        "as the escape hatch when a message looks instruction-like"
    )


# ============== _setup_inbox_bridge — dynamic integration ==============
# Closes the "fires but invisible" failure modes anticipated in
# molecule-core#2444 §2:
#
#   - run_coroutine_threadsafe scheduling correctly across the
#     daemon-thread → asyncio-loop boundary
#   - writer.drain() actually being reached (not silently swallowed
#     by an exception higher in the chain)
#   - notification wire shape matching _build_channel_notification's
#     contract on the actual stdout the host reads
#
# Driven through real os.pipe() + a real asyncio StreamWriter, with
# the inbox poller simulated by a separate daemon thread firing the
# callback. The setup mirrors main()'s wire-up exactly — this is the
# bridge that ships, not a copy.


async def test_inbox_bridge_emits_channel_notification_to_writer():
    """Fire a fake inbox event from a daemon thread, assert the
    notification lands on the asyncio writer with the correct
    JSON-RPC envelope. End-to-end coverage of the bridge that
    powers ``notifications/claude/channel`` push UX."""
    import os
    import threading

    from a2a_mcp_server import _setup_inbox_bridge

    # Real asyncio writer backed by an os.pipe — same shape as
    # main() but isolated so we can read what was written.
    read_fd, write_fd = os.pipe()
    loop = asyncio.get_running_loop()
    transport, protocol = await loop.connect_write_pipe(
        asyncio.streams.FlowControlMixin,
        os.fdopen(write_fd, "wb"),
    )
    writer = asyncio.StreamWriter(transport, protocol, None, loop)

    try:
        cb = _setup_inbox_bridge(writer, loop)

        msg = {
            "activity_id": "act-bridge-test",
            "text": "hello from peer",
            "peer_id": "11111111-2222-3333-4444-555555555555",
            "kind": "peer_agent",
            "method": "message/send",
            "created_at": "2026-05-01T22:00:00Z",
        }

        # Simulate the inbox poller daemon thread invoking the
        # callback from a non-asyncio context — exactly the
        # threading boundary the bridge has to cross.
        threading.Thread(target=cb, args=(msg,), daemon=True).start()

        # Give the scheduled coroutine a chance to run + drain
        # without coupling the test to wall-clock timing.
        for _ in range(20):
            await asyncio.sleep(0.05)
            data = os.read(read_fd, 65536) if _readable(read_fd) else b""
            if data:
                break
        else:
            data = b""

        assert data, (
            "no notification on stdout pipe — the bridge fired "
            "but the write didn't reach the writer (writer.drain "
            "swallowing or scheduling race)"
        )
        line = data.decode().strip()
        payload = json.loads(line)

        assert payload["jsonrpc"] == "2.0"
        assert payload["method"] == "notifications/claude/channel"
        assert payload["params"]["content"] == "hello from peer"
        meta = payload["params"]["meta"]
        assert meta["source"] == "molecule"
        assert meta["kind"] == "peer_agent"
        assert meta["peer_id"] == "11111111-2222-3333-4444-555555555555"
        assert meta["activity_id"] == "act-bridge-test"
        assert meta["ts"] == "2026-05-01T22:00:00Z"
    finally:
        writer.close()
        try:
            os.close(read_fd)
        except OSError:
            # read_fd may already be closed if writer.close() tore down the pair
            # during teardown — best-effort cleanup, no signal worth surfacing.
            pass


async def test_inbox_bridge_swallows_closed_pipe_drain_error(monkeypatch):
    """If the host disconnects mid-emission, ``writer.drain()`` raises
    on the closed pipe. The drain runs inside the coroutine scheduled
    by ``run_coroutine_threadsafe`` — that returns a
    ``concurrent.futures.Future`` whose ``.exception()`` reflects what
    the coroutine's final state was. The broad ``except Exception`` in
    ``_emit`` is what keeps that future in a successful (None) state
    instead of carrying the ``BrokenPipeError``.

    We capture the scheduled future and assert it completed cleanly.
    Narrowing the swallow (e.g. to ``except RuntimeError``) or
    removing it turns this red because the BrokenPipeError surfaces
    on the future.
    """
    import os
    from concurrent.futures import Future as ConcurrentFuture

    from a2a_mcp_server import _setup_inbox_bridge

    read_fd, write_fd = os.pipe()
    loop = asyncio.get_running_loop()
    transport, protocol = await loop.connect_write_pipe(
        asyncio.streams.FlowControlMixin,
        os.fdopen(write_fd, "wb"),
    )
    writer = asyncio.StreamWriter(transport, protocol, None, loop)

    # Close the read end so the next drain raises BrokenPipeError.
    os.close(read_fd)

    scheduled: list[ConcurrentFuture] = []
    real_run_threadsafe = asyncio.run_coroutine_threadsafe

    def _capture(coro, target_loop):
        fut = real_run_threadsafe(coro, target_loop)
        scheduled.append(fut)
        return fut

    monkeypatch.setattr(asyncio, "run_coroutine_threadsafe", _capture)

    try:
        cb = _setup_inbox_bridge(writer, loop)

        cb({
            "activity_id": "act-drain-fail",
            "text": "x",
            "peer_id": "",
            "kind": "canvas_user",
            "method": "",
            "created_at": "",
        })

        # Yield until the scheduled coroutine settles — drain raises
        # internally and (with swallow) returns None.
        deadline_ticks = 40
        while deadline_ticks > 0 and (not scheduled or not scheduled[0].done()):
            await asyncio.sleep(0.05)
            deadline_ticks -= 1
    finally:
        writer.close()

    assert scheduled, "_setup_inbox_bridge didn't call run_coroutine_threadsafe"
    fut = scheduled[0]
    assert fut.done(), "scheduled coroutine never finished — bridge hung on closed pipe"
    exc = fut.exception(timeout=0)
    assert exc is None, (
        f"_emit propagated {exc!r} from a closed-pipe drain. The broad "
        f"`except Exception` in `_emit` is what keeps this future "
        f"clean — narrowing it (to RuntimeError) or removing it "
        f"regresses this test."
    )


@pytest.mark.filterwarnings("ignore::RuntimeWarning")
def test_inbox_bridge_swallows_closed_loop_runtime_error():
    """If the asyncio loop has been closed (process shutting down),
    ``run_coroutine_threadsafe`` raises ``RuntimeError``. The bridge
    must swallow it — the poller thread mustn't crash during clean
    shutdown.

    The orphaned-coroutine RuntimeWarning is *expected* here: when
    the loop is closed, ``run_coroutine_threadsafe`` raises before
    it can take ownership of the coroutine, so Python complains that
    the coro was never awaited. In production this only happens
    during shutdown when the warning is harmless; the filter keeps
    test output clean.
    """
    from a2a_mcp_server import _setup_inbox_bridge

    # Closed loop reproduces the shutdown race.
    loop = asyncio.new_event_loop()
    loop.close()

    class _DummyWriter:
        def write(self, _data: bytes) -> None:  # pragma: no cover
            pass

        async def drain(self) -> None:  # pragma: no cover
            pass

    cb = _setup_inbox_bridge(_DummyWriter(), loop)  # type: ignore[arg-type]

    # Must not raise.
    cb({
        "activity_id": "act-shutdown",
        "text": "shutdown msg",
        "peer_id": "",
        "kind": "canvas_user",
        "method": "",
        "created_at": "",
    })


class TestStdioPipeAssertion:
    """Pin _assert_stdio_is_pipe_compatible — the friendly fail-fast guard
    that turns asyncio's `ValueError: Pipe transport is only for pipes,
    sockets and character devices` into a clear operator message + exit 2.
    See molecule-ai-workspace-runtime#61.
    """

    def test_pipe_pair_passes_silently(self):
        """Happy path — both fds are pipes (the production launch shape
        from any MCP client). Should return None without printing or
        exiting."""
        from a2a_mcp_server import _assert_stdio_is_pipe_compatible

        r, w = os.pipe()
        try:
            # No exit, no stderr noise. We don't capture stderr here
            # because pipe path should produce zero output.
            _assert_stdio_is_pipe_compatible(stdin_fd=r, stdout_fd=w)
        finally:
            os.close(r)
            os.close(w)

    def test_regular_file_stdout_exits_with_friendly_message(
        self, tmp_path, capsys
    ):
        """Reproducer for runtime#61: stdout redirected to a regular file.
        Pre-fix this would surface upstream as
        `ValueError: Pipe transport is only for pipes...`. Post-fix we
        exit with code 2 and a stderr message that names the symptom +
        fix."""
        from a2a_mcp_server import _assert_stdio_is_pipe_compatible

        # stdin = pipe (so we isolate the stdout failure path);
        # stdout = regular file (the bug condition).
        r, _w = os.pipe()
        regular = tmp_path / "captured.log"
        f = open(regular, "wb")
        try:
            with pytest.raises(SystemExit) as excinfo:
                _assert_stdio_is_pipe_compatible(
                    stdin_fd=r, stdout_fd=f.fileno()
                )
            assert excinfo.value.code == 2
            err = capsys.readouterr().err
            # Names the failing stream + the asyncio constraint that
            # would otherwise crash. Don't pin the exact wording — the
            # asserts pin the operator-recoverable signal only.
            assert "stdout" in err
            assert "regular file" in err
            assert "pipe" in err
        finally:
            f.close()
            os.close(r)

    def test_regular_file_stdin_exits_with_friendly_message(
        self, tmp_path, capsys
    ):
        """Symmetric case — stdin redirected from a regular file. Same
        asyncio constraint applies via connect_read_pipe."""
        from a2a_mcp_server import _assert_stdio_is_pipe_compatible

        regular = tmp_path / "input.json"
        regular.write_bytes(b'{"jsonrpc":"2.0","id":1,"method":"initialize"}\n')
        f = open(regular, "rb")
        _r, w = os.pipe()
        try:
            with pytest.raises(SystemExit) as excinfo:
                _assert_stdio_is_pipe_compatible(
                    stdin_fd=f.fileno(), stdout_fd=w
                )
            assert excinfo.value.code == 2
            err = capsys.readouterr().err
            assert "stdin" in err
            assert "regular file" in err
        finally:
            f.close()
            os.close(w)

    def test_closed_fd_exits_with_stat_error(self, capsys):
        """If stdio is closed (rare but seen in detached daemonized
        contexts), os.fstat raises OSError. We catch it and exit 2 with
        a guidance message instead of letting the traceback escape."""
        from a2a_mcp_server import _assert_stdio_is_pipe_compatible

        r, w = os.pipe()
        os.close(w)  # Now `w` is a stale fd — fstat will fail.
        try:
            with pytest.raises(SystemExit) as excinfo:
                _assert_stdio_is_pipe_compatible(
                    stdin_fd=r, stdout_fd=w
                )
            assert excinfo.value.code == 2
            err = capsys.readouterr().err
            assert "cannot stat stdout" in err
        finally:
            os.close(r)


def _readable(fd: int) -> bool:
    """True iff ``fd`` has bytes available without blocking. Lets
    us poll the pipe in a loop without the test hanging when the
    bridge fires later than expected."""
    import select

    rlist, _, _ = select.select([fd], [], [], 0)
    return bool(rlist)
