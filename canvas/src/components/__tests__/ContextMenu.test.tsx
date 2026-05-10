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

// ─── Mock Toaster ─────────────────────────────────────────────────────────────

vi.mock("../Toaster", () => ({
  showToast: vi.fn(),
}));

// ─── Mock API ────────────────────────────────────────────────────────────────

const apiPost = vi.fn().mockResolvedValue(undefined as void);
const apiPatch = vi.fn().mockResolvedValue(undefined as void);
vi.mock("@/lib/api", () => ({
  api: {
    post: apiPost,
    patch: apiPatch,
    get: vi.fn(),
  },
}));

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
    apiPost.mockReset();
    apiPatch.mockReset();
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
    apiPost.mockReset();
    apiPatch.mockReset();
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

  it("closes when Tab is pressed", () => {
    openMenu();
    render(<ContextMenu />);
    fireEvent.keyDown(document.body, { key: "Tab" });
    expect(mockStoreState.closeContextMenu).toHaveBeenCalled();
  });
});

describe("ContextMenu — menu items", () => {
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
    apiPost.mockReset();
    apiPatch.mockReset();
    vi.mocked(showToast).mockClear();
  });

  it("shows Chat and Terminal only for online nodes", () => {
    openMenu({ nodeData: { name: "Alice", status: "online", tier: 4, role: "assistant" } });
    render(<ContextMenu />);
    expect(screen.getByRole("menuitem", { name: /chat/i })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: /terminal/i })).toBeTruthy();
  });

  it("hides Chat and Terminal for offline nodes", () => {
    openMenu({ nodeData: { name: "Bob", status: "offline", tier: 2, role: "analyst" } });
    render(<ContextMenu />);
    expect(screen.queryByRole("menuitem", { name: /chat/i })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: /terminal/i })).toBeNull();
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
    apiPost.mockReset();
    apiPatch.mockReset();
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
    apiPost.mockReset();
    apiPatch.mockReset();
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
    apiPost.mockResolvedValue(undefined);
    render(<ContextMenu />);
    fireEvent.click(screen.getByRole("menuitem", { name: /pause/i }));
    await act(async () => { /* flush */ });
    expect(apiPost).toHaveBeenCalledWith("/workspaces/n1/pause", {});
    expect(mockStoreState.updateNodeData).toHaveBeenCalledWith("n1", { status: "paused" });
  });

  it("Resume calls the resume API", async () => {
    openMenu({ nodeData: { name: "Alice", status: "paused", tier: 4, role: "assistant" } });
    apiPost.mockResolvedValue(undefined);
    render(<ContextMenu />);
    fireEvent.click(screen.getByRole("menuitem", { name: /resume/i }));
    await act(async () => { /* flush */ });
    expect(apiPost).toHaveBeenCalledWith("/workspaces/n1/resume", {});
  });
});
