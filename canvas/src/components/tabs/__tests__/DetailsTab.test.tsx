// @vitest-environment jsdom
/**
 * Tests for DetailsTab — workspace detail panel with editable fields,
 * delete/restart workflows, peers list, error display, and section
 * composition.
 *
 * Covers:
 *   - View mode: all rows rendered (name, role, tier, status, URL, etc.)
 *   - Edit mode: name/role/tier fields become editable
 *   - Save workflow: calls PATCH and updates store
 *   - Cancel: reverts fields to original data
 *   - Delete: two-step confirm (confirm button shows alertdialog)
 *   - Delete confirm: calls DELETE and removes node from store
 *   - Restart button: calls POST /restart for failed/degraded/offline
 *   - Error section: shown for failed/degraded with lastSampleError
 *   - Skills section: rendered when agentCard has skills
 *   - Peers section: loads and displays peer list
 *   - Peers section: empty state when offline
 *   - ConsoleModal: opens/closes via button click
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { DetailsTab } from "../DetailsTab";
import type { WorkspaceNodeData } from "@/store/canvas";

const mockApi = vi.hoisted(() => ({
  get: vi.fn(),
  patch: vi.fn(),
  del: vi.fn(),
  post: vi.fn(),
}));

const mockUpdateNodeData = vi.hoisted(() => vi.fn());
const mockRemoveSubtree = vi.hoisted(() => vi.fn());
const mockSelectNode = vi.hoisted(() => vi.fn());

const mockUseCanvasStore = vi.hoisted(() => {
  const fn = (selector: (s: {
    updateNodeData: typeof mockUpdateNodeData;
    removeSubtree: typeof mockRemoveSubtree;
    selectNode: typeof mockSelectNode;
  }) => unknown) =>
    selector({
      updateNodeData: mockUpdateNodeData,
      removeSubtree: mockRemoveSubtree,
      selectNode: mockSelectNode,
    });
  return fn;
});

vi.mock("@/store/canvas", () => ({
  useCanvasStore: mockUseCanvasStore,
}));

vi.mock("@/lib/api", () => ({
  api: mockApi,
}));

vi.mock("@/components/BudgetSection", () => ({
  BudgetSection: () => <div data-testid="budget-section">BudgetSection</div>,
}));

vi.mock("@/components/WorkspaceUsage", () => ({
  WorkspaceUsage: () => <div data-testid="workspace-usage">WorkspaceUsage</div>,
}));

vi.mock("@/components/ConsoleModal", () => ({
  ConsoleModal: ({ open, onClose }: { open: boolean; onClose: () => void; workspaceId: string; workspaceName: string }) =>
    open ? (
      <div role="dialog" data-testid="console-modal">
        <button onClick={onClose}>Close Console</button>
      </div>
    ) : null,
}));

// ─── Fixtures ───────────────────────────────────────────────────────────────

const baseData: WorkspaceNodeData = {
  name: "Test Workspace",
  status: "online",
  tier: 2,
  url: "https://test.molecules.ai",
  parentId: null,
  activeTasks: 0,
  agentCard: null,
} as WorkspaceNodeData;

function data(overrides: Partial<WorkspaceNodeData> = {}): WorkspaceNodeData {
  return { ...baseData, ...overrides } as WorkspaceNodeData;
}

// ─── Helpers ───────────────────────────────────────────────────────────────

async function flush() {
  await act(async () => { await Promise.resolve(); });
}

// ─── Tests ────────────────────────────────────────────────────────────────

describe("DetailsTab — view mode", () => {
  beforeEach(() => {
    mockApi.get.mockReset();
    mockUpdateNodeData.mockReset();
    mockRemoveSubtree.mockReset();
    mockSelectNode.mockReset();
    mockApi.get.mockResolvedValue([]);
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("renders name, role, tier, status, URL, parent rows", () => {
    render(<DetailsTab workspaceId="ws-1" data={data({ role: "SEO Specialist", url: "https://example.com" })} />);
    expect(screen.getByText("Test Workspace")).toBeTruthy();
    expect(screen.getByText("SEO Specialist")).toBeTruthy();
    expect(screen.getByText("T2")).toBeTruthy();
    expect(screen.getByText("online")).toBeTruthy();
    expect(screen.getByText("https://example.com")).toBeTruthy();
    expect(screen.getByText("root")).toBeTruthy();
  });

  it("renders Edit button", () => {
    render(<DetailsTab workspaceId="ws-1" data={data()} />);
    expect(screen.getByRole("button", { name: /edit/i })).toBeTruthy();
  });

  it("renders BudgetSection and WorkspaceUsage", () => {
    render(<DetailsTab workspaceId="ws-1" data={data()} />);
    expect(screen.getByTestId("budget-section")).toBeTruthy();
    expect(screen.getByTestId("workspace-usage")).toBeTruthy();
  });

  it("renders Restart button for failed status", () => {
    render(<DetailsTab workspaceId="ws-1" data={data({ status: "failed" })} />);
    expect(screen.getByRole("button", { name: /retry/i })).toBeTruthy();
  });

  it("renders Restart button for offline status", () => {
    render(<DetailsTab workspaceId="ws-1" data={data({ status: "offline" })} />);
    expect(screen.getByRole("button", { name: /restart/i })).toBeTruthy();
  });

  it("renders Restart button for degraded status", () => {
    render(<DetailsTab workspaceId="ws-1" data={data({ status: "degraded" })} />);
    expect(screen.getByRole("button", { name: /restart/i })).toBeTruthy();
  });

  it("does not render Restart for online status", () => {
    render(<DetailsTab workspaceId="ws-1" data={data()} />);
    expect(screen.queryByRole("button", { name: /restart|retry/i })).toBeNull();
  });

  it("renders error section for failed status with lastSampleError", () => {
    render(
      <DetailsTab
        workspaceId="ws-1"
        data={data({ status: "failed", lastSampleError: "ModuleNotFoundError: No module named 'requests'" })}
      />,
    );
    expect(screen.getByTestId("details-error-log")).toBeTruthy();
    expect(screen.getByText(/ModuleNotFoundError/)).toBeTruthy();
  });

  it("renders error rate for degraded status", () => {
    render(<DetailsTab workspaceId="ws-1" data={data({ status: "degraded", lastErrorRate: 0.15 })} />);
    expect(screen.getByText(/15%/)).toBeTruthy();
  });

  it("renders Delete Workspace button in Danger Zone", () => {
    render(<DetailsTab workspaceId="ws-1" data={data()} />);
    expect(screen.getByRole("button", { name: /delete workspace/i })).toBeTruthy();
  });
});

describe("DetailsTab — edit mode", () => {
  beforeEach(() => {
    mockApi.patch.mockReset();
    mockUpdateNodeData.mockReset();
    mockApi.get.mockResolvedValue([]);
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("clicking Edit shows form fields", () => {
    render(<DetailsTab workspaceId="ws-1" data={data({ role: "Agent" })} />);
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    expect(screen.getByLabelText(/name/i)).toBeTruthy();
    expect(screen.getByLabelText(/role/i)).toBeTruthy();
    expect(screen.getByLabelText(/tier/i)).toBeTruthy();
  });

  it("Edit form pre-fills current values", () => {
    render(<DetailsTab workspaceId="ws-1" data={data({ name: "My WS", role: "Coder" })} />);
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    expect((screen.getByLabelText(/name/i) as HTMLInputElement).value).toBe("My WS");
    expect((screen.getByLabelText(/role/i) as HTMLInputElement).value).toBe("Coder");
  });

  it("Save calls PATCH and exits edit mode", async () => {
    mockApi.patch.mockResolvedValue({});
    render(<DetailsTab workspaceId="ws-1" data={data({ name: "WS" })} />);
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    await flush();
    const nameInput = screen.getByLabelText(/name/i) as HTMLInputElement;
    fireEvent.change(nameInput, { target: { value: "Renamed WS" } });
    await flush();
    // Use scoped search: BudgetSection also has a Save button
    const saveBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent === "Save" && !b.getAttribute("data-testid"),
    ) as HTMLButtonElement;
    fireEvent.click(saveBtn);
    await flush();
    expect(mockApi.patch).toHaveBeenCalledWith(
      "/workspaces/ws-1",
      expect.objectContaining({ name: "Renamed WS" }),
    );
    expect(mockUpdateNodeData).toHaveBeenCalledWith("ws-1", expect.objectContaining({ name: "Renamed WS" }));
    // Edit fields should no longer be visible
    expect(screen.queryByLabelText(/name/i)).toBeNull();
  });

  it("Cancel reverts to view mode without saving", async () => {
    mockApi.patch.mockResolvedValue({});
    render(<DetailsTab workspaceId="ws-1" data={data({ name: "Original" })} />);
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    await flush();
    const nameInput = screen.getByLabelText(/name/i) as HTMLInputElement;
    fireEvent.change(nameInput, { target: { value: "Changed" } });
    await flush();
    const cancelBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent === "Cancel" && !b.getAttribute("data-testid"),
    ) as HTMLButtonElement;
    fireEvent.click(cancelBtn);
    await flush();
    expect(mockApi.patch).not.toHaveBeenCalled();
    expect(screen.getByText("Original")).toBeTruthy();
    expect(screen.queryByLabelText(/name/i)).toBeNull();
  });

  it("Save shows error banner on failure", async () => {
    mockApi.patch.mockRejectedValue(new Error("Server error"));
    render(<DetailsTab workspaceId="ws-1" data={data()} />);
    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    await flush();
    const saveBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent === "Save" && !b.getAttribute("data-testid"),
    ) as HTMLButtonElement;
    fireEvent.click(saveBtn);
    await flush();
    expect(screen.getByText(/server error/i)).toBeTruthy();
  });
});

describe("DetailsTab — delete workflow", () => {
  beforeEach(() => {
    mockApi.del.mockReset();
    mockRemoveSubtree.mockReset();
    mockSelectNode.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("clicking Delete shows confirm dialog", async () => {
    render(<DetailsTab workspaceId="ws-1" data={data()} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /delete workspace/i }));
    await flush();
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    expect(screen.getByText(/confirm deletion/i)).toBeTruthy();
  });

  it("confirming delete calls DELETE and removes node from store", async () => {
    mockApi.del.mockResolvedValue(undefined);
    render(<DetailsTab workspaceId="ws-1" data={data()} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /delete workspace/i }));
    await flush();
    // Radix ConfirmDialog uses dispatchEvent with bubbling click
    const confirmBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent === "Confirm Delete",
    ) as HTMLButtonElement;
    fireEvent(confirmBtn, new MouseEvent("click", { bubbles: true }));
    await flush();
    expect(mockApi.del).toHaveBeenCalledWith("/workspaces/ws-1?confirm=true", {
      headers: { "X-Confirm-Name": "Test Workspace" },
    });
    expect(mockRemoveSubtree).toHaveBeenCalledWith("ws-1");
    expect(mockSelectNode).toHaveBeenCalledWith(null);
  });

  // internal#734: checking "also erase saved data" adds &erase_data=true so the
  // server prunes the data volume. Default (unchecked) must NOT send it.
  it("checking erase-saved-data sends erase_data=true on delete", async () => {
    mockApi.del.mockResolvedValue(undefined);
    render(<DetailsTab workspaceId="ws-1" data={data()} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /delete workspace/i }));
    await flush();
    fireEvent.click(screen.getByRole("checkbox", { name: /erase saved data/i }));
    const confirmBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent === "Confirm Delete",
    ) as HTMLButtonElement;
    fireEvent(confirmBtn, new MouseEvent("click", { bubbles: true }));
    await flush();
    expect(mockApi.del).toHaveBeenCalledWith("/workspaces/ws-1?confirm=true&erase_data=true", {
      headers: { "X-Confirm-Name": "Test Workspace" },
    });
  });

  it("cancelling delete returns to view mode", async () => {
    mockApi.del.mockResolvedValue(undefined);
    render(<DetailsTab workspaceId="ws-1" data={data()} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /delete workspace/i }));
    await flush();
    const cancelBtn = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent === "Cancel",
    ) as HTMLButtonElement;
    fireEvent(cancelBtn, new MouseEvent("click", { bubbles: true }));
    await flush();
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(screen.getByRole("button", { name: /delete workspace/i })).toBeTruthy();
  });
});

describe("DetailsTab — restart workflow", () => {
  beforeEach(() => {
    mockApi.post.mockReset();
    mockUpdateNodeData.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("Restart button calls POST /restart and sets status to provisioning", async () => {
    mockApi.post.mockResolvedValue(undefined);
    render(<DetailsTab workspaceId="ws-1" data={data({ status: "failed" })} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /retry/i }));
    await flush();
    expect(mockApi.post).toHaveBeenCalledWith("/workspaces/ws-1/restart", {});
    expect(mockUpdateNodeData).toHaveBeenCalledWith("ws-1", { status: "provisioning" });
  });

  it("Restart shows error on failure", async () => {
    mockApi.post.mockRejectedValue(new Error("Restart failed"));
    render(<DetailsTab workspaceId="ws-1" data={data({ status: "offline" })} />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /restart/i }));
    await flush();
    expect(screen.getByText(/restart failed/i)).toBeTruthy();
  });
});

describe("DetailsTab — peers section", () => {
  beforeEach(() => {
    mockApi.get.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("loads peers from API", async () => {
    mockApi.get.mockResolvedValue([
      { id: "p1", name: "Alice Agent", role: "seo", status: "online", tier: 2 },
      { id: "p2", name: "Bob Agent", role: null, status: "offline", tier: 3 },
    ]);
    render(<DetailsTab workspaceId="ws-1" data={data()} />);
    await flush();
    expect(screen.getByText("Alice Agent")).toBeTruthy();
    expect(screen.getByText("Bob Agent")).toBeTruthy();
  });

  it("shows 'No reachable peers' when list is empty", async () => {
    mockApi.get.mockResolvedValue([]);
    render(<DetailsTab workspaceId="ws-1" data={data()} />);
    await flush();
    expect(screen.getByText("No reachable peers")).toBeTruthy();
  });

  it("shows offline message when workspace is not online", async () => {
    mockApi.get.mockResolvedValue([]);
    render(<DetailsTab workspaceId="ws-1" data={data({ status: "provisioning" })} />);
    await flush();
    expect(screen.getByText(/only discoverable while the workspace is online/i)).toBeTruthy();
  });

  it("clicking peer name selects that node", async () => {
    mockApi.get.mockResolvedValue([{ id: "p1", name: "Alice Agent", role: null, status: "online", tier: 2 }]);
    render(<DetailsTab workspaceId="ws-1" data={data()} />);
    await flush();
    fireEvent.click(screen.getByText("Alice Agent"));
    await flush();
    expect(mockSelectNode).toHaveBeenCalledWith("p1");
  });
});

describe("DetailsTab — skills section", () => {
  beforeEach(() => {
    mockApi.get.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("renders skills from agentCard", () => {
    render(
      <DetailsTab
        workspaceId="ws-1"
        data={data({ agentCard: { name: "Test Agent", skills: [
          { id: "web-search", description: "Search the web" },
          { id: "code-interpreter" },
        ]} as unknown as WorkspaceNodeData["agentCard"] })}
      />,
    );
    expect(screen.getByText("web-search")).toBeTruthy();
    expect(screen.getByText("Search the web")).toBeTruthy();
    expect(screen.getByText("code-interpreter")).toBeTruthy();
  });

  it("does not render Skills section when agentCard is null", () => {
    render(<DetailsTab workspaceId="ws-1" data={data()} />);
    expect(screen.queryByText("Skills")).toBeNull();
  });
});

describe("DetailsTab — ConsoleModal", () => {
  beforeEach(() => {
    mockApi.get.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("View console output button opens ConsoleModal", async () => {
    render(
      <DetailsTab
        workspaceId="ws-1"
        data={data({ status: "failed", lastSampleError: "Traceback..." })}
      />,
    );
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /view console output/i }));
    await flush();
    expect(screen.getByTestId("console-modal")).toBeTruthy();
  });

  it("Close button closes ConsoleModal", async () => {
    render(
      <DetailsTab
        workspaceId="ws-1"
        data={data({ status: "failed", lastSampleError: "Traceback..." })}
      />,
    );
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /view console output/i }));
    await flush();
    expect(screen.getByTestId("console-modal")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /close console/i }));
    await flush();
    expect(screen.queryByTestId("console-modal")).toBeNull();
  });
});
