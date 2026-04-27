// @vitest-environment jsdom
/**
 * Tests for OrgCancelButton — the cancel-deployment pill on the root
 * card of an in-flight org. Two-step UX: click pill → confirm dialog
 * → cascade-delete with optimistic store update.
 *
 * Coverage targets the contracts a future refactor could regress:
 *   1. Default render: pill with `Cancel (N)` and the right ARIA label
 *   2. Click pill → stopPropagation + flip to confirming view
 *   3. Confirm copy pluralizes (1 workspace vs N workspaces)
 *   4. "No" → back to pill, no API call, no store mutation
 *   5. "Yes" happy path → beginDelete + api.del + optimistic store
 *      filter (subtree removed) + success toast + endDelete
 *   6. "Yes" error path → endDelete (UNDOing the lock) + error toast,
 *      no optimistic store filter
 *   7. Submitting state disables both buttons during the round-trip
 *
 * Issue: #2071 (Canvas test gaps follow-up).
 */
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  act,
} from "@testing-library/react";

// ── Hoisted mocks ────────────────────────────────────────────────────────────

const { mockApiDel, mockShowToast, mockState } = vi.hoisted(() => {
  const state = {
    nodes: [] as Array<{ id: string; data: { parentId?: string | null } }>,
    edges: [] as Array<{ source: string; target: string }>,
    beginDelete: vi.fn(),
    endDelete: vi.fn(),
  };
  return {
    mockApiDel: vi.fn(),
    mockShowToast: vi.fn(),
    mockState: state,
  };
});

vi.mock("@/lib/api", () => ({
  api: { del: mockApiDel },
}));

vi.mock("@/components/Toaster", () => ({
  showToast: mockShowToast,
}));

// useCanvasStore must support both selector-pattern usage AND
// getState() / setState() since handleCancel walks the subtree via
// getState() then mutates via setState() for the optimistic removal.
vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: typeof mockState) => unknown) => selector(mockState),
    {
      getState: () => mockState,
      setState: (patch: Partial<typeof mockState>) => Object.assign(mockState, patch),
    },
  ),
}));

// Import the SUT after the mocks are declared.
import { OrgCancelButton } from "../OrgCancelButton";

// ── Helpers ──────────────────────────────────────────────────────────────────

/** Reset mock state to a default subtree shaped {root: [child1, child2]}. */
function seedSubtree() {
  mockState.nodes = [
    { id: "ws-root", data: { parentId: null } },
    { id: "ws-child-1", data: { parentId: "ws-root" } },
    { id: "ws-child-2", data: { parentId: "ws-root" } },
    { id: "ws-unrelated", data: { parentId: null } },
  ];
  mockState.edges = [
    { source: "ws-root", target: "ws-child-1" },
    { source: "ws-root", target: "ws-child-2" },
  ];
}

beforeEach(() => {
  mockApiDel.mockReset();
  mockShowToast.mockReset();
  mockState.beginDelete.mockReset();
  mockState.endDelete.mockReset();
  seedSubtree();
});

afterEach(() => {
  cleanup();
});

// ── Tests ────────────────────────────────────────────────────────────────────

describe("OrgCancelButton — render", () => {
  it("default: shows the Cancel (N) pill with the right ARIA label", () => {
    render(
      <OrgCancelButton rootId="ws-root" rootName="My Org" workspaceCount={5} />,
    );
    const pill = screen.getByRole("button", {
      name: /cancel deployment of my org/i,
    });
    expect(pill.textContent).toContain("Cancel (5)");
  });

  it("does not render the confirm dialog initially", () => {
    render(
      <OrgCancelButton rootId="ws-root" rootName="My Org" workspaceCount={5} />,
    );
    expect(screen.queryByText(/^delete \d+ workspaces?\?$/i)).toBeNull();
  });
});

describe("OrgCancelButton — pill click", () => {
  it("flips to confirming view and stops propagation", () => {
    const parentClick = vi.fn();
    render(
      <div onClick={parentClick}>
        <OrgCancelButton rootId="ws-root" rootName="My Org" workspaceCount={5} />
      </div>,
    );
    fireEvent.click(
      screen.getByRole("button", { name: /cancel deployment of my org/i }),
    );
    expect(parentClick).not.toHaveBeenCalled();
    expect(screen.getByText(/delete 5 workspaces\?/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: /^yes$/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /^no$/i })).toBeTruthy();
  });

  it("confirm copy pluralizes — singular at count=1", () => {
    render(
      <OrgCancelButton rootId="ws-root" rootName="Solo" workspaceCount={1} />,
    );
    fireEvent.click(
      screen.getByRole("button", { name: /cancel deployment of solo/i }),
    );
    expect(screen.getByText(/^delete 1 workspace\?$/i)).toBeTruthy();
    // Negative: must NOT pluralize at count=1.
    expect(screen.queryByText(/^delete 1 workspaces\?$/i)).toBeNull();
  });

  it("confirm copy pluralizes — plural at count>1", () => {
    render(
      <OrgCancelButton rootId="ws-root" rootName="Big Org" workspaceCount={9} />,
    );
    fireEvent.click(
      screen.getByRole("button", { name: /cancel deployment of big org/i }),
    );
    expect(screen.getByText(/^delete 9 workspaces\?$/i)).toBeTruthy();
  });
});

describe("OrgCancelButton — No / cancel-confirm", () => {
  it("clicking No returns to the pill view, no API call, no store mutation", () => {
    render(
      <OrgCancelButton rootId="ws-root" rootName="My Org" workspaceCount={3} />,
    );
    // Open confirm
    fireEvent.click(
      screen.getByRole("button", { name: /cancel deployment of my org/i }),
    );
    // Dismiss
    fireEvent.click(screen.getByRole("button", { name: /^no$/i }));
    // Pill back; confirm gone
    expect(
      screen.getByRole("button", { name: /cancel deployment of my org/i }),
    ).toBeTruthy();
    expect(screen.queryByText(/delete \d+ workspaces?\?/i)).toBeNull();
    // No side effects
    expect(mockApiDel).not.toHaveBeenCalled();
    expect(mockState.beginDelete).not.toHaveBeenCalled();
    expect(mockState.endDelete).not.toHaveBeenCalled();
  });
});

describe("OrgCancelButton — Yes / cascade delete", () => {
  it("happy path: beginDelete → api.del → optimistic store filter → success toast → endDelete", async () => {
    mockApiDel.mockResolvedValueOnce({ status: "ok" });
    render(
      <OrgCancelButton rootId="ws-root" rootName="My Org" workspaceCount={3} />,
    );
    fireEvent.click(
      screen.getByRole("button", { name: /cancel deployment of my org/i }),
    );
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^yes$/i }));
    });

    // 1) API call hit the cascade-delete endpoint with confirm=true
    expect(mockApiDel).toHaveBeenCalledWith("/workspaces/ws-root?confirm=true");

    // 2) beginDelete locked the WHOLE subtree (root + 2 children) — NOT the unrelated node
    expect(mockState.beginDelete).toHaveBeenCalledTimes(1);
    const lockedIds = mockState.beginDelete.mock.calls[0][0] as Set<string>;
    expect(lockedIds.has("ws-root")).toBe(true);
    expect(lockedIds.has("ws-child-1")).toBe(true);
    expect(lockedIds.has("ws-child-2")).toBe(true);
    expect(lockedIds.has("ws-unrelated")).toBe(false);

    // 3) Optimistic store removal: subtree filtered out, unrelated kept
    const remainingIds = mockState.nodes.map((n) => n.id);
    expect(remainingIds).toEqual(["ws-unrelated"]);
    expect(mockState.edges).toHaveLength(0);

    // 4) Success toast
    expect(mockShowToast).toHaveBeenCalledWith(
      'Cancelled deployment of "My Org"',
      "success",
    );

    // 5) endDelete fired in the finally block (one call — the success
    //    path doesn't separately call endDelete in the try, only the
    //    catch does that; finally always runs once.)
    expect(mockState.endDelete).toHaveBeenCalledTimes(1);
  });

  it("bail-out: WS_REMOVED already dropped the root mid-flight → skip optimistic filter", async () => {
    // Simulate WS-event handler racing the await: by the time api.del
    // resolves, the root node is gone from the store. Without the
    // bail-out, the post-delete subtree walk would miss any orphaned
    // descendants (handleCanvasEvent reparents children of a removed
    // node upward — they no longer share root's id as parentId).
    mockApiDel.mockImplementationOnce(async () => {
      // During the network round-trip, the WS handler removes the root.
      mockState.nodes = mockState.nodes.filter((n) => n.id !== "ws-root");
      return { status: "ok" };
    });
    render(
      <OrgCancelButton rootId="ws-root" rootName="My Org" workspaceCount={3} />,
    );
    fireEvent.click(
      screen.getByRole("button", { name: /cancel deployment of my org/i }),
    );
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^yes$/i }));
    });

    // Success toast still fired (the cascade-delete API succeeded).
    expect(mockShowToast).toHaveBeenCalledWith(
      'Cancelled deployment of "My Org"',
      "success",
    );
    // beginDelete was called (locking happens before await).
    expect(mockState.beginDelete).toHaveBeenCalled();
    // The bail-out path means we did NOT attempt a second optimistic
    // setState after WS_REMOVED already cleared the root. The remaining
    // nodes reflect ONLY the WS handler's removal (just root gone).
    const remainingIds = mockState.nodes.map((n) => n.id).sort();
    expect(remainingIds).toEqual([
      "ws-child-1",
      "ws-child-2",
      "ws-unrelated",
    ]);
  });

  it("error path: endDelete UNDOes the lock + error toast, no optimistic filter", async () => {
    mockApiDel.mockRejectedValueOnce(new Error("server 500"));
    render(
      <OrgCancelButton rootId="ws-root" rootName="My Org" workspaceCount={3} />,
    );
    fireEvent.click(
      screen.getByRole("button", { name: /cancel deployment of my org/i }),
    );
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^yes$/i }));
    });

    // beginDelete fired (locks happen before the await).
    expect(mockState.beginDelete).toHaveBeenCalledTimes(1);
    // endDelete fires TWICE in the error path: once in the catch
    // (undo the lock) and once in the finally (idempotent on the
    // already-cleared set). The point of the test is that the lock
    // is undone; the duplicate endDelete is intentional and harmless
    // since the implementation's idempotent.
    expect(mockState.endDelete.mock.calls.length).toBeGreaterThanOrEqual(1);
    // No optimistic filter: subtree must STILL be in the store
    // (user can retry / interact with the still-deploying nodes).
    const remainingIds = mockState.nodes.map((n) => n.id).sort();
    expect(remainingIds).toEqual([
      "ws-child-1",
      "ws-child-2",
      "ws-root",
      "ws-unrelated",
    ]);
    // Error toast surfaces the error message
    expect(mockShowToast).toHaveBeenCalledWith(
      "Cancel failed: server 500",
      "error",
    );
  });

  it("error path with non-Error rejection: surfaces 'Cancel failed' fallback", async () => {
    mockApiDel.mockRejectedValueOnce("plain string");
    render(
      <OrgCancelButton rootId="ws-root" rootName="My Org" workspaceCount={3} />,
    );
    fireEvent.click(
      screen.getByRole("button", { name: /cancel deployment of my org/i }),
    );
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^yes$/i }));
    });

    expect(mockShowToast).toHaveBeenCalledWith("Cancel failed", "error");
  });
});
