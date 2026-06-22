import { describe, it, expect, vi, beforeEach, afterAll } from "vitest";

// ---------------------------------------------------------------------------
// Tests for the hydrateCanvas() function in src/lib/hydrate.ts.
// These tests import and exercise the REAL function, not a reimplementation.
// ---------------------------------------------------------------------------

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

// Mock canvas store — hydrateCanvas calls useCanvasStore.getState()
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

// Import the REAL function under test
import { hydrateCanvas } from "@/lib/hydrate";
import { PLATFORM_URL } from "@/lib/api";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function mockSuccess(body: unknown) {
  mockFetch.mockResolvedValueOnce({
    ok: true,
    json: () => Promise.resolve(body),
    text: () => Promise.resolve(JSON.stringify(body)),
  } as unknown as Response);
}

function mockNetworkError(message = "ECONNREFUSED") {
  mockFetch.mockRejectedValueOnce(new Error(message));
}

// Speed up tests by mocking setTimeout
vi.useFakeTimers();

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

beforeEach(() => {
  vi.clearAllMocks();
});

afterAll(() => {
  vi.useRealTimers();
});

// ---------------------------------------------------------------------------
// Tests — success path
// ---------------------------------------------------------------------------

describe("hydrateCanvas — success path", () => {
  it("returns no error and hydrates store when both fetches succeed", async () => {
    mockSuccess([{ id: "ws-1" }]);
    mockSuccess({ x: 0, y: 0, zoom: 1 });

    const result = await hydrateCanvas();

    expect(result.error).toBeNull();
    expect(mockHydrate).toHaveBeenCalledWith([{ id: "ws-1" }]);
    expect(mockSetViewport).toHaveBeenCalledWith({ x: 0, y: 0, zoom: 1 });
  });

  it("succeeds when viewport fetch fails (viewport is optional)", async () => {
    mockSuccess([{ id: "ws-1" }]);
    mockNetworkError("viewport not found");

    const result = await hydrateCanvas();

    expect(result.error).toBeNull();
    expect(mockHydrate).toHaveBeenCalledWith([{ id: "ws-1" }]);
    expect(mockSetViewport).not.toHaveBeenCalled();
  });

  it("skips setViewport when viewport is null", async () => {
    mockSuccess([{ id: "ws-1" }]);
    mockSuccess(null);

    const result = await hydrateCanvas();

    expect(result.error).toBeNull();
    expect(mockHydrate).toHaveBeenCalledWith([{ id: "ws-1" }]);
    expect(mockSetViewport).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// Tests — error & retry path
// ---------------------------------------------------------------------------

describe("hydrateCanvas — error and retry", () => {
  it("returns error message after all 3 retries are exhausted", async () => {
    // All 3 attempts fail
    mockNetworkError("fail 1");
    mockNetworkError("fail 2");
    mockNetworkError("fail 3");

    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    const promise = hydrateCanvas();

    // Advance through the two retry delays (1s, 2s)
    await vi.advanceTimersByTimeAsync(1000);
    await vi.advanceTimersByTimeAsync(2000);

    const result = await promise;
    spy.mockRestore();

    expect(result.error).toBe(
      `Unable to connect to platform at ${PLATFORM_URL}. Check that the platform is running.`
    );
    expect(mockHydrate).not.toHaveBeenCalled();
  });

  it("error message includes the PLATFORM_URL", async () => {
    mockNetworkError("err1");
    mockNetworkError("err2");
    mockNetworkError("err3");

    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    const promise = hydrateCanvas();
    await vi.advanceTimersByTimeAsync(1000);
    await vi.advanceTimersByTimeAsync(2000);
    const result = await promise;
    spy.mockRestore();

    expect(result.error).toContain(PLATFORM_URL);
    expect(result.error).toContain("Check that the platform is running");
  });

  it("succeeds on retry (fails first, succeeds on attempt 2)", async () => {
    // Attempt 1: workspaces fails, viewport also consumed by Promise.all
    mockNetworkError("offline");
    mockSuccess(null); // viewport mock for attempt 1 (consumed but irrelevant)
    // Attempt 2: both succeed
    mockSuccess([{ id: "ws-2" }]);
    mockSuccess({ x: 5, y: 5, zoom: 2 });

    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    const promise = hydrateCanvas();

    // Advance past the first retry delay (1s)
    await vi.advanceTimersByTimeAsync(1000);

    const result = await promise;
    spy.mockRestore();

    expect(result.error).toBeNull();
    expect(mockHydrate).toHaveBeenCalledWith([{ id: "ws-2" }]);
    expect(mockSetViewport).toHaveBeenCalledWith({ x: 5, y: 5, zoom: 2 });
  });

  it("calls onRetrying callback before each retry", async () => {
    mockNetworkError("fail 1");
    mockNetworkError("fail 2");
    mockNetworkError("fail 3");

    const onRetrying = vi.fn();
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    const promise = hydrateCanvas(onRetrying);

    await vi.advanceTimersByTimeAsync(1000);
    await vi.advanceTimersByTimeAsync(2000);

    await promise;
    spy.mockRestore();

    // onRetrying should be called before attempt 2 and attempt 3
    expect(onRetrying).toHaveBeenCalledTimes(2);
    expect(onRetrying).toHaveBeenCalledWith(1);
    expect(onRetrying).toHaveBeenCalledWith(2);
  });

  it("logs to console.error on each failed attempt", async () => {
    mockNetworkError("err1");
    mockNetworkError("err2");
    mockNetworkError("err3");

    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    const promise = hydrateCanvas();
    await vi.advanceTimersByTimeAsync(1000);
    await vi.advanceTimersByTimeAsync(2000);
    await promise;

    // 3 failed attempts = 3 console.error calls
    expect(spy).toHaveBeenCalledTimes(3);
    expect(spy).toHaveBeenCalledWith("Initial hydration failed:", expect.any(Error));
    spy.mockRestore();
  });

  it("manual retry: calling hydrateCanvas again after failure succeeds", async () => {
    // All 3 retries fail
    mockNetworkError("f1");
    mockNetworkError("f2");
    mockNetworkError("f3");

    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    const promise1 = hydrateCanvas();
    await vi.advanceTimersByTimeAsync(1000);
    await vi.advanceTimersByTimeAsync(2000);
    const firstResult = await promise1;
    expect(firstResult.error).not.toBeNull();

    // Manual retry — succeeds on first attempt
    mockSuccess([]);
    mockSuccess(null);
    const retryResult = await hydrateCanvas();
    spy.mockRestore();

    expect(retryResult.error).toBeNull();
    expect(mockHydrate).toHaveBeenCalledWith([]);
  });
});
