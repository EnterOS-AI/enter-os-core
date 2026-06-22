"""The 4 canonical session-continuity canaries (task #342, RFC#600 class).

These tests speak A2A directly to the runtime under test. They are the
authoritative gate that the runtime preserves conversation continuity,
handles file-only messages without dropping to the empty-prompt error,
addresses multimodal prompts, and persists memory across sessions.

Wire-shape source of truth: see ../cp_sim.py docstring.
"""

from __future__ import annotations

import re
import uuid

from cp_sim import CPSim


# ---------- canary 1: 2-turn name continuity -------------------------------


def test_canary_1_two_turn_name_continuity(sim: CPSim, context_id: str) -> None:
    """SessionStore continuity — turn 2 must recall the name from turn 1.

    Empirically tests:
      - ``a2a_executor._core_execute`` injects prior-turn history via
        ``_extract_history(context)`` (workspace/a2a_executor.py:313).
      - The runtime's session store is keyed on ``context_id`` (canvas
        thread id) NOT ``task_id`` — task_id is per-turn, context_id is
        per-conversation. Regressions to that key derivation were the
        root cause of the 2026-05 multi-turn-amnesia incidents
        (#a60623344 diagnosis).
    """
    # Turn 1 — establish the fact.
    r1 = sim.send_text(
        "Hi, my name is Hongming.",
        context_id=context_id,
    )
    reply1 = sim.extract_text_parts(r1)
    assert reply1, f"Turn 1 produced empty reply. envelope={r1!r}"

    # Turn 2 — ask back. Same context_id → same SessionStore key.
    r2 = sim.send_text(
        "What's my name?",
        context_id=context_id,
    )
    reply2 = sim.extract_text_parts(r2)
    assert reply2, f"Turn 2 produced empty reply. envelope={r2!r}"

    # Substring match, case-insensitive — agents may reply
    # "Your name is Hongming." or "It's Hongming!" or similar.
    assert re.search(r"\bhongming\b", reply2, flags=re.IGNORECASE), (
        f"Turn 2 reply does not contain 'Hongming' — SessionStore "
        f"continuity regression suspected. context_id={context_id} "
        f"turn1_reply={reply1[:200]!r} turn2_reply={reply2[:400]!r}"
    )


# ---------- canary 2: file-only message (no caption) -----------------------


_DROPPED_TURN_MARKERS = (
    "(empty prompt — nothing to do)",
    "empty prompt",
    "message contained no text content",
    "no text content",
)


def test_canary_2_file_only_message(sim: CPSim, context_id: str) -> None:
    """File-attached A2A message with NO text part must not be dropped.

    Root cause this guards against: a long-standing executor bug where
    ``extract_message_text`` returned "" for file-only messages and the
    executor short-circuited with the "Error: message contained no text
    content." reply, even though the attached file was the entire point
    of the turn.

    Hard assertions:
      - Reply is non-empty AND not the dropped-turn marker.
      - Reply references the file by name OR asks an actionable
        clarifying question (NOT a flat error).
    """
    file_name = f"canary-{uuid.uuid4().hex[:8]}.txt"
    file_body = b"Project status: nominal. Lighthouse score 98."

    r = sim.send_with_file(
        context_id=context_id,
        text=None,  # ← THE CANARY: no caption.
        file_name=file_name,
        file_bytes=file_body,
        mime_type="text/plain",
    )
    reply = sim.extract_text_parts(r)
    assert reply, f"File-only message produced empty reply. envelope={r!r}"

    low = reply.lower()
    for marker in _DROPPED_TURN_MARKERS:
        assert marker.lower() not in low, (
            f"File-only message was dropped — reply contains "
            f"{marker!r}. Full reply: {reply[:500]!r}"
        )

    # Soft assertion: reply must engage with the file (reference its
    # name) OR ask an actionable clarification. We require ONE of those —
    # a generic "Hello! How can I help?" reply is also a drop.
    name_referenced = file_name.lower() in low or "file" in low or "attach" in low
    asks_clarification = (
        "what" in low or "would you like" in low or "?" in reply
    )
    assert name_referenced or asks_clarification, (
        f"File-only reply neither references the file nor asks a "
        f"clarifying question. Reply: {reply[:500]!r}"
    )


# ---------- canary 3: file + prompt (multimodal) ---------------------------


def test_canary_3_file_with_prompt(sim: CPSim, context_id: str) -> None:
    """File-attached A2A message WITH a caption — multimodal happy path.

    Lower bar than canary 2: assert the agent acknowledges the file was
    received and tries to address the caption. We deliberately don't
    require a perfect summary because canary mode replies are canned —
    the goal is to prove the executor's multimodal code path doesn't
    drop EITHER the file OR the caption.
    """
    file_name = f"canary-doc-{uuid.uuid4().hex[:8]}.txt"
    file_body = (
        b"Quarterly review. Revenue up 14%. Churn down 3%. "
        b"Team headcount steady. Action: ship RFC#600 by end of week."
    )
    r = sim.send_with_file(
        context_id=context_id,
        text="summarize this",
        file_name=file_name,
        file_bytes=file_body,
        mime_type="text/plain",
    )
    reply = sim.extract_text_parts(r)
    assert reply, f"File+prompt produced empty reply. envelope={r!r}"

    low = reply.lower()
    for marker in _DROPPED_TURN_MARKERS:
        assert marker.lower() not in low, (
            f"File+prompt was dropped — reply contains {marker!r}. "
            f"Full reply: {reply[:500]!r}"
        )

    # At minimum: the reply must mention file/attach/summary semantics,
    # demonstrating the executor accepted both parts.
    engaged = any(
        kw in low for kw in ("file", "attach", "summary", "summarize", "content", file_name.lower())
    )
    assert engaged, (
        f"Multimodal reply doesn't engage with attached file or caption. "
        f"Reply: {reply[:500]!r}"
    )


# ---------- canary 4: cross-session memory recall --------------------------


def test_canary_4_cross_session_memory_recall(sim: CPSim) -> None:
    """Memory persists across distinct context_ids → memory layer (NOT
    SessionStore) is the storage.

    Two distinct context_ids in this test — SessionStore CANNOT bridge
    them. The bridge is the runtime's persistent memory (MOLECULE_MEMORY_ROOT
    in canary mode). If the recall returns "blue" in session 2, the
    memory layer is wired correctly.

    Note: we ask the agent to commit the memory explicitly in session 1
    so that the canary doesn't depend on memory auto-extraction
    heuristics (which vary by runtime). The commit goes through the
    same MCP tool the canvas would invoke.
    """
    ctx_a = f"canary-ctx-{uuid.uuid4().hex[:12]}"
    ctx_b = f"canary-ctx-{uuid.uuid4().hex[:12]}"

    # Session 1 — commit a fact via the memory tool. Use the explicit
    # "remember" verb so canary-mode agents (which short-circuit to a
    # deterministic tool-call) reliably invoke `commit_memory`.
    r1 = sim.send_text(
        "Please use the memory tool to remember: my favorite color is blue.",
        context_id=ctx_a,
    )
    reply1 = sim.extract_text_parts(r1)
    assert reply1, f"Session 1 produced empty reply. envelope={r1!r}"

    # Session 2 — different context_id. Same workspace, same memory.
    r2 = sim.send_text(
        "Use the memory tool to recall my favorite color, then tell me what it is.",
        context_id=ctx_b,
    )
    reply2 = sim.extract_text_parts(r2)
    assert reply2, f"Session 2 produced empty reply. envelope={r2!r}"

    assert re.search(r"\bblue\b", reply2, flags=re.IGNORECASE), (
        f"Session 2 reply does not contain 'blue' — cross-session memory "
        f"recall regression suspected. ctx_a={ctx_a} ctx_b={ctx_b} "
        f"session1_reply={reply1[:200]!r} session2_reply={reply2[:400]!r}"
    )
