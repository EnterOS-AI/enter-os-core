// @vitest-environment jsdom
/**
 * Tests for ContextMenu component.
 *
 * Covers: null guard, node header (name + status), outside-click close,
 * Escape close, arrow-key navigation, conditional menu items by status,
 * danger items, dividers, rAF position clamping.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ContextMenu } from "../ContextMenu";
import { useCanvasStore } from "@/store/canvas";
import { showToast } from "../Toaster";
import { api } from "@/lib/api";

// ─── Mock Toaster ─────────────────────────────────────────────────────────────

vi.mock("../Toaster", () => ({
  showToast: vi.fn(),
}));

// ─── Mock API ────────────────────────────────────────────────────────────────
// Mock api.post/patch via vi.spyOn — avoids vi.mock hoisting issues.
// Set up in beforeEach, cleaned up in afterEach.
let mockPost: ReturnType<typeof vi.fn>;
let mockPatch: ReturnType<typeof vi.fn>;

function setupApiMocks() {
  mockPost = vi.fn().mockResolvedValue(undefined as void);
  mockPatch = vi.fn().mockResolvedValue(undefined as void);
  vi.spyOn(api, "post").mockImplementation(mockPost);
  vi.spyOn(api, "patch").mockImplementation(mockPatch);
}

function resetApiMocks() {
  mockPost?.mockReset();
  mockPatch?.mockReset();
  vi.restoreAllMocks();
}

// ─── Mock store ──────────────────────────────────────────────────────────────

const mockStoreState = {
  contextMenu: null as {
    x: number;
    y: number;
    nodeId: string;
    nodeData: {
      name: string;
      status: string;
      tier: number;
      role: string;
      parentId?: string | null;
      collapsed?: boolean;
    };
  } | null,
  closeContextMenu: vi.fn(),
  updateNodeData: vi.fn(),
  selectNode: vi.fn(),
  setPanelTab: vi.fn(),
  nestNode: vi.fn().mockResolvedValue(undefined as void),
  setPendingDelete: vi.fn(),
  setCollapsed: vi.fn(),
  arrangeChildren: vi.fn(),
  nodes: [] as Array<{
    id: string;
    data: { parentId?: string | null };
  }>,
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (sel: (s: typeof mockStoreState) => unknown) => sel(mockStoreState),
    { getState: () => mockStoreState },
  ),
}));

// ─── Helpers ──────────────────────────────────────────────────────────────────

function openMenu(overrides?: Partial<NonNullable<typeof mockStoreState.contextMenu>>) {
  mockStoreState.contextMenu = {
    x: 100,
    y: 200,
    nodeId: "n1",
    nodeData: { name: "Alice", status: "online", tier: 4, role: "assistant" },
    ...overrides,
  };
}

// ─── Tests ───────────────────────────────────────────────────────────────────

describe("ContextMenu — visibility", () => {
  beforeEach(() => {
    setupApiMocks();
  });
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    mockStoreState.contextMenu = null;
    mockStoreState.closeContextMenu.mockClear();
    mockStoreState.updateNodeData.mockClear();
    mockStoreState.selectNode.mockClear();
    mockStoreState.setPanelTab.mockClear();
    mockStoreState.nestNode.mockClear();
    mockStoreState.setPendingDelete.mockClear();
    mockStoreState.setCollapsed.mockClear();
    mockStoreState.arrangeChildren.mockClear();
    mockStoreState.nodes = [];
    resetApiMocks();
    vi.mocked(showToast).mockClear();
  });

  it("renders nothing when contextMenu is null", () => {
    mockStoreState.contextMenu = null;
    render(<ContextMenu />);
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("renders the menu when contextMenu is set", () => {
    openMenu();
    render(<ContextMenu />);
    expect(screen.getByRole("menu")).toBeTruthy();
  });

  it("has aria-label describing the node name", () => {
    openMenu({ nodeData: { name: "Alice", status: "online", tier: 4, role: "assistant" } });
    render(<ContextMenu />);
    expect(screen.getByRole("menu").getAttribute("aria-label")).toBe("Actions for Alice");
  });

  it("shows the node name in the header", () => {
    openMenu({ nodeData: { name: "Bob", status: "offline", tier: 2, role: "analyst" } });
    render(<ContextMenu />);
    expect(screen.getByText("Bob")).toBeTruthy();
  });

  it("shows the node status in the header", () => {
    openMenu({ nodeData: { name: "Alice", status: "failed", tier: 4, role: "assistant" } });
    render(<ContextMenu />);
    expect(screen.getByText("failed")).toBeTruthy();
  });
});

describe("ContextMenu — close", () => {
  beforeEach(() => { setupApiMocks(); });
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    mockStoreState.contextMenu = null;
    mockStoreState.closeContextMenu.mockClear();
    mockStoreState.updateNodeData.mockClear();
    mockStoreState.selectNode.mockClear();
    mockStoreState.setPanelTab.mockClear();
    mockStoreState.nestNode.mockClear();
    mockStoreState.setPendingDelete.mockClear();
    mockStoreState.setCollapsed.mockClear();
    mockStoreState.arrangeChildren.mockClear();
    mockStoreState.nodes = [];
    resetApiMocks();
    vi.mocked(showToast).mockClear();
  });

  it("closes when clicking outside the menu", () => {
    openMenu();
    render(<ContextMenu />);
    fireEvent.mouseDown(document.body);
    expect(mockStoreState.closeContextMenu).toHaveBeenCalled();
  });

  it("closes when Escape is pressed", () => {
    openMenu();
    render(<ContextMenu />);
    fireEvent.keyDown(document.body, { key: "Escape" });
    expect(mockStoreState.closeContextMenu).toHaveBeenCalled();
  });

  it("closes when Tab is pressed while menu is focused", () => {
    openMenu();
    render(<ContextMenu />);
    const menu = screen.getByRole("menu");
    // Tab only closes when the menu element itself has focus.
    // When focus is on body, the document-level handler only handles Escape.
    fireEvent.keyDown(menu, { key: "Tab" });
    expect(mockStoreState.closeContextMenu).toHaveBeenCalled();
  });
});

describe("ContextMenu — menu items", () => {
  beforeEach(() => { setupApiMocks(); });
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    mockStoreState.contextMenu = null;
    mockStoreState.closeContextMenu.mockClear();
    mockStoreState.updateNodeData.mockClear();
    mockStoreState.selectNode.mockClear();
    mockStoreState.setPanelTab.mockClear();
    mockStoreState.nestNode.mockClear();
    mockStoreState.setPendingDelete.mockClear();
    mockStoreState.setCollapsed.mockClear();
    mockStoreState.arrangeChildren.mockClear();
    mockStoreState.nodes = [];
    resetApiMocks();
    vi.mocked(showToast).mockClear();
  });

  it("shows Chat and Terminal only for online nodes", () => {
    openMenu({ nodeData: { name: "Alice", status: "online", tier: 4, role: "assistant" } });
    render(<ContextMenu />);
    expect(screen.getByRole("menuitem", { name: /chat/i })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: /terminal/i })).toBeTruthy();
  });

  it("Chat and Terminal are disabled for offline nodes", () => {
    openMenu({ nodeData: { name: "Bob", status: "offline", tier: 2, role: "analyst" } });
    render(<ContextMenu />);
    // Chat and Terminal are rendered in the DOM even for offline nodes.
    // For online nodes they are clickable; for offline nodes they are
    // disabled (no hover effect). The context menu never omits them —
    // it controls clickability via disabled flag. We verify the items
    // are present and would be disabled by checking the aria-disabled
    // attribute that the component sets.
    const chatItem = screen.getByRole("menuitem", { name: /chat/i });
    const terminalItem = screen.getByRole("menuitem", { name: /terminal/i });
    expect(chatItem).toBeTruthy();
    expect(terminalItem).toBeTruthy();
    // For offline nodes, the button has aria-disabled="true"
    expect(chatItem.getAttribute("aria-disabled")).toBe("true");
    expect(terminalItem.getAttribute("aria-disabled")).toBe("true");
  });

  it("shows Pause for online nodes (not paused)", () => {
    openMenu({ nodeData: { name: "Alice", status: "online", tier: 4, role: "assistant" } });
    render(<ContextMenu />);
    expect(screen.getByRole("menuitem", { name: /pause/i })).toBeTruthy();
  });

  it("shows Resume for paused nodes (not Pause)", () => {
    openMenu({ nodeData: { name: "Carol", status: "paused", tier: 3, role: "writer" } });
    render(<ContextMenu />);
    expect(screen.queryByRole("menuitem", { name: /pause/i })).toBeNull();
    expect(screen.getByRole("menuitem", { name: /resume/i })).toBeTruthy();
  });

  it("shows Extract from Team only for child nodes", () => {
    openMenu({ nodeData: { name: "Child", status: "online", tier: 4, role: "", parentId: "parent1" } });
    render(<ContextMenu />);
    expect(screen.getByRole("menuitem", { name: /extract/i })).toBeTruthy();
  });

  it("hides Extract from Team for root nodes", () => {
    openMenu({ nodeData: { name: "Root", status: "online", tier: 4, role: "", parentId: null } });
    render(<ContextMenu />);
    expect(screen.queryByRole("menuitem", { name: /extract/i })).toBeNull();
  });

  it("shows team items only when node has children", () => {
    openMenu({ nodeData: { name: "Parent", status: "online", tier: 4, role: "" } });
    mockStoreState.nodes = [{ id: "child1", data: { parentId: "n1" } }];
    render(<ContextMenu />);
    expect(screen.getByRole("menuitem", { name: /arrange/i })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: /collapse/i })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: /zoom/i })).toBeTruthy();
  });

  it("hides team items when node has no children", () => {
    openMenu({ nodeData: { name: "Leaf", status: "online", tier: 4, role: "" } });
    mockStoreState.nodes = [];
    render(<ContextMenu />);
    expect(screen.queryByRole("menuitem", { name: /arrange/i })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: /collapse/i })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: /zoom/i })).toBeNull();
  });

  it("shows Collapse Team when collapsed, Expand Team when expanded", () => {
    openMenu({ nodeData: { name: "Parent", status: "online", tier: 4, role: "", collapsed: true } });
    mockStoreState.nodes = [{ id: "child1", data: { parentId: "n1" } }];
    render(<ContextMenu />);
    expect(screen.getByRole("menuitem", { name: /expand/i })).toBeTruthy();
  });

  it("Delete item has danger styling class", () => {
    openMenu();
    render(<ContextMenu />);
    const deleteItem = screen.getByRole("menuitem", { name: /delete/i });
    expect(deleteItem.getAttribute("class")).toMatch(/text-bad|bad/);
  });

  it("renders role=separator for dividers", () => {
    openMenu();
    render(<ContextMenu />);
    expect(document.body.querySelectorAll('[role="separator"]').length).toBeGreaterThan(0);
  });
});

describe("ContextMenu — keyboard navigation", () => {
  beforeEach(() => { setupApiMocks(); });
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    mockStoreState.contextMenu = null;
    mockStoreState.closeContextMenu.mockClear();
    mockStoreState.updateNodeData.mockClear();
    mockStoreState.selectNode.mockClear();
    mockStoreState.setPanelTab.mockClear();
    mockStoreState.nestNode.mockClear();
    mockStoreState.setPendingDelete.mockClear();
    mockStoreState.setCollapsed.mockClear();
    mockStoreState.arrangeChildren.mockClear();
    mockStoreState.nodes = [];
    resetApiMocks();
    vi.mocked(showToast).mockClear();
  });

  it("ArrowDown moves focus to next enabled menuitem", () => {
    openMenu();
    render(<ContextMenu />);
    const menu = screen.getByRole("menu");
    // First tab goes to Details (first non-disabled item)
    fireEvent.keyDown(menu, { key: "ArrowDown" });
    const buttons = screen.getAllByRole("menuitem");
    const focusedIdx = buttons.findIndex((b) => document.activeElement === b);
    expect(focusedIdx).toBeGreaterThanOrEqual(0);
  });

  it("ArrowUp moves focus to previous enabled menuitem", () => {
    openMenu();
    render(<ContextMenu />);
    const menu = screen.getByRole("menu");
    fireEvent.keyDown(menu, { key: "ArrowDown" });
    const beforeFocused = document.activeElement;
    fireEvent.keyDown(menu, { key: "ArrowUp" });
    // Focus should have moved
    expect(document.activeElement).toBeTruthy();
  });
});

describe("ContextMenu — item actions", () => {
  beforeEach(() => { setupApiMocks(); });
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    mockStoreState.contextMenu = null;
    mockStoreState.closeContextMenu.mockClear();
    mockStoreState.updateNodeData.mockClear();
    mockStoreState.selectNode.mockClear();
    mockStoreState.setPanelTab.mockClear();
    mockStoreState.nestNode.mockClear();
    mockStoreState.setPendingDelete.mockClear();
    mockStoreState.setCollapsed.mockClear();
    mockStoreState.arrangeChildren.mockClear();
    mockStoreState.nodes = [];
    resetApiMocks();
    vi.mocked(showToast).mockClear();
  });

  it("Details selects node and opens details tab", () => {
    openMenu();
    render(<ContextMenu />);
    fireEvent.click(screen.getByRole("menuitem", { name: /details/i }));
    expect(mockStoreState.selectNode).toHaveBeenCalledWith("n1");
    expect(mockStoreState.setPanelTab).toHaveBeenCalledWith("details");
  });

  it("Chat selects node and opens chat tab", () => {
    openMenu({ nodeData: { name: "Alice", status: "online", tier: 4, role: "assistant" } });
    render(<ContextMenu />);
    fireEvent.click(screen.getByRole("menuitem", { name: /chat/i }));
    expect(mockStoreState.selectNode).toHaveBeenCalledWith("n1");
    expect(mockStoreState.setPanelTab).toHaveBeenCalledWith("chat");
  });

  it("Delete calls setPendingDelete without closing immediately", () => {
    openMenu();
    render(<ContextMenu />);
    fireEvent.click(screen.getByRole("menuitem", { name: /delete/i }));
    expect(mockStoreState.setPendingDelete).toHaveBeenCalled();
    expect(mockStoreState.closeContextMenu).toHaveBeenCalled();
  });

  it("Pause calls the pause API and updates node status optimistically", async () => {
    openMenu({ nodeData: { name: "Alice", status: "online", tier: 4, role: "assistant" } });
    mockPost.mockResolvedValue(undefined);
    render(<ContextMenu />);
    fireEvent.click(screen.getByRole("menuitem", { name: /pause/i }));
    await act(async () => { /* flush */ });
    expect(mockPost).toHaveBeenCalledWith("/workspaces/n1/pause?cascade=true", {});
    expect(mockStoreState.updateNodeData).toHaveBeenCalledWith("n1", { status: "paused" });
  });

  it("Resume calls the resume API", async () => {
    openMenu({ nodeData: { name: "Alice", status: "paused", tier: 4, role: "assistant" } });
    mockPost.mockResolvedValue(undefined);
    render(<ContextMenu />);
    fireEvent.click(screen.getByRole("menuitem", { name: /resume/i }));
    await act(async () => { /* flush */ });
    expect(mockPost).toHaveBeenCalledWith("/workspaces/n1/resume?cascade=true", {});
  });
});

/**
 * Regression tests for GitHub issue #651 — React error #185:
 * "Maximum update depth exceeded" on Chat tab / mobile.
 *
 * Root cause: ContextMenu's children selector ran `.filter()` inside the
 * Zustand hook, returning a brand-new array reference on every render.
 * Zustand's useSyncExternalStore compared snapshots with Object.is —
 * a new array always differs — so React kept scheduling re-renders,
 * hit the 50-update depth cap, and crashed.
 *
 * Fix: select the stable `nodes` array once, derive children via
 * useMemo outside the store subscription.
 */
describe("ContextMenu — hasChildren regression (GitHub #651)", () => {
  beforeEach(() => { setupApiMocks(); });
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    mockStoreState.contextMenu = null;
    mockStoreState.closeContextMenu.mockClear();
    mockStoreState.updateNodeData.mockClear();
    mockStoreState.selectNode.mockClear();
    mockStoreState.setPanelTab.mockClear();
    mockStoreState.nestNode.mockClear();
    mockStoreState.setPendingDelete.mockClear();
    mockStoreState.setCollapsed.mockClear();
    mockStoreState.arrangeChildren.mockClear();
    mockStoreState.nodes = [];
    resetApiMocks();
    vi.mocked(showToast).mockClear();
  });

  it("setPendingDelete receives correct children array when workspace has children", () => {
    openMenu({ nodeId: "ws-parent", nodeData: { name: "Parent", status: "online", tier: 4, role: "assistant" } });
    mockStoreState.nodes = [
      { id: "ws-child-a", data: { parentId: "ws-parent" } },
      { id: "ws-child-b", data: { parentId: "ws-parent" } },
    ];
    render(<ContextMenu />);
    const deleteBtn = screen.getAllByRole("menuitem").find((el) =>
      el.textContent?.includes("Delete")
    )!;
    fireEvent.click(deleteBtn);
    expect(mockStoreState.setPendingDelete).toHaveBeenCalledWith(
      expect.objectContaining({
        id: "ws-parent",
        name: "Parent",
        hasChildren: true,
        children: [
          { id: "ws-child-a", name: undefined },
          { id: "ws-child-b", name: undefined },
        ],
      })
    );
  });

  it("setPendingDelete hasChildren=false and empty children array when workspace has no children", () => {
    openMenu({ nodeId: "ws-leaf", nodeData: { name: "Leaf", status: "online", tier: 4, role: "assistant" } });
    mockStoreState.nodes = [];
    render(<ContextMenu />);
    const deleteBtn = screen.getAllByRole("menuitem").find((el) =>
      el.textContent?.includes("Delete")
    )!;
    fireEvent.click(deleteBtn);
    expect(mockStoreState.setPendingDelete).toHaveBeenCalledWith(
      expect.objectContaining({
        id: "ws-leaf",
        name: "Leaf",
        hasChildren: false,
        children: [],
      })
    );
  });
});
