// @vitest-environment jsdom
/**
 * Tests for ActivityTab (issue #1037)
 *
 * Covers:
 *  - Filter bar renders all 6 filter options with aria-pressed states
 *  - Filter click triggers API reload with correct query param
 *  - Auto-refresh toggle (5s polling) renders correctly as Live/Paused
 *  - Loading spinner shows while fetching
 *  - Error banner renders on API failure
 *  - Empty state renders when no activities
 *  - ActivityRow: collapsed/expanded states, A2A flow with workspace name resolution,
 *    error styling, duration_ms, status icons
 *  - Refresh button reloads data
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor, act } from "@testing-library/react";

import type { ActivityEntry } from "@/types/activity";

// Hoist mock functions so vi.mock factory can reference them
const { mockGet } = vi.hoisted(() => ({
  mockGet: vi.fn(),
}));

vi.mock("@/lib/api", () => ({
  api: { get: mockGet, post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: (selector: (s: { nodes: unknown[] }) => unknown) =>
    selector({ nodes: [] }),
}));

vi.mock("@/hooks/useWorkspaceName", () => ({
  useWorkspaceName: () => () => "Test WS",
}));

import {
  emitSocketEvent,
  _resetSocketEventListenersForTests,
} from "@/store/socket-events";
import { ActivityTab } from "../tabs/ActivityTab";

// ── Fixtures ──────────────────────────────────────────────────────────────────

function makeEntry(overrides: Partial<ActivityEntry> = {}): ActivityEntry {
  return {
    id: "entry-1",
    workspace_id: "ws-1",
    activity_type: "agent_log",
    source_id: null,
    target_id: null,
    method: null,
    summary: null,
    request_body: null,
    response_body: null,
    duration_ms: null,
    status: "ok",
    error_detail: null,
    created_at: new Date(Date.now() - 30_000).toISOString(),
    ...overrides,
  };
}

function makeA2AEntry(
  sourceId: string,
  targetId: string,
  summary: string,
  status: string = "ok"
): ActivityEntry {
  return {
    id: "a2a-entry-1",
    workspace_id: "ws-1",
    activity_type: "a2a_send",
    source_id: sourceId,
    target_id: targetId,
    method: "A2A.delegate",
    summary,
    request_body: null,
    response_body: null,
    duration_ms: 1234,
    status,
    error_detail: null,
    created_at: new Date(Date.now() - 60_000).toISOString(),
  };
}

// ── Helper: click a button via fireEvent wrapped in act ───────────────────────
function clickButton(name: string | RegExp) {
  act(() => {
    fireEvent.click(screen.getByRole("button", { name }));
  });
}

// ── Suite 1: Filter bar ───────────────────────────────────────────────────────

describe("ActivityTab — filter bar", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockGet.mockResolvedValue([]);
  });
  afterEach(() => cleanup());

  it("renders all 7 filter options", () => {
    render(<ActivityTab workspaceId="ws-1" />);
    const filters = ["All", "A2A In", "A2A Out", "Tasks", "Skill Promo", "Logs", "Errors"];
    for (const f of filters) {
      expect(screen.getByRole("button", { name: new RegExp(f, "i") })).toBeTruthy();
    }
  });

  it('renders "All" as aria-pressed="true" by default', () => {
    render(<ActivityTab workspaceId="ws-1" />);
    expect(screen.getByRole("button", { name: /all/i }).getAttribute("aria-pressed")).toBe("true");
  });

  it("other filters default to aria-pressed=\"false\"", () => {
    render(<ActivityTab workspaceId="ws-1" />);
    expect(screen.getByRole("button", { name: /a2a in/i }).getAttribute("aria-pressed")).toBe("false");
    expect(screen.getByRole("button", { name: /tasks/i }).getAttribute("aria-pressed")).toBe("false");
  });

  it("clicking Errors filter sets it to aria-pressed=\"true\" and All to false", async () => {
    render(<ActivityTab workspaceId="ws-1" />);
    clickButton(/errors/i);
    expect(screen.getByRole("button", { name: /errors/i }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByRole("button", { name: /all/i }).getAttribute("aria-pressed")).toBe("false");
  });

  it("clicking A2A In filter triggers reload with correct type param", async () => {
    render(<ActivityTab workspaceId="ws-1" />);
    clickButton(/a2a in/i);
    await waitFor(() => {
      expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-1/activity?type=a2a_receive");
    });
  });

  it("clicking All triggers reload without type param", async () => {
    render(<ActivityTab workspaceId="ws-1" />);
    clickButton(/tasks/i); // change filter to "Tasks"
    mockGet.mockClear();
    clickButton(/all/i);  // change back to "All"
    await waitFor(() => {
      expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-1/activity");
    });
  });
});

// ── Suite 2: Loading, error, empty states ─────────────────────────────────────

describe("ActivityTab — states", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });
  afterEach(() => cleanup());

  it("shows loading text while initial fetch is in-flight", () => {
    mockGet.mockImplementation(() => new Promise(() => {})); // never resolves
    render(<ActivityTab workspaceId="ws-1" />);
    expect(screen.getByText("Loading activity...")).toBeTruthy();
  });

  it("shows error banner on API failure", async () => {
    mockGet.mockRejectedValueOnce(new Error("db connection lost"));
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText(/db connection lost/i)).toBeTruthy();
    });
  });

  it("shows empty state when no activities", async () => {
    mockGet.mockResolvedValueOnce([]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText(/no activity recorded yet/i)).toBeTruthy();
    });
  });
});

// ── Suite 3: ActivityRow rendering ─────────────────────────────────────────────

describe("ActivityTab — ActivityRow content", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockGet.mockResolvedValue([]);
  });
  afterEach(() => cleanup());

  it("renders type badge for a2a_send", async () => {
    mockGet.mockResolvedValueOnce([makeEntry({ activity_type: "a2a_send", summary: "delegation" })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText("A2A OUT")).toBeTruthy();
    });
  });

  it("renders type badge for task_update", async () => {
    mockGet.mockResolvedValueOnce([makeEntry({ activity_type: "task_update", summary: "task done" })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText("TASK")).toBeTruthy();
    });
  });

  it("renders type badge for skill_promotion", async () => {
    mockGet.mockResolvedValueOnce([makeEntry({ activity_type: "skill_promotion", summary: "promoted" })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText("PROMO")).toBeTruthy();
    });
  });

  it("renders type badge for error activity_type", async () => {
    mockGet.mockResolvedValueOnce([makeEntry({ activity_type: "error" })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText(/ERROR/)).toBeTruthy();
    });
  });

  it("renders method text when present", async () => {
    mockGet.mockResolvedValueOnce([makeEntry({ method: "GET /api/tasks" })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText("GET /api/tasks")).toBeTruthy();
    });
  });

  it("renders duration_ms when present", async () => {
    mockGet.mockResolvedValueOnce([makeEntry({ duration_ms: 5432 })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText("5432ms")).toBeTruthy();
    });
  });

  it("renders summary text when present", async () => {
    mockGet.mockResolvedValueOnce([makeEntry({ summary: "Deployed marketing agent" })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText(/marketing agent/i)).toBeTruthy();
    });
  });

  it("error status entry renders ERROR badge", async () => {
    mockGet.mockResolvedValueOnce([makeEntry({ activity_type: "error", status: "error", error_detail: "timeout" })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText(/ERROR/)).toBeTruthy();
    });
  });

  it("error entry shows error_detail when expanded", async () => {
    mockGet.mockResolvedValueOnce([
      makeEntry({
        activity_type: "error",
        status: "error",
        error_detail: "Connection refused",
        request_body: null,
        response_body: null,
      }),
    ]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText(/ERROR/)).toBeTruthy();
    });
    // Click the row's toggle button to expand the entry
    const errorRow = screen.getByText(/ERROR/).closest("button");
    act(() => {
      fireEvent.click(errorRow as HTMLElement);
    });
    await waitFor(() => {
      expect(screen.getAllByText(/Connection refused/).length).toBeGreaterThan(0);
    });
  });
});

// ── Suite 4: A2A flow indicators ─────────────────────────────────────────────

describe("ActivityTab — A2A flow indicators", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockGet.mockResolvedValue([]);
  });
  afterEach(() => cleanup());

  it("renders resolved source name from useWorkspaceName hook", async () => {
    mockGet.mockResolvedValueOnce([
      makeA2AEntry("ws-agent-1", "ws-agent-2", "Analysis task", "ok"),
    ]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      // resolveName is mocked to return "Test WS"
      expect(screen.getAllByText("Test WS").length).toBeGreaterThan(0);
    });
  });

  it("renders arrow between source and target names", async () => {
    mockGet.mockResolvedValueOnce([
      makeA2AEntry("ws-agent-1", "ws-agent-2", "Analysis task"),
    ]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText("→")).toBeTruthy();
    });
  });
});

// ── Suite 5: Auto-refresh toggle ──────────────────────────────────────────────

describe("ActivityTab — auto-refresh toggle", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockGet.mockResolvedValue([]);
  });
  afterEach(() => cleanup());

  it("renders Live label by default", () => {
    render(<ActivityTab workspaceId="ws-1" />);
    expect(screen.getByText(/Live/)).toBeTruthy();
  });

  it("clicking Live pauses auto-refresh and shows Paused", async () => {
    render(<ActivityTab workspaceId="ws-1" />);
    clickButton(/live/i);
    await waitFor(() => {
      expect(screen.getByText(/Paused/)).toBeTruthy();
    });
  });

  it("clicking Paused resumes auto-refresh and shows Live", async () => {
    render(<ActivityTab workspaceId="ws-1" />);
    clickButton(/live/i);
    clickButton(/paused/i);
    await waitFor(() => {
      expect(screen.getByText(/Live/)).toBeTruthy();
    });
  });
});

// ── Suite 6: Refresh button ──────────────────────────────────────────────────

describe("ActivityTab — refresh button", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockGet.mockResolvedValue([]);
  });
  afterEach(() => cleanup());

  it("renders a Refresh button", () => {
    render(<ActivityTab workspaceId="ws-1" />);
    expect(screen.getByRole("button", { name: /refresh/i })).toBeTruthy();
  });

  it("clicking Refresh reloads data", async () => {
    render(<ActivityTab workspaceId="ws-1" />);
    clickButton(/refresh/i);
    await waitFor(() => {
      expect(mockGet).toHaveBeenCalled();
    });
  });
});

// ── Suite 6.5: ACTIVITY_LOGGED subscription (#61 stage 3) ─────────────────────
//
// Pin the post-#61 behaviour: WS push extends the rendered list with NO
// additional HTTP fetch. The 5s polling loop is gone; live updates
// arrive over the WebSocket bus.

describe("ActivityTab — #61 stage 3: ACTIVITY_LOGGED subscription", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockGet.mockResolvedValue([]);
    _resetSocketEventListenersForTests();
  });
  afterEach(() => {
    cleanup();
    _resetSocketEventListenersForTests();
  });

  function emitActivity(overrides: {
    workspaceId?: string;
    activityType?: string;
    summary?: string;
    id?: string;
  } = {}) {
    const realNow = Date.now();
    emitSocketEvent({
      event: "ACTIVITY_LOGGED",
      workspace_id: overrides.workspaceId ?? "ws-1",
      timestamp: new Date(realNow).toISOString(),
      payload: {
        id: overrides.id ?? `act-${Math.random().toString(36).slice(2)}`,
        activity_type: overrides.activityType ?? "agent_log",
        source_id: null,
        target_id: null,
        method: null,
        summary: overrides.summary ?? "live-pushed",
        status: "ok",
        created_at: new Date(realNow - 5_000).toISOString(),
      },
    });
  }

  it("WS push for matching workspace prepends to the list with NO HTTP call", async () => {
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText(/0 activities|no activity/i)).toBeTruthy();
    });
    mockGet.mockClear();

    await act(async () => {
      emitActivity({ summary: "live-row-from-bus" });
    });

    await waitFor(() => {
      expect(screen.getByText(/live-row-from-bus/)).toBeTruthy();
    });
    expect(mockGet).not.toHaveBeenCalled();
  });

  it("WS push for a different workspace is ignored", async () => {
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => screen.getByText(/no activity/i));

    await act(async () => {
      emitActivity({
        workspaceId: "ws-other",
        summary: "should-not-render-other-ws",
      });
    });

    expect(screen.queryByText(/should-not-render-other-ws/)).toBeNull();
  });

  it("WS push respects the active filter — non-matching activity_type is ignored", async () => {
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => screen.getByText(/no activity/i));

    // Apply "Tasks" filter.
    clickButton(/tasks/i);
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /tasks/i }).getAttribute("aria-pressed"),
      ).toBe("true");
    });

    // Push an a2a_send (does NOT match task_update filter).
    await act(async () => {
      emitActivity({
        activityType: "a2a_send",
        summary: "should-not-render-filter-mismatch",
      });
    });

    expect(
      screen.queryByText(/should-not-render-filter-mismatch/),
    ).toBeNull();
  });

  it("WS push respects the active filter — matching activity_type is rendered", async () => {
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => screen.getByText(/no activity/i));

    clickButton(/tasks/i);
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /tasks/i }).getAttribute("aria-pressed"),
      ).toBe("true");
    });

    await act(async () => {
      emitActivity({
        activityType: "task_update",
        summary: "task-filter-match",
      });
    });

    await waitFor(() => {
      expect(screen.getByText(/task-filter-match/)).toBeTruthy();
    });
  });

  it("WS push while autoRefresh is paused is ignored", async () => {
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => screen.getByText(/no activity/i));

    // Toggle Live → Paused.
    clickButton(/live/i);
    await waitFor(() => {
      expect(screen.getByText(/Paused/)).toBeTruthy();
    });

    await act(async () => {
      emitActivity({ summary: "should-not-render-paused" });
    });

    expect(screen.queryByText(/should-not-render-paused/)).toBeNull();
  });

  it("WS push for a row already in the list is deduped (no double-render)", async () => {
    // Bootstrap with one row — same id as the WS push to trigger dedup.
    mockGet.mockResolvedValueOnce([
      makeEntry({ id: "shared-id", summary: "bootstrap-summary" }),
    ]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText(/bootstrap-summary/)).toBeTruthy();
    });
    mockGet.mockClear();

    // Push a row with the SAME id but a different summary — must not
    // render the new summary; original row stays.
    await act(async () => {
      emitActivity({
        id: "shared-id",
        summary: "should-not-replace-existing",
      });
    });

    expect(screen.queryByText(/should-not-replace-existing/)).toBeNull();
    // Also verify count didn't grow.
    expect(screen.getByText(/1 activities/)).toBeTruthy();
  });

  it("does NOT poll on a 5s interval after mount (post-#61)", async () => {
    vi.useFakeTimers();
    try {
      render(<ActivityTab workspaceId="ws-1" />);
      // Drain the mount-time bootstrap promise.
      await act(async () => {
        await Promise.resolve();
        await Promise.resolve();
      });
      const callsAfterBootstrap = mockGet.mock.calls.length;
      expect(callsAfterBootstrap).toBeGreaterThanOrEqual(1);

      // Pre-#61: a 30s clock advance fires 6 more polls. Post-#61: 0.
      await act(async () => {
        vi.advanceTimersByTime(30_000);
      });
      expect(mockGet.mock.calls.length).toBe(callsAfterBootstrap);
    } finally {
      vi.useRealTimers();
    }
  });
});

// ── Suite 7: Activity count ───────────────────────────────────────────────────

describe("ActivityTab — activity count", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });
  afterEach(() => cleanup());

  it("shows correct count for all activities", async () => {
    mockGet.mockResolvedValueOnce([
      makeEntry({ id: "e1" }),
      makeEntry({ id: "e2" }),
      makeEntry({ id: "e3" }),
    ]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText("3 activities")).toBeTruthy();
    });
  });

  it("shows count with filter name for filtered results", async () => {
    // Always return one entry so any API call sees the correct count
    mockGet.mockResolvedValue([makeEntry({ id: "e1" })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText("1 activities")).toBeTruthy();
    });
    clickButton(/tasks/i);
    await waitFor(() => {
      expect(screen.getByText(/1 task update entries/)).toBeTruthy();
    });
  });
});