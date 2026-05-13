// @vitest-environment jsdom
/**
 * Tests for ActivityTab — activity ledger with live updates, filtering,
 * expand/collapse, and A2A error hint rendering.
 *
 * Covers:
 *   - Loading state
 *   - Error state (network failure)
 *   - Empty state (no activities)
 *   - Activity list rendering (single + multiple)
 *   - Filter bar: 7 filters, active filter highlighted
 *   - Each filter updates the rendered list
 *   - Auto-refresh toggle (Live / Paused)
 *   - Refresh button calls API
 *   - Full Trace button opens ConversationTraceModal
 *   - Duration display in activity rows
 *   - Expand/collapse row details
 *   - A2A rows show source → target name flow
 *   - Error rows styled differently
 *   - Error detail shown when expanded
 *   - getSkills exported function (standalone unit)
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ActivityTab } from "../ActivityTab";
import type { ActivityEntry } from "@/types/activity";

const mockApiGet = vi.fn();

const mockUseSocketEvent = vi.fn();
const mockUseWorkspaceName = vi.fn<(id: string | null) => string>((_id: string | null) => "Test Workspace");
const mockConversationTraceModal = vi.fn(() => null);
const mockConversationTraceModalRender = vi.fn(
  ({ open }: { open: boolean }) => (open ? <div data-testid="trace-modal">Trace</div> : null),
);

vi.mock("@/hooks/useSocketEvent", () => ({
  useSocketEvent: (...args: unknown[]) => mockUseSocketEvent(...args),
}));

vi.mock("@/hooks/useWorkspaceName", () => ({
  useWorkspaceName: () => mockUseWorkspaceName,
}));

vi.mock("@/components/ConversationTraceModal", () => ({
  ConversationTraceModal: (props: { open: boolean; onClose: () => void; workspaceId: string }) =>
    props.open ? <div data-testid="trace-modal">Trace</div> : null,
}));

vi.mock("@/lib/api", () => ({
  api: { get: (...args: unknown[]) => mockApiGet(...args) },
}));

// ─── Fixtures ───────────────────────────────────────────────────────────────

function activity(overrides: Partial<ActivityEntry> = {}): ActivityEntry {
  return {
    id: "act-1",
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
    created_at: new Date(Date.now() - 60_000).toISOString(),
    ...overrides,
  };
}

// ─── Helpers ────────────────────────────────────────────────────────────────

async function flush() {
  await act(async () => { await Promise.resolve(); });
}

// ─── Tests ────────────────────────────────────────────────────────────────

describe("ActivityTab — loading / error / empty", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockUseSocketEvent.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("shows loading state initially", () => {
    mockApiGet.mockImplementation(() => new Promise(() => {}));
    render(<ActivityTab workspaceId="ws-1" />);
    expect(screen.getByText("Loading activity...")).toBeTruthy();
  });

  it("shows error banner when API fails", async () => {
    mockApiGet.mockRejectedValue(new Error("network failure"));
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/network failure/i)).toBeTruthy();
  });

  it("shows empty state when no activities", async () => {
    mockApiGet.mockResolvedValue([]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("No activity recorded yet")).toBeTruthy();
  });
});

describe("ActivityTab — list rendering", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockUseSocketEvent.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("renders a single activity row", async () => {
    mockApiGet.mockResolvedValue([activity({ id: "a1", activity_type: "agent_log" })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("LOG")).toBeTruthy();
  });

  it("renders multiple activity rows", async () => {
    mockApiGet.mockResolvedValue([
      activity({ id: "a1", activity_type: "agent_log" }),
      activity({ id: "a2", activity_type: "task_update" }),
    ]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("LOG")).toBeTruthy();
    expect(screen.getByText("TASK")).toBeTruthy();
  });

  it("shows duration when duration_ms is present", async () => {
    mockApiGet.mockResolvedValue([
      activity({ id: "a1", duration_ms: 1234, activity_type: "agent_log" }),
    ]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("1234ms")).toBeTruthy();
  });

  it("shows summary text when present", async () => {
    mockApiGet.mockResolvedValue([
      activity({ id: "a1", summary: "Delegated task to SEO Agent", activity_type: "a2a_send" }),
    ]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/Delegated task to SEO Agent/)).toBeTruthy();
  });
});

describe("ActivityTab — filter bar", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockUseSocketEvent.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("renders all 7 filter buttons", async () => {
    mockApiGet.mockResolvedValue([]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByRole("button", { name: /all/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /a2a in/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /a2a out/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /tasks/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /skill promo/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /logs/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /errors/i })).toBeTruthy();
  });

  it("active filter has aria-pressed=true", async () => {
    mockApiGet.mockResolvedValue([]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    const allBtn = screen.getByRole("button", { name: /all/i });
    expect(allBtn.getAttribute("aria-pressed")).toBe("true");
  });

  it("clicking a filter updates aria-pressed and re-fetches", async () => {
    mockApiGet.mockResolvedValue([]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    const errorsBtn = screen.getByRole("button", { name: /errors/i });
    await act(async () => { errorsBtn.click(); });
    await flush();
    expect(errorsBtn.getAttribute("aria-pressed")).toBe("true");
    // API was called with ?type=error
    expect(mockApiGet).toHaveBeenLastCalledWith("/workspaces/ws-1/activity?type=error");
  });

  it("clicking All removes the type query param", async () => {
    mockApiGet.mockResolvedValue([]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    // First click a specific filter
    const errorsBtn = screen.getByRole("button", { name: /errors/i });
    await act(async () => { errorsBtn.click(); });
    await flush();
    // Then click All
    const allBtn = screen.getByRole("button", { name: /all/i });
    await act(async () => { allBtn.click(); });
    await flush();
    expect(mockApiGet).toHaveBeenLastCalledWith("/workspaces/ws-1/activity");
  });
});

describe("ActivityTab — auto-refresh toggle", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockUseSocketEvent.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("renders Live by default", async () => {
    mockApiGet.mockResolvedValue([]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("⟳ Live")).toBeTruthy();
  });

  it("clicking Live toggles to Paused", async () => {
    mockApiGet.mockResolvedValue([]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    const liveBtn = screen.getByText("⟳ Live");
    await act(async () => { liveBtn.click(); });
    await flush();
    expect(screen.getByText("⟳ Paused")).toBeTruthy();
  });

  it("clicking Paused toggles back to Live", async () => {
    mockApiGet.mockResolvedValue([]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    const liveBtn = screen.getByText("⟳ Live");
    await act(async () => { liveBtn.click(); });
    await flush();
    const pausedBtn = screen.getByText("⟳ Paused");
    await act(async () => { pausedBtn.click(); });
    await flush();
    expect(screen.getByText("⟳ Live")).toBeTruthy();
  });
});

describe("ActivityTab — refresh button", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockUseSocketEvent.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("Refresh calls the API", async () => {
    mockApiGet.mockResolvedValue([]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    const refreshBtn = screen.getByRole("button", { name: /refresh/i });
    await act(async () => { refreshBtn.click(); });
    await flush();
    // loadActivities called again (second call)
    expect(mockApiGet.mock.calls.length).toBeGreaterThanOrEqual(2);
  });
});

describe("ActivityTab — Full Trace button", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockUseSocketEvent.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("Full Trace button opens the trace modal", async () => {
    mockApiGet.mockResolvedValue([]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    const traceBtn = screen.getByRole("button", { name: /full trace/i });
    await act(async () => { traceBtn.click(); });
    await flush();
    expect(screen.getByTestId("trace-modal")).toBeTruthy();
  });
});

describe("ActivityTab — row expand / collapse", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockUseSocketEvent.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("row is collapsed by default (shows ▶)", async () => {
    mockApiGet.mockResolvedValue([activity({ id: "a1", activity_type: "agent_log" })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("▶")).toBeTruthy();
  });

  it("clicking a row expands it (shows ▼)", async () => {
    mockApiGet.mockResolvedValue([activity({ id: "a1", activity_type: "agent_log" })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    const rowBtn = screen.getByText("LOG").closest("button") as HTMLButtonElement;
    await act(async () => { rowBtn.click(); });
    await flush();
    expect(screen.getByText("▼")).toBeTruthy();
  });

  it("clicking expanded row collapses it", async () => {
    mockApiGet.mockResolvedValue([activity({ id: "a1", activity_type: "agent_log" })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    const rowBtn = screen.getByText("LOG").closest("button") as HTMLButtonElement;
    await act(async () => { rowBtn.click(); }); // expand
    await flush();
    await act(async () => { rowBtn.click(); }); // collapse
    await flush();
    expect(screen.getByText("▶")).toBeTruthy();
  });
});

describe("ActivityTab — A2A rows with source/target", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockUseSocketEvent.mockReset();
    mockUseWorkspaceName.mockImplementation((id: string | null) => {
      if (id === "ws-agent-1") return "Alice Agent";
      if (id === "ws-agent-2") return "Bob Agent";
      return "Unknown";
    });
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("shows source → target for a2a_receive rows", async () => {
    mockApiGet.mockResolvedValue([
      activity({
        id: "a1",
        activity_type: "a2a_receive",
        source_id: "ws-agent-1",
        target_id: "ws-agent-2",
        method: "message/send",
      }),
    ]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("Alice Agent")).toBeTruthy();
    expect(screen.getByText("→")).toBeTruthy();
    expect(screen.getByText("Bob Agent")).toBeTruthy();
  });

  it("shows A2A OUT badge for a2a_send rows", async () => {
    mockApiGet.mockResolvedValue([
      activity({
        id: "a1",
        activity_type: "a2a_send",
        source_id: "ws-agent-1",
        target_id: "ws-agent-2",
      }),
    ]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("A2A OUT")).toBeTruthy();
  });
});

describe("ActivityTab — error rows", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockUseSocketEvent.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("error status row renders with ERROR badge", async () => {
    mockApiGet.mockResolvedValue([
      activity({ id: "a1", activity_type: "error", status: "error" }),
    ]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("ERROR")).toBeTruthy();
  });

  it("error detail is shown when row is expanded", async () => {
    mockApiGet.mockResolvedValue([
      activity({
        id: "a1",
        activity_type: "error",
        status: "error",
        error_detail: "Connection refused",
        duration_ms: null,
      }),
    ]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    const rowBtn = screen.getByText("ERROR").closest("button") as HTMLButtonElement;
    await act(async () => { rowBtn.click(); });
    await flush();
    // Text appears twice: collapsed-row preview + expanded detail section
    expect(screen.getAllByText("Connection refused")).toHaveLength(2);
  });
});

describe("ActivityTab — type badge rendering", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockUseSocketEvent.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("renders correct badge text for each type", async () => {
    const types: ActivityEntry["activity_type"][] = [
      "a2a_receive", "a2a_send", "task_update", "skill_promotion", "agent_log", "error",
    ];
    const entries = types.map((t, i) =>
      activity({ id: `a${i}`, activity_type: t }),
    );
    mockApiGet.mockResolvedValue(entries);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("A2A IN")).toBeTruthy();
    expect(screen.getByText("A2A OUT")).toBeTruthy();
    expect(screen.getByText("TASK")).toBeTruthy();
    expect(screen.getByText("PROMO")).toBeTruthy();
    expect(screen.getByText("LOG")).toBeTruthy();
    expect(screen.getByText("ERROR")).toBeTruthy();
  });
});

describe("ActivityTab — count display", () => {
  beforeEach(() => {
    mockApiGet.mockReset();
    mockUseSocketEvent.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("shows count with 'activities' label when filter=all", async () => {
    mockApiGet.mockResolvedValue([
      activity({ id: "a1" }),
      activity({ id: "a2" }),
    ]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/2 activities/)).toBeTruthy();
  });

  it("shows count with filter label when non-all filter selected", async () => {
    mockApiGet.mockResolvedValue([activity({ id: "a1", activity_type: "error" })]);
    render(<ActivityTab workspaceId="ws-1" />);
    await flush();
    const errorsBtn = screen.getByRole("button", { name: /errors/i });
    await act(async () => { errorsBtn.click(); });
    await flush();
    expect(screen.getByText(/1 error entries/)).toBeTruthy();
  });
});

describe("getSkills — unit", () => {
  it("returns empty array for null card", async () => {
    const { getSkills } = await import("../DetailsTab");
    expect(getSkills(null)).toEqual([]);
  });

  it("returns empty array when skills is not an array", async () => {
    const { getSkills } = await import("../DetailsTab");
    expect(getSkills({ name: "test" } as Record<string, unknown>)).toEqual([]);
  });

  it("extracts skill ids and descriptions", async () => {
    const { getSkills } = await import("../DetailsTab");
    const card = {
      skills: [
        { id: "web-search", description: "Search the web" },
        { name: "code-interpreter" },
        { id: "analytics" },
      ],
    };
    const result = getSkills(card as Record<string, unknown>);
    expect(result).toEqual([
      { id: "web-search", description: "Search the web" },
      { id: "code-interpreter" },
      { id: "analytics" },
    ]);
  });

  it("filters out skills with no id or name", async () => {
    const { getSkills } = await import("../DetailsTab");
    const card = { skills: [{ description: "no id" }, { id: "valid" }] };
    expect(getSkills(card as Record<string, unknown>)).toEqual([{ id: "valid" }]);
  });
});
