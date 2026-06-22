// @vitest-environment jsdom
/**
 * Tests for RequestsInbox — the unified Tasks/Approvals inbox (RFC P3 canvas).
 *
 * Covers: rendering a task + an approval item from /requests/pending, the
 * Done/Approve/Reject actions (asserting the right POST /requests/:id/respond
 * body), and the More-Info thread (GET /requests/:id load + POST
 * /requests/:id/messages on send).
 *
 * Mock style mirrors ApprovalBanner.test.tsx: vi.hoisted refs + file-level
 * vi.mock for @/lib/api, @/lib/auth, @/components/Toaster, and the socket bus.
 * vi.resetModules() in afterEach undoes the mocks for other files.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RequestsInbox, type RequestRow } from "../RequestsInbox";
import { showToast } from "@/components/Toaster";

const { mockApiGet, mockApiPost, mockFetchSession, mockSubscribe } = vi.hoisted(() => ({
  mockApiGet: vi.fn<(args: unknown[]) => Promise<unknown>>(),
  mockApiPost: vi.fn<(args: unknown[]) => Promise<unknown>>(),
  mockFetchSession: vi.fn<(args: unknown[]) => Promise<unknown>>(),
  mockSubscribe: vi.fn(() => () => {}),
}));

vi.mock("@/components/Toaster", () => ({ showToast: vi.fn() }));
vi.mock("@/lib/api", () => ({ api: { get: mockApiGet, post: mockApiPost } }));
vi.mock("@/lib/auth", () => ({ fetchSession: mockFetchSession }));
vi.mock("@/store/socket-events", () => ({ subscribeSocketEvents: mockSubscribe }));
// CSS modules → empty object; icons → trivial stubs so render is DOM-only.
vi.mock("../Concierge.module.css", () => ({ default: {} }));

const taskRow = (id = "t1"): RequestRow => ({
  id,
  kind: "task",
  requester_type: "agent",
  requester_id: "ws-9",
  org_id: "org-1",
  recipient_type: "user",
  recipient_id: "u-1",
  title: "Review the Q3 deck",
  detail: "Needs your eyes before send.",
  status: "pending",
  responder_type: null,
  responder_id: null,
  priority: null,
  created_at: "2026-06-10T10:00:00Z",
  updated_at: "2026-06-10T10:00:00Z",
  responded_at: null,
  workspace_name: "Researcher",
});

const approvalRow = (id = "a1"): RequestRow => ({
  ...taskRow(id),
  kind: "approval",
  title: "Delete production volume",
  detail: "Destructive",
  workspace_name: "Ops Agent",
});

beforeEach(() => {
  mockApiGet.mockReset().mockResolvedValue([]);
  mockApiPost.mockReset().mockResolvedValue({});
  mockFetchSession.mockReset().mockResolvedValue({ user_id: "user-42", org_id: "org-1", email: "x@y.z" });
  mockSubscribe.mockReset().mockReturnValue(() => {});
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.resetModules();
});

describe("RequestsInbox — task tab", () => {
  it("fetches /requests/pending?kind=task and renders a task item", async () => {
    mockApiGet.mockResolvedValue([taskRow("t1")]);
    await act(async () => { render(<RequestsInbox kind="task" />); });
    expect(mockApiGet).toHaveBeenCalledWith("/requests/pending?kind=task");
    expect(screen.getByText("Review the Q3 deck")).toBeTruthy();
    expect(screen.getByText(/Researcher/)).toBeTruthy();
    expect(screen.getByTestId("request-status").textContent).toContain("pending");
  });

  it("POSTs respond done with responder_type:user on Done click", async () => {
    mockApiGet.mockResolvedValue([taskRow("t1")]);
    await act(async () => { render(<RequestsInbox kind="task" />); });
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: /done/i })); });
    expect(mockApiPost).toHaveBeenCalledWith(
      "/requests/t1/respond",
      expect.objectContaining({ action: "done", responder_type: "user", responder_id: "user-42" }),
    );
    expect(screen.queryByText("Review the Q3 deck")).toBeNull(); // optimistically removed
  });

  it("POSTs respond rejected on Reject click", async () => {
    mockApiGet.mockResolvedValue([taskRow("t1")]);
    await act(async () => { render(<RequestsInbox kind="task" />); });
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: /reject/i })); });
    expect(mockApiPost).toHaveBeenCalledWith(
      "/requests/t1/respond",
      expect.objectContaining({ action: "rejected" }),
    );
  });

  it("renders the empty-state copy when no tasks", async () => {
    mockApiGet.mockResolvedValue([]);
    await act(async () => { render(<RequestsInbox kind="task" />); });
    expect(screen.getByText(/Nothing needs you right now/i)).toBeTruthy();
  });
});

describe("RequestsInbox — approval tab", () => {
  it("fetches /requests/pending?kind=approval and renders an approval item", async () => {
    mockApiGet.mockResolvedValue([approvalRow("a1")]);
    await act(async () => { render(<RequestsInbox kind="approval" />); });
    expect(mockApiGet).toHaveBeenCalledWith("/requests/pending?kind=approval");
    expect(screen.getByText("Delete production volume")).toBeTruthy();
  });

  it("POSTs respond approved on Approve click", async () => {
    mockApiGet.mockResolvedValue([approvalRow("a1")]);
    await act(async () => { render(<RequestsInbox kind="approval" />); });
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: /approve/i })); });
    expect(mockApiPost).toHaveBeenCalledWith(
      "/requests/a1/respond",
      expect.objectContaining({ action: "approved", responder_type: "user" }),
    );
  });

  it("POSTs respond rejected on Reject click (approval)", async () => {
    mockApiGet.mockResolvedValue([approvalRow("a1")]);
    await act(async () => { render(<RequestsInbox kind="approval" />); });
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: /reject/i })); });
    expect(mockApiPost).toHaveBeenCalledWith(
      "/requests/a1/respond",
      expect.objectContaining({ action: "rejected" }),
    );
  });
});

describe("RequestsInbox — More Info thread", () => {
  it("opens the thread, loads GET /requests/:id, and posts a message", async () => {
    // First GET (pending list) → one task. Then GET /requests/t1 → thread.
    mockApiGet.mockImplementation((path: string) => {
      if (path === "/requests/pending?kind=task") return Promise.resolve([taskRow("t1")]);
      if (path === "/requests/t1") {
        return Promise.resolve({
          request: taskRow("t1"),
          messages: [
            { id: "m1", request_id: "t1", author_type: "agent", author_id: "ws-9", body: "Why rejected?", created_at: "2026-06-10T10:01:00Z" },
          ],
        });
      }
      return Promise.resolve([]);
    });

    await act(async () => { render(<RequestsInbox kind="task" />); });
    // Open More Info.
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: /more info/i })); });
    expect(mockApiGet).toHaveBeenCalledWith("/requests/t1");
    expect(screen.getByTestId("more-info-thread")).toBeTruthy();
    expect(screen.getByText("Why rejected?")).toBeTruthy();

    // Type + send a reply.
    const input = screen.getByTestId("more-info-input") as HTMLInputElement;
    await act(async () => { fireEvent.change(input, { target: { value: "Here is more context" } }); });
    await act(async () => { fireEvent.click(screen.getByTestId("more-info-send")); });
    expect(mockApiPost).toHaveBeenCalledWith(
      "/requests/t1/messages",
      expect.objectContaining({ body: "Here is more context", author_type: "user" }),
    );
  });

  it("Send is disabled when the draft is empty", async () => {
    mockApiGet.mockImplementation((path: string) => {
      if (path === "/requests/pending?kind=task") return Promise.resolve([taskRow("t1")]);
      if (path === "/requests/t1") return Promise.resolve({ request: taskRow("t1"), messages: [] });
      return Promise.resolve([]);
    });
    await act(async () => { render(<RequestsInbox kind="task" />); });
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: /more info/i })); });
    const send = screen.getByTestId("more-info-send") as HTMLButtonElement;
    expect(send.disabled).toBe(true);
  });
});

describe("RequestsInbox — card body markdown + truncation", () => {
  it("renders markdown formatting instead of raw literals", async () => {
    mockApiGet.mockResolvedValue([
      { ...taskRow("t1"), detail: "**bold** and `code` and [link](https://example.com)" },
    ]);
    const { container } = await act(async () => render(<RequestsInbox kind="task" />));

    expect(container.querySelector("strong")?.textContent).toBe("bold");
    expect(container.querySelector("code")?.textContent).toBe("code");
    const link = container.querySelector("a");
    expect(link).toBeTruthy();
    expect(link?.getAttribute("href")).toBe("https://example.com");
    expect(link?.getAttribute("target")).toBe("_blank");
    expect(screen.queryByText("\*\*bold\*\*")).toBeNull();
  });

  it("renders lists and fenced code blocks", async () => {
    mockApiGet.mockResolvedValue([
      {
        ...taskRow("t1"),
        detail: "- one\n- two\n\n```\nhello\n```",
      },
    ]);
    const { container } = await act(async () => render(<RequestsInbox kind="task" />));

    expect(container.querySelectorAll("li").length).toBe(2);
    expect(container.querySelector("pre")?.textContent).toContain("hello");
  });

  it("shows a Show more / Show less toggle for long bodies", async () => {
    const longDetail = "Line one.\nLine two.\nLine three.\nLine four.\nLine five.\nLine six.";
    mockApiGet.mockResolvedValue([{ ...taskRow("t1"), detail: longDetail }]);
    await act(async () => { render(<RequestsInbox kind="task" />); });

    const toggle = screen.getByRole("button", { name: /show more/i });
    expect(toggle).toBeTruthy();
    expect(toggle.getAttribute("aria-expanded")).toBe("false");

    await act(async () => { fireEvent.click(toggle); });
    expect(screen.getByRole("button", { name: /show less/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /show less/i }).getAttribute("aria-expanded")).toBe("true");
  });

  it("does not show a toggle for short bodies", async () => {
    mockApiGet.mockResolvedValue([taskRow("t1")]);
    await act(async () => { render(<RequestsInbox kind="task" />); });
    expect(screen.queryByRole("button", { name: /show more/i })).toBeNull();
  });
});

describe("RequestsInbox — tab-switch wrong-action race (core#2766)", () => {
  it("clears stale rows immediately on kind switch before the new fetch resolves", async () => {
    mockApiGet.mockResolvedValue([approvalRow("a1")]);
    const { rerender } = render(<RequestsInbox kind="approval" />);
    await waitFor(() =>
      expect(screen.getByText("Delete production volume")).toBeTruthy(),
    );
    expect(screen.getByRole("button", { name: /approve/i })).toBeTruthy();

    // Switch to the Tasks tab with a fetch that never resolves in this test.
    mockApiGet.mockImplementation(() => new Promise(() => {}));
    rerender(<RequestsInbox kind="task" />);

    // The stale approval row must be gone instantly; otherwise the user could
    // see approval cards under the Tasks tab and the wrong primary action.
    expect(screen.queryByText("Delete production volume")).toBeNull();
    expect(screen.queryByTestId("request-item")).toBeNull();
    expect(screen.queryByRole("button", { name: /approve/i })).toBeNull();
  });

  it("ignores a stale approval fetch that resolves after switching to Tasks", async () => {
    let resolveApproval: (value: RequestRow[]) => void = () => {};
    mockApiGet.mockImplementation((path: string) => {
      if (path === "/requests/pending?kind=approval") {
        return new Promise<RequestRow[]>((res) => {
          resolveApproval = res;
        });
      }
      return Promise.resolve([]);
    });

    const { rerender } = render(<RequestsInbox kind="approval" />);
    // The approval fetch is in flight but has not resolved yet.
    expect(screen.queryByText("Delete production volume")).toBeNull();

    // Switch to Tasks before the approval response lands.
    rerender(<RequestsInbox kind="task" />);

    // The old approval response finally arrives; it must be ignored.
    await act(async () => {
      resolveApproval([approvalRow("a1")]);
    });
    expect(screen.queryByText("Delete production volume")).toBeNull();
    expect(screen.queryByTestId("request-item")).toBeNull();
    expect(screen.queryByRole("button", { name: /approve/i })).toBeNull();
  });

  it("derives action buttons from row.kind, not the selected tab", async () => {
    // Defensive: even if a mismatched row somehow reaches the list, its action
    // must be driven by row.kind so an approval can never be actioned as "done".
    mockApiGet.mockResolvedValue([approvalRow("a1")]);
    await act(async () => { render(<RequestsInbox kind="task" />); });

    expect(screen.queryByRole("button", { name: /done/i })).toBeNull();
    expect(screen.getByRole("button", { name: /approve/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /reject/i })).toBeTruthy();
  });
});

describe("RequestsInbox — live refresh + toasts", () => {
  it("subscribes to the socket bus on mount", async () => {
    await act(async () => { render(<RequestsInbox kind="task" />); });
    expect(mockSubscribe).toHaveBeenCalled();
  });

  it("shows an error toast when respond POST fails and keeps the item", async () => {
    mockApiGet.mockResolvedValue([taskRow("t1")]);
    mockApiPost.mockImplementation(() => Promise.reject(new Error("boom")));
    await act(async () => { render(<RequestsInbox kind="task" />); });
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: /done/i })); });
    expect(vi.mocked(showToast)).toHaveBeenCalledWith("Failed to record response", "error");
    expect(screen.getByText("Review the Q3 deck")).toBeTruthy();
  });
});
