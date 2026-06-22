// @vitest-environment jsdom
//
// MobileInbox (core#2697 Phase 1) — Tasks/Approvals on mobile. Verifies it
// loads pending requests from GET /requests/pending?kind=… and that a decision
// POSTs /requests/{id}/respond with the right action + optimistically drops the
// row — the decision-on-the-go flow that was entirely missing on mobile.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, waitFor } from "@testing-library/react";

afterEach(cleanup);

vi.mock("@/lib/api");
vi.mock("@/lib/auth", () => ({ fetchSession: vi.fn().mockResolvedValue({ user_id: "u-test" }) }));
vi.mock("@/hooks/useSocketEvent", () => ({ useSocketEvent: vi.fn() }));

import { api } from "@/lib/api";
import { useSocketEvent } from "@/hooks/useSocketEvent";
import type { RequestRow } from "@/components/concierge/RequestsInbox";
import { MobileInbox } from "../MobileInbox";

const approval = {
  id: "req-1", kind: "approval", requester_type: "workspace", requester_id: "ws-9",
  org_id: null, recipient_type: "user", recipient_id: "", title: "Delete prod secret?",
  detail: "Agent wants to delete TEST_KEY", status: "pending", responder_type: null,
  responder_id: null, priority: null, created_at: "", updated_at: "", responded_at: null,
  workspace_name: "Dev Engineer",
};

beforeEach(() => {
  vi.mocked(api.get).mockResolvedValue([approval]);
  vi.mocked(api.post).mockResolvedValue({});
});

function deferred<T = unknown>() {
  let resolve: (value: T) => void = () => {};
  let reject: (reason?: unknown) => void = () => {};
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe("MobileInbox", () => {
  it("loads pending approvals from /requests/pending?kind=approval", async () => {
    const { getByText } = render(<MobileInbox dark={false} />);
    await waitFor(() => getByText("Delete prod secret?"));
    expect(api.get).toHaveBeenCalledWith("/requests/pending?kind=approval");
  });

  it("Approve POSTs /requests/{id}/respond with action=approved and drops the row", async () => {
    const { getByText, queryByText, container } = render(<MobileInbox dark={false} />);
    await waitFor(() => getByText("Delete prod secret?"));
    await act(async () => {
      fireEvent.click(getByText("Approve"));
    });
    expect(api.post).toHaveBeenCalledWith(
      "/requests/req-1/respond",
      expect.objectContaining({ action: "approved", responder_type: "user" }),
    );
    await waitFor(() => expect(queryByText("Delete prod secret?")).toBeNull());
    expect(container.querySelectorAll('[data-testid="inbox-row"]').length).toBe(0);
  });

  it("does not action a stale approval row as a task during tab switch (core#2766)", async () => {
    // Simulate a delayed task fetch after switching tabs. The old approval row
    // is still rendered while the new fetch is in flight; its primary action
    // must remain "approved", not flip to "done" because the active tab changed.
    const approvalFetch = deferred<RequestRow[]>();
    const taskFetch = deferred<RequestRow[]>();
    vi.mocked(api.get).mockImplementation((url: string | undefined) => {
      if (typeof url === "string" && url.includes("kind=approval")) {
        return approvalFetch.promise as Promise<unknown>;
      }
      return taskFetch.promise as Promise<unknown>;
    });

    const { getByRole, getByText, queryByText } = render(<MobileInbox dark={false} />);
    await act(async () => {
      approvalFetch.resolve([approval]);
    });
    await waitFor(() => expect(getByText("Delete prod secret?")).toBeTruthy());

    // Switch to Tasks before the task fetch resolves.
    fireEvent.click(getByRole("tab", { name: "Tasks" }));

    // Switch to Tasks. The new load clears stale rows immediately.
    fireEvent.click(getByRole("tab", { name: "Tasks" }));
    expect(queryByText("Delete prod secret?")).toBeNull();

    // The old approval fetch now resolves. With the sequence guard it
    // must be ignored: the Task tab should NOT show approval rows.
    await act(async () => {
      approvalFetch.resolve([approval]);
    });
    await waitFor(() => {
      expect(queryByText("Delete prod secret?")).toBeNull();
    });

    // Once the task fetch resolves, the Tasks tab shows its own content.
    await act(async () => {
      taskFetch.resolve([]);
    });
    await waitFor(() => {
      expect(getByText(/No pending tasks/i)).toBeTruthy();
    });
  });

  // ─────────────────────────────────────────────────────────────────────
  // Audit F1 (HIGH): MobileInbox.tsx:155-162 — backend fetch FAILURE must
  // render a distinct error/retry state, NOT the silent "No pending
  // approvals" empty copy. Critical for destructive-approvals review:
  // a clean-looking inbox during a backend outage would let destructive
  // actions go unreviewed.
  // ─────────────────────────────────────────────────────────────────────
  it("F1: backend fetch failure renders error/retry, not silent 'No pending approvals'", async () => {
    vi.mocked(api.get).mockRejectedValue(new Error("network down"));
    const { getByTestId, queryByText } = render(<MobileInbox dark={false} />);

    // The error state + retry button must render.
    await waitFor(() => {
      expect(getByTestId("inbox-load-error")).toBeTruthy();
    });
    expect(getByTestId("inbox-retry")).toBeTruthy();

    // The silent "No pending approvals" copy MUST NOT render during a
    // backend fetch failure — that's the bug F1 fixes.
    expect(queryByText(/No pending approvals/i)).toBeNull();

    // The retry button must call load() — second GET also rejects, but
    // we assert the click is wired (no throw, error state persists).
    await act(async () => {
      fireEvent.click(getByTestId("inbox-retry"));
    });
    await waitFor(() => {
      expect(getByTestId("inbox-load-error")).toBeTruthy();
    });
  });

  // ─────────────────────────────────────────────────────────────────────
  // Audit F2 (HIGH): MobileInbox.tsx:81-101 — `respond` prev-closure must
  // NOT wipe a row that arrived via WS during the in-flight POST on
  // failure. The server is the source of truth, so the fix re-loads from
  // the server rather than restoring a stale `items` snapshot.
  // ─────────────────────────────────────────────────────────────────────
  it("F2: POST failure re-loads from server (preserves a row that arrived via WS mid-POST)", async () => {
    const rowA: RequestRow = { ...approval, id: "req-A", title: "Row A: delete prod secret?" };
    const rowB: RequestRow = { ...approval, id: "req-B", title: "Row B: rotate prod key?" };

    // First GET (initial load): server returns [A]. The user clicks
    // Approve on A; the POST fails. The post-failure re-load (the
    // second GET) simulates the server returning [A, B] — A is still
    // pending (POST failed), and B arrived via WS during the in-flight
    // POST. The OLD snapshot-restore would wipe B; the new load()
    // re-fetch must preserve it.
    let getCallCount = 0;
    vi.mocked(api.get).mockImplementation(() => {
      getCallCount++;
      if (getCallCount === 1) return Promise.resolve([rowA]);
      return Promise.resolve([rowA, rowB]);
    });
    vi.mocked(api.post).mockRejectedValue(new Error("server down"));

    const { getByText } = render(<MobileInbox dark={false} />);
    await waitFor(() => expect(getByText("Row A: delete prod secret?")).toBeTruthy());

    // User clicks Approve on A → optimistic removal + POST in flight.
    await act(async () => {
      fireEvent.click(getByText("Approve"));
    });

    // After the post-failure re-load, BOTH A (server still has it) AND
    // B (WS-arrived during the in-flight POST) must be visible. B
    // surviving is the load-bearing assertion for F2.
    await waitFor(() => {
      expect(getByText("Row B: rotate prod key?")).toBeTruthy();
    });
    expect(getByText("Row A: delete prod secret?")).toBeTruthy();
  });

  // ─────────────────────────────────────────────────────────────────────
  // Audit F3 (HIGH): MobileInbox.tsx:77-79 — WS REQUEST_RESPONDED fired
  // during the optimistic-removal / in-flight-POST window must NOT cause
  // the just-acted row to briefly reappear (flicker). The fix filters
  // justActedRef out of the load() response.
  // ─────────────────────────────────────────────────────────────────────
  it("F3: WS REQUEST_RESPONDED during optimistic removal does not flicker the row", async () => {
    // Capture the useSocketEvent callback so we can fire a synthetic
    // WS event from the test.
    let socketCb: ((msg: { event?: string }) => void) | null = null;
    vi.mocked(useSocketEvent).mockImplementation((cb) => {
      socketCb = cb as typeof socketCb;
    });

    const rowA: RequestRow = { ...approval, id: "req-A", title: "Row A: delete prod secret?" };
    vi.mocked(api.get).mockResolvedValue([rowA]);
    // Hold the POST open so the WS race has a window to fire in.
    const postDeferred = deferred<unknown>();
    vi.mocked(api.post).mockReturnValue(postDeferred.promise as Promise<unknown>);

    const { getByText, queryByText } = render(<MobileInbox dark={false} />);
    await waitFor(() => expect(getByText("Row A: delete prod secret?")).toBeTruthy());

    // User clicks Approve → optimistic removal; POST is in flight.
    await act(async () => {
      fireEvent.click(getByText("Approve"));
    });
    await waitFor(() => expect(queryByText("Row A: delete prod secret?")).toBeNull());

    // Race: a WS REQUEST_RESPONDED fires before the POST lands. The
    // load() triggered by it must re-fetch (the server still has A as
    // pending because the POST hasn't returned). The OLD code would
    // re-surface A → flicker. The NEW code filters justActedRef so A
    // stays gone.
    vi.mocked(api.get).mockResolvedValueOnce([rowA]);
    await act(async () => {
      socketCb?.({ event: "REQUEST_RESPONDED" });
    });
    // Give the re-fetch + setItems microtask queue a tick to settle.
    await act(async () => {
      await Promise.resolve();
    });

    // Critical: A must NOT have reappeared. Flicker prevented.
    expect(queryByText("Row A: delete prod secret?")).toBeNull();

    // Now resolve the POST (success) — A is permanently responded.
    await act(async () => {
      postDeferred.resolve({});
    });
    expect(queryByText("Row A: delete prod secret?")).toBeNull();
  });
});
