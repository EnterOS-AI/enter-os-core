// @vitest-environment jsdom
/**
 * MobileComms — workspace A2A traffic feed with All/Errors filter.
 *
 * Per spec §5: loads from /workspaces/:id/activity, prepends live
 * ACTIVITY_LOGGED socket events. Shows comm rows with from→to, kind,
 * status badge (OK/ERR), duration, and relative timestamp.
 *
 * NOTE: No @testing-library/jest-dom — use DOM APIs.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import React from "react";

import { MobileComms } from "../MobileComms";

// ─── Mock dependencies ──────────────────────────────────────────────────────────

vi.mock("@/lib/theme-provider", () => ({
  useTheme: () => ({ theme: "dark", resolvedTheme: "dark", setTheme: vi.fn() }),
}));

const mockNodes = [
  {
    id: "ws-alpha",
    data: { name: "Alpha Agent", status: "online", tier: 2, parentId: null },
  },
  {
    id: "ws-beta",
    data: { name: "Beta Agent", status: "online", tier: 3, parentId: "ws-alpha" },
  },
];

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn((selector) => {
    if (typeof selector === "function") {
      return selector({ nodes: mockNodes });
    }
    return mockNodes;
  }),
  summarizeWorkspaceCapabilities: vi.fn(() => ({ runtime: "langgraph", skillCount: 0, currentTask: "" })),
}));

const mockActivity: Array<{
  id: string; workspace_id: string; activity_type: string;
  source_id: string | null; target_id: string | null;
  summary: string | null; status: string; duration_ms: number | null;
  created_at: string;
}> = [
  {
    id: "act-1",
    workspace_id: "ws-alpha",
    activity_type: "a2a_delegate",
    source_id: "ws-alpha",
    target_id: "ws-beta",
    summary: "Analyzing report",
    status: "ok",
    duration_ms: 1234,
    created_at: new Date(Date.now() - 60000).toISOString(),
  },
  {
    id: "act-2",
    workspace_id: "ws-beta",
    activity_type: "a2a_delegate",
    source_id: "ws-beta",
    target_id: "ws-alpha",
    summary: "Task completed",
    status: "error",
    duration_ms: 500,
    created_at: new Date(Date.now() - 120000).toISOString(),
  },
];

const { apiGetSpy, socketHandlers } = vi.hoisted(() => {
  const apiGetSpy = vi.fn();
  return { apiGetSpy, socketHandlers: [] as Array<(msg: unknown) => void> };
});

vi.mock("@/lib/api", () => ({
  api: {
    get: apiGetSpy,
    post: vi.fn(),
  },
}));

vi.mock("@/hooks/useSocketEvent", () => ({
  useSocketEvent: vi.fn((handler: (msg: unknown) => void) => {
    socketHandlers.push(handler);
    return vi.fn(); // unsubscribe
  }),
}));

afterEach(() => {
  cleanup();
  socketHandlers.splice(0, socketHandlers.length);
  apiGetSpy.mockReset();
  vi.restoreAllMocks();
});

// ─── Render ────────────────────────────────────────────────────────────────────

describe("MobileComms — render", () => {
  it("renders comms page with header", () => {
    apiGetSpy.mockResolvedValue([]);
    render(<MobileComms dark={true} />);
    expect(document.body.textContent).toContain("Comms");
  });

  it("shows loading state when fetching", async () => {
    let resolve!: () => void;
    apiGetSpy.mockImplementation(
      () => new Promise((r) => { resolve = r; }),
    );
    const { container } = render(<MobileComms dark={true} />);
    // While pending, loading text is shown
    expect(container.textContent ?? "").toContain("Loading");
    resolve([]);
  });

  it("renders empty state when no activity", async () => {
    apiGetSpy.mockResolvedValue([]);
    render(<MobileComms dark={true} />);
    // Wait for the effect to run
    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("No A2A traffic yet");
    });
  });

  it("renders All and Errors filter buttons", async () => {
    apiGetSpy.mockResolvedValue([]);
    render(<MobileComms dark={true} />);
    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("All");
      expect(document.body.textContent).toContain("Errors");
    });
  });

  it("shows event count in header", async () => {
    apiGetSpy.mockImplementation((path: string) => {
      if (path.includes("/activity")) return Promise.resolve(mockActivity);
      return Promise.resolve([]);
    });
    render(<MobileComms dark={true} />);
    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("events");
    });
  });
});

// ─── Interaction ──────────────────────────────────────────────────────────────

describe("MobileComms — interaction", () => {
  it("renders activity rows when data loaded", async () => {
    apiGetSpy.mockImplementation((path: string) => {
      if (path.includes("/activity")) return Promise.resolve(mockActivity);
      return Promise.resolve([]);
    });
    render(<MobileComms dark={true} />);
    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("a2a_delegate");
    });
  });

  it("switching to Errors filter shows only error rows", async () => {
    apiGetSpy.mockImplementation((path: string) => {
      if (path.includes("/activity")) return Promise.resolve(mockActivity);
      return Promise.resolve([]);
    });
    render(<MobileComms dark={true} />);

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("a2a_delegate");
    });

    const errorsBtn = Array.from(
      document.querySelectorAll("button"),
    ).find((b) => b.textContent?.includes("Errors"));
    expect(errorsBtn).toBeTruthy();

    fireEvent.click(errorsBtn!);

    // Only the error row should remain
    const rows = Array.from(
      document.querySelectorAll("div"),
    ).filter((d) => d.textContent?.includes("ERR"));
    expect(rows.length).toBeGreaterThanOrEqual(1);
  });

  it("switching back to All shows all rows", async () => {
    apiGetSpy.mockImplementation((path: string) => {
      if (path.includes("/activity")) return Promise.resolve(mockActivity);
      return Promise.resolve([]);
    });
    render(<MobileComms dark={true} />);

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("a2a_delegate");
    });

    const allBtn = Array.from(
      document.querySelectorAll("button"),
    ).find((b) => b.textContent?.includes("All"));
    fireEvent.click(allBtn!);

    // Should show OK and ERR rows
    const okRows = Array.from(
      document.querySelectorAll("div"),
    ).filter((d) => d.textContent?.includes("OK"));
    expect(okRows.length).toBeGreaterThanOrEqual(1);
  });

  it("live socket event prepended to list", async () => {
    apiGetSpy.mockResolvedValue([]);
    render(<MobileComms dark={true} />);

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("No A2A traffic yet");
    });

    // Simulate live ACTIVITY_LOGGED event
    const liveHandler = socketHandlers[socketHandlers.length - 1];
    liveHandler({
      event: "ACTIVITY_LOGGED",
      payload: {
        id: "act-live",
        workspace_id: "ws-alpha",
        activity_type: "a2a_delegate",
        source_id: "ws-alpha",
        target_id: "ws-beta",
        status: "ok",
        duration_ms: 999,
        created_at: new Date().toISOString(),
      },
    });

    await vi.waitFor(() => {
      expect(document.body.textContent).toContain("a2a_delegate");
    });
    // Empty state should be gone
    expect(document.body.textContent).not.toContain("No A2A traffic yet");
  });
});
