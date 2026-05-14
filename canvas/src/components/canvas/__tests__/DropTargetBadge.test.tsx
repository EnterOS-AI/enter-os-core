// @vitest-environment jsdom
/**
 * Tests for DropTargetBadge — floating drag affordance rendered over the
 * ReactFlow canvas while a workspace node is being dragged onto a parent.
 *
 * Covers:
 *   - Renders nothing when dragOverNodeId is null
 *   - Renders nothing when target node not found in store
 *   - Renders nothing when getInternalNode returns null
 *   - Renders ghost slot + badge when valid target is found
 *   - Ghost hidden when slot falls outside parent bounds
 *   - Badge text includes the target workspace name
 *   - Badge positioned via screen-space coordinates from flowToScreenPosition
 */
import React from "react";
import { render, screen, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { DropTargetBadge } from "../DropTargetBadge";

// ─── Mutable store state — hoisted so vi.mock factory closures capture the ref ─

let _storeState: {
  dragOverNodeId: string | null;
  nodes: Array<{
    id: string;
    data: Record<string, unknown>;
    parentId: string | null;
    measured?: { width: number; height: number };
  }>;
} = {
  dragOverNodeId: null,
  nodes: [],
};

const _subscribers = new Set<() => void>();
function _notifySubscribers() {
  for (const fn of _subscribers) fn();
}

const _mockUseCanvasStore = vi.hoisted(() => {
  const impl = (selector: (s: typeof _storeState) => unknown) => selector(_storeState);
  return impl;
});

// Module-level mutable impl — setFlowMock() swaps it out per test.
let _flowImpl: (arg: { x: number; y: number }) => { x: number; y: number } =
  ({ x, y }) => ({ x: x * 2, y: y * 2 });

let _flowToScreenPosition = vi.hoisted(() =>
  vi.fn((arg: { x: number; y: number }) => _flowImpl(arg)),
);

let _getInternalNode = vi.hoisted(() =>
  vi.fn<(id: string) => {
    internals: { positionAbsolute: { x: number; y: number } };
    measured?: { width: number; height: number };
  } | null>(() => null),
);

const _mockUseReactFlow = vi.hoisted(() =>
  vi.fn(() => ({
    getInternalNode: _getInternalNode,
    flowToScreenPosition: _flowToScreenPosition,
  })),
);

// ─── Module mocks ─────────────────────────────────────────────────────────────

vi.mock("@/store/canvas", () => ({
  useCanvasStore: _mockUseCanvasStore,
}));

vi.mock("@xyflow/react", () => ({
  useReactFlow: _mockUseReactFlow,
}));

// ─── Helpers ──────────────────────────────────────────────────────────────────

function setStore(state: Partial<typeof _storeState>) {
  _storeState = { ..._storeState, ...state };
  _notifySubscribers();
}

// Helper to set per-test flowToScreenPosition mock — replaces _flowImpl.
function setFlowMock(impl: (arg: { x: number; y: number }) => { x: number; y: number }) {
  _flowImpl = impl;
}

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("DropTargetBadge — renders nothing when not dragging", () => {
  afterEach(() => {
    cleanup();
    _storeState = { dragOverNodeId: null, nodes: [] };
    _getInternalNode.mockReset().mockReturnValue(null);
    _flowImpl = ({ x, y }) => ({ x: x * 2, y: y * 2 });
  });

  it("returns null when dragOverNodeId is null", () => {
    setStore({ dragOverNodeId: null });
    render(<DropTargetBadge />);
    expect(document.body.textContent).toBe("");
  });

  it("returns null when target node not found in store nodes array", () => {
    setStore({ dragOverNodeId: "ws-target", nodes: [] });
    render(<DropTargetBadge />);
    expect(document.body.textContent).toBe("");
  });
});

describe("DropTargetBadge — renders nothing when getInternalNode is null", () => {
  afterEach(() => {
    cleanup();
    _storeState = { dragOverNodeId: null, nodes: [] };
    _getInternalNode.mockReset().mockReturnValue(null);
    _flowImpl = ({ x, y }) => ({ x: x * 2, y: y * 2 });
  });

  it("returns null when getInternalNode returns null (node not in RF viewport)", () => {
    _getInternalNode.mockReturnValue(null);
    setStore({
      dragOverNodeId: "ws-target",
      nodes: [{ id: "ws-target", data: { name: "Target WS" }, parentId: null }],
    });
    render(<DropTargetBadge />);
    expect(document.body.textContent).toBe("");
  });
});

describe("DropTargetBadge — renders ghost slot + badge for valid drag target", () => {
  afterEach(() => {
    cleanup();
    _storeState = { dragOverNodeId: null, nodes: [] };
    _getInternalNode.mockReset().mockReturnValue(null);
    _flowImpl = ({ x, y }) => ({ x: x * 2, y: y * 2 });
  });

  it("renders the drop badge with target name", () => {
    _getInternalNode.mockReturnValue({
      internals: { positionAbsolute: { x: 100, y: 200 } },
      measured: { width: 220, height: 120 },
    });
    _flowToScreenPosition
      .mockReturnValueOnce({ x: 500, y: 400 }) // slotTL
      .mockReturnValueOnce({ x: 900, y: 600 }) // slotBR
      .mockReturnValueOnce({ x: 700, y: 200 }); // badge

    setStore({
      dragOverNodeId: "ws-target",
      nodes: [
        { id: "ws-target", data: { name: "SEO Workspace" }, parentId: null, measured: { width: 220, height: 120 } },
      ],
    });
    render(<DropTargetBadge />);
    expect(screen.getByText(/Drop into: SEO Workspace/)).toBeTruthy();
  });

  it("renders the ghost slot div via data-testid", () => {
    // measured.height must be large enough that parentBR.y > slotTL.y=330 so
    // ghostVisible = (slotTL.y < parentBR.y) is true.
    // parentBR.y = abs.y + measured.height = 200 + h > 330 → h > 130
    _getInternalNode.mockReturnValue({
      internals: { positionAbsolute: { x: 100, y: 200 } },
      measured: { width: 220, height: 500 },
    });
    // Component calls flowToScreenPosition 5 times (confirmed via debug):
    // 1) badge     {x:210, y:200} -> {x:420, y:400}     (badge center)
    // 2) slotTL    {x:116, y:330} -> {x:232, y:660}     (slot origin)
    // 3) slotBR    {x:356, y:460} -> {x:712, y:920}     (ghost uses this)
    // 4) parentTL   {x:100, y:200} -> {x:200, y:400}     (parent origin)
    // 5) parentBR  {x:320, y:320} -> {x:640, y:640}     (parent corner)
    setFlowMock(({ x, y }: { x: number; y: number }) => {
      if (x === 210 && y === 200) return { x: 420, y: 400 };
      if (x === 116 && y === 330) return { x: 232, y: 660 };
      if (x === 356 && y === 460) return { x: 712, y: 920 };
      if (x === 100 && y === 200) return { x: 200, y: 400 };
      // 5th call: parentBR = abs + {w:220, h:500} = {320, 700}
      if (x === 320 && y === 700) return { x: 640, y: 1400 };
      return { x: x * 2, y: y * 2 };
    });

    setStore({
      dragOverNodeId: "ws-target",
      nodes: [
        { id: "ws-target", data: { name: "Target" }, parentId: null, measured: { width: 220, height: 500 } },
      ],
    });
    render(<DropTargetBadge />);
    expect(screen.getByTestId("ghost-slot")).toBeTruthy();
    // Ghost uses slotBR from 3rd call: slotBR - slotTL = (712-232, 920-660)
    expect(screen.getByTestId("ghost-slot").style.left).toBe("232px");
    expect(screen.getByTestId("ghost-slot").style.top).toBe("660px");
    expect(screen.getByTestId("ghost-slot").style.width).toBe("480px");
    expect(screen.getByTestId("ghost-slot").style.height).toBe("260px");
  });

  it("ghost is hidden when slot falls entirely outside parent bounds", () => {
    _getInternalNode.mockReturnValue({
      internals: { positionAbsolute: { x: 100, y: 200 } },
      measured: { width: 220, height: 120 },
    });
    // Set slotBR (3rd call) to be inside parent to hide ghost.
    // slotBR.x ≤ parentTL.x makes slotBR.x - slotTL.x < 0 → ghostVisible = false.
    setFlowMock(({ x, y }: { x: number; y: number }) => {
      if (x === 210 && y === 200) return { x: 420, y: 400 }; // badge (1st call)
      if (x === 116 && y === 330) return { x: 232, y: 660 }; // slotTL (2nd call)
      if (x === 356 && y === 460) return { x: 150, y: 460 }; // slotBR (3rd): slotBR.x=150 < parentTL.x=200 → hidden
      if (x === 100 && y === 200) return { x: 200, y: 400 }; // parentTL (4th call)
      if (x === 320 && y === 320) return { x: 640, y: 640 }; // parentBR (5th call)
      return { x: x * 2, y: y * 2 };
    });

    setStore({
      dragOverNodeId: "ws-target",
      nodes: [
        { id: "ws-target", data: { name: "Tiny" }, parentId: null, measured: { width: 220, height: 120 } },
      ],
    });
    render(<DropTargetBadge />);
    // Badge should still render, ghost should not
    expect(screen.getByText(/Drop into: Tiny/)).toBeTruthy();
    expect(screen.queryByTestId("ghost-slot")).toBeNull();
  });

  it("badge is absolutely positioned with left and top from flowToScreenPosition", () => {
    _getInternalNode.mockReturnValue({
      internals: { positionAbsolute: { x: 100, y: 200 } },
      measured: { width: 220, height: 120 },
    });
    setFlowMock(({ x, y }: { x: number; y: number }) => {
      if (x === 210 && y === 200) return { x: 420, y: 400 };
      if (x === 116 && y === 330) return { x: 232, y: 660 };
      if (x === 356 && y === 460) return { x: 712, y: 920 };
      if (x === 100 && y === 200) return { x: 200, y: 400 };
      if (x === 320 && y === 320) return { x: 640, y: 640 };
      return { x: x * 2, y: y * 2 };
    });

    setStore({
      dragOverNodeId: "ws-target",
      nodes: [
        { id: "ws-target", data: { name: "Target" }, parentId: null, measured: { width: 220, height: 120 } },
      ],
    });
    render(<DropTargetBadge />);
    expect(screen.getByTestId("drop-badge")).toBeTruthy();
    // Badge uses 1st call: {x:210,y:200} -> {x:420,y:400}, badge.y = 400-6 = 394
    expect(screen.getByTestId("drop-badge").style.left).toBe("420px");
    expect(screen.getByTestId("drop-badge").style.top).toBe("394px");
    expect(screen.getByText(/Drop into: Target/)).toBeTruthy();
  });
});
