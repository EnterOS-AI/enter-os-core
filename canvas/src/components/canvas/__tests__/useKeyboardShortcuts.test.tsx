// @vitest-environment jsdom
/**
 * Tests for canvas keyboard shortcuts (useKeyboardShortcuts hook).
 *
 * Covers: Esc, Enter/Shift+Enter, Cmd+]/[, Z, and Arrow keys.
 *
 * The hook is tested by dispatching KeyboardEvents at the window and
 * asserting the resulting store mutations / dispatched events.
 */
import React from "react";
import { render, cleanup, fireEvent } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useKeyboardShortcuts } from "../useKeyboardShortcuts";
import { useCanvasStore } from "@/store/canvas";

// ─── Mock store ──────────────────────────────────────────────────────────────

const mockSavePosition = vi.fn().mockResolvedValue(undefined);

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    vi.fn((sel) => sel(mockStoreState)),
    {
      getState: () => mockStoreState,
    }
  ),
}));

// Module-level mutable state so tests can mutate between cases
const mockStoreState = {
  selectedNodeId: null as string | null,
  selectedNodeIds: new Set<string>(),
  nodes: [] as Array<{ id: string; position: { x: number; y: number }; data: { parentId?: string | null } }>,
  contextMenu: null as { x: number; y: number; nodeId: string } | null,
  closeContextMenu: vi.fn(),
  selectNode: vi.fn(),
  clearSelection: vi.fn(),
  bumpZOrder: vi.fn(),
  savePosition: mockSavePosition,
  moveNode: vi.fn(),
};

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  // Reset to default empty state between tests
  mockStoreState.selectedNodeId = null;
  mockStoreState.selectedNodeIds = new Set();
  mockStoreState.nodes = [];
  mockStoreState.contextMenu = null;
  mockStoreState.closeContextMenu.mockClear();
  mockStoreState.selectNode.mockClear();
  mockStoreState.clearSelection.mockClear();
  mockStoreState.bumpZOrder.mockClear();
  mockStoreState.moveNode.mockClear();
  mockStoreState.savePosition.mockClear();
});

// ─── Test wrapper ────────────────────────────────────────────────────────────

function ShortcutTestComponent() {
  useKeyboardShortcuts();
  return <div data-testid="canvas-root" />;
}

function renderWithProvider() {
  return render(<ShortcutTestComponent />);
}

// ─── Tests ───────────────────────────────────────────────────────────────────

describe("Esc — deselect / close context menu", () => {
  it("closes the context menu when one is open", () => {
    mockStoreState.contextMenu = { x: 100, y: 100, nodeId: "n1" };
    renderWithProvider();
    fireEvent.keyDown(window, { key: "Escape" });
    expect(mockStoreState.closeContextMenu).toHaveBeenCalledTimes(1);
  });

  it("clears the batch selection when no context menu is open", () => {
    mockStoreState.contextMenu = null;
    mockStoreState.selectedNodeIds = new Set(["n1", "n2"]);
    renderWithProvider();
    fireEvent.keyDown(window, { key: "Escape" });
    expect(mockStoreState.clearSelection).toHaveBeenCalledTimes(1);
  });

  it("deselects the focused node when no batch selection exists", () => {
    mockStoreState.contextMenu = null;
    mockStoreState.selectedNodeIds = new Set();
    mockStoreState.selectedNodeId = "n1";
    renderWithProvider();
    fireEvent.keyDown(window, { key: "Escape" });
    expect(mockStoreState.selectNode).toHaveBeenCalledWith(null);
  });
});

describe("Enter — hierarchy navigation", () => {
  beforeEach(() => {
    mockStoreState.selectedNodeId = "n1";
    mockStoreState.nodes = [
      { id: "n1", position: { x: 0, y: 0 }, data: { parentId: null } },
      { id: "n2", position: { x: 100, y: 0 }, data: { parentId: "n1" } },
      { id: "n3", position: { x: 200, y: 0 }, data: { parentId: null } },
    ];
  });

  it("navigates to the first child on Enter", () => {
    renderWithProvider();
    fireEvent.keyDown(window, { key: "Enter" });
    expect(mockStoreState.selectNode).toHaveBeenCalledWith("n2");
  });

  it("navigates to the parent on Shift+Enter", () => {
    mockStoreState.nodes = [
      { id: "n1", position: { x: 0, y: 0 }, data: { parentId: null } },
      { id: "n2", position: { x: 100, y: 0 }, data: { parentId: "n1" } },
    ];
    mockStoreState.selectedNodeId = "n2";
    renderWithProvider();
    fireEvent.keyDown(window, { key: "Enter", shiftKey: true });
    expect(mockStoreState.selectNode).toHaveBeenCalledWith("n1");
  });

  it("does NOT navigate when no node is selected", () => {
    mockStoreState.selectedNodeId = null;
    renderWithProvider();
    fireEvent.keyDown(window, { key: "Enter" });
    expect(mockStoreState.selectNode).not.toHaveBeenCalled();
  });
});

describe("Cmd+]/[ — z-order bump", () => {
  beforeEach(() => {
    mockStoreState.selectedNodeId = "n1";
  });

  it("bumps z-order forward on Cmd+]", () => {
    renderWithProvider();
    fireEvent.keyDown(window, { key: "]", metaKey: true });
    expect(mockStoreState.bumpZOrder).toHaveBeenCalledWith("n1", 1);
  });

  it("bumps z-order backward on Cmd+[", () => {
    renderWithProvider();
    fireEvent.keyDown(window, { key: "[", metaKey: true });
    expect(mockStoreState.bumpZOrder).toHaveBeenCalledWith("n1", -1);
  });

  it("uses Ctrl as the modifier key", () => {
    renderWithProvider();
    fireEvent.keyDown(window, { key: "]", ctrlKey: true });
    expect(mockStoreState.bumpZOrder).toHaveBeenCalledWith("n1", 1);
  });
});

describe("Z — zoom-to-team", () => {
  let dispatchedEvents: CustomEvent[] = [];

  beforeEach(() => {
    dispatchedEvents = [];
    mockStoreState.selectedNodeId = "n1";
    mockStoreState.nodes = [
      { id: "n1", position: { x: 0, y: 0 }, data: { parentId: null } },
      { id: "n2", position: { x: 100, y: 0 }, data: { parentId: "n1" } },
    ];
    window.addEventListener("molecule:zoom-to-team", (e) => {
      dispatchedEvents.push(e as CustomEvent);
    });
  });

  afterEach(() => {
    window.removeEventListener("molecule:zoom-to-team", () => {});
  });

  it("dispatches zoom-to-team when the selected node has children", () => {
    renderWithProvider();
    fireEvent.keyDown(window, { key: "z" });
    expect(dispatchedEvents).toHaveLength(1);
    expect(dispatchedEvents[0].detail.nodeId).toBe("n1");
  });

  it("does NOT fire when no node is selected", () => {
    mockStoreState.selectedNodeId = null;
    renderWithProvider();
    fireEvent.keyDown(window, { key: "z" });
    expect(dispatchedEvents).toHaveLength(0);
  });

  it("does NOT fire when the node has no children", () => {
    mockStoreState.nodes = [
      { id: "n1", position: { x: 0, y: 0 }, data: { parentId: null } },
    ];
    renderWithProvider();
    fireEvent.keyDown(window, { key: "z" });
    expect(dispatchedEvents).toHaveLength(0);
  });

  it("skips when the target element is an input", () => {
    renderWithProvider();
    const input = document.createElement("input");
    document.body.appendChild(input);
    fireEvent.keyDown(input, { key: "z" });
    expect(dispatchedEvents).toHaveLength(0);
    document.body.removeChild(input);
  });
});

describe("Arrow keys — keyboard node movement", () => {
  beforeEach(() => {
    mockStoreState.selectedNodeId = "n1";
    mockStoreState.nodes = [
      { id: "n1", position: { x: 100, y: 200 }, data: { parentId: null } },
    ];
  });

  it("moves the selected node down on ArrowDown", () => {
    renderWithProvider();
    fireEvent.keyDown(window, { key: "ArrowDown" });
    expect(mockStoreState.moveNode).toHaveBeenCalledWith("n1", 0, 10);
  });

  it("moves the selected node up on ArrowUp", () => {
    renderWithProvider();
    fireEvent.keyDown(window, { key: "ArrowUp" });
    expect(mockStoreState.moveNode).toHaveBeenCalledWith("n1", 0, -10);
  });

  it("moves the selected node right on ArrowRight", () => {
    renderWithProvider();
    fireEvent.keyDown(window, { key: "ArrowRight" });
    expect(mockStoreState.moveNode).toHaveBeenCalledWith("n1", 10, 0);
  });

  it("moves the selected node left on ArrowLeft", () => {
    renderWithProvider();
    fireEvent.keyDown(window, { key: "ArrowLeft" });
    expect(mockStoreState.moveNode).toHaveBeenCalledWith("n1", -10, 0);
  });

  it("moves 50 px when Shift is held", () => {
    renderWithProvider();
    fireEvent.keyDown(window, { key: "ArrowDown", shiftKey: true });
    expect(mockStoreState.moveNode).toHaveBeenCalledWith("n1", 0, 50);
  });

  it("does NOT fire when no node is selected", () => {
    mockStoreState.selectedNodeId = null;
    renderWithProvider();
    fireEvent.keyDown(window, { key: "ArrowDown" });
    expect(mockStoreState.moveNode).not.toHaveBeenCalled();
  });

  it("skips when the target element is an input", () => {
    renderWithProvider();
    const input = document.createElement("input");
    document.body.appendChild(input);
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(mockStoreState.moveNode).not.toHaveBeenCalled();
    document.body.removeChild(input);
  });

  it("skips when a modal dialog is already open", () => {
    renderWithProvider();
    const dialog = document.createElement("div");
    dialog.setAttribute("role", "dialog");
    dialog.setAttribute("aria-modal", "true");
    document.body.appendChild(dialog);
    fireEvent.keyDown(window, { key: "ArrowDown" });
    expect(mockStoreState.moveNode).not.toHaveBeenCalled();
    document.body.removeChild(dialog);
  });

  it("prevents default browser scroll on arrow keys", () => {
    renderWithProvider();
    const preventDefault = vi.fn();
    fireEvent.keyDown(window, {
      key: "ArrowDown",
      preventDefault,
    });
    expect(preventDefault).toHaveBeenCalled();
  });
});

describe("all shortcuts respect inInput guard", () => {
  it("ArrowDown is skipped in an input element", () => {
    mockStoreState.selectedNodeId = "n1";
    renderWithProvider();
    const textarea = document.createElement("textarea");
    document.body.appendChild(textarea);
    fireEvent.keyDown(textarea, { key: "ArrowDown" });
    expect(mockStoreState.moveNode).not.toHaveBeenCalled();
    document.body.removeChild(textarea);
  });

  it("Enter navigation is skipped in an input element", () => {
    mockStoreState.selectedNodeId = "n1";
    mockStoreState.nodes = [
      { id: "n1", position: { x: 0, y: 0 }, data: { parentId: null } },
      { id: "n2", position: { x: 100, y: 0 }, data: { parentId: "n1" } },
    ];
    renderWithProvider();
    const input = document.createElement("input");
    document.body.appendChild(input);
    fireEvent.keyDown(input, { key: "Enter" });
    expect(mockStoreState.selectNode).not.toHaveBeenCalled();
    document.body.removeChild(input);
  });
});
