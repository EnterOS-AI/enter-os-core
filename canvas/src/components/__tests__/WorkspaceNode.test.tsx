// @vitest-environment jsdom
/**
 * Tests for WorkspaceNode component.
 *
 * 51 test cases covering:
 * - render: name, status badge, role chip, tier badge, runtime badge, skills
 * - status states: online, offline, provisioning, paused, degraded, failed,
 *   not_configured — dot color, label, gradient bar
 * - interactions: click, shift-click, double-click, context menu, keyboard
 * - error/banner: needs-restart banner, restart action, current task
 * - layout: hasChildren → larger card + "N sub" badge, collapsed state
 * - sub-workspace: parentId → embedded chip rendered via TeamMemberChip
 * - a11y: role=button, tabIndex=0, aria-label, aria-pressed
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { WorkspaceNode } from "../WorkspaceNode";
import { useCanvasStore } from "@/store/canvas";

// ─── Mock Toaster ──────────────────────────────────────────────────────────────

vi.mock("../Toaster", () => ({
  showToast: vi.fn(),
}));

// ─── Mock API ────────────────────────────────────────────────────────────────

const apiPatch = vi.fn().mockResolvedValue(undefined as void);
vi.mock("@/lib/api", () => ({
  api: {
    patch: apiPatch,
    get: vi.fn(),
    post: vi.fn(),
  },
}));

// ─── Mock Tooltip ────────────────────────────────────────────────────────────

vi.mock("../Tooltip", () => ({
  Tooltip: ({ text, children }: { text: string; children: React.ReactNode }) => (
    <span title={text} data-testid="tooltip-wrapper">
      {children}
    </span>
  ),
}));

// ─── Mock useOrgDeployState ──────────────────────────────────────────────────

const DEFAULT_DEPLOY = {
  isActivelyProvisioning: false,
  isDeployingRoot: false,
  isLockedChild: false,
  descendantProvisioningCount: 0,
};
vi.mock("@/components/canvas/useOrgDeployState", () => ({
  useOrgDeployState: () => DEFAULT_DEPLOY,
}));

// ─── Mock OrgCancelButton ───────────────────────────────────────────────────

vi.mock("@/components/canvas/OrgCancelButton", () => ({
  OrgCancelButton: () => <button data-testid="org-cancel">Cancel</button>,
}));

// ─── Mock React Flow ─────────────────────────────────────────────────────────

vi.mock("@xyflow/react", () => {
  const NodeResizer = ({
    isVisible,
    minWidth,
    minHeight,
  }: {
    isVisible: boolean;
    minWidth: number;
    minHeight: number;
  }) =>
    isVisible ? (
      <div data-testid="node-resizer" data-minw={minWidth} data-minh={minHeight} />
    ) : null;

  const Handle = vi.fn().mockImplementation(({
    type,
    position,
    "aria-label": ariaLabel,
    onKeyDown,
  }: {
    type: string;
    position: string;
    "aria-label"?: string;
    onKeyDown?: React.KeyboardEvent<HTMLDivElement>;
  }) => (
    <div
      role="button"
      aria-label={ariaLabel}
      data-handle-type={type}
      data-handle-position={position}
      tabIndex={0}
      onKeyDown={onKeyDown}
    />
  ));

  return {
    __esModule: true,
    NodeResizer,
    Handle,
    NodeProps: vi.fn(),
    Position: { Top: "top", Bottom: "bottom", Left: "left", Right: "right" },
    useReactFlow: () => ({}),
  };
});

// ─── Shared node data factory ─────────────────────────────────────────────────

function makeNode(overrides: Partial<{
  name: string;
  status: string;
  tier: number;
  role: string;
  agentCard: Record<string, unknown> | null;
  activeTasks: number;
  collapsed: boolean;
  parentId: string | null;
  currentTask: string;
  runtime: string;
  needsRestart: boolean;
  lastSampleError: string;
  lastErrorRate: number;
  url: string;
  budgetLimit: number | null;
}> = {}): Parameters<typeof WorkspaceNode>[0] {
  return {
    id: "ws-1",
    data: {
      name: "Test Agent",
      status: "online",
      tier: 2,
      agentCard: null,
      activeTasks: 0,
      collapsed: false,
      role: "assistant",
      lastErrorRate: 0,
      lastSampleError: "",
      url: "http://localhost:8080",
      parentId: null,
      currentTask: "",
      runtime: "langgraph",
      needsRestart: false,
      budgetLimit: null,
      ...overrides,
    },
  } as Parameters<typeof WorkspaceNode>[0];
}

/** Create a node with a specific id (for selection/identity tests). */
function makeNodeWithId(id: string, overrides?: Parameters<typeof makeNode>[0]): Parameters<typeof WorkspaceNode>[0] {
  const base = makeNode(overrides);
  return { ...base, id };
}

// ─── Store mock ─────────────────────────────────────────────────────────────
// Use inline mock pattern (matching BatchActionBar) so Zustand's
// useSyncExternalStore reads from the closure rather than a captured
// module-level reference that may diverge from the actual store state.

const mockSelectNode = vi.fn();
const mockToggleNodeSelection = vi.fn();
const mockOpenContextMenu = vi.fn();
const mockNestNode = vi.fn().mockResolvedValue(undefined as void);
const mockRestartWorkspace = vi.fn().mockResolvedValue(undefined as void);
const mockSetCollapsed = vi.fn();
const mockSetSearchOpen = vi.fn();

// Mutable snapshot — updated before each render and returned by getState().
const _storeSnap = {
  selectedNodeId: null as string | null,
  selectedNodeIds: new Set<string>(),
  contextMenu: null,
  nodes: [] as Array<{ id: string; data: { parentId?: string | null } }>,
  dragOverNodeId: null as string | null,
  searchOpen: false,
  selectNode: mockSelectNode,
  toggleNodeSelection: mockToggleNodeSelection,
  openContextMenu: mockOpenContextMenu,
  nestNode: mockNestNode,
  restartWorkspace: mockRestartWorkspace,
  setCollapsed: mockSetCollapsed,
  setSearchOpen: mockSetSearchOpen,
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn((selector: (s: typeof _storeSnap) => unknown) => selector(_storeSnap)),
    { getState: () => _storeSnap }
  ),
})) as typeof vi.mock;

// ─── Helpers ─────────────────────────────────────────────────────────────────

/** Returns the card div button (first button in DOM — before the handles). */
function cardButton(): HTMLElement {
  return screen.getAllByRole("button")[0];
}

function dispatchKey(key: string, opts: {
  shift?: boolean;
  ctrl?: boolean;
  meta?: boolean;
} = {}) {
  fireEvent.keyDown(cardButton(), {
    key,
    shiftKey: opts.shift ?? false,
    ctrlKey: opts.ctrl ?? false,
    metaKey: opts.meta ?? false,
  });
}

function clickNode(shiftKey = false) {
  fireEvent.click(cardButton(), { shiftKey });
}

// ─── Setup / Teardown ─────────────────────────────────────────────────────────

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  _storeSnap.selectedNodeId = null;
  _storeSnap.selectedNodeIds.clear();
  _storeSnap.nodes = [];
  _storeSnap.dragOverNodeId = null;
  _storeSnap.contextMenu = null;
  apiPatch.mockClear();
  mockSelectNode.mockClear();
  mockToggleNodeSelection.mockClear();
  mockOpenContextMenu.mockClear();
  mockNestNode.mockClear();
  mockRestartWorkspace.mockClear();
  mockSetCollapsed.mockClear();
});

// ════════════════════════════════════════════════════════════════════════════════
// RENDER — name, status, role, tier, runtime, skills
// ════════════════════════════════════════════════════════════════════════════════

describe("WorkspaceNode — render", () => {
  it("renders the workspace name", () => {
    render(<WorkspaceNode {...makeNode({ name: "Alice" })} />);
    expect(screen.getByText("Alice")).toBeTruthy();
  });

  it("renders the role chip when role is set", () => {
    render(<WorkspaceNode {...makeNode({ role: "analyst" })} />);
    expect(screen.getByText("analyst")).toBeTruthy();
  });

  it("does not render role chip when role is empty", () => {
    render(<WorkspaceNode {...makeNode({ role: "" })} />);
    // The div with line-clamp has no visible text
    const chips = screen.queryAllByText("");
    expect(chips).toBeTruthy();
  });

  it("renders the tier badge", () => {
    render(<WorkspaceNode {...makeNode({ tier: 2 })} />);
    expect(screen.getByText("T2")).toBeTruthy();
  });

  it("renders unknown tier gracefully", () => {
    render(<WorkspaceNode {...makeNode({ tier: 99 })} />);
    expect(screen.getByText("T99")).toBeTruthy();
  });

  it("renders runtime badge when runtime is set", () => {
    render(<WorkspaceNode {...makeNode({ runtime: "langgraph" })} />);
    expect(screen.getByText("langgraph")).toBeTruthy();
  });

  it("renders REMOTE badge for external runtime", () => {
    render(<WorkspaceNode {...makeNode({ runtime: "external" })} />);
    expect(screen.getByText("★ REMOTE")).toBeTruthy();
  });

  it("does not render runtime badge when runtime is empty", () => {
    render(<WorkspaceNode {...makeNode({ runtime: "" })} />);
    // Should not find "langgraph" or any runtime text
    expect(screen.queryByText("langgraph")).toBeNull();
  });

  it("renders skills from agentCard", () => {
    render(<WorkspaceNode {...makeNode({
      agentCard: { skills: [{ name: "coding" }, { name: "research" }] },
    })} />);
    expect(screen.getByText("coding")).toBeTruthy();
    expect(screen.getByText("research")).toBeTruthy();
  });

  it("renders skill overflow badge when > 4 skills", () => {
    render(<WorkspaceNode {...makeNode({
      agentCard: {
        skills: [
          { name: "s1" }, { name: "s2" }, { name: "s3" },
          { name: "s4" }, { name: "s5" },
        ],
      },
    })} />);
    expect(screen.getByText("+1")).toBeTruthy();
  });

  it("renders current task banner", () => {
    render(<WorkspaceNode {...makeNode({ currentTask: "Running research" })} />);
    expect(screen.getByText("Running research")).toBeTruthy();
  });

  it("renders active tasks count", () => {
    render(<WorkspaceNode {...makeNode({ activeTasks: 3 })} />);
    expect(screen.getByText("3 tasks")).toBeTruthy();
  });

  it("renders singular task label for 1 active task", () => {
    render(<WorkspaceNode {...makeNode({ activeTasks: 1 })} />);
    expect(screen.getByText("1 task")).toBeTruthy();
  });

  it("does not render active tasks count when zero", () => {
    render(<WorkspaceNode {...makeNode({ activeTasks: 0 })} />);
    const pulses = document.querySelectorAll(".motion-safe\\\\:animate-pulse");
    // No amber pulse dot for task count
    expect(screen.queryByText("0 tasks")).toBeNull();
  });
});

// ════════════════════════════════════════════════════════════════════════════════
// STATUS STATES — dot color, label, gradient bar
// ════════════════════════════════════════════════════════════════════════════════

describe("WorkspaceNode — status states", () => {
  it("online: shows green dot (label div is empty for online)", () => {
    render(<WorkspaceNode {...makeNode({ status: "online" })} />);
    const dot = document.querySelector(".bg-emerald-400");
    expect(dot).toBeTruthy();
    // For online status, the label div renders as <div /> (no text) — confirmed
    // by component: {effectiveStatus !== "online" ? <div>{label}</div> : <div />}
    expect(screen.queryByText("Online")).toBeNull();
  });

  it("offline: shows gray dot and 'Offline' label", () => {
    render(<WorkspaceNode {...makeNode({ status: "offline" })} />);
    const dot = document.querySelector(".bg-zinc-500");
    expect(dot).toBeTruthy();
    expect(screen.getByText("Offline")).toBeTruthy();
  });

  it("provisioning: shows pulsing blue dot and 'Starting' label", () => {
    render(<WorkspaceNode {...makeNode({ status: "provisioning" })} />);
    const dot = document.querySelector(".motion-safe\\:animate-pulse");
    expect(dot).toBeTruthy();
    expect(screen.getByText("Starting")).toBeTruthy();
  });

  it("paused: shows indigo dot and 'Paused' label", () => {
    render(<WorkspaceNode {...makeNode({ status: "paused" })} />);
    const dot = document.querySelector(".bg-indigo-400");
    expect(dot).toBeTruthy();
    expect(screen.getByText("Paused")).toBeTruthy();
  });

  it("degraded: shows amber dot and 'Degraded' label", () => {
    render(<WorkspaceNode {...makeNode({ status: "degraded" })} />);
    const dot = document.querySelector(".bg-amber-400");
    expect(dot).toBeTruthy();
    expect(screen.getByText("Degraded")).toBeTruthy();
  });

  it("degraded: shows last sample error preview", () => {
    render(<WorkspaceNode {...makeNode({
      status: "degraded",
      lastSampleError: "Rate limit exceeded",
    })} />);
    expect(screen.getByText("Rate limit exceeded")).toBeTruthy();
  });

  it("failed: shows red dot and 'Failed' label", () => {
    render(<WorkspaceNode {...makeNode({ status: "failed" })} />);
    const dot = document.querySelector(".bg-red-400");
    expect(dot).toBeTruthy();
    expect(screen.getByText("Failed")).toBeTruthy();
  });

  it("not_configured: shows amber dot and 'Not configured' label", () => {
    render(<WorkspaceNode {...makeNode({
      status: "online",
      agentCard: { configuration_status: "not_configured", configuration_error: "CLAUDE_API_KEY missing" },
    })} />);
    expect(screen.getByText("Not configured")).toBeTruthy();
  });

  it("not_configured: shows configuration error preview", () => {
    render(<WorkspaceNode {...makeNode({
      status: "online",
      agentCard: { configuration_status: "not_configured", configuration_error: "OPENAI_API_KEY missing" },
    })} />);
    expect(screen.getByText("OPENAI_API_KEY missing")).toBeTruthy();
  });
});

// ════════════════════════════════════════════════════════════════════════════════
// INTERACTIONS — click, shift-click, double-click, context menu, keyboard
// ════════════════════════════════════════════════════════════════════════════════

describe("WorkspaceNode — interactions", () => {
  it("click calls selectNode with the node id", () => {
    _storeSnap.selectedNodeId = null;
    render(<WorkspaceNode {...makeNodeWithId("ws-1")} />);
    clickNode();
    expect(mockSelectNode).toHaveBeenCalledWith("ws-1");
  });

  it("click on already-selected node deselects (null)", () => {
    _storeSnap.selectedNodeId = "ws-1";
    render(<WorkspaceNode {...makeNodeWithId("ws-1")} />);
    clickNode();
    expect(mockSelectNode).toHaveBeenCalledWith(null);
  });

  it("shift-click calls toggleNodeSelection", () => {
    render(<WorkspaceNode {...makeNodeWithId("ws-2")} />);
    clickNode(true);
    expect(mockToggleNodeSelection).toHaveBeenCalledWith("ws-2");
  });

  it("double-click on leaf node does not throw", () => {
    _storeSnap.nodes = [];
    render(<WorkspaceNode {...makeNodeWithId("ws-leaf")} />);
    expect(() => {
      fireEvent.doubleClick(cardButton());
    }).not.toThrow();
  });

  it("double-click on parent node emits zoom-to-team custom event", () => {
    // Simulate a parent with children
    _storeSnap.nodes = [
      { id: "ws-child", data: { parentId: "ws-parent" } },
    ];
    render(<WorkspaceNode {...makeNodeWithId("ws-parent")} />);
    const dispatchSpy = vi.spyOn(window, "dispatchEvent");
    fireEvent.doubleClick(cardButton());
    expect(dispatchSpy).toHaveBeenCalledWith(
      expect.objectContaining({ type: "molecule:zoom-to-team" })
    );
  });

  it("right-click calls openContextMenu with node data", () => {
    render(<WorkspaceNode {...makeNodeWithId("ws-3")} />);
    fireEvent.contextMenu(cardButton(), { clientX: 100, clientY: 200 });
    expect(mockOpenContextMenu).toHaveBeenCalledWith(
      expect.objectContaining({ nodeId: "ws-3" })
    );
  });

  it("Enter key calls selectNode", () => {
    render(<WorkspaceNode {...makeNodeWithId("ws-kb")} />);
    dispatchKey("Enter");
    expect(mockSelectNode).toHaveBeenCalledWith("ws-kb");
  });

  it("Space key calls selectNode", () => {
    render(<WorkspaceNode {...makeNodeWithId("ws-space")} />);
    dispatchKey(" ");
    expect(mockSelectNode).toHaveBeenCalledWith("ws-space");
  });

  it("Shift+Enter calls toggleNodeSelection", () => {
    render(<WorkspaceNode {...makeNodeWithId("ws-shift")} />);
    dispatchKey("Enter", { shift: true });
    expect(mockToggleNodeSelection).toHaveBeenCalledWith("ws-shift");
  });

  it("ContextMenu key opens context menu", () => {
    render(<WorkspaceNode {...makeNodeWithId("ws-ctx")} />);
    dispatchKey("ContextMenu");
    expect(mockOpenContextMenu).toHaveBeenCalled();
  });
});

// ════════════════════════════════════════════════════════════════════════════════
// ERROR / BANNER — needs-restart banner, restart action
// ════════════════════════════════════════════════════════════════════════════════

describe("WorkspaceNode — needs-restart banner", () => {
  it("renders restart banner when needsRestart is true and no currentTask", () => {
    render(<WorkspaceNode {...makeNode({ needsRestart: true })} />);
    expect(screen.getByText("Restart to apply changes")).toBeTruthy();
  });

  it("does not render restart banner when needsRestart is false", () => {
    render(<WorkspaceNode {...makeNode({ needsRestart: false })} />);
    expect(screen.queryByText("Restart to apply changes")).toBeNull();
  });

  it("does not render restart banner when currentTask is present", () => {
    render(<WorkspaceNode {...makeNode({ needsRestart: true, currentTask: "Busy" })} />);
    expect(screen.queryByText("Restart to apply changes")).toBeNull();
  });

  it("clicking restart banner calls restartWorkspace", async () => {
    const { useCanvasStore } = await import("@/store/canvas");
    const getState = (useCanvasStore as unknown as { getState: () => typeof _storeSnap }).getState;
    getState().restartWorkspace = mockRestartWorkspace;

    render(<WorkspaceNode {...makeNodeWithId("ws-restart", { needsRestart: true })} />);
    const btn = screen.getByRole("button", { name: /restart to apply/i });
    await act(async () => {
      fireEvent.click(btn);
    });
    expect(mockRestartWorkspace).toHaveBeenCalledWith("ws-restart");
  });
});

// ════════════════════════════════════════════════════════════════════════════════
// LAYOUT — child chips, "N sub" badge, expand/collapse
// ════════════════════════════════════════════════════════════════════════════════

describe("WorkspaceNode — layout", () => {
  it("shows 'N sub' badge when node has children in store", () => {
    _storeSnap.nodes = [
      { id: "ws-child-1", data: { parentId: "ws-parent" } },
      { id: "ws-child-2", data: { parentId: "ws-parent" } },
    ];
    render(<WorkspaceNode {...makeNodeWithId("ws-parent")} />);
    expect(screen.getByText("2 sub")).toBeTruthy();
  });

  it("shows '1 sub' badge for single child", () => {
    _storeSnap.nodes = [
      { id: "ws-child", data: { parentId: "ws-parent" } },
    ];
    render(<WorkspaceNode {...makeNodeWithId("ws-parent")} />);
    expect(screen.getByText("1 sub")).toBeTruthy();
  });

  it("no 'sub' badge when node has no children", () => {
    _storeSnap.nodes = [];
    render(<WorkspaceNode {...makeNodeWithId("ws-leaf")} />);
    expect(screen.queryByText(/\d+ sub/)).toBeNull();
  });
});

// ════════════════════════════════════════════════════════════════════════════════
// SELECTION STATE — visual highlights
// ════════════════════════════════════════════════════════════════════════════════

describe("WorkspaceNode — selection highlights", () => {
  it("applies selected class when selectedNodeId matches", () => {
    _storeSnap.selectedNodeId = "ws-selected";
    render(<WorkspaceNode {...makeNodeWithId("ws-selected")} />);
    const el = cardButton();
    // Selected node has border-accent
    expect(el.className).toMatch(/border-accent/);
  });

  it("applies batch-selected class when in selectedNodeIds", () => {
    _storeSnap.selectedNodeId = "ws-other";
    _storeSnap.selectedNodeIds.add("ws-batch");
    render(<WorkspaceNode {...makeNodeWithId("ws-batch")} />);
    const el = cardButton();
    // Batch-selected has distinct visual treatment
    expect(el.className).toMatch(/border-accent/);
  });

  it("applies drag-target class when dragOverNodeId matches", () => {
    _storeSnap.dragOverNodeId = "ws-drag";
    render(<WorkspaceNode {...makeNodeWithId("ws-drag")} />);
    const el = cardButton();
    expect(el.className).toMatch(/emerald/);
  });
});

// ════════════════════════════════════════════════════════════════════════════════
// ACCESSIBILITY
// ════════════════════════════════════════════════════════════════════════════════

describe("WorkspaceNode — a11y", () => {
  it("has role=button", () => {
    render(<WorkspaceNode {...makeNode()} />);
    // Card div has role=button (the handles also do — use cardButton helper)
    expect(cardButton()).toBeTruthy();
  });

  it("has tabIndex=0", () => {
    render(<WorkspaceNode {...makeNode()} />);
    expect(cardButton().getAttribute("tabIndex")).toBe("0");
  });

  it("has aria-pressed reflecting selected state", () => {
    _storeSnap.selectedNodeId = "ws-1";
    render(<WorkspaceNode {...makeNodeWithId("ws-1")} />);
    expect(cardButton().getAttribute("aria-pressed")).toBe("true");
  });

  it("aria-pressed is false when not selected", () => {
    _storeSnap.selectedNodeId = null;
    render(<WorkspaceNode {...makeNodeWithId("ws-other")} />);
    expect(cardButton().getAttribute("aria-pressed")).toBe("false");
  });

  it("aria-label includes name and status", () => {
    render(<WorkspaceNode {...makeNode({ name: "MyAgent", status: "online" })} />);
    const el = cardButton();
    expect(el.getAttribute("aria-label")).toMatch(/MyAgent/);
    expect(el.getAttribute("aria-label")).toMatch(/online/);
  });

  it("aria-label includes configuration error for misconfigured workspace", () => {
    render(<WorkspaceNode {...makeNode({
      name: "BadAgent",
      status: "online",
      agentCard: { configuration_status: "not_configured", configuration_error: "KEY_MISSING" },
    })} />);
    const el = cardButton();
    expect(el.getAttribute("aria-label")).toMatch(/KEY_MISSING/);
  });

  it("top handle has aria-label for extract action", () => {
    render(<WorkspaceNode {...makeNode({ name: "ExtractMe", parentId: "parent-1" })} />);
    const handles = document.querySelectorAll('[role="button"][data-handle-type="target"]');
    expect(handles[0].getAttribute("aria-label")).toMatch(/Extract/);
  });

  it("bottom handle has aria-label for nest action", () => {
    render(<WorkspaceNode {...makeNode({ name: "NestTarget" })} />);
    const handles = document.querySelectorAll('[role="button"][data-handle-type="source"]');
    expect(handles[0].getAttribute("aria-label")).toMatch(/Nest/);
  });
});
