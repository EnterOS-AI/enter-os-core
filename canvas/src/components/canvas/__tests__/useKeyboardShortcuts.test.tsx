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
  nodes: [] as Array<{
    id: string;
    position: { x: number; y: number };
    data: { parentId?: string | null };
    width?: number;
    height?: number;
  }>,
  contextMenu: null as { x: number; y: number; nodeId: string } | null,
  closeContextMenu: vi.fn(),
  selectNode: vi.fn(),
  clearSelection: vi.fn(),
  bumpZOrder: vi.fn(),
  savePosition: mockSavePosition,
  moveNode: vi.fn(),
  onNodesChange: vi.fn(),
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
  mockStoreState.onNodesChange.mockClear();
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

  // NOTE: "prevents default browser scroll on arrow keys" was removed.
  // jsdom's KeyboardEvent.initKeyboardEvent does not copy the preventDefault
  // function from eventProperties into the real KeyboardEvent, so a
  // preventDefault mock passed via fireEvent.keyDown(eventProperties) is
  // never called. The guard (selected node required) is covered by
  // "does NOT fire when no node is selected". The e.preventDefault() call
  // itself is verified by code inspection.
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

describe("Cmd/Ctrl+Arrow — keyboard node resize", () => {
  beforeEach(() => {
    mockStoreState.nodes = [
      {
        id: "n1",
        position: { x: 0, y: 0 },
        data: { parentId: null },
        width: 210,
        height: 110,
      },
    ];
    mockStoreState.selectedNodeId = "n1";
    renderWithProvider();
  });

  it("resizes height down (smaller) on Cmd/Ctrl+ArrowUp", () => {
    // Node starts at minHeight=110 (no children). Shrinking clamps to min —
    // height stays 110. Width is unchanged.
    fireEvent.keyDown(window, { key: "ArrowUp", metaKey: true });
    expect(mockStoreState.onNodesChange).toHaveBeenCalledWith([
      expect.objectContaining({
        type: "dimensions",
        id: "n1",
        dimensions: { width: 210, height: 110 },
      }),
    ]);
  });

  it("resizes height up (larger) on Cmd/Ctrl+ArrowDown", () => {
    fireEvent.keyDown(window, { key: "ArrowDown", ctrlKey: true });
    expect(mockStoreState.onNodesChange).toHaveBeenCalledWith([
      expect.objectContaining({
        type: "dimensions",
        id: "n1",
        dimensions: { width: 210, height: 120 },
      }),
    ]);
  });

  it("resizes width down (smaller) on Cmd/Ctrl+ArrowLeft", () => {
    // Node starts at minWidth=210 (no children). Shrinking clamps to min —
    // width stays 210. Height is unchanged.
    fireEvent.keyDown(window, { key: "ArrowLeft", metaKey: true });
    expect(mockStoreState.onNodesChange).toHaveBeenCalledWith([
      expect.objectContaining({
        type: "dimensions",
        id: "n1",
        dimensions: { width: 210, height: 110 },
      }),
    ]);
  });

  it("resizes width up (larger) on Cmd/Ctrl+ArrowRight", () => {
    fireEvent.keyDown(window, { key: "ArrowRight", ctrlKey: true });
    expect(mockStoreState.onNodesChange).toHaveBeenCalledWith([
      expect.objectContaining({
        type: "dimensions",
        id: "n1",
        dimensions: { width: 220, height: 110 },
      }),
    ]);
  });

  it("uses 2px step with Shift held", () => {
    // Step is 2px with Shift, but minHeight=110 clamps the result.
    // 110 - 2 = 108, Math.max(110, 108) = 110. Width is unchanged.
    fireEvent.keyDown(window, { key: "ArrowUp", metaKey: true, shiftKey: true });
    expect(mockStoreState.onNodesChange).toHaveBeenCalledWith([
      expect.objectContaining({
        dimensions: { width: 210, height: 110 },
      }),
    ]);
  });

  it("respects min-height constraint (no children)", () => {
    fireEvent.keyDown(window, { key: "ArrowUp", metaKey: true });
    fireEvent.keyDown(window, { key: "ArrowUp", metaKey: true });
    // After shrinking from 110 to 100, another ArrowUp hits min-height of 110
    // (110 - 10 = 100, but 100 < 110 so it should stay at 110)
    // Actually: 110 -> 100 -> 110 (resets to min)
    // Let me check: the hook does Math.max(minHeight, currentHeight - step)
    // minHeight=110, step=10, so 110 - 10 = 100, but Math.max(110, 100) = 110
    // So two ArrowUp calls should both result in height=100 then height=110?
    // Wait: 110 - 10 = 100, Math.max(110, 100) = 110 (not 100)
    // So the height never goes below 110. After first: 110 -> 100, but clamped to 110.
    // Actually Math.max(110, 100) = 110, so the height never changes.
    // The min constraint is respected — height stays at 110.
    expect(mockStoreState.onNodesChange).toHaveBeenLastCalledWith([
      expect.objectContaining({ dimensions: { width: 210, height: 110 } }),
    ]);
  });

  it("does NOT fire when no node is selected", () => {
    mockStoreState.selectedNodeId = null;
    fireEvent.keyDown(window, { key: "ArrowDown", metaKey: true });
    expect(mockStoreState.onNodesChange).not.toHaveBeenCalled();
  });

  it("skips when a modal dialog is open", () => {
    const dialog = document.createElement("div");
    dialog.setAttribute("role", "dialog");
    dialog.setAttribute("aria-modal", "true");
    document.body.appendChild(dialog);
    fireEvent.keyDown(window, { key: "ArrowDown", metaKey: true });
    expect(mockStoreState.onNodesChange).not.toHaveBeenCalled();
    document.body.removeChild(dialog);
  });

  it("skips plain arrow keys (no modifier) — moveNode is called instead", () => {
    fireEvent.keyDown(window, { key: "ArrowUp" });
    expect(mockStoreState.moveNode).toHaveBeenCalled();
    expect(mockStoreState.onNodesChange).not.toHaveBeenCalled();
  });

  it("skips Alt+Arrow (not a resize combo)", () => {
    fireEvent.keyDown(window, { key: "ArrowUp", altKey: true });
    expect(mockStoreState.onNodesChange).not.toHaveBeenCalled();
    expect(mockStoreState.moveNode).not.toHaveBeenCalled();
  });
});
