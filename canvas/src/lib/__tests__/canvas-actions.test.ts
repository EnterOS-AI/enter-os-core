/**
 * Tests for canvas-actions — two helpers that mark workspaces as
 * "needs restart" after a global / per-workspace config change.
 *
 * Used by:
 *   - markAllWorkspacesNeedRestart: triggered after a global secret
 *     change so the user sees a Restart Pending pill on every node
 *   - markWorkspaceNeedsRestart: per-workspace targeted after a single
 *     workspace's config edit
 *
 * Both reach into the canvas store via getState(), so the tests
 * mock the store's selector + getState shape. The bug surface is
 * tiny but the consequences of regressing markAllWorkspacesNeedRestart
 * are real — silently miss the pill on global secret changes and the
 * user can't tell which workspaces need a restart.
 *
 * Issue: #1815 follow-up — canvas-actions.ts was at 25% coverage.
 */
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vitest";

// ── Hoisted mocks ────────────────────────────────────────────────────────────

const { mockState } = vi.hoisted(() => ({
  mockState: {
    nodes: [] as Array<{ id: string }>,
    updateNodeData: vi.fn(),
  },
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: typeof mockState) => unknown) => selector(mockState),
    { getState: () => mockState },
  ),
}));

// Import the SUT after the mocks are declared.
import {
  markAllWorkspacesNeedRestart,
  markWorkspaceNeedsRestart,
} from "../canvas-actions";

beforeEach(() => {
  mockState.updateNodeData.mockReset();
  mockState.nodes = [];
});

afterEach(() => {
  vi.restoreAllMocks();
});

// ── markAllWorkspacesNeedRestart ─────────────────────────────────────────────

describe("markAllWorkspacesNeedRestart", () => {
  it("calls updateNodeData on every node with needsRestart: true", () => {
    mockState.nodes = [
      { id: "ws-a" },
      { id: "ws-b" },
      { id: "ws-c" },
    ];

    markAllWorkspacesNeedRestart();

    expect(mockState.updateNodeData).toHaveBeenCalledTimes(3);
    expect(mockState.updateNodeData).toHaveBeenCalledWith("ws-a", { needsRestart: true });
    expect(mockState.updateNodeData).toHaveBeenCalledWith("ws-b", { needsRestart: true });
    expect(mockState.updateNodeData).toHaveBeenCalledWith("ws-c", { needsRestart: true });
  });

  it("is a no-op when the canvas has no workspaces", () => {
    mockState.nodes = [];

    markAllWorkspacesNeedRestart();

    expect(mockState.updateNodeData).not.toHaveBeenCalled();
  });

  it("preserves call ordering for deterministic UI updates", () => {
    // Pinning the iteration order so a future refactor (e.g. switching
    // to forEach with shuffled keys, or adding async batching) doesn't
    // silently change the order updates fire — matters when the toolbar
    // observes per-node data changes incrementally.
    mockState.nodes = [
      { id: "ws-1" },
      { id: "ws-2" },
      { id: "ws-3" },
    ];

    markAllWorkspacesNeedRestart();

    const callOrder = mockState.updateNodeData.mock.calls.map((c) => c[0]);
    expect(callOrder).toEqual(["ws-1", "ws-2", "ws-3"]);
  });
});

// ── markWorkspaceNeedsRestart ────────────────────────────────────────────────

describe("markWorkspaceNeedsRestart", () => {
  it("calls updateNodeData on the named workspace only", () => {
    markWorkspaceNeedsRestart("ws-target");

    expect(mockState.updateNodeData).toHaveBeenCalledTimes(1);
    expect(mockState.updateNodeData).toHaveBeenCalledWith("ws-target", {
      needsRestart: true,
    });
  });

  it("does not enumerate the nodes list (purely targeted)", () => {
    // Defensive: if a future refactor accidentally wired this function
    // through the per-node iteration path of markAll, every workspace
    // would be marked. Pin that the function fires exactly ONCE
    // regardless of how many nodes are in the store.
    mockState.nodes = [
      { id: "ws-other-1" },
      { id: "ws-other-2" },
      { id: "ws-target" },
    ];

    markWorkspaceNeedsRestart("ws-target");

    expect(mockState.updateNodeData).toHaveBeenCalledTimes(1);
    expect(mockState.updateNodeData).toHaveBeenCalledWith("ws-target", {
      needsRestart: true,
    });
  });
});
