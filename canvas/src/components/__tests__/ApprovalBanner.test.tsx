// @vitest-environment jsdom
/**
 * Tests for ApprovalBanner component.
 *
 * Covers: renders nothing when no approvals, polls /approvals/pending,
 * shows approval cards, approve/deny decisions, toast notifications.
 *
 * Uses vi.hoisted + vi.mock (file-level) for @/lib/api. vi.resetModules()
 * in every afterEach undoes the mock so other test files that import the
 * real api module (e.g. socket.url.test.ts) are unaffected.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { ApprovalBanner } from "../ApprovalBanner";
import { showToast } from "@/components/Toaster";

// ─── Hoisted mock refs ─────────────────────────────────────────────────────────
// vi.hoisted runs in the same hoisting phase as vi.mock factories, so these
// refs are stable across all tests and available inside the mock factory.
const { mockApiGet, mockApiPost } = vi.hoisted(() => ({
  mockApiGet: vi.fn<(args: unknown[]) => Promise<unknown>>(),
  mockApiPost: vi.fn<(args: unknown[]) => Promise<unknown>>(),
}));

// ─── Helpers ──────────────────────────────────────────────────────────────────

const pendingApproval = (id = "a1", workspaceId = "ws-1"): {
  id: string;
  workspace_id: string;
  workspace_name: string;
  action: string;
  reason: string | null;
  status: string;
  created_at: string;
} => ({
  id,
  workspace_id: workspaceId,
  workspace_name: "Test Workspace",
  action: "Run code execution",
  reason: "Requires human approval due to workspace policy",
  status: "pending",
  created_at: "2026-05-10T10:00:00Z",
});

// ─── Static mocks (file-level — no other test needs the real modules) ─────────

vi.mock("@/components/Toaster", () => ({
  showToast: vi.fn(),
}));

// vi.resetModules() in afterEach undoes this mock so other files that import
// the real api module are unaffected.
vi.mock("@/lib/api", () => ({
  api: {
    get: mockApiGet,
    post: mockApiPost,
  },
}));

// ─── Tests ─────────────────────────────────────────────────────────────────────

describe("ApprovalBanner — empty state", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    mockApiGet.mockReset().mockResolvedValue([]);
    mockApiPost.mockReset().mockResolvedValue({});
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.resetModules();
  });

  it("renders nothing when there are no pending approvals", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(mockApiGet).toHaveBeenCalled();
  });

  it("does not render any approve/deny buttons when list is empty", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    expect(screen.queryByRole("button", { name: /approve/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /deny/i })).toBeNull();
  });
});

describe("ApprovalBanner — renders approval cards", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    mockApiGet.mockReset().mockResolvedValue([
      pendingApproval("a1"),
      pendingApproval("a2", "ws-2"),
    ]);
    mockApiPost.mockReset().mockResolvedValue({});
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.resetModules();
  });

  it("renders an alert card for each pending approval", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    expect(screen.getAllByRole("alert")).toHaveLength(2);
  });

  it("displays the workspace name and action text", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    expect(screen.getAllByText(/test workspace needs approval/i)).toHaveLength(2);
  });

  it("displays the reason when present", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    expect(screen.getAllByText(/requires human approval/i)).toHaveLength(2);
  });

  it("omits the reason div when reason is null", async () => {
    mockApiGet.mockReset().mockResolvedValue([{
      ...pendingApproval("a1"),
      reason: null,
    }]);
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    expect(screen.queryByText(/requires human approval/i)).toBeNull();
  });

  it("renders both Approve and Deny buttons per card", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    const approveBtns = screen.getAllByRole("button", { name: /Approve/i });
    const denyBtns = screen.getAllByRole("button", { name: /Deny/i });
    expect(approveBtns.length).toBeGreaterThanOrEqual(2);
    expect(denyBtns.length).toBeGreaterThanOrEqual(2);
  });

  it("has aria-live=assertive on the alert container", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    expect(screen.getAllByRole("alert")[0].getAttribute("aria-live")).toBe("assertive");
  });
});

describe("ApprovalBanner — decisions", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    mockApiGet.mockReset().mockResolvedValue([pendingApproval("a1")]);
    mockApiPost.mockReset().mockResolvedValue({});
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.resetModules();
  });

  it("calls POST /workspaces/:id/approvals/:id/decide on Approve click", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    fireEvent.click(screen.getAllByRole("button", { name: /approve/i })[0]);
    await act(async () => { /* flush */ });
    expect(mockApiPost).toHaveBeenCalledWith(
      "/workspaces/ws-1/approvals/a1/decide",
      expect.objectContaining({ decision: "approved" })
    );
  });

  it("calls POST with decision=denied on Deny click", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    fireEvent.click(screen.getAllByRole("button", { name: /deny/i })[0]);
    await act(async () => { /* flush */ });
    expect(mockApiPost).toHaveBeenCalledWith(
      "/workspaces/ws-1/approvals/a1/decide",
      expect.objectContaining({ decision: "denied" })
    );
  });

  it("removes the card from state after a successful decision", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    expect(screen.getAllByRole("alert")).toHaveLength(1);
    fireEvent.click(screen.getAllByRole("button", { name: /approve/i })[0]);
    await act(async () => { /* flush */ });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("shows a success toast on approve", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    fireEvent.click(screen.getAllByRole("button", { name: /approve/i })[0]);
    await act(async () => { /* flush */ });
    expect(vi.mocked(showToast)).toHaveBeenCalledWith("Approved", "success");
  });

  it("shows an info toast on deny", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    fireEvent.click(screen.getAllByRole("button", { name: /deny/i })[0]);
    await act(async () => { /* flush */ });
    expect(vi.mocked(showToast)).toHaveBeenCalledWith("Denied", "info");
  });

  it("shows an error toast when POST fails", async () => {
    // mockImplementation preserves the vi.fn() wrapper (unlike mockReset() which
    // strips it and causes the real fetch() to fire — the root cause of the
    // original flakiness in this file).
    mockApiPost.mockImplementation(() => Promise.reject(new Error("Network error")));
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    fireEvent.click(screen.getAllByRole("button", { name: /approve/i })[0]);
    await act(async () => { /* flush */ });
    expect(vi.mocked(showToast)).toHaveBeenCalledWith(
      "Failed to submit decision",
      "error"
    );
  });

  it("keeps the card visible when the POST fails", async () => {
    // Same mockImplementation pattern — preserves the wrapper so the component's
    // catch block runs instead of the real fetch().
    mockApiPost.mockImplementation(() => Promise.reject(new Error("Network error")));
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    fireEvent.click(screen.getAllByRole("button", { name: /approve/i })[0]);
    await act(async () => { /* flush */ });
    expect(screen.getAllByRole("alert")).toHaveLength(1);
  });
});

describe("ApprovalBanner — handles empty list from server", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    mockApiGet.mockReset().mockResolvedValue([]);
    mockApiPost.mockReset().mockResolvedValue({});
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.resetModules();
  });

  it("shows nothing when the API returns an empty array on first poll", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
