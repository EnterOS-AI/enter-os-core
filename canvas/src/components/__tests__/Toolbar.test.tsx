// @vitest-environment jsdom
/**
 * Toolbar tests.
 *
 * Covers:
 *   - Renders with 0 workspaces
 *   - Shows online/offline/failed/provisioning status pills when nodes exist
 *   - WebSocket status pill: connected → "Live"
 *   - WebSocket status pill: connecting → "Reconnecting"
 *   - WebSocket status pill: disconnected → "Offline"
 *   - Stop All button visible when activeTasks > 0
 *   - Restart Pending button visible when needsRestart nodes exist
 *   - Help button opens the help popover
 *   - Help popover closes on Escape or pointer-outside
 *   - KeyboardShortcutsDialog opens via ? shortcut (when not in input)
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import React from "react";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// Reset store state between tests so mutations don't leak.
beforeEach(() => {
  defaultStore.nodes = [];
  defaultStore.wsStatus = "connected";
  defaultStore.showA2AEdges = false;
  defaultStore.selectedNodeId = null;
  mockSetShowA2AEdges.mockClear();
  mockSetPanelTab.mockClear();
  mockSetSearchOpen.mockClear();
  mockUpdateNodeData.mockClear();
});

// ── Mock targets ───────────────────────────────────────────────────────────────

vi.mock("@/components/Toaster", () => ({
  showToast: vi.fn(),
}));

vi.mock("@/components/ConfirmDialog", () => ({
  ConfirmDialog: () => null,
}));

vi.mock("@/components/settings/SettingsButton", () => ({
  SettingsButton: () => null,
}));

vi.mock("@/components/settings/SettingsPanel", () => ({
  settingsGearRef: { current: null },
}));

vi.mock("@/components/ThemeToggle", () => ({
  ThemeToggle: () => null,
}));

vi.mock("@/components/KeyboardShortcutsDialog", () => ({
  KeyboardShortcutsDialog: ({ open }: { open: boolean; onClose: () => void }) =>
    open ? <div role="dialog" data-testid="shortcuts-dialog">Shortcuts</div> : null,
}));

vi.mock("@/lib/design-tokens", () => ({
  statusDotClass: (status: string) => {
    const map: Record<string, string> = {
      online: "bg-emerald-400",
      offline: "bg-zinc-500",
      paused: "bg-indigo-400",
      degraded: "bg-amber-400",
      failed: "bg-red-400",
      provisioning: "bg-sky-400",
    };
    return map[status] ?? "bg-zinc-500";
  },
}));

vi.mock("@/lib/api", () => ({
  api: {
    post: vi.fn(() => Promise.resolve()),
  },
}));

// ── Store mocks ────────────────────────────────────────────────────────────────

const mockSetShowA2AEdges = vi.fn();
const mockSetPanelTab = vi.fn();
const mockSetSearchOpen = vi.fn();
const mockUpdateNodeData = vi.fn();

const makeNodes = (
  statuses: Array<"online" | "offline" | "failed" | "provisioning">,
  activeTasks: number[] = [],
  needsRestart: boolean[] = [],
  parentIds: (string | null)[] = []
) => {
  return statuses.map((status, i) => ({
    id: `ws-${i}`,
    data: {
      name: `Workspace ${i}`,
      role: "agent",
      tier: 1,
      status,
      parentId: parentIds[i] ?? null,
      activeTasks: activeTasks[i] ?? 0,
      needsRestart: needsRestart[i] ?? false,
    },
  }));
};

// Nodes must be React Flow nodes (id + data), but Toolbar only reads data fields.
// makeNodes returns { id, data: { activeTasks, needsRestart, ... } }.
const toStoreNodes = (nodes: ReturnType<typeof makeNodes>) =>
  nodes.map((n) => ({ id: n.id, data: n.data }));

const defaultStore = {
  nodes: [] as ReturnType<typeof makeNodes>,
  wsStatus: "connected" as "connected" | "connecting" | "disconnected",
  showA2AEdges: false,
  selectedNodeId: null as string | null,
  sidePanelWidth: 480,
  setShowA2AEdges: mockSetShowA2AEdges,
  setPanelTab: mockSetPanelTab,
  setSearchOpen: mockSetSearchOpen,
  updateNodeData: mockUpdateNodeData,
  selectedNodeIds: new Set<string>(),
  clearSelection: vi.fn(),
  batchRestart: vi.fn(() => Promise.resolve()),
  batchPause: vi.fn(() => Promise.resolve()),
  batchDelete: vi.fn(() => Promise.resolve()),
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn((selector: (s: typeof defaultStore) => unknown) =>
    selector(defaultStore)
  ),
}));

// ── Component under test ───────────────────────────────────────────────────────
import { Toolbar } from "../Toolbar";

// ── Tests ─────────────────────────────────────────────────────────────────────

describe("Toolbar — workspace count display", () => {
  it("shows '0 workspaces' when the canvas is empty", () => {
    render(<Toolbar />);
    expect(screen.getByText(/0 workspaces?/)).toBeTruthy();
  });

  it("shows 'N workspaces' when nodes exist", () => {
    defaultStore.nodes = toStoreNodes(makeNodes(["online", "online"]));
    render(<Toolbar />);
    expect(screen.getByText(/2 workspaces?/)).toBeTruthy();
  });
});

describe("Toolbar — status pills", () => {
  it("shows the online pill when nodes are online", () => {
    defaultStore.nodes = toStoreNodes(makeNodes(["online"]));
    render(<Toolbar />);
    // StatusPill uses aria-label
    expect(screen.getByLabelText(/1 online/i)).toBeTruthy();
  });

  it("shows the offline pill only when offline nodes exist", () => {
    defaultStore.nodes = toStoreNodes(makeNodes(["offline"]));
    render(<Toolbar />);
    expect(screen.getByLabelText(/1 offline/i)).toBeTruthy();
  });

  it("shows the failed pill when failed nodes exist", () => {
    defaultStore.nodes = toStoreNodes(makeNodes(["failed"]));
    render(<Toolbar />);
    expect(screen.getByLabelText(/1 failed/i)).toBeTruthy();
  });

  it("shows the provisioning pill when provisioning nodes exist", () => {
    defaultStore.nodes = toStoreNodes(makeNodes(["provisioning"]));
    render(<Toolbar />);
    expect(screen.getByLabelText(/1 starting/i)).toBeTruthy();
  });

  it("suppresses offline pill when no offline nodes", () => {
    defaultStore.nodes = toStoreNodes(makeNodes(["online", "online"]));
    render(<Toolbar />);
    expect(screen.queryByLabelText(/offline/i)).toBeNull();
  });
});

describe("Toolbar — WebSocket status pill", () => {
  it('shows "Live" when connected', () => {
    defaultStore.wsStatus = "connected";
    render(<Toolbar />);
    expect(screen.getByText("Live")).toBeTruthy();
  });

  it('shows "Reconnecting" when connecting', () => {
    defaultStore.wsStatus = "connecting";
    render(<Toolbar />);
    expect(screen.getByText("Reconnecting")).toBeTruthy();
  });

  it('shows "Offline" when disconnected', () => {
    defaultStore.wsStatus = "disconnected";
    render(<Toolbar />);
    expect(screen.getByText("Offline")).toBeTruthy();
  });
});

describe("Toolbar — Stop All", () => {
  it("is hidden when no active tasks", () => {
    defaultStore.nodes = toStoreNodes(makeNodes(["online"], [0]));
    render(<Toolbar />);
    expect(screen.queryByRole("button", { name: /Stop All/i })).toBeNull();
  });

  it("is visible when active tasks > 0", () => {
    defaultStore.nodes = toStoreNodes(makeNodes(["online", "online"], [2, 2]));
    render(<Toolbar />);
    // aria-label: "Stop all running tasks (2)"
    expect(screen.getByRole("button", { name: /stop all running tasks/i })).toBeTruthy();
  });
});

describe("Toolbar — Restart Pending", () => {
  it("is hidden when no nodes need restart", () => {
    defaultStore.nodes = toStoreNodes(makeNodes(["online"], [], [false]));
    render(<Toolbar />);
    expect(screen.queryByRole("button", { name: /Restart Pending/i })).toBeNull();
  });

  it("is visible when nodes need restart", () => {
    defaultStore.nodes = toStoreNodes(makeNodes(["online"], [], [true]));
    render(<Toolbar />);
    // aria-label: "Restart 1 workspaces pending config or secret changes"
    expect(screen.getByRole("button", { name: /restart 1 workspace/i })).toBeTruthy();
  });
});

describe("Toolbar — Help popover", () => {
  it("opens when help button is clicked", () => {
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    fireEvent.click(helpBtn);
    expect(screen.getByRole("dialog")).toBeTruthy();
  });

  it("closes when close button is clicked", () => {
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    fireEvent.click(helpBtn);
    expect(screen.getByRole("dialog")).toBeTruthy();
    const closeBtn = screen.getByRole("button", { name: /close help dialog/i });
    fireEvent.click(closeBtn);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("closes when pointer is pressed outside the help popover", () => {
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    fireEvent.click(helpBtn);
    expect(screen.getByRole("dialog")).toBeTruthy();
    // Simulate pointerdown outside the help popover (not on the help button)
    fireEvent.pointerDown(document.body);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("opens on click even after a previous pointer-outside close", () => {
    // Regression: clicking outside closed the popover AND toggled the button
    // state, so the next click on the button would close it again.
    // The fix makes the button always open (never toggle) so re-opening works.
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    fireEvent.click(helpBtn);
    expect(screen.getByRole("dialog")).toBeTruthy();
    // Click outside (pointerdown on body, not on help button)
    fireEvent.pointerDown(document.body);
    expect(screen.queryByRole("dialog")).toBeNull();
    // Click the help button again — must re-open, not double-close
    fireEvent.click(helpBtn);
    expect(screen.getByRole("dialog")).toBeTruthy();
  });
});

describe("Toolbar — A2A edges toggle", () => {
  it("calls setShowA2AEdges on click", () => {
    defaultStore.showA2AEdges = false;
    render(<Toolbar />);
    const toggle = screen.getByRole("button", { name: /show a2a edges/i });
    fireEvent.click(toggle);
    expect(mockSetShowA2AEdges).toHaveBeenCalledWith(true);
  });
});

describe("Toolbar — ? shortcut opens shortcuts dialog", () => {
  it("opens KeyboardShortcutsDialog when ? is pressed outside an input", () => {
    render(<Toolbar />);
    expect(screen.queryByTestId("shortcuts-dialog")).toBeNull();
    fireEvent.keyDown(window, { key: "?" });
    expect(screen.getByTestId("shortcuts-dialog")).toBeTruthy();
  });

  it("does not fire ? shortcut when focus is in an input", () => {
    render(
      <div>
        <input data-testid="test-input" type="text" />
        <Toolbar />
      </div>
    );
    const input = screen.getByTestId("test-input");
    fireEvent.focus(input);
    // Fire on the input element so e.target.tagName === "INPUT" is true
    fireEvent.keyDown(input, { key: "?" });
    expect(screen.queryByTestId("shortcuts-dialog")).toBeNull();
  });
});
