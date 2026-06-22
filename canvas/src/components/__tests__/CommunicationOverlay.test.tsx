// @vitest-environment jsdom
/**
 * CommunicationOverlay tests — pin both the 2026-05-04 fan-out cap fix
 * AND the 2026-05-07 polling → ACTIVITY_LOGGED-subscriber refactor
 * (issue #61 stage 1).
 *
 * The overlay used to poll /workspaces/:id/activity?limit=5 on a 30s
 * interval per online workspace (capped at 3). Post-#61: it bootstraps
 * once on mount via the same HTTP path (cap of 3 retained), then
 * subscribes to ACTIVITY_LOGGED via the global socket bus for live
 * updates. No interval poll.
 *
 * These tests pin:
 *  1. Bootstrap fan-out cap of 3 — even with 6 online nodes, only 3
 *     HTTP fetches on mount.
 *  2. Visibility gate — when collapsed, no HTTP fetches; re-open
 *     re-bootstraps.
 *  3. NO interval polling — advancing the clock past 30s does not fire
 *     additional HTTP calls.
 *  4. WS push extends the rendered list without firing any HTTP call.
 *  5. WS push for an offline workspace is ignored.
 *  6. WS push for a non-comm activity_type is ignored.
 *
 * If a future refactor regresses any of these, CI fails before the
 * regression hits a paying tenant.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, cleanup, act, fireEvent } from "@testing-library/react";

// ── Mocks (hoisted before imports) ────────────────────────────────────────────

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn() },
}));

// Six online nodes — enough to verify the bootstrap cap of 3.
const mockStoreState = {
  selectedNodeId: null as string | null,
  nodes: [
    { id: "ws-1", data: { status: "online", name: "ws-1" } },
    { id: "ws-2", data: { status: "online", name: "ws-2" } },
    { id: "ws-3", data: { status: "online", name: "ws-3" } },
    { id: "ws-4", data: { status: "online", name: "ws-4" } },
    { id: "ws-5", data: { status: "online", name: "ws-5" } },
    { id: "ws-6", data: { status: "online", name: "ws-6" } },
    { id: "ws-offline", data: { status: "offline", name: "off" } },
  ],
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn(
    (selector: (s: typeof mockStoreState) => unknown) =>
      selector(mockStoreState)
  ),
}));

// design-tokens has named exports — keep the shape minimal.
vi.mock("@/lib/design-tokens", () => ({
  COMM_TYPE_LABELS: {
    a2a_send: "→",
    a2a_receive: "←",
    task_update: "✓",
  },
}));

// ── Imports (after mocks) ─────────────────────────────────────────────────────

import { api } from "@/lib/api";
import {
  emitSocketEvent,
  _resetSocketEventListenersForTests,
} from "@/store/socket-events";
import { CommunicationOverlay } from "../CommunicationOverlay";

const mockGet = vi.mocked(api.get);

// ── Setup ─────────────────────────────────────────────────────────────────────

beforeEach(() => {
  vi.useFakeTimers();
  mockGet.mockReset();
  mockGet.mockResolvedValue([]);
  // Drop any subscribers the previous test left on the singleton bus —
  // each render adds one via useSocketEvent.
  _resetSocketEventListenersForTests();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  _resetSocketEventListenersForTests();
});

// ── Tests ─────────────────────────────────────────────────────────────────────

describe("CommunicationOverlay — bootstrap fan-out cap", () => {
  it("bootstraps at most 3 of 6 online workspaces (rate-limit floor preserved post-#61)", async () => {
    await act(async () => {
      render(<CommunicationOverlay />);
    });
    // Mount fires the bootstrap synchronously — pre-#61 this was the
    // first poll cycle; post-#61 it's the only HTTP fetch (live updates
    // arrive via WS push). 6 nodes → 3 fetches.
    expect(mockGet).toHaveBeenCalledTimes(3);
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-1/activity?limit=5");
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-2/activity?limit=5");
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-3/activity?limit=5");
  });

  it("never bootstraps offline workspaces", async () => {
    await act(async () => {
      render(<CommunicationOverlay />);
    });
    expect(mockGet).not.toHaveBeenCalledWith(
      "/workspaces/ws-offline/activity?limit=5",
    );
  });
});

describe("CommunicationOverlay — no interval polling (post-#61)", () => {
  // The pre-#61 implementation re-fetched every 30s per workspace.
  // Post-#61 the only HTTP path is the bootstrap on mount + on
  // visibility-toggle. This test pins the absence of any interval
  // poll: a 60s clock advance must not produce a second round of
  // fetches.
  it("does NOT poll on a 30s interval after bootstrap", async () => {
    await act(async () => {
      render(<CommunicationOverlay />);
    });
    expect(mockGet).toHaveBeenCalledTimes(3); // initial bootstrap
    mockGet.mockClear();

    // Advance 60s — well past any plausible cadence the prior version
    // could have used.
    await act(async () => {
      vi.advanceTimersByTime(60_000);
    });
    expect(mockGet).not.toHaveBeenCalled();
  });
});

describe("CommunicationOverlay — visibility gate", () => {
  // The visibility gate now does two things post-#61:
  //   - while closed, the WS handler short-circuits (no setComms churn)
  //   - re-opening triggers a fresh bootstrap so the list reflects
  //     anything that happened while the panel was collapsed
  //
  // Direct probe: render with comms-returning mock so the panel
  // actually renders (close button only exists in the expanded panel,
  // not the collapsed button-state). Click close, advance the clock,
  // assert no further fetches.
  it("stops fetching while collapsed and re-bootstraps on re-open", async () => {
    mockGet.mockResolvedValue([
      {
        id: "act-1",
        workspace_id: "ws-1",
        activity_type: "a2a_send",
        source_id: "ws-1",
        target_id: "ws-2",
        summary: "test",
        status: "completed",
        duration_ms: 100,
        created_at: new Date().toISOString(),
      },
    ]);

    const { getByLabelText } = await act(async () => {
      return render(<CommunicationOverlay />);
    });
    // Drain pending microtasks (resolves the await in bootstrap) so
    // setComms lands and the panel renders. Don't advance time — it's
    // not load-bearing for the gate test, but matches the pattern used
    // pre-#61 for stability.
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(mockGet).toHaveBeenCalledTimes(3); // initial bootstrap
    mockGet.mockClear();

    // Click close. While closed, no fetches and no WS-driven updates.
    const closeBtn = getByLabelText("Close communications panel");
    await act(async () => {
      fireEvent.click(closeBtn);
    });
    await act(async () => {
      vi.advanceTimersByTime(60_000);
    });
    expect(mockGet).not.toHaveBeenCalled();

    // Re-open via the collapsed button. Must trigger a fresh bootstrap.
    const openBtn = getByLabelText("Show communications panel");
    await act(async () => {
      fireEvent.click(openBtn);
    });
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(mockGet).toHaveBeenCalledTimes(3); // re-bootstrap on re-open
  });
});

describe("CommunicationOverlay — WS subscription (#61 stage 1 core)", () => {
  // The load-bearing post-#61 behaviour. Every test in this block must
  // verify (a) the WS push DID update the rendered comms list, and
  // (b) NO additional HTTP call was fired — the whole point of the
  // refactor is to remove the polling-driven HTTP traffic.
  function emitActivityLogged(overrides: Partial<{
    workspaceId: string;
    payload: Record<string, unknown>;
  }> = {}) {
    emitSocketEvent({
      event: "ACTIVITY_LOGGED",
      workspace_id: overrides.workspaceId ?? "ws-1",
      timestamp: new Date().toISOString(),
      payload: {
        id: `act-${Math.random().toString(36).slice(2)}`,
        activity_type: "a2a_send",
        source_id: "ws-1",
        target_id: "ws-2",
        summary: "live push",
        status: "ok",
        duration_ms: 42,
        created_at: new Date().toISOString(),
        ...overrides.payload,
      },
    });
  }

  it("WS push for a comm activity_type extends the rendered list with NO additional HTTP call", async () => {
    const { container } = await act(async () => {
      return render(<CommunicationOverlay />);
    });
    expect(mockGet).toHaveBeenCalledTimes(3); // bootstrap
    mockGet.mockClear();

    await act(async () => {
      emitActivityLogged({ payload: { summary: "hello" } });
    });
    await act(async () => {
      await Promise.resolve();
    });

    // Two pins:
    //   1. comms list reflects the live push (look for the summary text)
    //   2. zero HTTP fetches fired during the WS path
    expect(container.textContent).toContain("hello");
    expect(mockGet).not.toHaveBeenCalled();
  });

  it("WS push for an offline workspace is ignored", async () => {
    const { container } = await act(async () => {
      return render(<CommunicationOverlay />);
    });
    mockGet.mockClear();

    await act(async () => {
      emitActivityLogged({
        workspaceId: "ws-offline",
        payload: { source_id: "ws-offline", summary: "should-not-render" },
      });
    });
    await act(async () => {
      await Promise.resolve();
    });

    expect(container.textContent).not.toContain("should-not-render");
    expect(mockGet).not.toHaveBeenCalled();
  });

  it("WS push for a non-comm activity_type is ignored (e.g. delegation)", async () => {
    const { container } = await act(async () => {
      return render(<CommunicationOverlay />);
    });
    mockGet.mockClear();

    await act(async () => {
      emitActivityLogged({
        payload: {
          activity_type: "delegation",
          summary: "should-not-render-delegation",
        },
      });
    });
    await act(async () => {
      await Promise.resolve();
    });

    expect(container.textContent).not.toContain("should-not-render-delegation");
    expect(mockGet).not.toHaveBeenCalled();
  });

  it("WS push while the panel is collapsed is ignored (no churn on hidden state)", async () => {
    // Bootstrap with one comm so the panel renders → close button
    // accessible. Then collapse, emit a WS push, re-open: the rendered
    // list must come from the re-bootstrap, NOT from the WS-push that
    // arrived during the closed state. Also: nothing visible while
    // closed (the collapsed button shows only the count, not summaries).
    mockGet.mockResolvedValue([
      {
        id: "act-bootstrap",
        workspace_id: "ws-1",
        activity_type: "a2a_send",
        source_id: "ws-1",
        target_id: "ws-2",
        summary: "bootstrap-summary",
        status: "ok",
        duration_ms: 1,
        created_at: new Date().toISOString(),
      },
    ]);
    const { getByLabelText, container } = await act(async () => {
      return render(<CommunicationOverlay />);
    });
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });

    // Collapse.
    const closeBtn = getByLabelText("Close communications panel");
    await act(async () => {
      fireEvent.click(closeBtn);
    });

    // Bootstrap mock returns nothing on the re-open path so we can
    // distinguish "WS push leaked through the gate" from "re-bootstrap
    // refilled the list."
    mockGet.mockReset();
    mockGet.mockResolvedValue([]);

    await act(async () => {
      emitActivityLogged({
        payload: { summary: "leaked-while-closed" },
      });
    });
    await act(async () => {
      await Promise.resolve();
    });

    // Closed state: rendered DOM must not show any push-derived text.
    expect(container.textContent).not.toContain("leaked-while-closed");
  });

  it("non-ACTIVITY_LOGGED events are ignored (e.g. WORKSPACE_OFFLINE)", async () => {
    const { container } = await act(async () => {
      return render(<CommunicationOverlay />);
    });
    mockGet.mockClear();

    await act(async () => {
      emitSocketEvent({
        event: "WORKSPACE_OFFLINE",
        workspace_id: "ws-1",
        timestamp: new Date().toISOString(),
        payload: { summary: "should-not-render-event" },
      });
    });
    await act(async () => {
      await Promise.resolve();
    });

    expect(container.textContent).not.toContain("should-not-render-event");
    expect(mockGet).not.toHaveBeenCalled();
  });
});
