// @vitest-environment jsdom
/**
 * Tests for TracesTab — Langfuse trace viewer.
 *
 * Coverage:
 *   - Loading state
 *   - Error state
 *   - Empty state (no traces)
 *   - Trace list rendering
 *   - Expand/collapse rows with aria attributes
 *   - Status dot colors (ERROR vs success)
 *   - Latency formatting (ms vs seconds)
 *   - Token count display
 *   - Cost display
 *   - Input/output rendering (string and object)
 *   - Refresh button
 *   - formatTime relative timestamps
 *   - "How to enable tracing" collapsed hint
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { TracesTab } from "../TracesTab";

const mockGet = vi.hoisted(() => vi.fn<[], Promise<unknown>>());

vi.mock("@/lib/api", () => ({
  api: { get: mockGet },
}));

// ─── Fixtures ─────────────────────────────────────────────────────────────────

const TRACE_FIXTURE = {
  id: "trace-abc123",
  name: "security-scan",
  timestamp: new Date(Date.now() - 60000).toISOString(),
  latency: 450,
  input: { query: "scan for vulnerabilities" },
  output: { result: "No issues found" },
  status: "success",
  totalCost: 0.00234,
  usage: { input: 120, output: 85, total: 205 },
};

function trace(overrides: Partial<typeof TRACE_FIXTURE> = {}): typeof TRACE_FIXTURE {
  return { ...TRACE_FIXTURE, ...overrides };
}

// ─── Helpers ───────────────────────────────────────────────────────────────────

async function flush() {
  await act(async () => { await Promise.resolve(); });
}

// The trace row button's accessible name is "{name} {relativeTime} {latency}{tokCount}".
// Filter all buttons to find the trace row buttons.
function getTraceButtons() {
  return screen
    .getAllByRole("button")
    .filter((b) => b.getAttribute("aria-controls")?.startsWith("trace-detail-"));
}

// ─── Tests ─────────────────────────────────────────────────────────────────────

describe("TracesTab", () => {
  beforeEach(() => {
    mockGet.mockReset();
    vi.useRealTimers();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  // ── Loading ─────────────────────────────────────────────────────────────────

  it("shows loading state when traces are being fetched", async () => {
    mockGet.mockImplementation(() => new Promise(() => {}));
    render(<TracesTab workspaceId="ws-1" />);
    await act(async () => { /* flush initial render */ });
    expect(screen.getByText("Loading traces...")).toBeTruthy();
  });

  // ── Error ──────────────────────────────────────────────────────────────────

  it("shows error banner when GET /traces rejects", async () => {
    mockGet.mockRejectedValue(new Error("gateway timeout"));
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/gateway timeout/i)).toBeTruthy();
  });

  it("shows 'Failed to load traces' when GET rejects with non-Error", async () => {
    mockGet.mockRejectedValue("unknown");
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/Failed to load traces/i)).toBeTruthy();
  });

  // ── Empty state ───────────────────────────────────────────────────────────

  it("shows empty state when API returns empty list", async () => {
    mockGet.mockResolvedValue({ data: [] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("No traces yet")).toBeTruthy();
  });

  it("shows 'How to enable tracing' hint under empty state", async () => {
    mockGet.mockResolvedValue({ data: [] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/how to enable tracing/i)).toBeTruthy();
    expect(screen.getByText(/LANGFUSE_HOST/i)).toBeTruthy();
  });

  it("hides empty state when error is present", async () => {
    mockGet.mockRejectedValue(new Error("error"));
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.queryByText("No traces yet")).toBeFalsy();
  });

  // ── Trace list ─────────────────────────────────────────────────────────────

  it("renders trace name in the list", async () => {
    mockGet.mockResolvedValue({ data: [trace({ name: "my-trace" })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("my-trace")).toBeTruthy();
  });

  it("shows trace count in header", async () => {
    mockGet.mockResolvedValue({
      data: [
        trace({ id: "t1" }),
        trace({ id: "t2" }),
        trace({ id: "t3" }),
      ],
    });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("3 traces")).toBeTruthy();
  });

  it("renders multiple traces", async () => {
    mockGet.mockResolvedValue({
      data: [
        trace({ id: "t1", name: "trace-alpha" }),
        trace({ id: "t2", name: "trace-beta" }),
      ],
    });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("trace-alpha")).toBeTruthy();
    expect(screen.getByText("trace-beta")).toBeTruthy();
  });

  it("shows 'trace' when name is empty", async () => {
    mockGet.mockResolvedValue({ data: [trace({ name: "" })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("trace")).toBeTruthy();
  });

  // ── Status dot ─────────────────────────────────────────────────────────────

  it("applies bg-bad to ERROR traces", async () => {
    mockGet.mockResolvedValue({ data: [trace({ status: "ERROR" })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    const dot = getTraceButtons()[0].querySelector("div[class*='rounded-full']");
    expect(dot?.className).toContain("bg-bad");
  });

  it("applies bg-good to success traces", async () => {
    mockGet.mockResolvedValue({ data: [trace({ status: "success" })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    const dot = getTraceButtons()[0].querySelector("div[class*='rounded-full']");
    expect(dot?.className).toContain("bg-good");
  });

  // ── Latency formatting ──────────────────────────────────────────────────────

  it("shows latency in milliseconds when < 1000ms", async () => {
    mockGet.mockResolvedValue({ data: [trace({ latency: 450 })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("450ms")).toBeTruthy();
  });

  it("shows latency in seconds when >= 1000ms", async () => {
    mockGet.mockResolvedValue({ data: [trace({ latency: 2500 })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("2.5s")).toBeTruthy();
  });

  it("hides latency when null", async () => {
    mockGet.mockResolvedValue({ data: [trace({ latency: undefined })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.queryByText(/ms/)).toBeFalsy();
  });

  // ── Token count ────────────────────────────────────────────────────────────

  it("shows total token count from usage.total", async () => {
    mockGet.mockResolvedValue({ data: [trace({ usage: { input: 100, output: 50, total: 150 } })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("150 tok")).toBeTruthy();
  });

  it("hides token count when usage is undefined", async () => {
    mockGet.mockResolvedValue({ data: [trace({ usage: undefined })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.queryByText(/tok/)).toBeFalsy();
  });

  // ── Expand/collapse ─────────────────────────────────────────────────────────

  it("shows '▶' when trace is collapsed", async () => {
    mockGet.mockResolvedValue({ data: [trace()] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText("▶")).toBeTruthy();
  });

  it("shows '▼' when trace is expanded", async () => {
    mockGet.mockResolvedValue({ data: [trace()] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    act(() => { getTraceButtons()[0].click(); });
    await flush();
    expect(screen.getByText("▼")).toBeTruthy();
  });

  it("shows '▼' when all traces are collapsed", async () => {
    mockGet.mockResolvedValue({ data: [trace()] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.queryByText("▼")).toBeFalsy();
    expect(screen.getByText("▶")).toBeTruthy();
  });

  it("shows input/output panel when trace is expanded", async () => {
    mockGet.mockResolvedValue({ data: [trace()] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    act(() => { getTraceButtons()[0].click(); });
    await flush();
    expect(screen.getByText(/INPUT/i)).toBeTruthy();
    expect(screen.getByText(/OUTPUT/i)).toBeTruthy();
  });

  it("shows JSON stringified input when input is an object", async () => {
    mockGet.mockResolvedValue({ data: [trace({ input: { query: "test" } })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    act(() => { getTraceButtons()[0].click(); });
    await flush();
    expect(screen.getByText(/"query": "test"/)).toBeTruthy();
  });

  it("shows raw string when input is a string", async () => {
    mockGet.mockResolvedValue({ data: [trace({ input: "plain text input" })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    act(() => { getTraceButtons()[0].click(); });
    await flush();
    expect(screen.getByText("plain text input")).toBeTruthy();
  });

  it("shows trace ID in expanded panel", async () => {
    mockGet.mockResolvedValue({ data: [trace({ id: "trace-xyz-999" })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    act(() => { getTraceButtons()[0].click(); });
    await flush();
    expect(screen.getByText("trace-xyz-999")).toBeTruthy();
  });

  it("shows cost when totalCost is present", async () => {
    mockGet.mockResolvedValue({ data: [trace({ totalCost: 0.001234 })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    act(() => { getTraceButtons()[0].click(); });
    await flush();
    expect(screen.getByText(/\$0.001234/)).toBeTruthy();
  });

  it("hides cost section when totalCost is null", async () => {
    mockGet.mockResolvedValue({ data: [trace({ totalCost: undefined })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    act(() => { getTraceButtons()[0].click(); });
    await flush();
    expect(screen.queryByText(/cost/i)).toBeFalsy();
  });

  it("has aria-expanded=true on expanded row", async () => {
    mockGet.mockResolvedValue({ data: [trace()] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    const btn = getTraceButtons()[0];
    expect(btn.getAttribute("aria-expanded")).toBe("false");
    act(() => { btn.click(); });
    await flush();
    expect(btn.getAttribute("aria-expanded")).toBe("true");
  });

  it("has aria-expanded=false on collapsed row", async () => {
    mockGet.mockResolvedValue({ data: [trace()] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(getTraceButtons()[0].getAttribute("aria-expanded")).toBe("false");
  });

  it("has aria-controls linking row to its detail panel", async () => {
    mockGet.mockResolvedValue({ data: [trace({ id: "trace-abc123" })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(getTraceButtons()[0].getAttribute("aria-controls")).toBe("trace-detail-trace-abc123");
  });

  // ── Refresh ────────────────────────────────────────────────────────────────

  it("Refresh button triggers a new GET", async () => {
    mockGet.mockResolvedValue({ data: [trace()] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    mockGet.mockClear();
    fireEvent.click(screen.getByRole("button", { name: /refresh/i }));
    await flush();
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-1/traces");
  });

  // ── formatTime ─────────────────────────────────────────────────────────────

  it("shows 'Xs ago' for traces under 1 minute", async () => {
    const timestamp = new Date(Date.now() - 30_000).toISOString();
    mockGet.mockResolvedValue({ data: [trace({ timestamp, id: "t-30s" })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    // 30s ago
    expect(screen.getByText(/\d+s ago/)).toBeTruthy();
  });

  it("shows 'Xm ago' for traces under 1 hour", async () => {
    const timestamp = new Date(Date.now() - 120_000).toISOString();
    mockGet.mockResolvedValue({ data: [trace({ timestamp, id: "t-2m" })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/\dm ago/)).toBeTruthy();
  });

  it("shows 'Xh ago' for traces under 1 day", async () => {
    const timestamp = new Date(Date.now() - 3_600_000).toISOString();
    mockGet.mockResolvedValue({ data: [trace({ timestamp, id: "t-1h" })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(/\dh ago/)).toBeTruthy();
  });

  it("shows locale date for traces older than 24 hours", async () => {
    const oldDate = new Date(Date.now() - 172_800_000);
    mockGet.mockResolvedValue({ data: [trace({ timestamp: oldDate.toISOString(), id: "t-old" })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    expect(screen.getByText(oldDate.toLocaleDateString())).toBeTruthy();
  });

  // ── Edge cases ─────────────────────────────────────────────────────────────

  it("handles traces with no input or output", async () => {
    mockGet.mockResolvedValue({ data: [trace({ input: undefined, output: undefined })] });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    act(() => { getTraceButtons()[0].click(); });
    await flush();
    expect(screen.queryByText(/INPUT/i)).toBeFalsy();
    expect(screen.queryByText(/OUTPUT/i)).toBeFalsy();
  });

  it("shows only one expanded trace at a time", async () => {
    mockGet.mockResolvedValue({
      data: [
        trace({ id: "t1", name: "Alpha" }),
        trace({ id: "t2", name: "Beta" }),
      ],
    });
    render(<TracesTab workspaceId="ws-1" />);
    await flush();
    const [btn1, btn2] = getTraceButtons();
    act(() => { btn1.click(); });
    await flush();
    expect(btn1.getAttribute("aria-expanded")).toBe("true");
    expect(btn2.getAttribute("aria-expanded")).toBe("false");
    act(() => { btn2.click(); });
    await flush();
    expect(btn1.getAttribute("aria-expanded")).toBe("false");
    expect(btn2.getAttribute("aria-expanded")).toBe("true");
  });
});
