// @vitest-environment jsdom
/**
 * A2ATopologyOverlay tests — issue #744
 *
 * Split into two suites:
 *  1. buildA2AEdges — pure aggregation function (no mocks needed)
 *  2. A2ATopologyOverlay component — side-effect behavior (API + store mocks)
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, cleanup, waitFor, act } from "@testing-library/react";

// ── Mocks (hoisted before imports) ────────────────────────────────────────────

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn() },
}));

// MarkerType is a plain enum — mock @xyflow/react with it intact
vi.mock("@xyflow/react", () => ({
  MarkerType: { ArrowClosed: "arrowclosed" },
}));

// Minimal canvas store mock — selectors drive real state via the selector fn
const mockStoreState = {
  showA2AEdges: true,
  nodes: [
    { id: "ws-a", hidden: false, data: {} },
    { id: "ws-b", hidden: false, data: {} },
    { id: "ws-hidden", hidden: true, data: {} }, // nested — should be excluded
  ],
  setA2AEdges: vi.fn(),
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn(
    (selector: (s: typeof mockStoreState) => unknown) =>
      selector(mockStoreState)
  ),
}));

// ── Imports (after mocks) ─────────────────────────────────────────────────────

import { api } from "@/lib/api";
import {
  buildA2AEdges,
  formatA2ARelativeTime,
  A2ATopologyOverlay,
  A2A_WINDOW_MS,
  A2A_HOT_MS,
} from "../A2ATopologyOverlay";
import type { ActivityEntry } from "@/types/activity";

const mockGet = vi.mocked(api.get);

// ── Helpers ───────────────────────────────────────────────────────────────────

const NOW = 1_745_000_000_000; // fixed "now" for deterministic tests

function makeRow(overrides: Partial<ActivityEntry> = {}): ActivityEntry {
  return {
    id: "row-1",
    workspace_id: "ws-a",
    activity_type: "delegation",
    source_id: "ws-a",
    target_id: "ws-b",
    method: "delegate",
    summary: null,
    request_body: null,
    response_body: null,
    duration_ms: null,
    status: "completed",
    error_detail: null,
    created_at: new Date(NOW - 60_000).toISOString(), // 1 minute ago
    ...overrides,
  };
}

// ── Suite 1: buildA2AEdges (pure function) ────────────────────────────────────

describe("buildA2AEdges — filtering", () => {
  it("returns [] for empty input", () => {
    expect(buildA2AEdges([], NOW)).toEqual([]);
  });

  it("discards rows older than the 60-minute window", () => {
    const old = makeRow({
      created_at: new Date(NOW - A2A_WINDOW_MS - 1).toISOString(),
    });
    expect(buildA2AEdges([old], NOW)).toEqual([]);
  });

  it("keeps rows exactly at the window boundary (cutoff exclusive)", () => {
    const boundary = makeRow({
      created_at: new Date(NOW - A2A_WINDOW_MS + 1000).toISOString(),
    });
    expect(buildA2AEdges([boundary], NOW)).toHaveLength(1);
  });

  it("discards delegate_result rows (avoids double-counting)", () => {
    const result = makeRow({ method: "delegate_result" });
    expect(buildA2AEdges([result], NOW)).toEqual([]);
  });

  it("discards rows with null source_id", () => {
    const row = makeRow({ source_id: null });
    expect(buildA2AEdges([row], NOW)).toEqual([]);
  });

  it("discards rows with null target_id", () => {
    const row = makeRow({ target_id: null });
    expect(buildA2AEdges([row], NOW)).toEqual([]);
  });
});

describe("buildA2AEdges — aggregation", () => {
  it("aggregates multiple delegate rows on the same pair into one edge", () => {
    const rows = [
      makeRow({ id: "r1", created_at: new Date(NOW - 10_000).toISOString() }),
      makeRow({ id: "r2", created_at: new Date(NOW - 20_000).toISOString() }),
      makeRow({ id: "r3", created_at: new Date(NOW - 30_000).toISOString() }),
    ];
    const edges = buildA2AEdges(rows, NOW);
    expect(edges).toHaveLength(1);
    expect(edges[0].label).toMatch(/^3 calls/);
  });

  it("produces separate edges for different source→target pairs", () => {
    const rows = [
      makeRow({ source_id: "ws-a", target_id: "ws-b" }),
      makeRow({ source_id: "ws-b", target_id: "ws-a" }),
    ];
    const edges = buildA2AEdges(rows, NOW);
    expect(edges).toHaveLength(2);
    const ids = edges.map((e) => e.id).sort();
    expect(ids).toContain("a2a-ws-a-ws-b");
    expect(ids).toContain("a2a-ws-b-ws-a");
  });

  it("uses the latest created_at timestamp as lastAt for label recency", () => {
    const recent = NOW - 2 * 60_000; // 2 min ago
    const older = NOW - 30 * 60_000; // 30 min ago
    const rows = [
      makeRow({ id: "r1", created_at: new Date(older).toISOString() }),
      makeRow({ id: "r2", created_at: new Date(recent).toISOString() }),
    ];
    const [edge] = buildA2AEdges(rows, NOW);
    // Label should show 2m ago (the most recent), not 30m ago
    expect(edge.label).toContain("2m ago");
    expect(edge.label).not.toContain("30m ago");
  });
});

describe("buildA2AEdges — edge properties", () => {
  it("assigns correct id format: a2a-{source}-{target}", () => {
    const [edge] = buildA2AEdges([makeRow()], NOW);
    expect(edge.id).toBe("a2a-ws-a-ws-b");
  });

  it("marks edge as animated with violet stroke when lastAt < 5 min ago", () => {
    const row = makeRow({ created_at: new Date(NOW - A2A_HOT_MS + 10_000).toISOString() });
    const [edge] = buildA2AEdges([row], NOW);
    expect(edge.animated).toBe(true);
    expect((edge.style as { stroke: string }).stroke).toBe("#8b5cf6");
  });

  it("marks edge as non-animated with blue stroke when lastAt >= 5 min ago", () => {
    const row = makeRow({ created_at: new Date(NOW - A2A_HOT_MS - 10_000).toISOString() });
    const [edge] = buildA2AEdges([row], NOW);
    expect(edge.animated).toBe(false);
    expect((edge.style as { stroke: string }).stroke).toBe("#3b82f6");
  });

  it("sets pointerEvents: 'none' on style so nodes stay draggable", () => {
    const [edge] = buildA2AEdges([makeRow()], NOW);
    expect((edge.style as React.CSSProperties).pointerEvents).toBe("none");
  });

  it("tags the edge as type=a2a so React Flow renders the custom A2AEdge component", () => {
    // The custom edge portals labels above the node layer and makes
    // them clickable. Without type=a2a, RF falls back to the default
    // edge whose label sits in the SVG group (hidden under nodes,
    // pointerEvents:none). Regression guard for the hidden-label /
    // unclickable-label bug observed 2026-04-25.
    const [edge] = buildA2AEdges([makeRow()], NOW);
    expect(edge.type).toBe("a2a");
  });

  it("populates edge.data with the fields the custom edge component reads", () => {
    // A2AEdge reads count, lastAt, isHot, label from edge.data so the
    // shape upstream must keep emitting them. A future buildA2AEdges
    // refactor that drops any of these silently breaks the rendered
    // pill (label disappears, hot/warm color swap fails, click handler
    // can still fire but the label text vanishes).
    const [edge] = buildA2AEdges([makeRow()], NOW);
    const data = edge.data as Record<string, unknown>;
    expect(data.count).toBe(1);
    expect(typeof data.lastAt).toBe("number");
    expect(typeof data.isHot).toBe("boolean");
    expect(data.label).toMatch(/^1 call ·/);
  });

  it("label uses singular 'call' for count === 1", () => {
    const [edge] = buildA2AEdges([makeRow()], NOW);
    expect(edge.label).toMatch(/^1 call ·/);
  });

  it("label uses plural 'calls' for count > 1", () => {
    const rows = [makeRow({ id: "r1" }), makeRow({ id: "r2" })];
    const [edge] = buildA2AEdges(rows, NOW);
    expect(edge.label).toMatch(/^2 calls ·/);
  });
});

// ── Suite 2: formatA2ARelativeTime ───────────────────────────────────────────

describe("formatA2ARelativeTime", () => {
  it("returns 'just now' when diff < 60s", () => {
    expect(formatA2ARelativeTime(NOW - 30_000, NOW)).toBe("just now");
  });

  it("returns 'Xm ago' for minute-scale diffs", () => {
    expect(formatA2ARelativeTime(NOW - 3 * 60_000, NOW)).toBe("3m ago");
  });

  it("returns 'Xh ago' for hour-scale diffs", () => {
    expect(formatA2ARelativeTime(NOW - 2 * 3_600_000, NOW)).toBe("2h ago");
  });
});

// ── Suite 3: A2ATopologyOverlay component ─────────────────────────────────────

describe("A2ATopologyOverlay component", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    // Reset store state to defaults
    mockStoreState.showA2AEdges = true;
    mockStoreState.nodes = [
      { id: "ws-a", hidden: false, data: {} },
      { id: "ws-b", hidden: false, data: {} },
      { id: "ws-hidden", hidden: true, data: {} },
    ];
    mockStoreState.setA2AEdges = vi.fn();
  });

  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it("renders null (no DOM output)", () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue([] as any);
    const { container } = render(<A2ATopologyOverlay />);
    expect(container.firstChild).toBeNull();
  });

  it("fetches activity only for visible (non-hidden) nodes", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue([] as any);
    render(<A2ATopologyOverlay />);
    await act(async () => { await Promise.resolve(); });

    const paths = mockGet.mock.calls.map(([p]) => p as string);
    // ws-a and ws-b should be fetched; ws-hidden should NOT
    expect(paths.some((p) => p.includes("ws-a"))).toBe(true);
    expect(paths.some((p) => p.includes("ws-b"))).toBe(true);
    expect(paths.some((p) => p.includes("ws-hidden"))).toBe(false);
  });

  it("calls setA2AEdges([]) immediately when showA2AEdges is false", () => {
    mockStoreState.showA2AEdges = false;
    render(<A2ATopologyOverlay />);
    expect(mockStoreState.setA2AEdges).toHaveBeenCalledWith([]);
    expect(mockGet).not.toHaveBeenCalled();
  });

  it("passes built edges to setA2AEdges after fetch", async () => {
    const row = makeRow({ created_at: new Date(Date.now() - 60_000).toISOString() });
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue([row] as any);
    render(<A2ATopologyOverlay />);
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });

    const calls = mockStoreState.setA2AEdges.mock.calls;
    const lastCall = calls[calls.length - 1][0] as unknown[];
    // Should have produced at least one edge
    expect(lastCall.length).toBeGreaterThanOrEqual(1);
  });

  it("swallows per-workspace API errors (fail-safe)", async () => {
    mockGet.mockRejectedValue(new Error("Network error"));
    render(<A2ATopologyOverlay />);
    // Should not throw
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });
    // setA2AEdges should still be called with an empty array
    expect(mockStoreState.setA2AEdges).toHaveBeenCalled();
  });

  // Regression for the 2026-05-04 render-loop incident:
  // tenant heartbeats / status flips / peer-discovery writes mutated
  // canvas store .nodes ~5x/sec. Previously visibleIds was useMemo'd on
  // [nodes] so the array reference recreated on every store mutation,
  // causing fetchAndUpdate to recreate, the useEffect to re-fire, and
  // the 60-second polling fan-out to fire on EVERY store update. With
  // 5 visible workspaces and 5 store updates/sec, the canvas hammered
  // /workspaces/<id>/activity?type=delegation 25×/sec until edge rate
  // -limit returned 429 (per browser console captured by user).
  //
  // Fix: select a stable string key (sorted CSV of IDs) from Zustand
  // so the selector's shallow-equal short-circuit prevents re-renders
  // when the actual ID set hasn't changed.
  //
  // This test verifies the fetch fires ONCE on mount + only re-fires
  // when the visible ID set actually changes, NOT on every nodes[]
  // reference change.
  it("does not re-fetch when nodes[] reference changes but visible IDs are the same", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue([] as any);
    const { rerender } = render(<A2ATopologyOverlay />);
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });

    const callsAfterMount = mockGet.mock.calls.length;
    // Sanity: 2 visible nodes (ws-a, ws-b) → 2 fan-out requests on mount
    expect(callsAfterMount).toBe(2);

    // Simulate a store mutation that changes the nodes array reference
    // (e.g. status flip on a node) WITHOUT changing the set of visible
    // IDs. Pre-fix: this triggered a re-fetch storm. Post-fix: the
    // sorted-CSV selector returns the same key, Zustand's shallow-equal
    // short-circuits, useMemo keeps the same visibleIds, fetchAndUpdate
    // keeps the same identity, useEffect does NOT re-fire.
    mockStoreState.nodes = [
      { id: "ws-a", hidden: false, data: { newStatus: "online" } },  // mutated
      { id: "ws-b", hidden: false, data: {} },
      { id: "ws-hidden", hidden: true, data: {} },
    ];
    rerender(<A2ATopologyOverlay />);
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });

    // No additional fetches should have fired.
    expect(mockGet.mock.calls.length).toBe(callsAfterMount);
  });

  it("re-fetches when the visible ID set actually changes", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue([] as any);
    const { rerender } = render(<A2ATopologyOverlay />);
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });

    const callsAfterMount = mockGet.mock.calls.length;
    expect(callsAfterMount).toBe(2);

    // Add a new visible workspace — the visible-ID-set actually changed.
    mockStoreState.nodes = [
      { id: "ws-a", hidden: false, data: {} },
      { id: "ws-b", hidden: false, data: {} },
      { id: "ws-c", hidden: false, data: {} }, // NEW
      { id: "ws-hidden", hidden: true, data: {} },
    ];
    rerender(<A2ATopologyOverlay />);
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });

    // Should have fetched the additional workspace + the existing two
    // (the effect re-fires once with the new ID set). Total: 2 + 3 = 5.
    expect(mockGet.mock.calls.length).toBe(callsAfterMount + 3);
    const allPaths = mockGet.mock.calls.map(([p]) => p as string);
    expect(allPaths.some((p) => p.includes("ws-c"))).toBe(true);
  });
});
