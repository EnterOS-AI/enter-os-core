// @vitest-environment jsdom
/**
 * Tests for canvas/src/lib/hydrate.ts — exponential-backoff canvas store hydration.
 *
 * 7 cases:
 *   1. Success on first attempt → { error: null }
 *   2. Viewport fetch fails (non-fatal) → store still hydrates, returns { error: null }
 *   3. Success after 1 retry → onRetrying(1) called once, final result { error: null }
 *   4. Success after 2 retries → onRetrying called for each failed attempt
 *   5. All attempts fail → returns the error message after MAX_RETRIES
 *   6. onRetrying called with correct attempt number on each retry
 *   7. Exponential backoff delays: 1s, 2s, 4s for attempts 1, 2, 3
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "@/lib/api";
import { useCanvasStore } from "@/store/canvas";
import { hydrateCanvas, MAX_RETRIES } from "../hydrate";

// ─── Mock api ──────────────────────────────────────────────────────────────────
// PLATFORM_URL must be a named export — hydrate.ts imports it directly, not via api.
vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn<(path: string) => Promise<unknown>>(),
  },
  PLATFORM_URL: "http://localhost:8080",
}));

// ─── Mock store ────────────────────────────────────────────────────────────────

const mockHydrate = vi.fn();
const mockSetViewport = vi.fn();

vi.mock("@/store/canvas", () => ({
  useCanvasStore: {
    getState: () => ({
      hydrate: mockHydrate,
      setViewport: mockSetViewport,
    }),
  },
}));

// ─── Helpers ───────────────────────────────────────────────────────────────────

const mockApiGet = vi.mocked(api.get);

function makeWorkspace(id = "ws-1") {
  return {
    id,
    name: "Test WS",
    role: "assistant",
    tier: 1,
    status: "online" as const,
    agent_card: null,
    url: "http://localhost:9000",
    parent_id: null,
    active_tasks: 0,
    last_error_rate: 0,
    last_sample_error: "",
    uptime_seconds: 60,
    current_task: "",
    x: 0,
    y: 0,
    collapsed: false,
    runtime: "",
    budget_limit: null,
  };
}

// ─── Setup / teardown ──────────────────────────────────────────────────────────

beforeEach(() => {
  vi.clearAllMocks();
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

// ─── Tests ─────────────────────────────────────────────────────────────────────

describe("hydrateCanvas — success paths", () => {
  it("returns { error: null } on first-attempt success", async () => {
    mockApiGet
      .mockResolvedValueOnce([makeWorkspace()])           // /workspaces
      .mockResolvedValueOnce({ x: 0, y: 0, zoom: 1 }); // /canvas/viewport

    const result = await hydrateCanvas();

    expect(result).toEqual({ error: null });
    expect(mockHydrate).toHaveBeenCalledOnce();
    expect(mockSetViewport).toHaveBeenCalledWith({ x: 0, y: 0, zoom: 1 });
  });

  it("viewport fetch failure is non-fatal — store still hydrates", async () => {
    mockApiGet
      .mockResolvedValueOnce([makeWorkspace()])                            // /workspaces OK
      .mockRejectedValueOnce(new Error("viewport down"));                   // /canvas/viewport fails

    const result = await hydrateCanvas();

    expect(result).toEqual({ error: null });
    expect(mockHydrate).toHaveBeenCalledOnce();
    expect(mockSetViewport).not.toHaveBeenCalled();
  });

  it("returns { error: null } after 1 retry", async () => {
    const onRetrying = vi.fn();

    // Each attempt makes 2 parallel api.get calls (workspaces + viewport).
    // Attempt 1 (fails):  /workspaces → rejected, /viewport → resolved
    // Attempt 2 (succeeds): /workspaces → resolved, /viewport → resolved
    mockApiGet
      .mockRejectedValueOnce(new Error("network down"))     // attempt 1: /workspaces
      .mockResolvedValueOnce({ x: 0, y: 0, zoom: 1 })     // attempt 1: /viewport
      .mockResolvedValueOnce([makeWorkspace()])            // attempt 2: /workspaces
      .mockResolvedValueOnce({ x: 0, y: 0, zoom: 1 });   // attempt 2: /viewport

    const promise = hydrateCanvas(onRetrying);

    // Advance past the first backoff delay (1000 * 2^0 = 1000 ms)
    await vi.advanceTimersByTimeAsync(1000);
    await vi.runAllTimersAsync();

    const result = await promise;

    expect(result).toEqual({ error: null });
    expect(onRetrying).toHaveBeenCalledTimes(1);
    expect(onRetrying).toHaveBeenCalledWith(1);
  });

  it("onRetrying called once per failed attempt before next retry", async () => {
    const onRetrying = vi.fn();

    // Attempt 1: both calls fail
    // Attempt 2: both calls fail
    // Attempt 3: both calls succeed → hydrate succeeds
    mockApiGet
      .mockRejectedValueOnce(new Error("attempt 1"))     // a1: /workspaces
      .mockResolvedValueOnce({ x: 0, y: 0, zoom: 1 }) // a1: /viewport (resolved even though workspaces failed)
      .mockRejectedValueOnce(new Error("attempt 2"))     // a2: /workspaces
      .mockResolvedValueOnce({ x: 0, y: 0, zoom: 1 }) // a2: /viewport
      .mockResolvedValueOnce([makeWorkspace()])           // a3: /workspaces
      .mockResolvedValueOnce({ x: 0, y: 0, zoom: 1 }); // a3: /viewport

    const promise = hydrateCanvas(onRetrying);
    await vi.runAllTimersAsync();

    const result = await promise;

    expect(result).toEqual({ error: null });
    expect(onRetrying).toHaveBeenCalledTimes(2);
    expect(onRetrying).toHaveBeenNthCalledWith(1, 1);
    expect(onRetrying).toHaveBeenNthCalledWith(2, 2);
  });
});

describe("hydrateCanvas — failure paths", () => {
  it("returns error message after all MAX_RETRIES attempts exhausted", async () => {
    for (let i = 0; i < MAX_RETRIES; i++) {
      mockApiGet.mockRejectedValueOnce(new Error(`attempt ${i + 1} failed`));
    }

    const promise = hydrateCanvas();
    await vi.runAllTimersAsync();
    const result = await promise;

    expect(result.error).not.toBeNull();
    expect(result.error).toContain("Unable to connect to platform");
    expect(mockHydrate).not.toHaveBeenCalled();
  });

  it("onRetrying called MAX_RETRIES-1 times before final exhausted attempt", async () => {
    const onRetrying = vi.fn();

    for (let i = 0; i < MAX_RETRIES; i++) {
      mockApiGet.mockRejectedValueOnce(new Error(`attempt ${i + 1}`));
    }

    const promise = hydrateCanvas(onRetrying);
    await vi.runAllTimersAsync();
    await promise;

    // onRetrying is called after each failed attempt, before the next attempt.
    // With MAX_RETRIES=3: called after attempt 1 (→2) and after attempt 2 (→3).
    expect(onRetrying).toHaveBeenCalledTimes(MAX_RETRIES - 1);
  });
});

describe("hydrateCanvas — exponential backoff timing", () => {
  it("total elapsed time equals sum of exponential delays 1s + 2s + 4s", async () => {
    const onRetrying = vi.fn();

    for (let i = 0; i < MAX_RETRIES; i++) {
      mockApiGet.mockRejectedValueOnce(new Error(`attempt ${i + 1}`));
    }

    const start = Date.now();
    const promise = hydrateCanvas(onRetrying);

    // Advance all timers at once and let fake timers resolve everything
    await vi.runAllTimersAsync();
    await promise;

    const elapsed = Date.now() - start;

    // Total expected: 1000 (delay1) + 2000 (delay2) = 3000 ms
    // (no delay after the final attempt 3 — function returns immediately)
    expect(elapsed).toBeGreaterThanOrEqual(2999);
    expect(elapsed).toBeLessThan(5000); // sanity cap
    expect(onRetrying).toHaveBeenCalledTimes(MAX_RETRIES - 1);
  });
});
