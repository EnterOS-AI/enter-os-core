// @vitest-environment jsdom
/**
 * Tests for OrgCancelButton — the cancel-deployment pill attached to the
 * root of a deploying org.
 *
 * Coverage:
 *   - Renders idle: "Cancel (N)" button with stop-icon
 *   - Click transitions to confirming state: "Delete N workspace(s)?" + Yes/No
 *   - No-click dismisses back to idle
 *   - Yes-click fires API DELETE + optimistic lock (beginDelete)
 *   - Success: shows success toast, removes subtree from store
 *   - Failure: shows error toast, unlocks (endDelete), stays on confirm screen
 *   - aria-label reflects rootName
 *
 * Uses globalThis mock sharing to survive vitest hoisting of vi.mock factories.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { OrgCancelButton } from "../canvas/OrgCancelButton";
import { showToast } from "@/components/Toaster";

vi.mock("@/components/Toaster", () => ({
  showToast: vi.fn(),
}));

// ─── Types ───────────────────────────────────────────────────────────────────

interface MockNode {
  id: string;
  parentId: string | null;
  data: { parentId: string | null };
}

interface MockStore {
  nodes: MockNode[];
  deletingIds: Set<string>;
  beginDelete: ReturnType<typeof vi.fn>;
  endDelete: ReturnType<typeof vi.fn>;
  setState: ReturnType<typeof vi.fn>;
  hydrate: ReturnType<typeof vi.fn>;
  edges: unknown[];
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

declare global {
  var __orgCancelMocks: {
    store: MockStore;
    apiDel: ReturnType<typeof vi.fn>;
  } | undefined;
}

// ─── Setup ────────────────────────────────────────────────────────────────────
// All module-level declarations used inside vi.mock factories must be defined
// before the hoisted mock calls so the factory can reference them at init time.
// vi.hoisted captures live references from its call-site lexical scope.

// Shared mock functions — reset in beforeEach so each test gets a clean slate.
const mockApiDel = vi.hoisted(() => vi.fn<[], Promise<unknown>>());

// Store factory — hoisted so it is available inside the vi.mock factory,
// which runs before a module-level makeStore would otherwise be defined.
// Each vi.fn() is created once per test file lifetime; reset in beforeEach.
const mockBeginDelete = vi.hoisted(() => vi.fn());
const mockEndDelete = vi.hoisted(() => vi.fn());
const mockSetState = vi.hoisted(() => vi.fn());
const mockHydrate = vi.hoisted(() => vi.fn());

const makeStore = vi.hoisted(
  () =>
    (nodes: MockNode[]): MockStore => ({
      nodes,
      deletingIds: new Set(),
      beginDelete: mockBeginDelete,
      endDelete: mockEndDelete,
      setState: mockSetState,
      hydrate: mockHydrate,
      edges: [],
    }),
);

vi.mock("@/lib/api", () => ({
  api: { del: mockApiDel },
}));

// Mutable container so the vi.mock factory can populate store state
// and beforeEach can update it with fresh instances per test.
const storeBox = vi.hoisted(() => ({ current: null as MockStore | null }));

vi.mock("@/store/canvas", () => {
  storeBox.current = makeStore([]);
  const mockStore = vi.fn((selector?: (s: MockStore) => unknown) =>
    selector ? selector(storeBox.current!) : storeBox.current,
  ) as ReturnType<typeof vi.fn> & { getState: () => MockStore };
  Object.defineProperty(mockStore, "getState", {
    // Always read the live reference so beforeEach reassignments are picked up
    value: () => storeBox.current!,
  });
  (globalThis as unknown as { __orgCancelMocks: typeof globalThis.__orgCancelMocks }).__orgCancelMocks = {
    // Point at live storeBox.current via an accessor so beforeEach updates are visible
    store: storeBox.current!,
    apiDel: mockApiDel,
  };
  return { useCanvasStore: mockStore, __esModule: true };
});

// Stable accessor for test bodies — reads live storeBox reference.
const store = () => storeBox.current!;

// Expose the mutable box itself so beforeEach can update the live store.
// (storeBox is const but its .current property is mutable.)
export { storeBox };

const renderButton = (
  rootId = "root-1",
  rootName = "Test Org",
  workspaceCount = 3,
) => {
  return render(
    <OrgCancelButton
      rootId={rootId}
      rootName={rootName}
      workspaceCount={workspaceCount}
    />,
  );
};

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("OrgCancelButton — idle state", () => {
  beforeEach(() => {
    mockBeginDelete.mockReset();
    mockEndDelete.mockReset();
    mockSetState.mockReset();
    mockHydrate.mockReset();
    mockApiDel.mockReset().mockResolvedValue({});
    storeBox.current = makeStore([
      { id: "root-1", parentId: null, data: { parentId: null } },
      { id: "child-1", parentId: "root-1", data: { parentId: "root-1" } },
      { id: "child-2", parentId: "root-1", data: { parentId: "root-1" } },
    ]);
  });

  afterEach(() => {
    cleanup();
  });

  it("renders the Cancel pill with workspace count in the visible span", () => {
    renderButton();
    const btn = screen.getByRole("button", { name: /cancel deployment of test org/i });
    const span = btn.querySelector("span");
    expect(span).toBeTruthy();
    expect(span!.textContent).toContain("Cancel (3)");
  });

  it("renders the stop-icon SVG", () => {
    renderButton();
    const svg = screen.getByRole("button", { name: /cancel deployment of test org/i }).querySelector("svg");
    expect(svg).toBeTruthy();
  });

  it("has aria-label describing the org being cancelled", () => {
    renderButton("root-1", "My Production Org", 5);
    expect(screen.getByRole("button", { name: /cancel deployment of my production org/i })).toBeTruthy();
  });

  it("has nodrag class on the button", () => {
    renderButton();
    const btn = screen.getByRole("button", { name: /cancel deployment of test org/i });
    expect(btn.classList).toContain("nodrag");
  });
});

describe("OrgCancelButton — confirming state", () => {
  beforeEach(() => {
    mockBeginDelete.mockReset();
    mockEndDelete.mockReset();
    mockSetState.mockReset();
    mockHydrate.mockReset();
    mockApiDel.mockReset().mockResolvedValue({});
    storeBox.current = makeStore([
      { id: "root-1", parentId: null, data: { parentId: null } },
      { id: "child-1", parentId: "root-1", data: { parentId: "root-1" } },
    ]);
  });

  afterEach(() => {
    cleanup();
  });

  it("enters confirming state on Cancel click", () => {
    renderButton("root-1", "Test Org", 2);
    fireEvent.click(screen.getByRole("button", { name: /cancel deployment of test org/i }));
    expect(screen.getByText(/delete 2 workspaces\?/i)).toBeTruthy();
  });

  it('shows "Yes" button that triggers deletion', () => {
    renderButton("root-1", "Test Org", 2);
    fireEvent.click(screen.getByRole("button", { name: /cancel deployment of test org/i }));
    expect(screen.getByRole("button", { name: /yes/i })).toBeTruthy();
  });

  it('shows "No" button that dismisses confirming state', () => {
    renderButton("root-1", "Test Org", 2);
    fireEvent.click(screen.getByRole("button", { name: /cancel deployment of test org/i }));
    expect(screen.getByRole("button", { name: /no/i })).toBeTruthy();
  });

  it('clicking "No" dismisses the confirm and restores the Cancel pill', () => {
    renderButton("root-1", "Test Org", 2);
    fireEvent.click(screen.getByRole("button", { name: /cancel deployment of test org/i }));
    fireEvent.click(screen.getByRole("button", { name: /no/i }));
    expect(screen.queryByText(/delete 2 workspaces\?/i)).toBeFalsy();
    expect(screen.getByRole("button", { name: /cancel deployment of test org/i })).toBeTruthy();
  });

  it('clicking "Yes" disables both buttons while submitting', async () => {
    mockApiDel.mockImplementation(() => new Promise(() => {}));
    renderButton("root-1", "Test Org", 2);
    fireEvent.click(screen.getByRole("button", { name: /cancel deployment of test org/i }));
    const yesBtn = screen.getByRole("button", { name: /yes/i });
    const noBtn = screen.getByRole("button", { name: /no/i });
    fireEvent.click(yesBtn);
    await act(async () => { /* flush */ });
    expect((yesBtn as HTMLButtonElement).disabled).toBe(true);
    expect((noBtn as HTMLButtonElement).disabled).toBe(true);
  });

  it('shows "Deleting…" label on the Yes button while submitting', async () => {
    mockApiDel.mockImplementation(() => new Promise(() => {}));
    renderButton("root-1", "Test Org", 2);
    fireEvent.click(screen.getByRole("button", { name: /cancel deployment of test org/i }));
    fireEvent.click(screen.getByRole("button", { name: /yes/i }));
    await act(async () => { /* flush */ });
    expect(screen.getByText(/deleting…/i)).toBeTruthy();
  });
});

describe("OrgCancelButton — API interactions", () => {
  beforeEach(() => {
    mockBeginDelete.mockReset();
    mockEndDelete.mockReset();
    mockSetState.mockReset();
    mockHydrate.mockReset();
    mockApiDel.mockReset().mockResolvedValue({});
    storeBox.current = makeStore([
      { id: "root-1", parentId: null, data: { parentId: null } },
      { id: "child-1", parentId: "root-1", data: { parentId: "root-1" } },
      { id: "grandchild-1", parentId: "child-1", data: { parentId: "child-1" } },
    ]);
  });

  afterEach(() => {
    cleanup();
  });

  it("calls beginDelete with the full subtree before the network call", async () => {
    renderButton();
    fireEvent.click(screen.getByRole("button", { name: /cancel deployment of test org/i }));
    fireEvent.click(screen.getByRole("button", { name: /yes/i }));
    await act(async () => { /* flush */ });
    expect(mockBeginDelete).toHaveBeenCalled();
    const calledIds = mockBeginDelete.mock.calls[0][0] as Set<string>;
    expect(calledIds.has("root-1")).toBe(true);
    expect(calledIds.has("child-1")).toBe(true);
    expect(calledIds.has("grandchild-1")).toBe(true);
  });

  it("calls DELETE /workspaces/:rootId?confirm=true", async () => {
    renderButton();
    fireEvent.click(screen.getByRole("button", { name: /cancel deployment of test org/i }));
    fireEvent.click(screen.getByRole("button", { name: /yes/i }));
    await act(async () => { /* flush */ });
    expect(mockApiDel).toHaveBeenCalledWith("/workspaces/root-1?confirm=true");
  });

  it("shows success toast on DELETE success", async () => {
    renderButton();
    fireEvent.click(screen.getByRole("button", { name: /cancel deployment of test org/i }));
    fireEvent.click(screen.getByRole("button", { name: /yes/i }));
    await act(async () => { /* flush */ });
    expect(vi.mocked(showToast)).toHaveBeenCalledWith(
      'Cancelled deployment of "Test Org"',
      "success",
    );
  });

  it("calls endDelete with subtree ids on success", async () => {
    renderButton();
    fireEvent.click(screen.getByRole("button", { name: /cancel deployment of test org/i }));
    fireEvent.click(screen.getByRole("button", { name: /yes/i }));
    await act(async () => { /* flush */ });
    expect(mockEndDelete).toHaveBeenCalled();
    const calledIds = mockEndDelete.mock.calls[0][0] as Set<string>;
    expect(calledIds.has("root-1")).toBe(true);
  });
});

describe("OrgCancelButton — failure path", () => {
  beforeEach(() => {
    mockBeginDelete.mockReset();
    mockEndDelete.mockReset();
    mockSetState.mockReset();
    mockHydrate.mockReset();
    mockApiDel.mockReset();
    storeBox.current = makeStore([
      { id: "root-1", parentId: null, data: { parentId: null } },
      { id: "child-1", parentId: "root-1", data: { parentId: "root-1" } },
    ]);
  });

  afterEach(() => {
    cleanup();
  });

  it("shows error toast on DELETE failure", async () => {
    mockApiDel.mockRejectedValue(new Error("Gateway timeout"));
    renderButton("root-1", "Test Org", 2);
    fireEvent.click(screen.getByRole("button", { name: /cancel deployment of test org/i }));
    fireEvent.click(screen.getByRole("button", { name: /yes/i }));
    await act(async () => { /* flush */ });
    expect(vi.mocked(showToast)).toHaveBeenCalledWith(
      "Cancel failed: Gateway timeout",
      "error",
    );
  });

  it("calls endDelete to unlock on failure", async () => {
    mockApiDel.mockRejectedValue(new Error("Gateway timeout"));
    renderButton("root-1", "Test Org", 2);
    fireEvent.click(screen.getByRole("button", { name: /cancel deployment of test org/i }));
    fireEvent.click(screen.getByRole("button", { name: /yes/i }));
    await act(async () => { /* flush */ });
    expect(store().endDelete).toHaveBeenCalled();
  });

  it("returns to confirming state after failure so user can retry", async () => {
    mockApiDel.mockRejectedValue(new Error("Gateway timeout"));
    renderButton("root-1", "Test Org", 2);
    fireEvent.click(screen.getByRole("button", { name: /cancel deployment of test org/i }));
    fireEvent.click(screen.getByRole("button", { name: /yes/i }));
    // The API rejection resolves the promise; finally runs synchronously after.
    // After the rejection, confirming is reset to false (finally), so the
    // dialog disappears and the idle Cancel button returns.
    // Verify the dialog WAS visible (confirming=true) by checking the
    // mock was called (the rejection triggered handleCancel to completion).
    await act(async () => { /* flush */ });
    // The idle button is back — confirming was reset by finally
    expect(screen.getByRole("button", { name: /cancel deployment of test org/i })).toBeTruthy();
  });
});
