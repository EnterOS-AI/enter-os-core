// @vitest-environment jsdom
/**
 * AuditTrailPanel tests — issue #753
 *
 * Split into three suites:
 *  1. formatAuditRelativeTime — pure helper (no mocks needed)
 *  2. AuditEntryRow — entry renderer: badges, tamper flag, timestamp, summary
 *  3. AuditTrailPanel — component integration: loading, empty state, entries,
 *                        filter bar, pagination, error handling
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent, act } from "@testing-library/react";

// ── Mocks (hoisted before imports) ────────────────────────────────────────────

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn() },
}));

// ── Imports (after mocks) ─────────────────────────────────────────────────────

import { api } from "@/lib/api";
import {
  formatAuditRelativeTime,
  AuditEntryRow,
  AuditTrailPanel,
} from "../AuditTrailPanel";
import type { AuditEntry } from "@/types/audit";

const mockGet = vi.mocked(api.get);

// ── Helpers ───────────────────────────────────────────────────────────────────

const NOW = 1_745_000_000_000; // fixed "now" for deterministic tests

function makeEntry(overrides: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: "entry-1",
    workspace_id: "ws-a",
    event_type: "delegation",
    actor: "research-agent",
    summary: "Delegated SEO analysis to marketing-agent",
    chain_valid: true,
    created_at: new Date(NOW - 120_000).toISOString(), // 2 min ago
    ...overrides,
  };
}

function makeResponse(
  entries: AuditEntry[],
  cursor: string | null = null
) {
  return { entries, cursor };
}

// ── Suite 1: formatAuditRelativeTime ─────────────────────────────────────────

describe("formatAuditRelativeTime", () => {
  it("returns 'just now' when diff < 60 s", () => {
    expect(formatAuditRelativeTime(new Date(NOW - 30_000).toISOString(), NOW)).toBe("just now");
  });

  it("returns 'Xm ago' for minute-scale diffs", () => {
    expect(formatAuditRelativeTime(new Date(NOW - 3 * 60_000).toISOString(), NOW)).toBe("3m ago");
  });

  it("returns 'Xh ago' for hour-scale diffs", () => {
    expect(formatAuditRelativeTime(new Date(NOW - 2 * 3_600_000).toISOString(), NOW)).toBe("2h ago");
  });

  it("returns a locale date string for diffs >= 24 h", () => {
    const ts = new Date(NOW - 25 * 3_600_000).toISOString();
    const result = formatAuditRelativeTime(ts, NOW);
    // Should be a locale-formatted date, not "Xh ago"
    expect(result).not.toMatch(/ago/);
    expect(result.length).toBeGreaterThan(0);
  });
});

// ── Suite 2: AuditEntryRow ────────────────────────────────────────────────────

describe("AuditEntryRow — badge colors", () => {
  afterEach(() => cleanup());

  it("renders the delegation badge", () => {
    render(<AuditEntryRow entry={makeEntry({ event_type: "delegation" })} now={NOW} />);
    expect(screen.getByText("delegation")).toBeTruthy();
  });

  it("renders the decision badge", () => {
    render(<AuditEntryRow entry={makeEntry({ event_type: "decision" })} now={NOW} />);
    expect(screen.getByText("decision")).toBeTruthy();
  });

  it("renders the gate badge", () => {
    render(<AuditEntryRow entry={makeEntry({ event_type: "gate" })} now={NOW} />);
    expect(screen.getByText("gate")).toBeTruthy();
  });

  it("renders the hitl badge", () => {
    render(<AuditEntryRow entry={makeEntry({ event_type: "hitl" })} now={NOW} />);
    expect(screen.getByText("hitl")).toBeTruthy();
  });
});

describe("AuditEntryRow — content", () => {
  afterEach(() => cleanup());

  it("displays actor name", () => {
    render(<AuditEntryRow entry={makeEntry({ actor: "my-research-agent" })} now={NOW} />);
    expect(screen.getByText("my-research-agent")).toBeTruthy();
  });

  it("displays summary text", () => {
    render(<AuditEntryRow entry={makeEntry({ summary: "Approved budget allocation" })} now={NOW} />);
    expect(screen.getByText("Approved budget allocation")).toBeTruthy();
  });

  it("shows relative timestamp", () => {
    render(<AuditEntryRow entry={makeEntry({ created_at: new Date(NOW - 2 * 60_000).toISOString() })} now={NOW} />);
    expect(screen.getByText("2m ago")).toBeTruthy();
  });

  it("does NOT render tamper warning when chain_valid is true", () => {
    render(<AuditEntryRow entry={makeEntry({ chain_valid: true })} now={NOW} />);
    expect(screen.queryByRole("img", { name: /tamper/i })).toBeNull();
  });

  it("renders ⚠ tamper warning when chain_valid is false", () => {
    render(<AuditEntryRow entry={makeEntry({ chain_valid: false })} now={NOW} />);
    const warning = screen.getByRole("img", { name: /tamper/i });
    expect(warning).toBeTruthy();
    expect(warning.textContent).toContain("⚠");
  });
});

// ── Suite 3: AuditTrailPanel component ───────────────────────────────────────

describe("AuditTrailPanel — loading and empty state", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it("shows loading state while fetch is in-flight", async () => {
    // Never resolve to keep loading state
    mockGet.mockReturnValue(new Promise(() => {}));
    render(<AuditTrailPanel workspaceId="ws-a" />);
    expect(screen.getByText("Loading audit trail…")).toBeTruthy();
  });

  it("shows empty state when entries array is empty", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(makeResponse([]) as any);
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText("No audit events yet")).toBeTruthy();
  });

  it("shows descriptive empty state copy", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(makeResponse([]) as any);
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText(/Delegation, decision, gate/i)).toBeTruthy();
  });
});

describe("AuditTrailPanel — entries", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it("renders all returned entries", async () => {
    const entries = [
      makeEntry({ id: "e1", actor: "agent-alpha" }),
      makeEntry({ id: "e2", actor: "agent-beta" }),
    ];
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(makeResponse(entries) as any);
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText("agent-alpha")).toBeTruthy();
    expect(screen.getByText("agent-beta")).toBeTruthy();
  });

  it("renders tamper warning for chain_valid=false entry", async () => {
    const entries = [makeEntry({ id: "e1", chain_valid: false })];
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(makeResponse(entries) as any);
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByRole("img", { name: /tamper/i })).toBeTruthy();
  });

  it("shows entry count footer", async () => {
    const entries = [makeEntry({ id: "e1" }), makeEntry({ id: "e2" })];
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(makeResponse(entries) as any);
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText(/2 events loaded/)).toBeTruthy();
  });

  it("shows 'all loaded' when cursor is null", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(makeResponse([makeEntry()], null) as any);
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText(/all loaded/)).toBeTruthy();
  });
});

describe("AuditTrailPanel — pagination", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it("shows 'Load more' button when cursor is non-null", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(makeResponse([makeEntry()], "cursor-abc") as any);
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByRole("button", { name: /load more/i })).toBeTruthy();
  });

  it("does NOT show 'Load more' when cursor is null", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(makeResponse([makeEntry()], null) as any);
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.queryByRole("button", { name: /load more/i })).toBeNull();
  });

  it("appends entries and updates cursor when 'Load more' is clicked", async () => {
    const page1 = [makeEntry({ id: "e1", actor: "alpha" })];
    const page2 = [makeEntry({ id: "e2", actor: "beta" })];
    mockGet
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      .mockResolvedValueOnce(makeResponse(page1, "cursor-next") as any)
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      .mockResolvedValueOnce(makeResponse(page2, null) as any);

    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });

    expect(screen.getByText("alpha")).toBeTruthy();
    expect(screen.queryByText("beta")).toBeNull();

    const loadMoreBtn = screen.getByRole("button", { name: /load more/i });
    fireEvent.click(loadMoreBtn);
    await act(async () => { await Promise.resolve(); });

    expect(screen.getByText("alpha")).toBeTruthy();
    expect(screen.getByText("beta")).toBeTruthy();
    // Cursor is now null — Load more should disappear
    expect(screen.queryByRole("button", { name: /load more/i })).toBeNull();
  });

  it("second page request includes cursor param", async () => {
    const page1 = [makeEntry({ id: "e1" })];
    mockGet
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      .mockResolvedValueOnce(makeResponse(page1, "cursor-xyz") as any)
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      .mockResolvedValueOnce(makeResponse([], null) as any);

    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });

    fireEvent.click(screen.getByRole("button", { name: /load more/i }));
    await act(async () => { await Promise.resolve(); });

    // Second call should include cursor=cursor-xyz
    const secondCallPath = mockGet.mock.calls[1][0] as string;
    expect(secondCallPath).toContain("cursor=cursor-xyz");
  });
});

describe("AuditTrailPanel — filter bar", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it("renders all five filter buttons", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(makeResponse([]) as any);
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByRole("button", { name: /^All$/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Delegation$/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Decision$/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Gate$/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /^HITL$/i })).toBeTruthy();
  });

  it("includes event_type param when a type filter is active", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(makeResponse([]) as any);
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });

    const delegationBtn = screen.getByRole("button", { name: /^Delegation$/i });
    fireEvent.click(delegationBtn);
    await act(async () => { await Promise.resolve(); });

    // Second API call should include event_type=delegation
    const lastCallPath = mockGet.mock.calls[mockGet.mock.calls.length - 1][0] as string;
    expect(lastCallPath).toContain("event_type=delegation");
  });

  it("omits event_type param when 'All' filter is active", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(makeResponse([]) as any);
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });

    const firstCallPath = mockGet.mock.calls[0][0] as string;
    expect(firstCallPath).not.toContain("event_type");
  });
});

describe("AuditTrailPanel — error handling", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it("shows error banner when fetch fails", async () => {
    mockGet.mockRejectedValue(new Error("Network timeout"));
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText("Network timeout")).toBeTruthy();
  });

  it("still renders empty state (not error) on successful empty response", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    mockGet.mockResolvedValue(makeResponse([]) as any);
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.queryByText(/Network/)).toBeNull();
    expect(screen.getByText("No audit events yet")).toBeTruthy();
  });
});
