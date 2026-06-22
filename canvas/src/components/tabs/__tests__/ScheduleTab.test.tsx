// @vitest-environment jsdom
/**
 * Tests for ScheduleTab — cron-based task scheduling.
 *
 * Coverage:
 *   - Loading state
 *   - Empty state (no schedules)
 *   - Schedule list rendering (single + multiple)
 *   - Status dot color (error/ok/idle)
 *   - Toggle enable/disable via status dot
 *   - Delete via ConfirmDialog
 *   - Run Now button triggers POST + POST
 *   - Create schedule form open/close
 *   - Edit schedule form pre-fills values
 *   - Form validation (disabled when cron/prompt empty)
 *   - Create POST with correct payload
 *   - Edit PATCH with correct payload
 *   - Error state surfaces API failures
 *   - Auto-refresh every 10s (spy)
 *   - cronToHuman formatting
 *   - relativeTime formatting
 *   - Reset form clears all fields
 *   - Disabled schedules are visually dimmed
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ScheduleTab } from "../ScheduleTab";

// Hoist mocks so vi.mock factory can reference them.
const mockGet = vi.hoisted(() => vi.fn<[], Promise<unknown[]>>());
const mockPost = vi.hoisted(() => vi.fn<[], Promise<unknown>>());
const mockPatch = vi.hoisted(() => vi.fn<[], Promise<unknown>>());
const mockDel = vi.hoisted(() => vi.fn<[], Promise<unknown>>());

vi.mock("@/lib/api", () => ({
  api: { get: mockGet, post: mockPost, patch: mockPatch, del: mockDel },
}));

// Capture ConfirmDialog state to drive from tests.
const confirmDialogState = vi.hoisted(
  () => ({
    open: false as boolean,
    onConfirm: undefined as (() => void) | undefined,
    onCancel: undefined as (() => void) | undefined,
  }),
);
const MockConfirmDialog = vi.hoisted(
  () =>
    vi.fn(({ open, onConfirm, onCancel }: {
      open: boolean;
      onConfirm: () => void;
      onCancel: () => void;
    }) => {
      confirmDialogState.open = open;
      confirmDialogState.onConfirm = onConfirm;
      confirmDialogState.onCancel = onCancel;
      return null;
    }),
);
vi.mock("@/components/ConfirmDialog", () => ({ ConfirmDialog: MockConfirmDialog }));

// ─── Fixtures ─────────────────────────────────────────────────────────────────

const SCHEDULE_FIXTURE = {
  id: "sch-1",
  workspace_id: "ws-1",
  name: "Daily Security Scan",
  cron_expr: "0 9 * * *",
  timezone: "UTC",
  prompt: "Run the security scan and report findings",
  enabled: true,
  last_run_at: new Date(Date.now() - 3600000).toISOString(),
  next_run_at: new Date(Date.now() + 82800000).toISOString(),
  run_count: 42,
  last_status: "ok",
  last_error: "",
  created_at: new Date().toISOString(),
};

function schedule(overrides: Partial<typeof SCHEDULE_FIXTURE> = {}): typeof SCHEDULE_FIXTURE {
  return { ...SCHEDULE_FIXTURE, ...overrides };
}

// ─── Helpers ───────────────────────────────────────────────────────────────────

async function flush() {
  await act(async () => { await Promise.resolve(); });
}

function typeIn(el: HTMLElement, value: string) {
  Object.defineProperty(el, "value", { value, writable: true, configurable: true });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  fireEvent.change(el as any, { target: el });
}

// Use mockResolvedValue so every GET call (including post-handler refreshes)
// returns the fixture. Handlers like toggle/delete/run/edit all call
// fetchSchedules() at the end, triggering a second GET.
function setupLoad(schedules: unknown[]) {
  mockGet.mockResolvedValue(schedules as unknown[]);
}

// ─── Tests ─────────────────────────────────────────────────────────────────────

describe("ScheduleTab", () => {
  beforeEach(() => {
    mockGet.mockReset();
    mockPost.mockReset();
    mockPatch.mockReset();
    mockDel.mockReset();
    MockConfirmDialog.mockClear();
    vi.useRealTimers();
    confirmDialogState.open = false;
    confirmDialogState.onConfirm = undefined;
    confirmDialogState.onCancel = undefined;
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  // ── Loading / Empty ──────────────────────────────────────────────────────────

  it("shows loading state when schedules are being fetched", async () => {
    mockGet.mockImplementation(() => new Promise(() => {}));
    render(<ScheduleTab workspaceId="ws-1" />);
    await act(async () => { /* flush initial render */ });
    expect(screen.getByText("Loading schedules...")).toBeTruthy();
  });

  it("shows empty state when API returns an empty list", async () => {
    setupLoad([]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("No schedules yet")).toBeTruthy();
    expect(screen.getByText(/run tasks automatically/i)).toBeTruthy();
  });

  // ── Schedule list ────────────────────────────────────────────────────────────

  it("renders a schedule with correct name and cron", async () => {
    setupLoad([schedule({ name: "Morning Report", cron_expr: "0 8 * * *" })]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("Morning Report")).toBeTruthy();
    expect(screen.getByText(/Daily at 08:00 UTC/i)).toBeTruthy();
  });

  it("renders multiple schedules", async () => {
    setupLoad([
      schedule({ id: "s1", name: "Morning Report", cron_expr: "0 8 * * *" }),
      schedule({ id: "s2", name: "Evening Cleanup", cron_expr: "0 22 * * *" }),
    ]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("Morning Report")).toBeTruthy();
    expect(screen.getByText("Evening Cleanup")).toBeTruthy();
  });

  it("shows disabled schedule with reduced opacity", async () => {
    setupLoad([schedule({ enabled: false })]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    const container = screen.getByText("Daily Security Scan").closest("div[class*='border-b']");
    expect(container?.className).toContain("opacity-50");
  });

  it("shows error dot when last_status is error", async () => {
    setupLoad([schedule({ last_status: "error", last_error: "timeout" })]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    const dot = screen.getByRole("button", { name: /click to disable/i });
    expect(dot.className).toContain("bg-red-400");
  });

  it("shows ok dot when last_status is ok", async () => {
    setupLoad([schedule({ last_status: "ok" })]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    const dot = screen.getByRole("button", { name: /click to disable/i });
    expect(dot.className).toContain("bg-emerald-400");
  });

  it("shows neutral dot when schedule is disabled (unknown status)", async () => {
    // enabled=false → title says "Click to enable"
    setupLoad([schedule({ enabled: false, last_status: "" })]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    const dot = screen.getByRole("button", { name: /click to enable/i });
    expect(dot.className).toContain("bg-surface-card");
  });

  it("shows last_error message when schedule failed", async () => {
    setupLoad([schedule({ last_error: "connection refused" })]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/Error: connection refused/i)).toBeTruthy();
  });

  it("truncates long prompt in schedule list", async () => {
    const longPrompt = "A".repeat(120);
    setupLoad([schedule({ prompt: longPrompt })]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    // Prompt is sliced at 80 chars + "..."
    expect(screen.getByText(new RegExp(`^${"A".repeat(80)}\\.\\.\\.$$`))).toBeTruthy();
  });

  // ── cronToHuman formatting ──────────────────────────────────────────────────

  it.each([
    ["* * * * *", "Every minute"],
    ["*/5 * * * *", "Every 5 minutes"],
    ["0 */4 * * *", "Every 4 hours"],
    ["0 9 * * *", "Daily at 09:00 UTC"],
    ["0 9 * * 1-5", "Weekdays at 09:00 UTC"],
    ["30 14 * * *", "Daily at 14:30 UTC"],
    ["*/15 * * * *", "Every 15 minutes"],
  ])("formats cron '%s' as '%s'", async (cron, expected) => {
    setupLoad([schedule({ cron_expr: cron })]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(new RegExp(expected, "i"))).toBeTruthy();
  });

  // ── relativeTime formatting ─────────────────────────────────────────────────

  it("shows 'never' when last_run_at is null", async () => {
    setupLoad([schedule({ last_run_at: null, next_run_at: null })]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    const spans = Array.from(document.querySelectorAll("span"));
    expect(spans.some(s => s.textContent === "Last: never")).toBeTruthy();
  });

  it("shows run_count in the list", async () => {
    setupLoad([schedule({ run_count: 99 })]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/Runs: 99/i)).toBeTruthy();
  });

  // ── Toggle ──────────────────────────────────────────────────────────────────

  it("PATCHes toggle endpoint when status dot is clicked", async () => {
    setupLoad([schedule()]);
    mockPatch.mockResolvedValue({});
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /click to disable/i }));
    await flush();
    expect(mockPatch).toHaveBeenCalledWith(
      "/workspaces/ws-1/schedules/sch-1",
      { enabled: false },
    );
  });

  it("toggling calls fetchSchedules to refresh the list", async () => {
    setupLoad([schedule()]);
    mockPatch.mockResolvedValue({});
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /click to disable/i }));
    await flush();
    // fetchSchedules calls GET again
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-1/schedules");
  });

  it("shows error when toggle fails", async () => {
    setupLoad([schedule()]);
    mockPatch.mockRejectedValue(new Error("toggle failed"));
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /click to disable/i }));
    await flush();
    // Component uses e.message (Error.message = "toggle failed")
    expect(screen.getByText(/toggle failed/i)).toBeTruthy();
  });

  // ── Delete ──────────────────────────────────────────────────────────────────

  it("opens ConfirmDialog when delete button is clicked", async () => {
    setupLoad([schedule()]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /delete schedule/i }));
    await flush();
    expect(confirmDialogState.open).toBe(true);
  });

  it("calls DEL when ConfirmDialog is confirmed", async () => {
    setupLoad([schedule()]);
    mockDel.mockResolvedValue({});
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /delete schedule/i }));
    await flush();
    confirmDialogState.onConfirm?.();
    await flush();
    expect(mockDel).toHaveBeenCalledWith("/workspaces/ws-1/schedules/sch-1");
  });

  it("calls fetchSchedules after delete", async () => {
    setupLoad([schedule()]);
    mockDel.mockResolvedValue({});
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /delete schedule/i }));
    await flush();
    confirmDialogState.onConfirm?.();
    await flush();
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-1/schedules");
  });

  it("closes ConfirmDialog when cancel is called", async () => {
    setupLoad([schedule()]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /delete schedule/i }));
    await flush();
    expect(confirmDialogState.open).toBe(true);
    confirmDialogState.onCancel?.();
    await flush();
    expect(confirmDialogState.open).toBe(false);
  });

  it("shows error when delete fails", async () => {
    setupLoad([schedule()]);
    mockDel.mockRejectedValue(new Error("delete failed"));
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /delete schedule/i }));
    await flush();
    confirmDialogState.onConfirm?.();
    await flush();
    expect(screen.getByText(/delete failed/i)).toBeTruthy();
  });

  // ── Run Now ──────────────────────────────────────────────────────────────────

  it("calls POST /schedules/:id/run and then POST /a2a when Run Now is clicked", async () => {
    setupLoad([schedule()]);
    mockPost
      .mockResolvedValueOnce({ prompt: "Run the security scan and report findings" })
      .mockResolvedValueOnce({});
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /run schedule/i }));
    await flush();
    expect(mockPost).toHaveBeenNthCalledWith(1, "/workspaces/ws-1/schedules/sch-1/run", {});
    expect(mockPost).toHaveBeenNthCalledWith(2, "/workspaces/ws-1/a2a", expect.objectContaining({ method: "message/send" }));
  });

  it("shows error when run now fails", async () => {
    setupLoad([schedule()]);
    mockPost.mockRejectedValue(new Error("run failed"));
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /run schedule/i }));
    await flush();
    // handleRunNow uses hardcoded "Failed to run schedule" on error
    expect(screen.getByText(/Failed to run schedule/i)).toBeTruthy();
  });

  // ── Create form ──────────────────────────────────────────────────────────────

  it("shows create form when + Add Schedule is clicked", async () => {
    setupLoad([]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add schedule/i }));
    await flush();
    expect(screen.getByLabelText("Schedule name")).toBeTruthy();
    expect(screen.getByLabelText("Cron Expression")).toBeTruthy();
    expect(screen.getByLabelText("Prompt / Task")).toBeTruthy();
  });

  it("pre-fills default cron (0 9 * * *) and timezone (UTC)", async () => {
    setupLoad([]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add schedule/i }));
    await flush();
    expect((screen.getByLabelText("Cron Expression") as HTMLInputElement).value).toBe("0 9 * * *");
    expect((screen.getByLabelText("Timezone") as HTMLSelectElement).value).toBe("UTC");
  });

  it("submit button is disabled when cron or prompt is empty", async () => {
    setupLoad([]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add schedule/i }));
    await flush();
    const submitBtn = screen.getByRole("button", { name: /create/i });
    expect((submitBtn as HTMLButtonElement).disabled).toBe(true);
  });

  it("submit button is enabled when cron and prompt are filled", async () => {
    setupLoad([]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add schedule/i }));
    await flush();
    typeIn(screen.getByLabelText("Prompt / Task") as HTMLElement, "Run a task");
    await flush();
    const submitBtn = screen.getByRole("button", { name: /create/i });
    expect((submitBtn as HTMLButtonElement).disabled).toBe(false);
  });

  it("POSTs correct payload when creating a schedule", async () => {
    setupLoad([]);
    mockPost.mockResolvedValue({});
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add schedule/i }));
    await flush();
    typeIn(screen.getByLabelText("Schedule name") as HTMLElement, "Morning Report");
    typeIn(screen.getByLabelText("Cron Expression") as HTMLElement, "0 8 * * *");
    typeIn(screen.getByLabelText("Prompt / Task") as HTMLElement, "Generate the morning report");
    await flush();
    act(() => { screen.getByRole("button", { name: /create/i }).click(); });
    await flush();
    await waitFor(() => {
      expect(screen.queryByRole("button", { name: /cancel/i })).not.toBeTruthy();
    });
    expect(mockPost).toHaveBeenCalledWith(
      "/workspaces/ws-1/schedules",
      expect.objectContaining({
        name: "Morning Report",
        cron_expr: "0 8 * * *",
        timezone: "UTC",
        prompt: "Generate the morning report",
        enabled: true,
      }),
    );
  });

  it("closes form and refreshes after successful create", async () => {
    setupLoad([]);
    mockPost.mockResolvedValue({});
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add schedule/i }));
    await flush();
    typeIn(screen.getByLabelText("Prompt / Task") as HTMLElement, "Run a task");
    await flush();
    act(() => { screen.getByRole("button", { name: /create/i }).click(); });
    await flush();
    await waitFor(() => {
      expect(screen.queryByLabelText("Schedule name")).not.toBeTruthy();
    });
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-1/schedules");
  });

  it("shows error message when create fails", async () => {
    setupLoad([]);
    mockPost.mockRejectedValue(new Error("validation failed"));
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add schedule/i }));
    await flush();
    typeIn(screen.getByLabelText("Prompt / Task") as HTMLElement, "Run a task");
    await flush();
    act(() => { screen.getByRole("button", { name: /create/i }).click(); });
    await flush();
    expect(screen.getByText(/validation failed/i)).toBeTruthy();
  });

  it("closes form when Cancel is clicked", async () => {
    setupLoad([]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add schedule/i }));
    await flush();
    expect(screen.getByLabelText("Schedule name")).toBeTruthy();
    act(() => { screen.getByRole("button", { name: /cancel/i }).click(); });
    await flush();
    await waitFor(() => {
      expect(screen.queryByLabelText("Schedule name")).not.toBeTruthy();
    });
  });

  // ── Edit form ────────────────────────────────────────────────────────────────

  it("opens edit form pre-filled with schedule data when Edit is clicked", async () => {
    setupLoad([schedule({ name: "Nightly Backup", cron_expr: "0 2 * * *" })]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /edit schedule/i }));
    await flush();
    expect((screen.getByLabelText("Schedule name") as HTMLInputElement).value).toBe("Nightly Backup");
    expect((screen.getByLabelText("Cron Expression") as HTMLInputElement).value).toBe("0 2 * * *");
  });

  it("shows 'Update' button in edit mode", async () => {
    setupLoad([schedule()]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /edit schedule/i }));
    await flush();
    expect(screen.getByRole("button", { name: /update/i })).toBeTruthy();
  });

  it("PATCHes correct payload when updating a schedule", async () => {
    setupLoad([schedule()]);
    mockPatch.mockResolvedValue({});
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /edit schedule/i }));
    await flush();
    typeIn(screen.getByLabelText("Schedule name") as HTMLElement, "Updated Name");
    typeIn(screen.getByLabelText("Prompt / Task") as HTMLElement, "New prompt");
    await flush();
    act(() => { screen.getByRole("button", { name: /update/i }).click(); });
    await flush();
    await waitFor(() => {
      expect(screen.queryByRole("button", { name: /cancel/i })).not.toBeTruthy();
    });
    expect(mockPatch).toHaveBeenCalledWith(
      "/workspaces/ws-1/schedules/sch-1",
      expect.objectContaining({
        name: "Updated Name",
        cron_expr: "0 9 * * *",
        timezone: "UTC",
        prompt: "New prompt",
        enabled: true,
      }),
    );
  });

  it("form reset clears name, cron, prompt, and enabled", async () => {
    setupLoad([schedule()]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    // Open + add schedule form
    fireEvent.click(screen.getByRole("button", { name: /\+ add schedule/i }));
    await flush();
    typeIn(screen.getByLabelText("Schedule name") as HTMLElement, "Temp Schedule");
    typeIn(screen.getByLabelText("Cron Expression") as HTMLElement, "*/15 * * * *");
    typeIn(screen.getByLabelText("Prompt / Task") as HTMLElement, "Temporary task");
    await flush();
    // Cancel
    act(() => { screen.getByRole("button", { name: /cancel/i }).click(); });
    await flush();
    // Open again — should be reset
    fireEvent.click(screen.getByRole("button", { name: /\+ add schedule/i }));
    await flush();
    expect((screen.getByLabelText("Schedule name") as HTMLInputElement).value).toBe("");
    expect((screen.getByLabelText("Cron Expression") as HTMLInputElement).value).toBe("0 9 * * *");
    expect((screen.getByLabelText("Prompt / Task") as HTMLTextAreaElement).value).toBe("");
  });

  // ── Error state ──────────────────────────────────────────────────────────────

  it("shows error banner when GET fails", async () => {
    mockGet.mockRejectedValue(new Error("network error"));
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    // Component now sets error state on GET failure
    expect(screen.getByText(/network error/i)).toBeTruthy();
  });

  it("shows generic error when GET rejects with non-Error", async () => {
    mockGet.mockRejectedValue("unknown failure");
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("unknown failure")).toBeTruthy();
  });

  // ── Auto-refresh ────────────────────────────────────────────────────────────

  it("sets up auto-refresh interval of 10 seconds", async () => {
    const setIntervalSpy = vi.spyOn(globalThis, "setInterval");
    setupLoad([schedule()]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    expect(setIntervalSpy).toHaveBeenCalledWith(expect.any(Function), 10000);
    setIntervalSpy.mockRestore();
  });

  it("clears the auto-refresh interval on unmount", async () => {
    const clearIntervalSpy = vi.spyOn(globalThis, "clearInterval");
    const setIntervalSpy = vi.spyOn(globalThis, "setInterval");
    setupLoad([schedule()]);
    const { unmount } = render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    expect(clearIntervalSpy).not.toHaveBeenCalled();
    unmount();
    expect(clearIntervalSpy).toHaveBeenCalled();
    setIntervalSpy.mockRestore();
    clearIntervalSpy.mockRestore();
  });

  // ── Misc ────────────────────────────────────────────────────────────────────

  it("shows no timezone suffix when timezone is UTC", async () => {
    setupLoad([schedule({ timezone: "UTC" })]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    expect(screen.queryByText(/\(UTC\)/)).not.toBeTruthy();
  });

  it("shows timezone suffix when non-UTC", async () => {
    setupLoad([schedule({ timezone: "America/New_York" })]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/\(America\/New_York\)/)).toBeTruthy();
  });

  it("checkbox toggles formEnabled state", async () => {
    setupLoad([]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add schedule/i }));
    await flush();
    const checkbox = screen.getByRole("checkbox");
    expect((checkbox as HTMLInputElement).checked).toBe(true);
    fireEvent.click(checkbox);
    await flush();
    expect((checkbox as HTMLInputElement).checked).toBe(false);
  });

  it("timezone select updates formTimezone", async () => {
    setupLoad([]);
    render(<ScheduleTab workspaceId="ws-1" />);
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /\+ add schedule/i }));
    await flush();
    fireEvent.change(screen.getByLabelText("Timezone"), { target: { value: "America/Los_Angeles" } });
    await flush();
    expect((screen.getByLabelText("Timezone") as HTMLSelectElement).value).toBe("America/Los_Angeles");
  });
});
