// @vitest-environment jsdom
/**
 * Tests for DropTargetBadge — the floating drag-target affordance.
 *
 * Two-layer visual contract:
 *   1. Ghost preview — dashed rect at the next default child slot
 *   2. Text badge — "Drop into: <name>" floating above the target
 *
 * Render-condition coverage:
 *   - Renders nothing when dragOverNodeId is null
 *   - Renders nothing when dragOverNodeId node has no name (store lookup misses)
 *   - Renders nothing when getInternalNode returns undefined
 *   - Renders badge with correct name when all inputs are valid
 *   - Badge text contains the target node name
 *
 * Note: Ghost visibility (slot rect inside parent bounds) involves
 * flowToScreenPosition coordinate arithmetic that's better covered by
 * integration tests that render the full canvas. Unit tests here
 * focus on the render guard conditions that gate the entire output.
 *
 * Issue: #2071 (Canvas test gaps follow-up).
 */
import React from "react";
import { render, cleanup } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { DropTargetBadge } from "../DropTargetBadge";
import type { WorkspaceNodeData } from "@/store/canvas";

// ── Mock @xyflow/react ───────────────────────────────────────────────────────

// VIEWPORT_OFFSET mirrors what flowToScreenPosition does in the real
// component: it shifts canvas-space coords into screen-space by a fixed
// viewport offset. Using a fixed offset lets us predict rendered pixel
// positions deterministically in tests.
function canvasToScreen(x: number, y: number) {
  return { x: x + 200, y: y + 100 };
}

const mockGetInternalNode = vi.fn<(id: string) => unknown>();
const mockFlowToScreenPosition = vi.fn<
  (pos: { x: number; y: number }) => { x: number; y: number }
>();

vi.mock("@xyflow/react", () => ({
  useReactFlow: () => ({
    getInternalNode: mockGetInternalNode,
    flowToScreenPosition: mockFlowToScreenPosition,
  }),
}));

// ── Mock canvas store ─────────────────────────────────────────────────────────

// vi.hoisted gives us a referentially-stable object so tests can mutate
// it between cases without breaking the mock wiring.
const { mockState } = vi.hoisted(() => ({
  mockState: {
    nodes: [] as Array<{
      id: string;
      data: WorkspaceNodeData;
    }>,
    dragOverNodeId: null as string | null,
  },
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (sel: (s: typeof mockState) => unknown) => sel(mockState),
    { getState: () => mockState },
  ),
}));

// ── Helpers ──────────────────────────────────────────────────────────────────

/** Store node fixture. Only the id and data.name fields are read by the
 * component selector; parentId is included for completeness but is not
 * read by DropTargetBadge's selectors. */
function storeNode(id: string, name: string): typeof mockState.nodes[number] {
  return { id, data: { name } as WorkspaceNodeData };
}

/** Minimal InternalNode shape that getInternalNode returns. The component
 * reads measured.width/height, width/height fallbacks, and
 * internals.positionAbsolute. */
function makeInternal(
  id: string,
  cx: number,
  cy: number,
  w = 400,
  h = 300,
): unknown {
  return {
    id,
    measured: { width: w, height: h },
    width: w,
    height: h,
    internals: { positionAbsolute: { x: cx, y: cy } },
  };
}

beforeEach(() => {
  mockGetInternalNode.mockReset();
  mockFlowToScreenPosition.mockReset();
  mockGetInternalNode.mockReturnValue(undefined);
  mockFlowToScreenPosition.mockImplementation(canvasToScreen);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  mockState.nodes = [];
  mockState.dragOverNodeId = null;
});

// ── Test cases ───────────────────────────────────────────────────────────────

describe("DropTargetBadge — render conditions", () => {
  it("renders nothing when dragOverNodeId is null (no store nodes)", () => {
    mockState.nodes = [];
    const { container } = render(<DropTargetBadge />);
    expect(container.textContent).toBe("");
  });

  it("renders nothing when dragOverNodeId is set but store has no matching node", () => {
    // Store has a node but not the drag-over target.
    mockState.nodes = [storeNode("other", "Other")];
    mockState.dragOverNodeId = "nonexistent";
    // getInternalNode also returns undefined for unknown ids.
    mockGetInternalNode.mockReturnValue(undefined);

    const { container } = render(<DropTargetBadge />);
    expect(container.textContent).toBe("");
  });

  it("renders nothing when getInternalNode returns undefined", () => {
    mockState.nodes = [storeNode("target", "My Workspace")];
    mockState.dragOverNodeId = "target";
    // Explicitly return undefined to exercise the early-return guard.
    mockGetInternalNode.mockReturnValue(undefined);

    const { container } = render(<DropTargetBadge />);
    expect(container.textContent).toBe("");
  });

  it("renders badge with correct name when all inputs are valid", () => {
    mockState.nodes = [storeNode("target", "My Workspace")];
    mockState.dragOverNodeId = "target";
    mockGetInternalNode.mockReturnValue(makeInternal("target", 0, 0));

    const { container } = render(<DropTargetBadge />);
    // Badge renders the name from the store node.
    expect(container.textContent).toContain("My Workspace");
  });

  it("badge text follows 'Drop into: <name>' format", () => {
    mockState.nodes = [storeNode("alpha", "Alpha Workspace")];
    mockState.dragOverNodeId = "alpha";
    mockGetInternalNode.mockReturnValue(makeInternal("alpha", 50, 50, 300, 200));

    const { container } = render(<DropTargetBadge />);
    expect(container.textContent).toMatch(/Drop into:/);
    expect(container.textContent).toContain("Alpha Workspace");
  });

  it("badge contains the exact target name from the store", () => {
    const name = "Engineering :: Backend :: API";
    mockState.nodes = [storeNode("api", name)];
    mockState.dragOverNodeId = "api";
    mockGetInternalNode.mockReturnValue(makeInternal("api", 100, 100, 500, 400));

    const { container } = render(<DropTargetBadge />);
    expect(container.textContent).toBe(`Drop into: ${name}`);
  });

  it("renders nothing when target name is null (node has no data.name)", () => {
    // A node in the store without a name field → selector returns null.
    mockState.nodes = [{ id: "nameless", data: {} as WorkspaceNodeData }];
    mockState.dragOverNodeId = "nameless";
    mockGetInternalNode.mockReturnValue(makeInternal("nameless", 0, 0));

    const { container } = render(<DropTargetBadge />);
    expect(container.textContent).toBe("");
  });
});
