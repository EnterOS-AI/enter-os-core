// @vitest-environment jsdom
/**
 * Tests for ApprovalBanner component.
 *
 * Covers: renders nothing when no approvals, polls /approvals/pending,
 * shows approval cards, approve/deny decisions, toast notifications.
 *
 * All blocks use vi.useFakeTimers() consistently in beforeEach/afterEach to
 * avoid polluting the fake-timer state for subsequent test files. The
 * vi.spyOn mocks are reset per-spy via mockReset() in afterEach so each
 * test gets a clean mock state without touching the module-level api mock.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { ApprovalBanner } from "../ApprovalBanner";
import { showToast } from "@/components/Toaster";
import { api } from "@/lib/api";

vi.mock("@/components/Toaster", () => ({
  showToast: vi.fn(),
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

// Shared spy references so individual tests can reset or reject the POST mock
// without needing to call spyOn again (which would create a duplicate spy).
let mockGet: ReturnType<typeof vi.spyOn>;
let mockPost: ReturnType<typeof vi.spyOn>;

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("ApprovalBanner — empty state", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.spyOn(api, "get").mockResolvedValueOnce([]);
  });

  afterEach(() => {
    cleanup();
    vi.useFakeTimers();
  });

  it("renders nothing when there are no pending approvals", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    expect(screen.queryByRole("alert")).toBeNull();
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
    mockGet = vi.spyOn(api, "get").mockResolvedValueOnce([
      pendingApproval("a1"),
      pendingApproval("a2", "ws-2"),
    ]);
  });

  afterEach(() => {
    cleanup();
    mockGet?.mockReset();
    vi.useFakeTimers();
  });

  it("renders an alert card for each pending approval", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    const alerts = screen.getAllByRole("alert");
    expect(alerts).toHaveLength(2);
  });

  it("displays the workspace name and action text", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    const nameEls = screen.getAllByText(/test workspace needs approval/i);
    expect(nameEls).toHaveLength(2);
  });

  it("displays the reason when present", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    const reasons = screen.getAllByText(/requires human approval/i);
    expect(reasons).toHaveLength(2);
  });

  it("omits the reason div when reason is null", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([{
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
    // 2 cards, each card has 1 Approve + 1 Deny button → 2 of each minimum
    expect(approveBtns.length).toBeGreaterThanOrEqual(2);
    expect(denyBtns.length).toBeGreaterThanOrEqual(2);
  });

  it("has aria-live=assertive on the alert container", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    const alert = screen.getAllByRole("alert")[0];
    expect(alert.getAttribute("aria-live")).toBe("assertive");
  });
});

describe("ApprovalBanner — decisions", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    mockGet = vi.spyOn(api, "get").mockResolvedValueOnce([pendingApproval("a1")]);
    mockPost = vi.spyOn(api, "post").mockResolvedValue({});
  });

  afterEach(() => {
    cleanup();
    mockGet?.mockReset();
    mockPost?.mockReset();
    vi.useFakeTimers();
  });

  it("calls POST /workspaces/:id/approvals/:id/decide on Approve click", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    fireEvent.click(screen.getAllByRole("button", { name: /approve/i })[0]);
    await act(async () => { /* flush */ });
    expect(vi.mocked(api.post)).toHaveBeenCalledWith(
      "/workspaces/ws-1/approvals/a1/decide",
      expect.objectContaining({ decision: "approved" })
    );
  });

  it("calls POST with decision=denied on Deny click", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    fireEvent.click(screen.getAllByRole("button", { name: /deny/i })[0]);
    await act(async () => { /* flush */ });
    expect(vi.mocked(api.post)).toHaveBeenCalledWith(
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
    mockPost.mockReset().mockRejectedValue(new Error("Network error"));
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
    // Reset the post mock before rejecting so the beforeEach's resolved value
    // is gone and we get a clean rejection instead of a resolved→rejected queue.
    mockPost.mockReset().mockRejectedValue(new Error("Network error"));
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
    vi.spyOn(api, "get").mockResolvedValueOnce([]);
  });

  afterEach(() => {
    cleanup();
    vi.useFakeTimers();
  });

  it("shows nothing when the API returns an empty array on first poll", async () => {
    render(<ApprovalBanner />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
