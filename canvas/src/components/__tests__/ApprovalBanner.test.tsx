// @vitest-environment jsdom
/**
 * Tests for ApprovalBanner component.
 *
 * Uses vi.hoisted + vi.mock for stable module-level API mocks that survive
 * vi.resetModules() cleanup. BeforeEach uses mockReset + mockResolvedValue
 * so each test gets a clean slate.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, waitFor, act } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { ApprovalBanner } from "../ApprovalBanner";
import { showToast } from "@/components/Toaster";
import { api } from "@/lib/api";

// ─── Module-level mocks ───────────────────────────────────────────────────────
// vi.hoisted captures stable references BEFORE hoisting so they are accessible
// in the test body after vi.mock registers.
const _mockGet = vi.hoisted<typeof api.get>(() => vi.fn<() => Promise<unknown[]>>());
const _mockPost = vi.hoisted<typeof api.post>(() => vi.fn<() => Promise<unknown>>());
const _mockToast = vi.hoisted<typeof showToast>(() => vi.fn());

vi.mock("@/lib/api", () => ({
  api: { get: _mockGet, post: _mockPost },
}));

vi.mock("@/components/Toaster", () => ({
  showToast: _mockToast,
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

// ─── Cleanup ─────────────────────────────────────────────────────────────────

beforeEach(() => {
  _mockGet.mockReset();
  _mockGet.mockResolvedValue([] as unknown[]);
  _mockPost.mockReset();
  _mockPost.mockResolvedValue({} as unknown);
  _mockToast.mockClear();
});

afterEach(() => {
  cleanup();
});

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("ApprovalBanner — empty state", () => {
  it("renders nothing when there are no pending approvals", async () => {
    _mockGet.mockResolvedValueOnce([] as unknown[]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("does not render any approve/deny buttons when list is empty", async () => {
    _mockGet.mockResolvedValueOnce([] as unknown[]);
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
    _mockGet.mockResolvedValueOnce([
      pendingApproval("a1"),
      pendingApproval("a2", "ws-2"),
    ] as unknown[]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    const alerts = screen.getAllByRole("alert");
    expect(alerts).toHaveLength(2);
  });

  it("displays the workspace name and action text", async () => {
    _mockGet.mockResolvedValueOnce([pendingApproval("a1")] as unknown[]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.getByText("Test Workspace needs approval")).toBeTruthy();
    expect(screen.getByText("Run code execution")).toBeTruthy();
  });

  it("displays the reason when present", async () => {
    _mockGet.mockResolvedValueOnce([pendingApproval("a1")] as unknown[]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.getByText(/Requires human approval/i)).toBeTruthy();
  });

  it("omits the reason div when reason is null", async () => {
    const approval = pendingApproval("a1");
    approval.reason = null;
    _mockGet.mockResolvedValueOnce([approval] as unknown[]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.queryByText(/Requires human approval/i)).toBeNull();
  });

  it("renders both Approve and Deny buttons per card", async () => {
    _mockGet.mockResolvedValueOnce([pendingApproval("a1")] as unknown[]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.getByRole("button", { name: /approve/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /deny/i })).toBeTruthy();
  });

  it("has aria-live=assertive on the alert container", async () => {
    _mockGet.mockResolvedValueOnce([pendingApproval("a1")] as unknown[]);
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
    _mockGet.mockResolvedValueOnce([pendingApproval("a1")] as unknown[]);
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
    _mockGet.mockResolvedValueOnce([approval] as unknown[]);
    _mockPost.mockResolvedValueOnce({} as unknown);

    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });

    fireEvent.click(screen.getByRole("button", { name: /approve/i }));

    await waitFor(() => {
      expect(_mockPost).toHaveBeenCalledWith(
        "/workspaces/ws-1/approvals/a1/decide",
        { decision: "approved", decided_by: "human" },
      );
    });
  });

  it("calls POST with decision=denied on Deny click", async () => {
    const approval = pendingApproval("a1", "ws-1");
    _mockGet.mockResolvedValueOnce([approval] as unknown[]);
    _mockPost.mockResolvedValueOnce({} as unknown);

    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });

    fireEvent.click(screen.getByRole("button", { name: /deny/i }));

    await waitFor(() => {
      expect(_mockPost).toHaveBeenCalledWith(
        "/workspaces/ws-1/approvals/a1/decide",
        { decision: "denied", decided_by: "human" },
      );
    });
  });

  it("removes the card from state after a successful decision", async () => {
    const approval = pendingApproval("a1", "ws-1");
    _mockGet.mockResolvedValueOnce([approval] as unknown[]);
    _mockPost.mockResolvedValueOnce({} as unknown);

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
    _mockGet.mockResolvedValueOnce([pendingApproval("a1")] as unknown[]);
    _mockPost.mockResolvedValueOnce({} as unknown);

    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });

    fireEvent.click(screen.getByRole("button", { name: /approve/i }));

    await waitFor(() => {
      expect(_mockToast).toHaveBeenCalledWith("Approved", "success");
    });
  });

  it("shows an info toast on deny", async () => {
    _mockGet.mockResolvedValueOnce([pendingApproval("a1")] as unknown[]);
    _mockPost.mockResolvedValueOnce({} as unknown);

    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });

    fireEvent.click(screen.getByRole("button", { name: /deny/i }));

    await waitFor(() => {
      expect(_mockToast).toHaveBeenCalledWith("Denied", "info");
    });
  });

  it("shows an error toast when POST fails", async () => {
    _mockGet.mockResolvedValueOnce([pendingApproval("a1")] as unknown[]);
    // Use mockImplementation instead of mockRejectedValueOnce so the vi.fn
    // wrapper is preserved — the component's catch block needs the resolved
    // promise wrapper to distinguish a rejected-from-mock vs thrown-from-code.
    _mockPost.mockImplementation(
      () => new Promise((_, reject) => reject(new Error("Network error"))),
    );

    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });

    fireEvent.click(screen.getByRole("button", { name: /approve/i }));

    await waitFor(() => {
      expect(_mockToast).toHaveBeenCalledWith("Failed to submit decision", "error");
    });
  });

  it("keeps the card visible when the POST fails", async () => {
    _mockGet.mockResolvedValueOnce([pendingApproval("a1")] as unknown[]);
    _mockPost.mockImplementation(
      () => new Promise((_, reject) => reject(new Error("Network error"))),
    );

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
    _mockGet.mockResolvedValueOnce([] as unknown[]);
    render(<ApprovalBanner />);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
