// @vitest-environment jsdom
/**
 * DetailsTab integration tests for issue #541.
 *
 * Budget-specific logic (stats, progress bar, PATCH /budget, 402 handling) is
 * fully covered by BudgetSection.test.tsx — this file focuses on:
 *   1. BudgetSection being mounted inside DetailsTab
 *   2. The workspace edit form (name / role / tier) no longer carrying
 *      budget_limit — that concern lives in BudgetSection now
 *   3. PATCH /workspaces/:id body integrity (no accidental budget_limit leak)
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";

// ── Mocks ─────────────────────────────────────────────────────────────────────

vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn(),
    patch: vi.fn(),
    del: vi.fn(),
    post: vi.fn(),
  },
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn((selector: (s: unknown) => unknown) =>
    selector({
      updateNodeData: mockUpdateNodeData,
      removeNode: vi.fn(),
      selectNode: vi.fn(),
    })
  ),
}));

vi.mock("../StatusDot", () => ({ StatusDot: () => null }));

// Mock BudgetSection — it has its own test suite (BudgetSection.test.tsx).
// Without this mock its internal api.get would fire against the shared mock
// and cause type errors when the return is not a valid BudgetData object.
vi.mock("../tabs/BudgetSection", () => ({
  BudgetSection: ({ workspaceId }: { workspaceId: string }) => (
    <div data-testid="budget-section-stub" data-ws={workspaceId} />
  ),
}));

// Mock WorkspaceUsage — it has its own test suite (WorkspaceUsage.test.tsx).
// Without this mock its internal api.get call races against the shared mock
// and crashes when the return value is not a valid WorkspaceMetrics object.
vi.mock("../WorkspaceUsage", () => ({
  WorkspaceUsage: ({ workspaceId }: { workspaceId: string }) => (
    <div data-testid="workspace-usage-stub" data-ws={workspaceId} />
  ),
}));

import { api } from "@/lib/api";
import { DetailsTab } from "../tabs/DetailsTab";

const mockPatch = vi.mocked(api.patch);
const mockGet = vi.mocked(api.get);
const mockUpdateNodeData = vi.fn();

// ── Helpers ───────────────────────────────────────────────────────────────────

function makeData(overrides: Record<string, unknown> = {}) {
  return {
    name: "Test Agent",
    role: "Researcher",
    tier: 1,
    status: "online",
    agentCard: null,
    activeTasks: 0,
    collapsed: false,
    lastErrorRate: 0,
    lastSampleError: "",
    url: "http://localhost:8080",
    parentId: null,
    currentTask: "",
    runtime: "langgraph",
    needsRestart: false,
    budgetLimit: null,
    budgetUsed: null,
    ...overrides,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  mockGet.mockResolvedValue([] as any);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  mockPatch.mockResolvedValue({} as any);
});

afterEach(() => {
  cleanup();
});

async function openEdit() {
  const editBtn = screen.getAllByRole("button").find((b) => b.textContent === "Edit");
  fireEvent.click(editBtn!);
  await waitFor(() =>
    expect(screen.getAllByRole("button").some((b) => b.textContent === "Save")).toBe(true)
  );
}

// ── BudgetSection mounting ────────────────────────────────────────────────────

describe("DetailsTab — BudgetSection integration", () => {
  it("renders BudgetSection with the correct workspaceId", () => {
    render(<DetailsTab workspaceId="ws-42" data={makeData()} />);
    const stub = screen.getByTestId("budget-section-stub");
    expect(stub).toBeTruthy();
    expect(stub.getAttribute("data-ws")).toBe("ws-42");
  });
});

// ── Workspace edit form (no budget_limit) ──────────────────────────────────────

describe("DetailsTab — workspace edit form does not include budget_limit", () => {
  it("does NOT show a 'Budget limit (USD)' input in the edit form", async () => {
    render(<DetailsTab workspaceId="ws-1" data={makeData()} />);
    await openEdit();
    // Budget limit (USD) was the old inline field label — must be absent now
    expect(screen.queryByPlaceholderText("Leave blank for unlimited")).toBeNull();
    expect(screen.queryByText("Budget limit (USD)")).toBeNull();
  });

  it("PATCH /workspaces/:id body does NOT include budget_limit", async () => {
    render(<DetailsTab workspaceId="ws-1" data={makeData({ name: "My Agent" })} />);
    await openEdit();

    const saveBtn = screen.getAllByRole("button").find((b) => b.textContent === "Save");
    fireEvent.click(saveBtn!);

    await waitFor(() => expect(mockPatch).toHaveBeenCalled());
    const body = mockPatch.mock.calls[0][1] as Record<string, unknown>;
    expect(Object.prototype.hasOwnProperty.call(body, "budget_limit")).toBe(false);
  });

  it("PATCH /workspaces/:id body includes name, role, and tier", async () => {
    render(
      <DetailsTab
        workspaceId="ws-1"
        data={makeData({ name: "Alpha", role: "Writer", tier: 2 })}
      />
    );
    await openEdit();

    const saveBtn = screen.getAllByRole("button").find((b) => b.textContent === "Save");
    fireEvent.click(saveBtn!);

    await waitFor(() => expect(mockPatch).toHaveBeenCalled());
    const body = mockPatch.mock.calls[0][1] as Record<string, unknown>;
    expect(body.name).toBe("Alpha");
    expect(body.role).toBe("Writer");
    expect(body.tier).toBe(2);
  });

  it("Cancel reverts name, role, tier without touching budget state", async () => {
    render(
      <DetailsTab
        workspaceId="ws-1"
        data={makeData({ name: "Original", role: "Dev" })}
      />
    );
    await openEdit();

    // Modify name
    fireEvent.change(
      screen.getAllByRole("textbox").find((i) => (i as HTMLInputElement).value === "Original")!,
      { target: { value: "Modified" } }
    );

    const cancelBtn = screen.getAllByRole("button").find((b) => b.textContent === "Cancel");
    fireEvent.click(cancelBtn!);

    // Should be back in read view — no Save button visible
    expect(screen.queryAllByRole("button").some((b) => b.textContent === "Save")).toBe(false);
    // Workspace info unchanged in read view
    expect(screen.getByText("Original")).toBeTruthy();
  });

  it("updateNodeData is called with name/role/tier but NOT budgetLimit on save", async () => {
    render(
      <DetailsTab
        workspaceId="ws-1"
        data={makeData({ name: "Bot", role: "Analyst", tier: 1 })}
      />
    );
    await openEdit();

    const saveBtn = screen.getAllByRole("button").find((b) => b.textContent === "Save");
    fireEvent.click(saveBtn!);

    await waitFor(() => expect(mockUpdateNodeData).toHaveBeenCalled());
    const updateArgs = mockUpdateNodeData.mock.calls[0][1] as Record<string, unknown>;
    expect(updateArgs.name).toBe("Bot");
    expect(updateArgs.role).toBe("Analyst");
    expect(updateArgs.tier).toBe(1);
    expect(Object.prototype.hasOwnProperty.call(updateArgs, "budgetLimit")).toBe(false);
  });
});

// ── budget-exceeded-badge removed from DetailsTab ────────────────────────────

describe("DetailsTab — no inline budget-exceeded-badge", () => {
  it("does NOT render budget-exceeded-badge even when budgetUsed > budgetLimit (BudgetSection owns that)", () => {
    render(
      <DetailsTab
        workspaceId="ws-1"
        data={makeData({ budgetLimit: 10, budgetUsed: 99 })}
      />
    );
    // The old inline badge is gone — BudgetSection.tsx owns the exceeded state
    expect(screen.queryByTestId("budget-exceeded-badge")).toBeNull();
  });

  it("does NOT render inline Budget limit row in read view", () => {
    render(
      <DetailsTab
        workspaceId="ws-1"
        data={makeData({ budgetLimit: 100 })}
      />
    );
    // "$100.00" and "Unlimited" are rendered by BudgetSection now
    expect(screen.queryByText("$100.00")).toBeNull();
    expect(screen.queryByText("Unlimited")).toBeNull();
  });
});
