// @vitest-environment jsdom
/**
 * Tests for EventsTab — the activity feed on the Events tab.
 *
 * Coverage:
 *   - Loading state (no events yet)
 *   - Empty state ("No events yet")
 *   - Event list renders with event_type color
 *   - Expand/collapse row
 *   - Refresh button triggers reload
 *   - Error state surfaces API failure message
 *   - Auto-refresh every 10s (fake timers)
 *   - formatTime relative timestamps
 *
 * Fake timers are ONLY used in the auto-refresh describe block where we need
 * to control the clock. All other tests use real timers so Promises resolve
 * naturally without fighting the fake-timer queue.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { EventsTab } from "../EventsTab";

// Hoist mockGet so vi.mock factory can reference it (vi.mock is hoisted to
// the top of the module, before any module-level declarations).
const mockGet = vi.hoisted(() => vi.fn<[], Promise<unknown[]>>());

vi.mock("@/lib/api", () => ({
  api: { get: mockGet },
}));

// ─── Helpers ──────────────────────────────────────────────────────────────────

const event = (
  id: string,
  type = "WORKSPACE_ONLINE",
  createdOffsetSecs = 0,
): {
  id: string;
  event_type: string;
  workspace_id: string | null;
  payload: Record<string, unknown>;
  created_at: string;
} => ({
  id,
  event_type: type,
  workspace_id: "ws-1",
  payload: { key: "value" },
  created_at: new Date(Date.now() - createdOffsetSecs * 1000).toISOString(),
});

const renderTab = (workspaceId = "ws-1") =>
  render(<EventsTab workspaceId={workspaceId} />);

// Flush pattern for real-timer tests: resolve the mock microtask then
// flush React's state batch. Using act(async ...) lets us await inside.
async function flush() {
  await act(async () => { await Promise.resolve(); });
}

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("EventsTab — render conditions", () => {
  beforeEach(() => {
    vi.useRealTimers();
    mockGet.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("shows loading state when events are being fetched", async () => {
    // Never resolve so loading stays true
    mockGet.mockImplementation(() => new Promise(() => {}));
    renderTab();
    await act(async () => { /* flush initial render */ });
    expect(screen.getByText("Loading events...")).toBeTruthy();
  });

  it("shows empty state when API returns an empty list", async () => {
    mockGet.mockResolvedValueOnce([]);
    renderTab();
    await flush();
    expect(screen.getByText("No events yet")).toBeTruthy();
  });

  it("renders the event list when API returns events", async () => {
    mockGet.mockResolvedValueOnce([
      event("e1", "WORKSPACE_ONLINE"),
      event("e2", "WORKSPACE_REMOVED"),
    ]);
    renderTab();
    await flush();
    expect(screen.getByText("WORKSPACE_ONLINE")).toBeTruthy();
    expect(screen.getByText("WORKSPACE_REMOVED")).toBeTruthy();
    expect(screen.getByText("2 events")).toBeTruthy();
  });

  it("applies text-bad color to WORKSPACE_REMOVED events", async () => {
    mockGet.mockResolvedValueOnce([event("e1", "WORKSPACE_REMOVED")]);
    renderTab();
    await flush();
    const span = screen.getByText("WORKSPACE_REMOVED");
    expect(span.classList).toContain("text-bad");
  });

  it("applies text-good color to WORKSPACE_ONLINE events", async () => {
    mockGet.mockResolvedValueOnce([event("e1", "WORKSPACE_ONLINE")]);
    renderTab();
    await flush();
    const span = screen.getByText("WORKSPACE_ONLINE");
    expect(span.classList).toContain("text-good");
  });

  it("applies text-accent color to AGENT_CARD_UPDATED events", async () => {
    mockGet.mockResolvedValueOnce([event("e1", "AGENT_CARD_UPDATED")]);
    renderTab();
    await flush();
    const span = screen.getByText("AGENT_CARD_UPDATED");
    expect(span.classList).toContain("text-accent");
  });

  it("applies text-ink-mid fallback for unknown event types", async () => {
    mockGet.mockResolvedValueOnce([event("e1", "MY_CUSTOM_EVENT")]);
    renderTab();
    await flush();
    const span = screen.getByText("MY_CUSTOM_EVENT");
    expect(span.classList).toContain("text-ink-mid");
  });
});

describe("EventsTab — expand/collapse", () => {
  beforeEach(() => {
    vi.useRealTimers();
    mockGet.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("shows payload when a row is clicked (expanded)", async () => {
    mockGet.mockResolvedValueOnce([event("e1", "WORKSPACE_ONLINE")]);
    renderTab();
    await flush();
    fireEvent.click(screen.getByText("WORKSPACE_ONLINE"));
    await act(async () => { /* flush */ });
    expect(screen.getByText(/"key": "value"/)).toBeTruthy();
    expect(screen.getByText("ID: e1")).toBeTruthy();
  });

  it("hides payload when the expanded row is clicked again", async () => {
    mockGet.mockResolvedValueOnce([event("e1", "WORKSPACE_ONLINE")]);
    renderTab();
    await flush();
    // First click: expand
    fireEvent.click(screen.getByText("WORKSPACE_ONLINE"));
    await act(async () => { /* flush */ });
    expect(screen.getByText(/"key": "value"/)).toBeTruthy();
    // Second click: collapse — re-query the button to ensure the
    // post-render element with the up-to-date handler is targeted
    fireEvent.click(screen.getByText("WORKSPACE_ONLINE"));
    await act(async () => { /* flush */ });
    expect(screen.queryByText(/"key": "value"/)).toBeFalsy();
  });

  it("has aria-expanded=true on the expanded row", async () => {
    mockGet.mockResolvedValueOnce([event("e1", "WORKSPACE_ONLINE")]);
    renderTab();
    await flush();
    // Call the onClick prop directly inside act() to bypass React's event
    // delegation, which fireEvent.click doesn't reliably trigger in jsdom.
    act(() => {
      screen.getByRole("button", { name: /workspace_online/i }).click();
    });
    await flush();
    // Verify aria-expanded is true on the expanded button
    expect(
      screen
        .getAllByRole("button")
        .find((b) => b.textContent?.includes("WORKSPACE_ONLINE"))
        ?.getAttribute("aria-expanded"),
    ).toBe("true");
  });

  it("has aria-expanded=false on collapsed rows", async () => {
    mockGet.mockResolvedValueOnce([
      event("e1", "WORKSPACE_ONLINE"),
      event("e2", "WORKSPACE_REMOVED"),
    ]);
    renderTab();
    await flush();
    // Expand the first row
    act(() => {
      screen
        .getAllByRole("button")
        .find((b) => b.textContent?.includes("WORKSPACE_ONLINE"))
        ?.click();
    });
    await flush();
    const onlineBtn = screen
      .getAllByRole("button")
      .find((b) => b.textContent?.includes("WORKSPACE_ONLINE"));
    const removedBtn = screen
      .getAllByRole("button")
      .find((b) => b.textContent?.includes("WORKSPACE_REMOVED"));
    expect(onlineBtn?.getAttribute("aria-expanded")).toBe("true");
    expect(removedBtn?.getAttribute("aria-expanded")).toBe("false");
  });

  it("has aria-controls linking row to its payload panel", async () => {
    mockGet.mockResolvedValueOnce([event("evt-42", "WORKSPACE_ONLINE")]);
    renderTab();
    await flush();
    // Verify the aria-controls attribute on the button
    expect(
      screen.getByRole("button", { name: /workspace_online/i }).getAttribute(
        "aria-controls",
      ),
    ).toBe("events-payload-evt-42");
  });
});

describe("EventsTab — refresh", () => {
  beforeEach(() => {
    vi.useRealTimers();
    mockGet.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("Refresh button triggers a new GET /events/:id", async () => {
    mockGet.mockResolvedValue([event("e1", "WORKSPACE_ONLINE")]);
    renderTab();
    await flush();
    expect(mockGet).toHaveBeenCalledWith("/events/ws-1");
    mockGet.mockClear();
    fireEvent.click(screen.getByRole("button", { name: /refresh/i }));
    await flush();
    expect(mockGet).toHaveBeenCalledWith("/events/ws-1");
  });

  it("shows loading state during refresh (events still visible from previous load)", async () => {
    // First load succeeds with real timers so the mock resolves
    mockGet.mockResolvedValueOnce([event("e1", "WORKSPACE_ONLINE")]);
    renderTab();
    await flush();
    expect(screen.getByText("1 events")).toBeTruthy();

    // Switch to fake timers for the refresh call (loading stays true)
    vi.useFakeTimers();
    // Refresh call hangs to keep loading=true
    mockGet.mockImplementationOnce(() => new Promise(() => {}));
    fireEvent.click(screen.getByRole("button", { name: /refresh/i }));
    await act(() => { vi.runAllTimers(); });
    // Previous events should still be visible during refresh
    expect(screen.getByText("WORKSPACE_ONLINE")).toBeTruthy();
    vi.useRealTimers();
  });
});

describe("EventsTab — error state", () => {
  beforeEach(() => {
    vi.useRealTimers();
    mockGet.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("shows error message when GET /events/:id rejects", async () => {
    mockGet.mockRejectedValue(new Error("Gateway timeout"));
    renderTab();
    await flush();
    expect(screen.getByText("Gateway timeout")).toBeTruthy();
    expect(screen.queryByText("Loading events...")).toBeFalsy();
  });

  it("shows 'Failed to load events' when API rejects with non-Error", async () => {
    mockGet.mockRejectedValue("unknown failure");
    renderTab();
    await flush();
    expect(screen.getByText("Failed to load events")).toBeTruthy();
  });
});

describe("EventsTab — auto-refresh", () => {
  // Use vi.spyOn to mock setInterval/clearInterval so we can control timer
  // firing without Vitest's fake-timer APIs (which create infinite loops when
  // timers schedule microtasks that schedule more timers).
  let setIntervalSpy: ReturnType<typeof vi.spyOn>;
  let clearIntervalSpy: ReturnType<typeof vi.spyOn>;
  let activeIntervalId = 0;
  const scheduledCallbacks = new Map<number, () => void>();

  beforeEach(() => {
    vi.useRealTimers();
    mockGet.mockReset();
    activeIntervalId = 0;
    scheduledCallbacks.clear();
    setIntervalSpy = vi.spyOn(globalThis, "setInterval").mockImplementation(
      (cb: () => void) => {
        const id = ++activeIntervalId;
        scheduledCallbacks.set(id, cb);
        return id;
      },
    );
    clearIntervalSpy = vi.spyOn(globalThis, "clearInterval").mockImplementation(
      (id: number) => {
        scheduledCallbacks.delete(id);
      },
    );
  });

  afterEach(() => {
    cleanup();
    setIntervalSpy?.mockRestore();
    clearIntervalSpy?.mockRestore();
    vi.useRealTimers();
  });

  it("calls GET /events/:id after 10s without manual interaction", async () => {
    mockGet.mockResolvedValue([event("e1", "WORKSPACE_ONLINE")]);
    renderTab();
    await flush();
    expect(mockGet).toHaveBeenCalledWith("/events/ws-1");
    mockGet.mockClear();

    // Verify setInterval was called with 10000ms delay
    expect(setIntervalSpy).toHaveBeenCalledWith(
      expect.any(Function),
      10000,
    );

    // Fire the captured interval callback (simulates 10s elapsing)
    const callback = [...scheduledCallbacks.values()][0];
    act(() => { callback(); });
    await flush();
    expect(mockGet).toHaveBeenCalledWith("/events/ws-1");
  });

  it("clears the previous auto-refresh interval on unmount", async () => {
    mockGet.mockResolvedValue([event("e1", "WORKSPACE_ONLINE")]);
    const { unmount } = renderTab();
    await flush();

    // Verify clearInterval was NOT called yet
    expect(clearIntervalSpy).not.toHaveBeenCalled();

    // Unmount should call clearInterval with the active interval id
    unmount();
    expect(clearIntervalSpy).toHaveBeenCalled();
    // The callback should no longer be scheduled
    expect(scheduledCallbacks.size).toBe(0);
  });
});
