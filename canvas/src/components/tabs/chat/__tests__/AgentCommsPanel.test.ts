// @vitest-environment jsdom
import { describe, it, expect, vi } from "vitest";

// Stub the canvas store before importing the SUT — toCommMessage calls
// useCanvasStore.getState() inside resolveName to look up peer names,
// which would otherwise hit the real Zustand store.
vi.mock("@/store/canvas", () => ({
  useCanvasStore: {
    getState: () => ({
      nodes: [
        { id: "ws-self", data: { name: "Self" } },
        { id: "ws-peer", data: { name: "Peer Agent" } },
      ],
    }),
  },
}));

import { toCommMessage, buildPeerSummary, type ActivityEntry } from "../AgentCommsPanel";

const SELF = "ws-self";
const PEER = "ws-peer";

function makeEntry(overrides: Partial<ActivityEntry> = {}): ActivityEntry {
  return {
    id: "act-1",
    activity_type: "a2a_send",
    source_id: SELF,
    target_id: PEER,
    method: "message/send",
    summary: "Delegating to Peer Agent",
    request_body: null,
    response_body: null,
    status: "ok",
    created_at: "2026-04-25T18:00:00Z",
    ...overrides,
  };
}

describe("toCommMessage — flow derivation", () => {
  it("a2a_send is always outbound (flow=out, peer=target)", () => {
    const m = toCommMessage(
      makeEntry({ activity_type: "a2a_send", source_id: SELF, target_id: PEER }),
      SELF,
    );
    expect(m).toBeTruthy();
    expect(m!.flow).toBe("out");
    expect(m!.peerId).toBe(PEER);
    expect(m!.peerName).toBe("Peer Agent");
  });

  it("a2a_receive from a peer (peer-initiated call) is inbound", () => {
    // Real incoming call: source = peer, target = us.
    const m = toCommMessage(
      makeEntry({
        activity_type: "a2a_receive",
        source_id: PEER,
        target_id: SELF,
      }),
      SELF,
    );
    expect(m!.flow).toBe("in");
    expect(m!.peerId).toBe(PEER);
    expect(m!.peerName).toBe("Peer Agent");
  });

  it("a2a_receive self-logged by our runtime AFTER an outbound call is OUTBOUND from the user's POV", () => {
    // workspace/a2a_tools.py:181 self-logs an a2a_receive on the
    // CALLER's workspace_id with source_id=us, target_id=peer.
    // From the user's perspective this row belongs to the outbound
    // delegation thread — render flow=out + peer=target so the
    // bubble right-justifies under "Delegating to peer" and the
    // Restart button targets the actual peer (NOT us). Regression
    // for the bug where these rows rendered as "← From Self" with
    // a Restart button that would have restarted the user's own
    // workspace.
    const m = toCommMessage(
      makeEntry({
        activity_type: "a2a_receive",
        source_id: SELF,
        target_id: PEER,
        summary: "Peer Agent failed",
        status: "error",
      }),
      SELF,
    );
    expect(m!.flow).toBe("out");
    expect(m!.peerId).toBe(PEER);
    expect(m!.peerName).toBe("Peer Agent");
    expect(m!.status).toBe("error");
  });

  it("returns null when no peer can be resolved", () => {
    // a2a_receive with both ids null — discard rather than render a
    // ghost bubble pointing at "Unknown".
    const m = toCommMessage(
      makeEntry({
        activity_type: "a2a_receive",
        source_id: null,
        target_id: null,
      }),
      SELF,
    );
    expect(m).toBeNull();
  });

  it("propagates status through to the message (drives error rendering)", () => {
    const m = toCommMessage(
      makeEntry({ status: "error", activity_type: "a2a_send" }),
      SELF,
    );
    expect(m!.status).toBe("error");
  });

  // --- delegation rows ---
  // The platform's /delegate handler writes activity_type='delegation'
  // for both the initial outbound (method='delegate') and the eventual
  // reply (method='delegate_result', status=queued|completed|failed).
  // Pre-fix the panel filtered these out and showed "no agent comms"
  // even when 6+ delegations existed in the DB.

  it("delegation 'delegate' row prefers request_body.task over the boilerplate summary", () => {
    // The platform's `summary` field is "Delegating to <UUID>" — useless
    // in chat. The real task text lives in request_body.task. Show that
    // so the user sees WHAT was delegated, not just where.
    const m = toCommMessage(
      makeEntry({
        activity_type: "delegation",
        method: "delegate",
        source_id: SELF,
        target_id: PEER,
        summary: "Delegating to ws-peer",
        request_body: { task: "Build me 10 landing pages" },
        status: "pending",
      }),
      SELF,
    );
    expect(m).toBeTruthy();
    expect(m!.flow).toBe("out");
    expect(m!.peerId).toBe(PEER);
    expect(m!.peerName).toBe("Peer Agent");
    expect(m!.text).toBe("Build me 10 landing pages");
    expect(m!.status).toBe("pending");
  });

  it("delegation 'delegate' row falls back to a name-resolved label when request_body is missing", () => {
    // Older rows or some queued paths don't have request_body.task.
    // Don't render the raw UUID — resolve to the peer name so the
    // bubble at least reads "Delegating to Peer Agent".
    const m = toCommMessage(
      makeEntry({
        activity_type: "delegation",
        method: "delegate",
        source_id: SELF,
        target_id: PEER,
        summary: "Delegating to ws-peer",
        request_body: null,
        status: "pending",
      }),
      SELF,
    );
    expect(m!.text).toBe("Delegating to Peer Agent");
  });

  it("delegation 'delegate_result' row is INBOUND so the chat shows alternating bubbles", () => {
    // Even though source_id=us (we wrote the row), the conversational
    // direction is peer → us. Render as flow="in" so the user sees
    // a chat-style back-and-forth instead of a one-sided "→ To X" wall.
    const m = toCommMessage(
      makeEntry({
        activity_type: "delegation",
        method: "delegate_result",
        source_id: SELF,
        target_id: PEER,
        summary: "Delegation completed (...)",
        response_body: { response_preview: "Done — ZIP at /tmp/x.zip" },
        status: "completed",
      }),
      SELF,
    );
    expect(m!.flow).toBe("in");
    expect(m!.text).toBe("Done — ZIP at /tmp/x.zip");
  });

  it("delegation 'delegate_result' queued row shows a human-readable wait message", () => {
    // "Delegation queued — target at capacity" is platform jargon.
    // Render with the resolved peer name so the user knows WHO is busy.
    const m = toCommMessage(
      makeEntry({
        activity_type: "delegation",
        method: "delegate_result",
        source_id: SELF,
        target_id: PEER,
        summary: "Delegation queued — target at capacity",
        response_body: { queued: true },
        status: "queued",
      }),
      SELF,
    );
    expect(m!.flow).toBe("in");
    expect(m!.status).toBe("queued");
    expect(m!.text).toContain("Peer Agent");
    expect(m!.text.toLowerCase()).toContain("busy");
  });

  it("delegation row with no target_id returns null", () => {
    // Defensive: a delegation row missing target_id can't be rendered
    // (we wouldn't know which peer to attribute it to). Drop instead
    // of rendering a ghost.
    const m = toCommMessage(
      makeEntry({
        activity_type: "delegation",
        target_id: null,
      }),
      SELF,
    );
    expect(m).toBeNull();
  });
});

// --- buildPeerSummary — peer-tab ordering + counts -------------------
//
// The grouped view sorts peer tabs by most-recent activity descending
// (Slack-style DM list) so active conversations rise to the top.
// These tests pin that ordering plus the count aggregation. Pure
// helper — no React render required.

describe("buildPeerSummary", () => {
  function msg(peerId: string, peerName: string, timestamp: string): never {
    // Cast through unknown — we only need the fields buildPeerSummary
    // reads (peerId, peerName, timestamp). Other CommMessage fields
    // are irrelevant to the sort/count logic.
    return {
      id: `id-${peerId}-${timestamp}`,
      flow: "out",
      peerId,
      peerName,
      text: "",
      responseText: null,
      status: "ok",
      timestamp,
    } as never;
  }

  it("collapses messages into one row per peer with correct count", () => {
    const summary = buildPeerSummary([
      msg("ws-a", "Alpha", "2026-04-25T10:00:00Z"),
      msg("ws-a", "Alpha", "2026-04-25T10:01:00Z"),
      msg("ws-b", "Bravo", "2026-04-25T10:02:00Z"),
    ]);
    expect(summary).toHaveLength(2);
    const byId = new Map(summary.map((s) => [s.peerId, s]));
    expect(byId.get("ws-a")?.count).toBe(2);
    expect(byId.get("ws-b")?.count).toBe(1);
  });

  it("orders peers by most-recent activity DESC", () => {
    // ws-old's last activity was at 10:00, ws-new's was at 10:30 —
    // ws-new should sort first because it's more recently active.
    const summary = buildPeerSummary([
      msg("ws-old", "Old", "2026-04-25T09:00:00Z"),
      msg("ws-old", "Old", "2026-04-25T10:00:00Z"),
      msg("ws-new", "New", "2026-04-25T10:30:00Z"),
    ]);
    expect(summary[0].peerId).toBe("ws-new");
    expect(summary[1].peerId).toBe("ws-old");
  });

  it("tracks lastTs as the maximum timestamp across that peer's messages", () => {
    // Out-of-order messages — buildPeerSummary should still pick the
    // newest. Pre-fix a naive "last-seen-wins" would have set lastTs
    // to the second message's timestamp (older).
    const summary = buildPeerSummary([
      msg("ws-a", "Alpha", "2026-04-25T11:00:00Z"),
      msg("ws-a", "Alpha", "2026-04-25T09:00:00Z"),
      msg("ws-a", "Alpha", "2026-04-25T10:00:00Z"),
    ]);
    expect(summary[0].lastTs).toBe("2026-04-25T11:00:00Z");
  });

  it("empty input returns empty array", () => {
    expect(buildPeerSummary([])).toEqual([]);
  });

  it("preserves the peer's display name from the first occurrence", () => {
    // If two messages for the same peerId carry different peerName
    // (shouldn't happen in practice, but defensive), the first wins
    // — matches what the user sees in the tile and avoids name flicker.
    const summary = buildPeerSummary([
      msg("ws-a", "Alpha", "2026-04-25T10:00:00Z"),
      msg("ws-a", "Renamed", "2026-04-25T10:01:00Z"),
    ]);
    expect(summary[0].peerName).toBe("Alpha");
  });
});
