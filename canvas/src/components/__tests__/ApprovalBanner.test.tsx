// @vitest-environment jsdom
/**
 * Tests for ApprovalBanner component.
 *
 * Covers: renders nothing when no approvals, polls /approvals/pending,
 * shows approval cards, approve/deny decisions, toast notifications.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, waitFor, act } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { ApprovalBanner } from "../ApprovalBanner";
import { showToast } from "@/components/Toaster";
import { api } from "@/lib/api";

vi.mock("@/components/Toaster", () => ({
  showToast: vi.fn(),
}));

afterEach(cleanup);

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

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("ApprovalBanner — empty state", () => {
  it("renders nothing when there are no pending approvals", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("does not render any approve/deny buttons when list is empty", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.queryByRole("button", { name: /approve/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /deny/i })).toBeNull();
  });
});

describe("ApprovalBanner — renders approval cards", () => {
  it("renders an alert card for each pending approval", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([
      pendingApproval("a1"),
      pendingApproval("a2", "ws-2"),
    ]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    const alerts = screen.getAllByRole("alert");
    expect(alerts).toHaveLength(2);
  });

  it("displays the workspace name and action text", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([pendingApproval("a1")]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.getByText("Test Workspace needs approval")).toBeTruthy();
    expect(screen.getByText("Run code execution")).toBeTruthy();
  });

  it("displays the reason when present", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([pendingApproval("a1")]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.getByText(/Requires human approval/i)).toBeTruthy();
  });

  it("omits the reason div when reason is null", async () => {
    const approval = pendingApproval("a1");
    approval.reason = null;
    vi.spyOn(api, "get").mockResolvedValueOnce([approval]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.queryByText(/Requires human approval/i)).toBeNull();
  });

  it("renders both Approve and Deny buttons per card", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([pendingApproval("a1")]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.getByRole("button", { name: /approve/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /deny/i })).toBeTruthy();
  });

  it("has aria-live=assertive on the alert container", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([pendingApproval("a1")]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    const alert = screen.getByRole("alert");
    expect(alert.getAttribute("aria-live")).toBe("assertive");
  });
});

describe("ApprovalBanner — polling", () => {
  let clearIntervalSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    clearIntervalSpy = vi.spyOn(global, "clearInterval").mockImplementation(() => {});
  });

  afterEach(() => {
    clearIntervalSpy.mockRestore();
  });

  it("clears the polling interval on unmount", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([pendingApproval("a1")]);
    const { unmount } = render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    unmount();
    expect(clearIntervalSpy).toHaveBeenCalled();
  });
});

describe("ApprovalBanner — decisions", () => {
  it("calls POST /workspaces/:id/approvals/:id/decide on Approve click", async () => {
    const approval = pendingApproval("a1", "ws-1");
    vi.spyOn(api, "get").mockResolvedValueOnce([approval]);
    const postSpy = vi.spyOn(api, "post").mockResolvedValueOnce(undefined);

    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });

    fireEvent.click(screen.getByRole("button", { name: /approve/i }));

    await waitFor(() => {
      expect(postSpy).toHaveBeenCalledWith(
        "/workspaces/ws-1/approvals/a1/decide",
        { decision: "approved", decided_by: "human" }
      );
    });
  });

  it("calls POST with decision=denied on Deny click", async () => {
    const approval = pendingApproval("a1", "ws-1");
    vi.spyOn(api, "get").mockResolvedValueOnce([approval]);
    const postSpy = vi.spyOn(api, "post").mockResolvedValueOnce(undefined);

    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });

    fireEvent.click(screen.getByRole("button", { name: /deny/i }));

    await waitFor(() => {
      expect(postSpy).toHaveBeenCalledWith(
        "/workspaces/ws-1/approvals/a1/decide",
        { decision: "denied", decided_by: "human" }
      );
    });
  });

  it("removes the card from state after a successful decision", async () => {
    const approval = pendingApproval("a1", "ws-1");
    vi.spyOn(api, "get").mockResolvedValueOnce([approval]);
    vi.spyOn(api, "post").mockResolvedValueOnce(undefined);

    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });

    // One alert initially
    expect(screen.getAllByRole("alert")).toHaveLength(1);

    fireEvent.click(screen.getByRole("button", { name: /approve/i }));

    await waitFor(() => {
      expect(screen.queryByRole("alert")).toBeNull();
    });
  });

  it("shows a success toast on approve", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([pendingApproval("a1")]);
    vi.spyOn(api, "post").mockResolvedValueOnce(undefined);

    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });

    fireEvent.click(screen.getByRole("button", { name: /approve/i }));

    await waitFor(() => {
      expect(showToast).toHaveBeenCalledWith("Approved", "success");
    });
  });

  it("shows an info toast on deny", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([pendingApproval("a1")]);
    vi.spyOn(api, "post").mockResolvedValueOnce(undefined);

    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });

    fireEvent.click(screen.getByRole("button", { name: /deny/i }));

    await waitFor(() => {
      expect(showToast).toHaveBeenCalledWith("Denied", "info");
    });
  });

  it("shows an error toast when POST fails", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([pendingApproval("a1")]);
    vi.spyOn(api, "post").mockRejectedValueOnce(new Error("Network error"));

    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });

    fireEvent.click(screen.getByRole("button", { name: /approve/i }));

    await waitFor(() => {
      expect(showToast).toHaveBeenCalledWith("Failed to submit decision", "error");
    });
  });

  it("keeps the card visible when the POST fails", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([pendingApproval("a1")]);
    vi.spyOn(api, "post").mockRejectedValueOnce(new Error("Network error"));

    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });

    fireEvent.click(screen.getByRole("button", { name: /approve/i }));

    await waitFor(() => {
      // Card still shown because the request failed
      expect(screen.getByRole("alert")).toBeTruthy();
    });
  });
});

describe("ApprovalBanner — handles empty list from server", () => {
  it("shows nothing when the API returns an empty array on first poll", async () => {
    vi.spyOn(api, "get").mockResolvedValueOnce([]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
