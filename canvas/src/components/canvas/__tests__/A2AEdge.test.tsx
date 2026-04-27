// @vitest-environment jsdom
/**
 * Tests for A2AEdge — the custom React Flow edge that renders the
 * delegation count between two workspaces and routes a click into the
 * source workspace's Activity feed.
 *
 * Behavioural coverage for the four contracts the component owns:
 *   1. No label → render only the BaseEdge SVG, NO portaled HTML pill
 *      (renders nothing visible at the label layer)
 *   2. Click the pill → selectNode(source) AND setPanelTab("activity")
 *      when this is a *fresh* selection
 *   3. Click the pill on an already-selected source → selectNode(source)
 *      runs but setPanelTab is NOT called (preserves the user's current
 *      tab so they don't get yanked off Chat / Memory)
 *   4. isHot toggles the violet vs. blue accent classes — locks the
 *      buildA2AEdges output → A2AEdge styling contract
 *   5. ARIA label pluralizes correctly (1 delegation vs N delegations)
 *
 * Issue: #2071 (Canvas test gaps follow-up).
 */
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
  type Mock,
} from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
} from "@testing-library/react";
import type { ComponentProps } from "react";

// ── Hoisted mocks ────────────────────────────────────────────────────────────

// @xyflow/react is mocked end-to-end so the test doesn't need a
// ReactFlow provider / Pane / canvas. EdgeLabelRenderer normally
// portals into the canvas root; here it just renders children inline
// so screen.queryByRole picks up the pill.
vi.mock("@xyflow/react", () => ({
  BaseEdge: ({ id }: { id: string }) => <g data-testid={`base-edge-${id}`} />,
  EdgeLabelRenderer: ({ children }: { children: React.ReactNode }) => (
    <>{children}</>
  ),
  getBezierPath: () => ["M0,0 L1,1", 50, 50],
}));

// Canvas store mock — simulates selectNode + setPanelTab + the
// selectedNodeId state the click handler reads via getState().
const { mockSelectNode, mockSetPanelTab, mockState } = vi.hoisted(() => ({
  mockSelectNode: vi.fn(),
  mockSetPanelTab: vi.fn(),
  mockState: {
    selectedNodeId: null as string | null,
    selectNode: vi.fn(),
    setPanelTab: vi.fn(),
  },
}));

// Wire the hoisted mock fns onto the store-state object (vi.hoisted
// returns referentially-stable objects, so this assignment after
// hoisting is observed by every consumer).
mockState.selectNode = mockSelectNode;
mockState.setPanelTab = mockSetPanelTab;

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: typeof mockState) => unknown) => selector(mockState),
    { getState: () => mockState },
  ),
}));

// Import the SUT after the mocks are declared.
import { A2AEdge } from "../A2AEdge";

// ── Helpers ──────────────────────────────────────────────────────────────────

type A2AEdgeProps = ComponentProps<typeof A2AEdge>;

function defaultEdgeProps(over: Record<string, unknown> = {}): A2AEdgeProps {
  // EdgeProps is a discriminated union — the test fixture only supplies
  // the geometry/identity fields the component actually reads, so a
  // double-cast through unknown lets call sites spread without restating
  // every optional field React Flow declares.
  return {
    id: "edge-1",
    source: "ws-source",
    target: "ws-target",
    sourceX: 0,
    sourceY: 0,
    targetX: 100,
    targetY: 100,
    sourcePosition: "right",
    targetPosition: "left",
    style: {},
    ...over,
  } as unknown as A2AEdgeProps;
}

beforeEach(() => {
  mockSelectNode.mockReset();
  mockSetPanelTab.mockReset();
  mockState.selectedNodeId = null;
});

afterEach(() => {
  cleanup();
});

// ── Tests ────────────────────────────────────────────────────────────────────

describe("A2AEdge — render", () => {
  it("renders the BaseEdge but no pill when data.label is empty", () => {
    render(
      <A2AEdge
        {...defaultEdgeProps({
          data: { count: 0, lastAt: 0, isHot: false, label: "" },
        })}
      />,
    );
    expect(screen.getByTestId("base-edge-edge-1")).toBeTruthy();
    // No clickable pill rendered.
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("renders the pill with the label text when data.label is present", () => {
    render(
      <A2AEdge
        {...defaultEdgeProps({
          data: {
            count: 5,
            lastAt: 0,
            isHot: false,
            label: "5 calls · 2m ago",
          },
        })}
      />,
    );
    const btn = screen.getByRole("button");
    expect(btn.textContent).toBe("5 calls · 2m ago");
  });

  it("applies the violet accent classes when isHot is true", () => {
    render(
      <A2AEdge
        {...defaultEdgeProps({
          data: { count: 12, lastAt: 0, isHot: true, label: "12 calls · 1m ago" },
        })}
      />,
    );
    const btn = screen.getByRole("button");
    expect(btn.className).toContain("border-violet-500/60");
    expect(btn.className).toContain("text-violet-200");
    // Negative: blue accent must NOT appear when hot
    expect(btn.className).not.toContain("border-blue-500/60");
  });

  it("applies the blue accent classes when isHot is false", () => {
    render(
      <A2AEdge
        {...defaultEdgeProps({
          data: { count: 2, lastAt: 0, isHot: false, label: "2 calls · 8m ago" },
        })}
      />,
    );
    const btn = screen.getByRole("button");
    expect(btn.className).toContain("border-blue-500/60");
    expect(btn.className).toContain("text-blue-200");
  });

  it("ARIA label pluralizes (singular)", () => {
    render(
      <A2AEdge
        {...defaultEdgeProps({
          data: { count: 1, lastAt: 0, isHot: false, label: "1 call · 30s ago" },
        })}
      />,
    );
    const btn = screen.getByRole("button");
    // count=1 → "1 delegation from <when>." (no trailing s)
    expect(btn.getAttribute("aria-label")).toMatch(/^1 delegation from/);
    expect(btn.getAttribute("aria-label")).not.toMatch(/^1 delegations/);
  });

  it("ARIA label pluralizes (plural)", () => {
    render(
      <A2AEdge
        {...defaultEdgeProps({
          data: { count: 7, lastAt: 0, isHot: false, label: "7 calls · 1m ago" },
        })}
      />,
    );
    const btn = screen.getByRole("button");
    expect(btn.getAttribute("aria-label")).toMatch(/^7 delegations from/);
  });
});

describe("A2AEdge — click behaviour", () => {
  it("click on the pill selects the source workspace", () => {
    render(
      <A2AEdge
        {...defaultEdgeProps({
          source: "ws-alpha",
          data: { count: 4, lastAt: 0, isHot: false, label: "4 calls · 1m ago" },
        })}
      />,
    );
    fireEvent.click(screen.getByRole("button"));
    expect(mockSelectNode).toHaveBeenCalledWith("ws-alpha");
  });

  it("on FRESH selection, also switches the panel tab to Activity", () => {
    // Pre-state: nothing selected. The click should yank the user
    // into Activity to expose the discovery affordance.
    mockState.selectedNodeId = null;
    render(
      <A2AEdge
        {...defaultEdgeProps({
          source: "ws-alpha",
          data: { count: 4, lastAt: 0, isHot: false, label: "4 calls" },
        })}
      />,
    );
    fireEvent.click(screen.getByRole("button"));
    expect(mockSetPanelTab).toHaveBeenCalledWith("activity");
  });

  it("on RE-CLICK of an already-selected source, does NOT switch the tab", () => {
    // Pre-state: source is already selected; user may have intentionally
    // switched to Chat / Memory. Re-clicking the edge must NOT yank them
    // back to Activity. (Selector-store getState() returns mockState.)
    mockState.selectedNodeId = "ws-alpha";
    render(
      <A2AEdge
        {...defaultEdgeProps({
          source: "ws-alpha",
          data: { count: 4, lastAt: 0, isHot: false, label: "4 calls" },
        })}
      />,
    );
    fireEvent.click(screen.getByRole("button"));
    // selectNode still fires (cheap, idempotent on the same id).
    expect(mockSelectNode).toHaveBeenCalledWith("ws-alpha");
    // setPanelTab MUST NOT — that's the regression-locked guarantee.
    expect(mockSetPanelTab).not.toHaveBeenCalled();
  });

  it("click stops propagation so the canvas pane doesn't deselect", () => {
    // Without stopPropagation, clicking the edge label would bubble
    // to the canvas Pane and (per existing handlers) clear the
    // selection — exactly the opposite of what the click is meant to do.
    const paneClick = vi.fn();
    render(
      <div onClick={paneClick}>
        <A2AEdge
          {...defaultEdgeProps({
            source: "ws-alpha",
            data: { count: 4, lastAt: 0, isHot: false, label: "4 calls" },
          })}
        />
      </div>,
    );
    fireEvent.click(screen.getByRole("button"));
    expect(paneClick).not.toHaveBeenCalled();
    expect(mockSelectNode).toHaveBeenCalledWith("ws-alpha");
  });
});
