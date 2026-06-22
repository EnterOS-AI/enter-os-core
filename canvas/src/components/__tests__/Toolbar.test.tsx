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

vi.mock("@/store/canvas", () => {
  // useCanvasStore is used in two shapes:
  //   1. As a hook: `useCanvasStore((s) => s.x)` — selector path.
  //   2. As a static accessor: `useCanvasStore.getState().nodes` —
  //      used by stopAll's drain-poll loop (task #377 Toolbar fix) and
  //      restartAll's success-clear loop. Both read the LIVE
  //      defaultStore object so tests that mutate `defaultStore.nodes`
  //      mid-flight (e.g. simulating a TASK_UPDATED that drops
  //      activeTasks to 0) see the update on the next poll tick.
  const hook = vi.fn((selector: (s: typeof defaultStore) => unknown) =>
    selector(defaultStore)
  ) as unknown as ((selector: (s: typeof defaultStore) => unknown) => unknown) & {
    getState: () => typeof defaultStore;
  };
  hook.getState = () => defaultStore;
  return { useCanvasStore: hook };
});

// ── Component under test ───────────────────────────────────────────────────────
import { Toolbar } from "../Toolbar";
// Imported AFTER vi.mock("@/lib/api", ...) above (hoisted) so this
// resolves to the mock module; gives the new task #377 tests a typed
// handle on api.post without a CJS require() (Vitest runs ESM).
import { api as mockedApi } from "@/lib/api";

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

// ── Toolbar — Stop All polite-cancel flow (task #377) ───────────────────────

describe("Toolbar — Stop All polite cancel before restart (#377)", () => {
  // `api` resolves to the top-level vi.mock factory's mocked `post`.
  // We type-cast so TS allows mockReset/mockResolvedValue/mockImplementation
  // calls without leaking the mock surface into the production type.
  const api = mockedApi as unknown as { post: ReturnType<typeof vi.fn> };

  /**
   * Build a working set of two active workspaces so the assertions can
   * distinguish per-id behavior (drained vs undrained) within one test.
   */
  const seedTwoActive = () => {
    defaultStore.nodes = toStoreNodes(makeNodes(["online", "online"], [2, 2]));
  };

  /**
   * Drive an async useCallback handler to completion. Vitest's fake
   * timers don't see microtasks unless we yield between advances; the
   * helper interleaves `vi.advanceTimersByTimeAsync` with macrotask
   * yields so pending fetch resolutions and setTimeout callbacks both
   * settle before the assertion runs.
   */
  const advanceUntilSettled = async (ms: number) => {
    await vi.advanceTimersByTimeAsync(ms);
    // One extra tick lets any chained .then() after a setTimeout
    // resolution fire before the test moves on.
    await Promise.resolve();
  };

  beforeEach(() => {
    vi.useFakeTimers();
    api.post.mockReset();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("phase 1: issues tasks/cancel via /workspaces/:id/a2a BEFORE any /restart", async () => {
    seedTwoActive();
    // Hold both tasks/cancel responses open so the click handler is
    // observably paused at phase 1. We don't actually need to resolve
    // them for the order assertion — just inspect the call log.
    let resolveCancels!: () => void;
    const cancelGate = new Promise<void>((r) => { resolveCancels = r; });
    api.post.mockImplementation(async (path: string) => {
      if (path.endsWith("/a2a")) {
        await cancelGate;
      }
      return undefined;
    });

    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /stop all running tasks/i });
    fireEvent.click(btn);

    // Yield once so the click handler enters phase 1 and dispatches the
    // two /a2a POSTs.
    await Promise.resolve();
    await Promise.resolve();

    const a2aCalls = api.post.mock.calls.filter((c) => String(c[0]).endsWith("/a2a"));
    const restartCalls = api.post.mock.calls.filter((c) => String(c[0]).endsWith("/restart"));
    expect(a2aCalls.length).toBe(2);
    expect(restartCalls.length).toBe(0);

    // Each /a2a POST carries the canonical tasks/cancel envelope.
    for (const call of a2aCalls) {
      expect(call[1]).toEqual({ method: "tasks/cancel", params: {} });
    }

    // Release the gate so the test cleanup doesn't dangle.
    resolveCancels();
    await advanceUntilSettled(10_000);
  });

  it("phase 2: when activeTasks drains to 0 during the poll window, /restart is NOT called", async () => {
    seedTwoActive();
    api.post.mockResolvedValue(undefined);

    render(<Toolbar />);
    fireEvent.click(screen.getByRole("button", { name: /stop all running tasks/i }));

    // Let phase 1 fire (the two tasks/cancel calls).
    await Promise.resolve();
    await Promise.resolve();

    // Simulate the platform pushing TASK_UPDATED with active_tasks=0
    // on both workspaces — emulate by mutating the store directly,
    // which is what canvas-events.ts does in production.
    defaultStore.nodes = toStoreNodes(makeNodes(["online", "online"], [0, 0]));

    // Advance past the first poll interval (250ms) so the loop sees
    // the drained store and exits early.
    await advanceUntilSettled(400);
    // Drain any remaining timers so the handler returns cleanly.
    await advanceUntilSettled(10_000);

    const restartCalls = api.post.mock.calls.filter((c) => String(c[0]).endsWith("/restart"));
    expect(restartCalls.length).toBe(0);
  });

  it("phase 3: when activeTasks does NOT drain inside the timeout, falls through to /restart for each stuck workspace", async () => {
    seedTwoActive();
    api.post.mockResolvedValue(undefined);

    render(<Toolbar />);
    fireEvent.click(screen.getByRole("button", { name: /stop all running tasks/i }));

    // Phase 1 dispatch.
    await Promise.resolve();
    await Promise.resolve();

    // Do NOT drain — activeTasks stays at 2 for both. Advance past the
    // 8000ms drain timeout plus a buffer so phase 3's /restart POSTs fire.
    await advanceUntilSettled(9_000);
    await advanceUntilSettled(1_000);

    const a2aCalls = api.post.mock.calls.filter((c) => String(c[0]).endsWith("/a2a"));
    const restartCalls = api.post.mock.calls.filter((c) => String(c[0]).endsWith("/restart"));
    expect(a2aCalls.length).toBe(2);
    expect(restartCalls.length).toBe(2);

    // Order check: every /a2a call comes before every /restart call.
    const lastA2AIdx = Math.max(
      ...api.post.mock.calls.map((c, i) => (String(c[0]).endsWith("/a2a") ? i : -1))
    );
    const firstRestartIdx = Math.min(
      ...api.post.mock.calls.map((c, i) => (String(c[0]).endsWith("/restart") ? i : Infinity))
    );
    expect(lastA2AIdx).toBeLessThan(firstRestartIdx);
  });

  it("phase 3 selective: drains only one of two workspaces — /restart is called only for the stuck one", async () => {
    seedTwoActive();
    api.post.mockResolvedValue(undefined);

    render(<Toolbar />);
    fireEvent.click(screen.getByRole("button", { name: /stop all running tasks/i }));

    await Promise.resolve();
    await Promise.resolve();

    // ws-0 drains immediately, ws-1 stays stuck for the full timeout.
    defaultStore.nodes = toStoreNodes(makeNodes(["online", "online"], [0, 2]));
    await advanceUntilSettled(9_500);

    const restartCalls = api.post.mock.calls.filter((c) => String(c[0]).endsWith("/restart"));
    expect(restartCalls.length).toBe(1);
    expect(restartCalls[0][0]).toBe("/workspaces/ws-1/restart");
  });
});
