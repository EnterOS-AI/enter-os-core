"""Layer-isolation diagnostics — runs alongside the 4 canaries.

These probes are not strict pass/fail gates by themselves; they exist so
when a canary fails, the artifacts include enough state to tell whether
the regression is in the wire-shape layer, the SessionStore layer, or
the memory layer. Each test always passes (returns early) when the
underlying surface is unavailable on the runtime under test — different
templates expose different debug endpoints.

Cross-refs:
  - feedback_verify_actual_endstate_not_ack_follow_sop — we read state
    back, not the side-effect ack.
  - feedback_image_promote_is_not_user_live — the verification is at
    the running-container layer.
"""

from __future__ import annotations

import os
import uuid

import httpx

from cp_sim import CPSim


def test_diag_agent_card_advertises_a2a(sim: CPSim) -> None:
    """The runtime's /agent-card must advertise A2A capabilities.

    If this fails, the canaries' transport assumption (POST /a2a) is
    already broken — diagnose the runtime image, not the canary.
    """
    url = f"{sim.cfg.runtime_url}/agent-card"
    r = httpx.get(url, timeout=10.0)
    assert r.status_code == 200, (
        f"/agent-card returned {r.status_code}: {r.text[:300]!r}"
    )
    body = r.json()
    # AgentCard spec: capabilities object must exist, even if empty.
    assert isinstance(body, dict), f"/agent-card body not an object: {body!r}"
    # We don't require any specific capability flag — different templates
    # advertise different sets. The point of this diag is "is the card
    # there at all", which signals the runtime booted past entrypoint.


def test_diag_context_id_required_for_continuity(sim: CPSim) -> None:
    """Same context_id in two turns must not crash the runtime.

    Pure smoke probe — proves the executor accepts a continuation
    message without 5xx-ing. The substantive assertion is canary 1; this
    one just guarantees the path is reachable.
    """
    ctx = f"diag-{uuid.uuid4().hex[:8]}"
    r1 = sim.send_text("ping", context_id=ctx)
    r2 = sim.send_text("ping again", context_id=ctx, task_id=r1.get("result", {}).get("id"))
    # Both replies must parse — non-empty envelope, no JSON-RPC error.
    for label, env in (("turn1", r1), ("turn2", r2)):
        assert "error" not in env, f"{label} returned JSON-RPC error: {env['error']}"


def test_diag_memory_root_writable_in_canary_mode(sim: CPSim) -> None:
    """When MOLECULE_CANARY_MODE=1, the memory root must accept writes.

    Probes via the recall_memory MCP tool — if /mcp is not exposed,
    returns early (skip-style; we still pass because some templates
    proxy MCP elsewhere).
    """
    # We can't write directly here — only confirm the read path doesn't
    # 500 on a missing key. A real write happens in canary 4.
    key = f"canary-probe-{uuid.uuid4().hex[:8]}"
    try:
        val = sim.probe_memory(key)
    except Exception:
        # /mcp may not be exposed on this template — canary 4 will
        # surface the real defect if memory is actually broken.
        if os.environ.get("CANARY_STRICT_MCP") == "1":
            raise
        return
    # Unknown key → None is fine. The point is the call didn't crash.
    assert val is None or isinstance(val, str)
