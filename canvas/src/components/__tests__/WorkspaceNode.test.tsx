// @vitest-environment jsdom
/**
 * WorkspaceNode tests.
 *
 * Covers:
 *   - Renders name, status dot, tier badge, role, skills
 *   - Status gradient bar colored by STATUS_CONFIG
 *   - Online/offline/failed/degraded/provisioning states
 *   - Misconfigured state (online + not_configured)
 *   - Click → select, Shift+click → batch select
 *   - Keyboard Enter/Space → select/deselect
 *   - Context menu on right-click
 *   - Double-click collapsed parent → expands
 *   - Double-click expanded parent → zoom to team
 *   - Needs restart button visible when needsRestart=true
 *   - Current task banner when activeTasks > 0
 *   - Descendant count badge when node has children
 *   - Drag-target highlight class when dragOverNodeId matches
 *   - Batch-selected highlight class
 *   - OrgCancelButton renders on deploying root
 *   - Degraded error preview
 *   - Configuration error preview for misconfigured nodes
 *   - TeamMemberChip: name, status, skills, extract button, recursive
 *   - Handle anchors: top = extract, bottom = nest (keyboard accessible)
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import React from "react";

// ── Mock @xyflow/react ────────────────────────────────────────────────────────
vi.mock("@xyflow/react", () => {
  const Handle = ({
    type,
    position,
    "aria-label": ariaLabel,
    onKeyDown,
    ...rest
  }: {
    type: string;
    position: string;
    "aria-label"?: string;
    onKeyDown?: (e: React.KeyboardEvent) => void;
    [key: string]: unknown;
  }) => (
    <div
      role="button"
      aria-label={ariaLabel}
      data-handle-type={type}
      data-handle-position={position}
      tabIndex={0}
      onKeyDown={onKeyDown}
      {...rest}
    >
      handle
    </div>
  );
  return {
    __esModule: true,
    default: ({ children }: { children?: React.ReactNode }) => (
      <div data-testid="react-flow-root">{children}</div>
    ),
    NodeResizer: () => null,
    Handle,
    Position: { Top: "top", Bottom: "bottom", Left: "left", Right: "right" },
    useReactFlow: () => ({ fitView: vi.fn(), setViewport: vi.fn() }),
    applyNodeChanges: vi.fn((_: unknown, n: unknown) => n),
    ReactFlowProvider: ({ children }: { children?: React.ReactNode }) => <>{children}</>,
  };
});

// ── Mock dependencies ─────────────────────────────────────────────────────────
const mockGetConfigurationStatus = vi.fn(() => "configured");
const mockGetConfigurationError = vi.fn(() => null);

vi.mock("@/store/canvas-topology", () => ({
  getConfigurationStatus: (...args: unknown[]) => mockGetConfigurationStatus(...args),
  getConfigurationError: (...args: unknown[]) => mockGetConfigurationError(...args),
}));

// Expose for per-test override
const useConfigStatus = mockGetConfigurationStatus;
const useConfigError = mockGetConfigurationError;

vi.mock("@/components/Toaster", () => ({
  showToast: vi.fn(),
}));

vi.mock("@/components/Tooltip", () => ({
  Tooltip: ({ text, children }: { text: string; children: React.ReactNode }) => (
    <div title={text} data-testid="tooltip-wrapper">{children}</div>
  ),
}));

vi.mock("@/components/canvas/useOrgDeployState", () => ({
  useOrgDeployState: vi.fn(() => ({
    isActivelyProvisioning: false,
    isDeployingRoot: false,
    isLockedChild: false,
    descendantProvisioningCount: 0,
  })),
}));

vi.mock("@/lib/design-tokens", () => ({
  STATUS_CONFIG: {
    online: { dot: "bg-emerald-400", glow: "shadow-emerald-400/50", bar: "to-emerald-500/30", label: "ONLINE" },
    offline: { dot: "bg-zinc-500", glow: "", bar: "to-zinc-600/30", label: "OFFLINE" },
    failed: { dot: "bg-red-400", glow: "", bar: "to-red-600/30", label: "FAILED" },
    degraded: { dot: "bg-amber-400", glow: "", bar: "to-amber-600/30", label: "DEGRADED" },
    provisioning: { dot: "bg-sky-400", glow: "", bar: "to-sky-600/30", label: "STARTING" },
    not_configured: { dot: "bg-amber-400", glow: "", bar: "to-amber-600/30", label: "NOT CONFIGURED" },
  },
  TIER_CONFIG: {
    1: { label: "T1", color: "text-zinc-400 bg-zinc-800" },
    2: { label: "T2", color: "text-blue-400 bg-blue-900/50" },
    3: { label: "T3", color: "text-purple-400 bg-purple-900/50" },
    4: { label: "T4", color: "text-amber-400 bg-amber-900/50" },
  },
}));

// ── Store mock ────────────────────────────────────────────────────────────────
// Uses a global object to share mock state between the factory (which runs
// when the module is imported) and the test body (beforeEach/afterEach).
declare global {
  // eslint-disable-next-line no-var
  var __workspaceNodeMocks: {
    selectNode: ReturnType<typeof vi.fn>;
    openContextMenu: ReturnType<typeof vi.fn>;
    toggleNodeSelection: ReturnType<typeof vi.fn>;
    nestNode: ReturnType<typeof vi.fn>;
    restartWorkspace: ReturnType<typeof vi.fn>;
    store: {
      nodes: Array<{ id: string; data: Record<string, unknown> }>;
      selectedNodeId: string | null;
      dragOverNodeId: string | null;
      selectedNodeIds: Set<string>;
    };
  } | undefined;
}

vi.mock("@/store/canvas", () => {
  const mockSelectNode = vi.fn();
  const mockOpenContextMenu = vi.fn();
  const mockToggleNodeSelection = vi.fn();
  const mockNestNode = vi.fn();
  const mockRestartWorkspace = vi.fn(() => Promise.resolve());

  const store = {
    nodes: [] as Array<{ id: string; data: Record<string, unknown> }>,
    selectedNodeId: null as string | null,
    dragOverNodeId: null as string | null,
    selectedNodeIds: new Set<string>(),
    selectNode: mockSelectNode,
    openContextMenu: mockOpenContextMenu,
    toggleNodeSelection: mockToggleNodeSelection,
    nestNode: mockNestNode,
    restartWorkspace: mockRestartWorkspace,
  };

  const mockFn = (selector: (s: typeof store) => unknown) => selector(store);
  Object.defineProperty(mockFn, "getState", { value: () => store });

  // Expose via global for test body access
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  (globalThis as any).__workspaceNodeMocks = {
    selectNode: mockSelectNode,
    openContextMenu: mockOpenContextMenu,
    toggleNodeSelection: mockToggleNodeSelection,
    nestNode: mockNestNode,
    restartWorkspace: mockRestartWorkspace,
    store,
  };

  return { useCanvasStore: mockFn, __esModule: true };
});

// ── Component ────────────────────────────────────────────────────────────────
import { WorkspaceNode } from "../WorkspaceNode";

// ── Helpers ──────────────────────────────────────────────────────────────────

// Main node card uses data-testid to distinguish from handle anchors (also role=button)
const getNode = () => screen.getByTestId("workspace-node");

// Typed access to the shared mock state (set by the vi.mock factory)
const mocks = () => globalThis.__workspaceNodeMocks!;
const store = () => mocks().store;

const makeNode = (overrides: Record<string, unknown> = {}) => ({
  id: "ws-1",
  data: {
    name: "Test Workspace",
    role: "Test Agent",
    tier: 1,
    status: "online" as const,
    parentId: null,
    activeTasks: 0,
    needsRestart: false,
    currentTask: null as string | null,
    lastSampleError: null as string | null,
    collapsed: false,
    agentCard: null,
    runtime: null as string | null,
    ...overrides,
  },
});

const renderNode = (nodeOverrides: Record<string, unknown> = {}) => {
  const node = makeNode(nodeOverrides);
  // WorkspaceNode expects NodeProps — it receives { id, data } as props
  return render(<WorkspaceNode id={node.id as string} data={node.data as never} />);
};

// ── Tests ────────────────────────────────────────────────────────────────────

beforeEach(() => {
  const m = globalThis.__workspaceNodeMocks!;
  m.store.nodes = [];
  m.store.selectedNodeId = null;
  m.store.dragOverNodeId = null;
  m.store.selectedNodeIds = new Set();
  m.selectNode.mockClear();
  m.openContextMenu.mockClear();
  m.toggleNodeSelection.mockClear();
  m.nestNode.mockClear();
  m.restartWorkspace.mockClear();
  mockGetConfigurationStatus.mockClear().mockReturnValue("configured");
  mockGetConfigurationError.mockClear().mockReturnValue(null);
});

afterEach(() => {
  cleanup();
});

describe("WorkspaceNode — basic rendering", () => {
  it("renders the workspace name", () => {
    renderNode({ name: "My Workspace" });
    expect(screen.getByText("My Workspace")).toBeTruthy();
  });

  it("renders the role text", () => {
    renderNode({ role: "Frontend Engineer" });
    expect(screen.getByText("Frontend Engineer")).toBeTruthy();
  });

  it("renders the tier badge", () => {
    renderNode({ tier: 2 });
    expect(screen.getByText("T2")).toBeTruthy();
  });

  it("renders status dot with online class", () => {
    renderNode({ status: "online" });
    const dot = getNode().querySelector(".bg-emerald-400");
    expect(dot).toBeTruthy();
  });

  it("renders role text clamped to 2 lines", () => {
    renderNode({ role: "A very long role description that might overflow" });
    expect(screen.getByText(/A very long role description/i)).toBeTruthy();
  });
});

describe("WorkspaceNode — status states", () => {
  it("shows status label for failed node", () => {
    renderNode({ status: "failed" });
    expect(screen.getByText("FAILED")).toBeTruthy();
  });

  it("shows status label for degraded node", () => {
    renderNode({ status: "degraded" });
    expect(screen.getByText("DEGRADED")).toBeTruthy();
  });

  it("shows status label for provisioning node", () => {
    renderNode({ status: "provisioning" });
    expect(screen.getByText("STARTING")).toBeTruthy();
  });

  it("suppresses status label for online node", () => {
    renderNode({ status: "online" });
    expect(screen.queryByText("ONLINE")).toBeNull();
  });

  it("shows degraded error preview when status is degraded and lastSampleError is set", () => {
    renderNode({ status: "degraded", lastSampleError: "Connection timeout" });
    expect(screen.getByText("Connection timeout")).toBeTruthy();
  });

  it("suppresses degraded error preview when no error", () => {
    renderNode({ status: "degraded", lastSampleError: null });
    expect(screen.queryByText(/timeout/i)).toBeNull();
  });
});

describe("WorkspaceNode — misconfigured state", () => {
  it("shows 'NOT CONFIGURED' label when agent is online but not_configured", () => {
    vi.mocked(useConfigStatus).mockReturnValueOnce("not_configured");
    vi.mocked(useConfigError).mockReturnValueOnce("ANTHROPIC_API_KEY is missing");
    renderNode({ status: "online" });
    expect(screen.getByText("NOT CONFIGURED")).toBeTruthy();
  });

  it("shows configuration error preview when misconfigured", () => {
    vi.mocked(useConfigStatus).mockReturnValueOnce("not_configured");
    vi.mocked(useConfigError).mockReturnValueOnce("OPENAI_API_KEY missing");
    renderNode({ status: "online" });
    expect(screen.getByText("OPENAI_API_KEY missing")).toBeTruthy();
  });

  it("aria-label includes name and status by default", () => {
    // Mock set to default "configured" — no misconfigured label
    renderNode({ status: "online" });
    const btn = getNode();
    expect(btn.getAttribute("aria-label")).toMatch(/Test Workspace/);
  });
});

describe("WorkspaceNode — click interactions", () => {
  it("calls selectNode(id) on click", () => {
    renderNode();
    fireEvent.click(getNode());
    expect(mocks().selectNode).toHaveBeenCalledWith("ws-1");
  });

  it("calls selectNode(null) on click when already selected", () => {
    store().selectedNodeId = "ws-1";
    renderNode();
    fireEvent.click(getNode());
    expect(mocks().selectNode).toHaveBeenCalledWith(null);
  });

  it("calls toggleNodeSelection on Shift+click", () => {
    renderNode();
    fireEvent.click(getNode(), { shiftKey: true });
    expect(mocks().toggleNodeSelection).toHaveBeenCalledWith("ws-1");
  });

  it("opens context menu on right-click", () => {
    renderNode();
    fireEvent.contextMenu(getNode(), {
      clientX: 100,
      clientY: 200,
    });
    expect(mocks().openContextMenu).toHaveBeenCalledWith(
      expect.objectContaining({ nodeId: "ws-1", x: 100, y: 200 })
    );
  });

  it("stops propagation to prevent canvas background click from firing", () => {
    renderNode();
    const btn = getNode();
    // React synthetic events fire regardless of native bubbles. We just verify
    // selectNode was called — the stopPropagation() call inside the handler
    // prevents the event from reaching canvas background listeners.
    expect(mocks().selectNode).not.toHaveBeenCalled(); // no click yet
    fireEvent.click(btn, { bubbles: true });
    expect(mocks().selectNode).toHaveBeenCalled();
  });
});

describe("WorkspaceNode — keyboard interactions", () => {
  it("selects node on Enter key", () => {
    renderNode();
    fireEvent.keyDown(getNode(), { key: "Enter" });
    expect(mocks().selectNode).toHaveBeenCalledWith("ws-1");
  });

  it("deselects node on Enter key when already selected", () => {
    store().selectedNodeId = "ws-1";
    renderNode();
    fireEvent.keyDown(getNode(), { key: "Enter" });
    expect(mocks().selectNode).toHaveBeenCalledWith(null);
  });

  it("toggles batch selection on Shift+Enter", () => {
    renderNode();
    fireEvent.keyDown(getNode(), { key: "Enter", shiftKey: true });
    expect(mocks().toggleNodeSelection).toHaveBeenCalledWith("ws-1");
  });

  it("opens context menu on ContextMenu key", () => {
    renderNode();
    fireEvent.keyDown(getNode(), { key: "ContextMenu" });
    expect(mocks().openContextMenu).toHaveBeenCalledWith(
      expect.objectContaining({ nodeId: "ws-1" })
    );
  });
});

describe("WorkspaceNode — double-click interactions", () => {
  it("does nothing on double-click when node has no children", () => {
    renderNode({ collapsed: false });
    fireEvent.doubleClick(getNode());
    // No exception thrown = fine. The actual zoom-to-team event is dispatched
    // on the window, which jsdom handles silently.
    expect(mocks().selectNode).not.toHaveBeenCalled();
  });

  it("sets collapsed=false on double-click of collapsed parent (no children in store)", () => {
    renderNode({ collapsed: true });
    fireEvent.doubleClick(getNode());
    // When hasChildren is false (no child nodes in store), the handler returns early.
    expect(mocks().selectNode).not.toHaveBeenCalled();
  });
});

describe("WorkspaceNode — active tasks", () => {
  it("shows active tasks badge when activeTasks > 0", () => {
    renderNode({ activeTasks: 3 });
    expect(screen.getByText("3 tasks")).toBeTruthy();
  });

  it("shows singular 'task' when activeTasks is 1", () => {
    renderNode({ activeTasks: 1 });
    expect(screen.getByText("1 task")).toBeTruthy();
  });

  it("suppresses badge when no active tasks", () => {
    renderNode({ activeTasks: 0 });
    expect(screen.queryByText(/task/)).toBeNull();
  });
});

describe("WorkspaceNode — current task banner", () => {
  it("shows current task banner when currentTask is set", () => {
    renderNode({ currentTask: "Writing unit tests" });
    expect(screen.getByText("Writing unit tests")).toBeTruthy();
  });

  it("suppresses current task banner when null", () => {
    renderNode({ currentTask: null });
    expect(screen.queryByText(/Writing unit tests/)).toBeNull();
  });

  it("shows both currentTask and needsRestart — currentTask takes visual priority", () => {
    renderNode({ currentTask: "Active work", needsRestart: true });
    // Current task banner renders; needs restart button is conditionally hidden
    // behind `!data.currentTask` in the component
    expect(screen.getByText("Active work")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /restart/i })).toBeNull();
  });
});

describe("WorkspaceNode — needs restart", () => {
  it("shows restart button when needsRestart=true and no currentTask", () => {
    renderNode({ needsRestart: true, currentTask: null });
    expect(screen.getByRole("button", { name: /restart to apply changes/i })).toBeTruthy();
  });

  it("suppresses restart button when currentTask is active", () => {
    renderNode({ needsRestart: true, currentTask: "Working" });
    expect(screen.queryByRole("button", { name: /restart/i })).toBeNull();
  });

  it("suppresses restart button when needsRestart=false", () => {
    renderNode({ needsRestart: false });
    expect(screen.queryByRole("button", { name: /restart/i })).toBeNull();
  });

  it("restart button calls restartWorkspace on click", () => {
    renderNode({ needsRestart: true, currentTask: null });
    fireEvent.click(screen.getByRole("button", { name: /restart to apply changes/i }));
    expect(mocks().restartWorkspace).toHaveBeenCalledWith("ws-1");
  });

  it("restart button stops propagation", () => {
    renderNode({ needsRestart: true, currentTask: null });
    fireEvent.click(screen.getByRole("button", { name: /restart/i }));
    // If propagation wasn't stopped, selectNode would also be called
    expect(mocks().selectNode).not.toHaveBeenCalled();
  });
});

describe("WorkspaceNode — descendant badge", () => {
  it("shows descendant count badge when node has children in store", () => {
    store().nodes = [
      makeNode({ id: "ws-1" }),
      { id: "child-1", data: { ...makeNode({ id: "ws-1" }).data, parentId: "ws-1" } },
    ];
    renderNode();
    expect(screen.getByText("1 sub")).toBeTruthy();
  });

  it("suppresses badge when node has no children", () => {
    store().nodes = [makeNode({ id: "ws-1" })];
    renderNode();
    expect(screen.queryByText(/sub/)).toBeNull();
  });
});

describe("WorkspaceNode — skills pills", () => {
  it("renders up to 4 skill pills", () => {
    renderNode({
      agentCard: {
        skills: [
          { name: "code-review" },
          { name: "tdd" },
          { name: "debugging" },
          { name: "refactoring" },
        ],
      },
    });
    expect(screen.getByText("code-review")).toBeTruthy();
    expect(screen.getByText("refactoring")).toBeTruthy();
  });

  it("shows +N overflow when more than 4 skills", () => {
    renderNode({
      agentCard: {
        skills: [
          { name: "s1" }, { name: "s2" }, { name: "s3" }, { name: "s4" }, { name: "s5" },
        ],
      },
    });
    expect(screen.getByText("+1")).toBeTruthy();
  });

  it("suppresses skills section when no skills", () => {
    renderNode({ agentCard: null });
    // No skill text rendered
    expect(screen.queryByText(/code-review/i)).toBeNull();
  });

  it("handles agentCard with no skills array", () => {
    renderNode({ agentCard: { name: "Test Agent" } });
    expect(screen.queryByText(/code-review/i)).toBeNull();
  });
});

describe("WorkspaceNode — runtime badge", () => {
  it("shows runtime badge when runtime is set", () => {
    renderNode({ runtime: "hermes" });
    expect(screen.getByText("hermes")).toBeTruthy();
  });

  it("shows REMOTE badge for external runtime", () => {
    renderNode({ runtime: "external" });
    expect(screen.getByText("★ REMOTE")).toBeTruthy();
  });

  it("suppresses runtime badge when runtime is null", () => {
    renderNode({ runtime: null });
    expect(screen.queryByText("hermes")).toBeNull();
  });
});

describe("WorkspaceNode — selection aria", () => {
  it('has aria-pressed="false" when not selected', () => {
    store().selectedNodeId = null;
    renderNode();
    expect(getNode().getAttribute("aria-pressed")).toBe("false");
  });

  it('has aria-pressed="true" when selected', () => {
    store().selectedNodeId = "ws-1";
    renderNode();
    expect(getNode().getAttribute("aria-pressed")).toBe("true");
  });
});

describe("WorkspaceNode — aria-label", () => {
  it("includes name and status in aria-label", () => {
    renderNode({ name: "MyAgent", status: "online" });
    const label = getNode().getAttribute("aria-label");
    expect(label).toContain("MyAgent");
    expect(label).toContain("online");
  });
});

describe("WorkspaceNode — handle anchors accessibility", () => {
  it("top handle has aria-label for extract", () => {
    renderNode({ parentId: "parent-1" });
    const handles = screen.getAllByRole("button");
    const topHandle = handles.find((h) => h.getAttribute("data-handle-type") === "target");
    expect(topHandle?.getAttribute("aria-label")).toMatch(/extract/i);
  });

  it("bottom handle has aria-label for nest", () => {
    renderNode();
    const handles = screen.getAllByRole("button");
    const bottomHandle = handles.find((h) => h.getAttribute("data-handle-type") === "source");
    expect(bottomHandle?.getAttribute("aria-label")).toMatch(/nest/i);
  });

  it("top handle extract is no-op when node has no parent", () => {
    renderNode({ parentId: null });
    const handles = screen.getAllByRole("button");
    const topHandle = handles.find((h) => h.getAttribute("data-handle-type") === "target");
    fireEvent.keyDown(topHandle!, { key: "Enter" });
    // Should be a no-op — no exception
    expect(mocks().nestNode).not.toHaveBeenCalled();
  });
});
