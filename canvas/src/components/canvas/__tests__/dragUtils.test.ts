/**
 * Tests for dragUtils — the pure-ish geometry helpers the
 * useDragHandlers hook delegates to for nest/un-nest decisions.
 *
 * Why test these in isolation: useDragHandlers is 296 LOC of React
 * Flow + Zustand orchestration; the *decisions* live here. Pinning
 * shouldDetach + clampChildIntoParent locks the hot path the user
 * feels — drag-out hysteresis (does my drag count as un-nest?) and
 * drift-back-in clamping (did my child snap back into the parent?).
 *
 * Issue: #2071 (Canvas test gaps follow-up). The full useDragHandlers
 * orchestration test is left as a separate follow-up — it'd need
 * heavyweight React Flow / Zustand mocking that wouldn't change
 * these geometry contracts.
 */
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vitest";

// ── Hoisted mocks ────────────────────────────────────────────────────────────

// useCanvasStore is the only external touchpoint dragUtils has —
// clampChildIntoParent reads getState().nodes and writes setState().
// vi.hoisted gives us a referentially-stable state object so tests
// can mutate it between cases without losing the mock wiring.
const { mockState } = vi.hoisted(() => ({
  mockState: {
    nodes: [] as Array<{
      id: string;
      position: { x: number; y: number };
    }>,
  },
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: typeof mockState) => unknown) => selector(mockState),
    {
      getState: () => mockState,
      setState: (
        patcher:
          | Partial<typeof mockState>
          | ((s: typeof mockState) => Partial<typeof mockState>),
      ) => {
        const patch = typeof patcher === "function" ? patcher(mockState) : patcher;
        Object.assign(mockState, patch);
      },
    },
  ),
}));

// Import the SUT after the mocks are declared.
import {
  DETACH_FRACTION,
  shouldDetach,
  clampChildIntoParent,
} from "../dragUtils";

// ── Helpers ──────────────────────────────────────────────────────────────────

/**
 * Build a fake InternalNode that mirrors the shape useReactFlow returns:
 *   - measured.width/height (the live measured size React Flow tracks)
 *   - width/height fallbacks (used when measured isn't populated yet)
 *   - internals.positionAbsolute (canvas-coordinate top-left)
 */
function makeNode(
  id: string,
  x: number,
  y: number,
  w: number,
  h: number,
): unknown {
  return {
    id,
    measured: { width: w, height: h },
    width: w,
    height: h,
    internals: { positionAbsolute: { x, y } },
  };
}

/**
 * Make a getInternalNode resolver from a list of node fixtures.
 * Returns undefined for any unknown id — matches useReactFlow's
 * real shape so the missing-node fallback in shouldDetach can be
 * exercised cleanly.
 */
function makeGetInternalNode(
  fixtures: Record<string, unknown>,
): (id: string) => unknown {
  return (id: string) => fixtures[id];
}

beforeEach(() => {
  mockState.nodes = [];
});

afterEach(() => {
  vi.restoreAllMocks();
});

// ── shouldDetach ─────────────────────────────────────────────────────────────

describe("shouldDetach", () => {
  it("returns false when child sits fully inside parent (no overlap loss)", () => {
    // Parent at (0,0) 400x300; child at (50,50) 100x80 — fully inside.
    const get = makeGetInternalNode({
      child: makeNode("child", 50, 50, 100, 80),
      parent: makeNode("parent", 0, 0, 400, 300),
    });
    expect(shouldDetach("child", "parent", get as never)).toBe(false);
  });

  it("returns false when child has drifted just past the edge but stays under DETACH_FRACTION", () => {
    // Child width=100; DETACH_FRACTION=0.2 → must lose >20% (>20px)
    // outside on at least one axis. Drift the child 15px left of
    // the parent edge (loses 15/100 = 15% on X) — still nested.
    const get = makeGetInternalNode({
      child: makeNode("child", -15, 50, 100, 80),
      parent: makeNode("parent", 0, 0, 400, 300),
    });
    expect(shouldDetach("child", "parent", get as never)).toBe(false);
  });

  it("returns true when child is dragged past the X-axis hysteresis threshold", () => {
    // 25% of child outside on X (DETACH_FRACTION=0.2) — un-nest.
    const get = makeGetInternalNode({
      child: makeNode("child", -25, 50, 100, 80),
      parent: makeNode("parent", 0, 0, 400, 300),
    });
    expect(shouldDetach("child", "parent", get as never)).toBe(true);
  });

  it("returns true when child is dragged past the Y-axis hysteresis threshold", () => {
    // 25% of child outside on Y. Either axis crossing the threshold
    // triggers detach (matches the OR logic in the source).
    const get = makeGetInternalNode({
      child: makeNode("child", 50, -25, 100, 80),
      parent: makeNode("parent", 0, 0, 400, 300),
    });
    expect(shouldDetach("child", "parent", get as never)).toBe(true);
  });

  it("returns true (conservative) when child node is missing", () => {
    // Source comment: \"Returns true when we can't measure either node\"
    // — the conservative fallback so a missing measurement doesn't
    // accidentally pin a child to a parent during a real un-nest.
    const get = makeGetInternalNode({
      parent: makeNode("parent", 0, 0, 400, 300),
    });
    expect(shouldDetach("missing-child", "parent", get as never)).toBe(true);
  });

  it("returns true (conservative) when parent node is missing", () => {
    const get = makeGetInternalNode({
      child: makeNode("child", 50, 50, 100, 80),
    });
    expect(shouldDetach("child", "missing-parent", get as never)).toBe(true);
  });

  it("falls back to default 220x120 dimensions when measured is absent", () => {
    // Mirror real react-flow during initial mount: width/height come
    // from React Flow's defaults until measurement runs. shouldDetach
    // explicitly defaults to 220x120 in that case.
    const noMeasure = (id: string, x: number, y: number) => ({
      id,
      width: undefined,
      height: undefined,
      measured: undefined,
      internals: { positionAbsolute: { x, y } },
    });
    // Default child 220x120; parent 400x300; child at (-50, 0). 50/220
    // ≈ 22.7% off on X — JUST past 20% threshold → detach.
    const get = makeGetInternalNode({
      child: noMeasure("child", -50, 0),
      parent: makeNode("parent", 0, 0, 400, 300),
    });
    expect(shouldDetach("child", "parent", get as never)).toBe(true);
  });

  it("DETACH_FRACTION is exported as 0.2 (Miro/tldraw convention)", () => {
    // Pin the constant so a future refactor that bumps it to 0.3 has
    // to update this test deliberately — sudden behavior change
    // would otherwise just feel unresponsive on twitchy releases.
    expect(DETACH_FRACTION).toBe(0.2);
  });
});

// ── clampChildIntoParent ─────────────────────────────────────────────────────

describe("clampChildIntoParent", () => {
  it("no-op when child is already inside parent bounds (no setState write)", () => {
    // Parent 400x300; child at relative (50, 50) 100x80 — fully inside.
    // Source bails before setState when clamped position == current.
    mockState.nodes = [
      { id: "child", position: { x: 50, y: 50 } },
    ];
    const get = makeGetInternalNode({
      child: makeNode("child", 0, 0, 100, 80), // absolute pos doesn't matter for clamp; only `cur.position` matters
      parent: makeNode("parent", 0, 0, 400, 300),
    });
    const before = mockState.nodes;
    clampChildIntoParent("child", "parent", get as never);
    // Same array reference — proves no setState was called.
    expect(mockState.nodes).toBe(before);
    expect(mockState.nodes[0].position).toEqual({ x: 50, y: 50 });
  });

  it("clamps to (0, 0) when child has drifted past the top-left corner", () => {
    // Negative relative position = past the parent's top-left edge.
    mockState.nodes = [
      { id: "child", position: { x: -10, y: -20 } },
    ];
    const get = makeGetInternalNode({
      child: makeNode("child", 0, 0, 100, 80),
      parent: makeNode("parent", 0, 0, 400, 300),
    });
    clampChildIntoParent("child", "parent", get as never);
    expect(mockState.nodes[0].position).toEqual({ x: 0, y: 0 });
  });

  it("clamps to (parentW - childW, parentH - childH) at the bottom-right", () => {
    // Child sticks out past the parent's bottom-right corner.
    // Parent 400x300, child 100x80 → max valid relative pos is (300, 220).
    mockState.nodes = [
      { id: "child", position: { x: 500, y: 400 } },
    ];
    const get = makeGetInternalNode({
      child: makeNode("child", 0, 0, 100, 80),
      parent: makeNode("parent", 0, 0, 400, 300),
    });
    clampChildIntoParent("child", "parent", get as never);
    expect(mockState.nodes[0].position).toEqual({ x: 300, y: 220 });
  });

  it("clamps independently on each axis (X clamps, Y stays)", () => {
    // Child past edge on X but inside on Y — only X position is updated.
    mockState.nodes = [
      { id: "child", position: { x: -50, y: 100 } },
    ];
    const get = makeGetInternalNode({
      child: makeNode("child", 0, 0, 100, 80),
      parent: makeNode("parent", 0, 0, 400, 300),
    });
    clampChildIntoParent("child", "parent", get as never);
    expect(mockState.nodes[0].position).toEqual({ x: 0, y: 100 });
  });

  it("returns early when child node not in store (no setState)", () => {
    mockState.nodes = [
      { id: "different", position: { x: 0, y: 0 } },
    ];
    const get = makeGetInternalNode({
      child: makeNode("child", 0, 0, 100, 80),
      parent: makeNode("parent", 0, 0, 400, 300),
    });
    const before = mockState.nodes;
    clampChildIntoParent("child", "parent", get as never);
    expect(mockState.nodes).toBe(before);
  });

  it("returns early when child internalNode is missing", () => {
    mockState.nodes = [
      { id: "child", position: { x: -10, y: -20 } },
    ];
    // Only parent is registered — getInternalNode returns undefined for child.
    const get = makeGetInternalNode({
      parent: makeNode("parent", 0, 0, 400, 300),
    });
    const before = mockState.nodes[0].position;
    clampChildIntoParent("child", "parent", get as never);
    // Position untouched — the early-return short-circuited.
    expect(mockState.nodes[0].position).toEqual(before);
  });

  it("preserves other nodes in the store (only mutates the target)", () => {
    // Multiple children; clamping one must NOT touch the others.
    mockState.nodes = [
      { id: "child", position: { x: -10, y: -10 } },
      { id: "sibling", position: { x: 200, y: 200 } },
      { id: "stranger", position: { x: 999, y: 999 } },
    ];
    const get = makeGetInternalNode({
      child: makeNode("child", 0, 0, 100, 80),
      parent: makeNode("parent", 0, 0, 400, 300),
    });
    clampChildIntoParent("child", "parent", get as never);
    const byId = Object.fromEntries(mockState.nodes.map((n) => [n.id, n]));
    expect(byId.child.position).toEqual({ x: 0, y: 0 });
    expect(byId.sibling.position).toEqual({ x: 200, y: 200 });
    expect(byId.stranger.position).toEqual({ x: 999, y: 999 });
  });
});
