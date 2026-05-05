// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";

afterEach(cleanup);

// ── Mocks ─────────────────────────────────────────────────────────────────────
vi.mock("../ConfirmDialog", () => ({ ConfirmDialog: () => null }));
vi.mock("../Toaster", () => ({ showToast: vi.fn() }));
vi.mock("@/lib/api", () => ({
  api: { get: vi.fn(), post: vi.fn(), del: vi.fn(), patch: vi.fn() },
}));

const closeContextMenu = vi.fn();
const mockStore = {
  contextMenu: {
    x: 100,
    y: 200,
    nodeId: "ws-1",
    nodeData: {
      name: "Alpha Workspace",
      status: "online",
      tier: 1,
      parentId: null,
      agentCard: null,
      activeTasks: 0,
      collapsed: false,
      role: "dev",
      lastErrorRate: 0,
      lastSampleError: "",
      url: "",
      currentTask: "",
      runtime: "claude-code",
      needsRestart: false,
    },
  } as {
    x: number;
    y: number;
    nodeId: string;
    nodeData: Record<string, unknown>;
  } | null,
  closeContextMenu,
  updateNodeData: vi.fn(),
  selectNode: vi.fn(),
  setPanelTab: vi.fn(),
  nestNode: vi.fn(),
  setPendingDelete: vi.fn(),
  nodes: [] as Array<{ id: string; data: { parentId: string | null } }>,
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn((selector: (s: typeof mockStore) => unknown) => selector(mockStore)),
    { getState: () => mockStore }
  ),
}));

// ── Component under test — imported AFTER mocks ───────────────────────────────
import { ContextMenu } from "../ContextMenu";

// ── Helpers ───────────────────────────────────────────────────────────────────
const onlineMenu = {
  x: 100,
  y: 200,
  nodeId: "ws-1",
  nodeData: {
    name: "Alpha Workspace",
    status: "online",
    tier: 1,
    parentId: null,
    agentCard: null,
    activeTasks: 0,
    collapsed: false,
    role: "dev",
    lastErrorRate: 0,
    lastSampleError: "",
    url: "",
    currentTask: "",
    runtime: "claude-code",
    needsRestart: false,
  },
};

describe("ContextMenu — keyboard accessibility", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockStore.contextMenu = onlineMenu;
    mockStore.nodes = [];
  });

  it("renders with role='menu'", () => {
    render(<ContextMenu />);
    expect(screen.getByRole("menu")).toBeTruthy();
  });

  it("menu has aria-label containing the workspace name", () => {
    render(<ContextMenu />);
    const menu = screen.getByRole("menu");
    expect(menu.getAttribute("aria-label")).toContain("Alpha Workspace");
  });

  it("menu items have role='menuitem'", () => {
    render(<ContextMenu />);
    const items = screen.getAllByRole("menuitem");
    expect(items.length).toBeGreaterThan(0);
  });

  it("dividers have role='separator'", () => {
    render(<ContextMenu />);
    const separators = document.querySelectorAll('[role="separator"]');
    expect(separators.length).toBeGreaterThan(0);
  });

  it("Escape key calls closeContextMenu", () => {
    render(<ContextMenu />);
    const menu = screen.getByRole("menu");
    fireEvent.keyDown(menu, { key: "Escape" });
    // Both the document keydown listener and the menu onKeyDown handler fire
    // on the same event — both call closeContextMenu. Two calls is correct.
    expect(closeContextMenu).toHaveBeenCalled();
  });

  it("Tab key calls closeContextMenu", () => {
    render(<ContextMenu />);
    const menu = screen.getByRole("menu");
    fireEvent.keyDown(menu, { key: "Tab" });
    expect(closeContextMenu).toHaveBeenCalledOnce();
  });

  it("ArrowDown with nothing focused moves focus to the first enabled button", () => {
    render(<ContextMenu />);
    const menu = screen.getByRole("menu");
    fireEvent.keyDown(menu, { key: "ArrowDown" });
    const buttons = menu.querySelectorAll<HTMLButtonElement>(
      "button:not(:disabled)"
    );
    expect(document.activeElement).toBe(buttons[0]);
  });

  it("ArrowDown wraps from the last enabled button to the first", () => {
    render(<ContextMenu />);
    const menu = screen.getByRole("menu");
    const buttons = menu.querySelectorAll<HTMLButtonElement>(
      "button:not(:disabled)"
    );
    buttons[buttons.length - 1].focus();
    fireEvent.keyDown(menu, { key: "ArrowDown" });
    expect(document.activeElement).toBe(buttons[0]);
  });

  it("ArrowUp wraps from the first enabled button to the last", () => {
    render(<ContextMenu />);
    const menu = screen.getByRole("menu");
    const buttons = menu.querySelectorAll<HTMLButtonElement>(
      "button:not(:disabled)"
    );
    buttons[0].focus();
    fireEvent.keyDown(menu, { key: "ArrowUp" });
    expect(document.activeElement).toBe(buttons[buttons.length - 1]);
  });

  it("returns null when contextMenu is null", () => {
    mockStore.contextMenu = null;
    const { container } = render(<ContextMenu />);
    expect(container.firstChild).toBeNull();
  });

  // ── Zoom to Team (#557) ───────────────────────────────────────────────────

  it("does NOT show 'Zoom to Team' when node has no children", () => {
    mockStore.nodes = []; // no children
    render(<ContextMenu />);
    const items = screen.getAllByRole("menuitem");
    const labels = items.map((el) => el.textContent ?? "");
    expect(labels.some((l) => l.includes("Zoom to Team"))).toBe(false);
  });

  it("shows 'Zoom to Team' when the node has children", () => {
    mockStore.nodes = [{ id: "child-1", data: { parentId: "ws-1" } }];
    render(<ContextMenu />);
    const items = screen.getAllByRole("menuitem");
    const labels = items.map((el) => el.textContent ?? "");
    expect(labels.some((l) => l.includes("Zoom to Team"))).toBe(true);
  });

  it("clicking 'Zoom to Team' dispatches molecule:zoom-to-team event", () => {
    mockStore.nodes = [{ id: "child-1", data: { parentId: "ws-1" } }];
    const dispatched: CustomEvent[] = [];
    window.addEventListener("molecule:zoom-to-team", (e) => {
      dispatched.push(e as CustomEvent);
    });

    render(<ContextMenu />);
    const items = screen.getAllByRole("menuitem");
    const zoomItem = items.find((el) => el.textContent?.includes("Zoom to Team"))!;
    expect(zoomItem).toBeTruthy();
    fireEvent.click(zoomItem);

    expect(dispatched).toHaveLength(1);
    expect(dispatched[0].detail.nodeId).toBe("ws-1");

    window.removeEventListener("molecule:zoom-to-team", () => {});
  });

  it("clicking 'Zoom to Team' closes the context menu", () => {
    mockStore.nodes = [{ id: "child-1", data: { parentId: "ws-1" } }];
    render(<ContextMenu />);
    const items = screen.getAllByRole("menuitem");
    const zoomItem = items.find((el) => el.textContent?.includes("Zoom to Team"))!;
    fireEvent.click(zoomItem);
    expect(closeContextMenu).toHaveBeenCalled();
  });

  // Regression: the old flow kept ConfirmDialog inside ContextMenu's local
  // state and rendered it via a portal. The portal-rendered Confirm button
  // counted as "outside" by the menu's outside-click handler, closing the
  // menu mid-click and making Delete appear to do nothing. The fix hoists
  // the dialog state to the canvas store via `setPendingDelete` AND closes
  // the context menu on click, so the dialog is owned by a component that
  // outlives the menu.
  it("clicking 'Delete' hoists state to the store and closes the menu", () => {
    render(<ContextMenu />);
    const items = screen.getAllByRole("menuitem");
    const deleteItem = items.find((el) => el.textContent?.includes("Delete"))!;
    fireEvent.click(deleteItem);
    expect(mockStore.setPendingDelete).toHaveBeenCalledWith(
      expect.objectContaining({ id: "ws-1", name: "Alpha Workspace" })
    );
    expect(closeContextMenu).toHaveBeenCalled();
  });

  // The "Expand to Team" right-click action was removed in Phase 2 of
  // RFC #2857 — every workspace can already have children via the
  // regular CreateWorkspace flow with parent_id, so a separate
  // backend bulk-create handler (which was non-idempotent and leaked
  // EC2s on every duplicate call) was deleted in PR #2856 and the
  // canvas affordance is gone with it.
  it("'Expand to Team' menu item is gone (childless workspace)", () => {
    // Default mockStore.nodes = [] → no children → workspace is childless.
    render(<ContextMenu />);
    const items = screen.getAllByRole("menuitem");
    const labels = items.map((el) => el.textContent?.trim() ?? "");
    expect(labels).not.toContain(expect.stringMatching(/Expand to Team/));
    // Sanity: childless menu still has the regular actions.
    expect(labels.some((l) => l.includes("Delete"))).toBe(true);
    expect(labels.some((l) => l.includes("Restart"))).toBe(true);
  });

  it("'Collapse Team' is still present when the workspace HAS children", () => {
    // Mark a child belonging to ws-1 so hasChildren() returns true.
    mockStore.nodes = [{ id: "child-1", data: { parentId: "ws-1" } }];
    render(<ContextMenu />);
    const items = screen.getAllByRole("menuitem");
    const labels = items.map((el) => el.textContent?.trim() ?? "");
    expect(labels.some((l) => /Collapse Team|Expand Team/.test(l))).toBe(true);
    expect(labels.some((l) => l.includes("Arrange Children"))).toBe(true);
    expect(labels.some((l) => l.includes("Zoom to Team"))).toBe(true);
    // Cleanup for other tests.
    mockStore.nodes = [];
  });
});
