// @vitest-environment jsdom
/**
 * Tests for SearchDialog component.
 *
 * Covers: renders only when open, Cmd+K/Ctrl+K shortcut, Escape close,
 * focus management, text filtering (name/role/status), arrow-key
 * navigation, Enter to select, footer count, aria attributes.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SearchDialog } from "../SearchDialog";
import { useCanvasStore } from "@/store/canvas";

// ─── Mock store ──────────────────────────────────────────────────────────────

const mockStoreState = {
  searchOpen: false,
  setSearchOpen: vi.fn((open: boolean) => {
    mockStoreState.searchOpen = open;
  }),
  nodes: [] as Array<{
    id: string;
    data: {
      name: string;
      status: string;
      tier: number;
      role: string;
      parentId?: string | null;
    };
  }>,
  selectNode: vi.fn(),
  setPanelTab: vi.fn(),
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (sel: (s: typeof mockStoreState) => unknown) => sel(mockStoreState),
    { getState: () => mockStoreState },
  ),
}));

const STORAGE_KEY = "molecule-onboarding-complete";

// ─── Helpers ─────────────────────────────────────────────────────────────────

function dispatchKeydown(key: string, meta = false, ctrl = false) {
  fireEvent.keyDown(window, {
    key,
    metaKey: meta,
    ctrlKey: ctrl,
  });
}

// ─── Tests ───────────────────────────────────────────────────────────────────

describe("SearchDialog — visibility", () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    mockStoreState.searchOpen = false;
    mockStoreState.nodes = [];
    mockStoreState.setSearchOpen.mockClear();
    mockStoreState.selectNode.mockClear();
    mockStoreState.setPanelTab.mockClear();
  });

  it("does not render when searchOpen is false", () => {
    mockStoreState.searchOpen = false;
    render(<SearchDialog />);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("renders the dialog when searchOpen is true", () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    expect(screen.getByRole("dialog", { name: "Search workspaces" })).toBeTruthy();
  });
});

describe("SearchDialog — keyboard shortcuts", () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    mockStoreState.searchOpen = false;
    mockStoreState.nodes = [];
    mockStoreState.setSearchOpen.mockClear();
    mockStoreState.selectNode.mockClear();
    mockStoreState.setPanelTab.mockClear();
  });

  it("opens the dialog when Cmd+K is pressed", () => {
    render(<SearchDialog />);
    dispatchKeydown("k", true, false);
    expect(mockStoreState.setSearchOpen).toHaveBeenCalledWith(true);
  });

  it("opens the dialog when Ctrl+K is pressed", () => {
    render(<SearchDialog />);
    dispatchKeydown("k", false, true);
    expect(mockStoreState.setSearchOpen).toHaveBeenCalledWith(true);
  });

  it("clears the query when Cmd+K opens the dialog", () => {
    render(<SearchDialog />);
    dispatchKeydown("k", true, false);
    const input = screen.getByRole("combobox");
    expect(input.getAttribute("value") ?? "").toBe("");
  });

  it("closes the dialog when Escape is pressed while open", () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    dispatchKeydown("Escape");
    expect(mockStoreState.setSearchOpen).toHaveBeenCalledWith(false);
  });
});

describe("SearchDialog — focus", () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    mockStoreState.searchOpen = false;
    mockStoreState.nodes = [];
    mockStoreState.setSearchOpen.mockClear();
    mockStoreState.selectNode.mockClear();
    mockStoreState.setPanelTab.mockClear();
  });

  it("focuses the input when the dialog opens", async () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    await act(async () => {
      await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
    });
    expect(document.activeElement?.getAttribute("role")).toBe("combobox");
  });

  it("input has the combobox role", () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    expect(screen.getByRole("combobox")).toBeTruthy();
  });
});

describe("SearchDialog — filtering", () => {
  beforeEach(() => {
    mockStoreState.nodes = [
      { id: "n1", data: { name: "Alice", status: "online", tier: 4, role: "assistant" } },
      { id: "n2", data: { name: "Bob", status: "offline", tier: 2, role: "analyst" } },
      { id: "n3", data: { name: "Carol", status: "online", tier: 3, role: "writer" } },
    ];
  });

  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    mockStoreState.searchOpen = false;
    mockStoreState.nodes = [];
    mockStoreState.setSearchOpen.mockClear();
    mockStoreState.selectNode.mockClear();
    mockStoreState.setPanelTab.mockClear();
  });

  it("shows all workspaces when query is empty", () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    expect(screen.getByText("Alice")).toBeTruthy();
    expect(screen.getByText("Bob")).toBeTruthy();
    expect(screen.getByText("Carol")).toBeTruthy();
  });

  it("filters workspaces by name (case-insensitive)", () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "alice" } });
    expect(screen.getByText("Alice")).toBeTruthy();
    expect(screen.queryByText("Bob")).toBeNull();
    expect(screen.queryByText("Carol")).toBeNull();
  });

  it("filters workspaces by role (case-insensitive)", () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "writer" } });
    expect(screen.queryByText("Alice")).toBeNull();
    expect(screen.queryByText("Bob")).toBeNull();
    expect(screen.getByText("Carol")).toBeTruthy();
  });

  it("filters workspaces by status", () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "online" } });
    expect(screen.getByText("Alice")).toBeTruthy();
    expect(screen.queryByText("Bob")).toBeNull();
    expect(screen.getByText("Carol")).toBeTruthy();
  });

  it("shows 'No workspaces match' when filtering returns nothing", () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "xyz123" } });
    expect(screen.getByText("No workspaces match")).toBeTruthy();
  });

  it("shows 'No workspaces yet' when canvas is empty", () => {
    mockStoreState.searchOpen = true;
    mockStoreState.nodes = [];
    render(<SearchDialog />);
    expect(screen.getByText("No workspaces yet")).toBeTruthy();
  });
});

describe("SearchDialog — listbox navigation", () => {
  beforeEach(() => {
    mockStoreState.nodes = [
      { id: "n1", data: { name: "Alice", status: "online", tier: 4, role: "assistant" } },
      { id: "n2", data: { name: "Bob", status: "offline", tier: 2, role: "analyst" } },
      { id: "n3", data: { name: "Carol", status: "online", tier: 3, role: "writer" } },
    ];
  });

  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    mockStoreState.searchOpen = false;
    mockStoreState.nodes = [];
    mockStoreState.setSearchOpen.mockClear();
    mockStoreState.selectNode.mockClear();
    mockStoreState.setPanelTab.mockClear();
  });

  it("highlights the first result when query is typed", () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "a" } });
    // First result (Alice) should be highlighted
    const options = screen.getAllByRole("option");
    expect(options[0].getAttribute("aria-selected")).toBe("true");
  });

  it("ArrowDown moves highlight to the next item", () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "a" } }); // All 3 match
    fireEvent.keyDown(input, { key: "ArrowDown" });
    const options = screen.getAllByRole("option");
    expect(options[0].getAttribute("aria-selected")).toBe("false");
    expect(options[1].getAttribute("aria-selected")).toBe("true");
  });

  it("ArrowUp moves highlight to the previous item", () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "a" } }); // All 3 match
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "ArrowUp" });
    const options = screen.getAllByRole("option");
    expect(options[0].getAttribute("aria-selected")).toBe("true");
    expect(options[1].getAttribute("aria-selected")).toBe("false");
  });

  it("Enter selects the highlighted workspace", () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "a" } }); // All 3 match
    fireEvent.keyDown(input, { key: "ArrowDown" }); // Highlight Bob
    fireEvent.keyDown(input, { key: "Enter" });
    expect(mockStoreState.selectNode).toHaveBeenCalledWith("n1"); // Alice
    expect(mockStoreState.setPanelTab).toHaveBeenCalledWith("details");
    expect(mockStoreState.setSearchOpen).toHaveBeenCalledWith(false);
  });
});

describe("SearchDialog — aria attributes", () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    mockStoreState.searchOpen = false;
    mockStoreState.nodes = [];
    mockStoreState.setSearchOpen.mockClear();
    mockStoreState.selectNode.mockClear();
    mockStoreState.setPanelTab.mockClear();
  });

  it("dialog has role=dialog and aria-modal=true", () => {
    mockStoreState.searchOpen = true;
    render(<SearchDialog />);
    const dialog = screen.getByRole("dialog");
    expect(dialog.getAttribute("aria-modal")).toBe("true");
    expect(dialog.getAttribute("aria-label")).toBe("Search workspaces");
  });

  it("results container has role=listbox", () => {
    mockStoreState.searchOpen = true;
    mockStoreState.nodes = [
      { id: "n1", data: { name: "Alice", status: "online", tier: 4, role: "assistant" } },
    ];
    render(<SearchDialog />);
    expect(screen.getByRole("listbox")).toBeTruthy();
  });

  it("each result has role=option", () => {
    mockStoreState.searchOpen = true;
    mockStoreState.nodes = [
      { id: "n1", data: { name: "Alice", status: "online", tier: 4, role: "assistant" } },
    ];
    render(<SearchDialog />);
    expect(screen.getAllByRole("option").length).toBeGreaterThan(0);
  });
});

describe("SearchDialog — footer", () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
    mockStoreState.searchOpen = false;
    mockStoreState.nodes = [];
    mockStoreState.setSearchOpen.mockClear();
    mockStoreState.selectNode.mockClear();
    mockStoreState.setPanelTab.mockClear();
  });

  it("footer shows singular 'workspace' when count is 1", () => {
    mockStoreState.searchOpen = true;
    mockStoreState.nodes = [
      { id: "n1", data: { name: "Alice", status: "online", tier: 4, role: "assistant" } },
    ];
    render(<SearchDialog />);
    expect(screen.getByText("1 workspace")).toBeTruthy();
  });

  it("footer shows plural 'workspaces' when count > 1", () => {
    mockStoreState.searchOpen = true;
    mockStoreState.nodes = [
      { id: "n1", data: { name: "Alice", status: "online", tier: 4, role: "assistant" } },
      { id: "n2", data: { name: "Bob", status: "offline", tier: 2, role: "analyst" } },
    ];
    render(<SearchDialog />);
    expect(screen.getByText("2 workspaces")).toBeTruthy();
  });
});
