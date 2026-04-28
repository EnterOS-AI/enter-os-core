"""Pin peer-summary fallback when agent_card is missing.

Regresses the 2026-04-27 Design Director discovery bug:
`summarize_peer_cards()` previously skipped any peer whose `agent_card`
was null or unparseable, so a coordinator with freshly-created workers
saw an empty `## Your Peers` section in its system prompt and refused
to delegate. The registry endpoint already returns DB `name` + `role`
on every row regardless of agent_card state — falling back to those
keeps peers visible while A2A discovery catches up.
"""

from __future__ import annotations

from shared_runtime import build_peer_section, summarize_peer_cards


def _peer(**overrides):
    base = {
        "id": "ws-1",
        "name": "DB Name",
        "role": "DB Role",
        "status": "active",
        "agent_card": None,
    }
    base.update(overrides)
    return base


def test_summarize_includes_peer_with_null_agent_card_using_db_fields():
    summaries = summarize_peer_cards([_peer()])
    assert len(summaries) == 1
    assert summaries[0]["id"] == "ws-1"
    assert summaries[0]["name"] == "DB Name"
    assert summaries[0]["role"] == "DB Role"
    assert summaries[0]["status"] == "active"
    assert summaries[0]["skills"] == []


def test_summarize_prefers_agent_card_name_over_db_name():
    peer = _peer(
        agent_card={"name": "Card Name", "skills": [{"name": "draft-spec"}]}
    )
    summaries = summarize_peer_cards([peer])
    assert summaries[0]["name"] == "Card Name"
    assert summaries[0]["skills"] == ["draft-spec"]
    assert summaries[0]["role"] == "DB Role"


def test_summarize_handles_string_agent_card_json():
    peer = _peer(agent_card='{"name": "JSON Name", "skills": []}')
    summaries = summarize_peer_cards([peer])
    assert summaries[0]["name"] == "JSON Name"


def test_summarize_falls_back_when_agent_card_string_is_malformed():
    peer = _peer(agent_card="not-valid-json")
    summaries = summarize_peer_cards([peer])
    assert len(summaries) == 1
    assert summaries[0]["name"] == "DB Name"
    assert summaries[0]["role"] == "DB Role"
    assert summaries[0]["skills"] == []


def test_summarize_falls_back_when_agent_card_is_wrong_type():
    peer = _peer(agent_card=42)
    summaries = summarize_peer_cards([peer])
    assert len(summaries) == 1
    assert summaries[0]["name"] == "DB Name"


def test_summarize_handles_missing_role_and_name_with_unknown_default():
    peer = {"id": "ws-2", "status": "active", "agent_card": None}
    summaries = summarize_peer_cards([peer])
    assert summaries[0]["name"] == "Unknown"
    assert summaries[0]["role"] == ""


def test_build_peer_section_renders_role_when_skills_empty():
    section = build_peer_section([_peer()])
    assert "## Your Peers" in section
    assert "**DB Name**" in section
    assert "Role: DB Role" in section
    assert "Skills:" not in section


def test_build_peer_section_prefers_skills_over_role_when_card_present():
    peer = _peer(
        agent_card={"name": "Worker", "skills": [{"name": "design"}, {"name": "review"}]}
    )
    section = build_peer_section([peer])
    assert "Skills: design, review" in section
    assert "Role: DB Role" not in section


def test_build_peer_section_mixed_peers():
    peers = [
        _peer(id="ws-a"),
        _peer(
            id="ws-b",
            agent_card={"name": "Card B", "skills": [{"name": "build"}]},
        ),
    ]
    section = build_peer_section(peers)
    assert "id: `ws-a`" in section
    assert "id: `ws-b`" in section
    assert "Role: DB Role" in section
    assert "Skills: build" in section


def test_build_peer_section_empty_when_no_peers():
    assert build_peer_section([]) == ""
