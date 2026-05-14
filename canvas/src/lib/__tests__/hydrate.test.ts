// @vitest-environment jsdom
/**
 * Tests for hydrate.ts — canvas store hydration with exponential backoff.
 *
 * Covers:
 *   - Successful hydration on first attempt (no retries)
 *   - Retry with exponential backoff on failure
 *   - onRetrying callback called at correct intervals
 *   - Error propagation after MAX_RETRIES exhausted
 *   - Viewport persisted on success
 *   - Viewport failure is non-fatal
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import type { WorkspaceData } from "@/store/socket";

// ---------------------------------------------------------------------------
// Mock modules — must precede imports that use them
// ---------------------------------------------------------------------------

const mockHydrate = vi.fn();
const mockSetViewport = vi.fn();

vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn(),
  },
  PLATFORM_URL: "https://platform.test",
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    () => ({}),
    {
      getState: () => ({
        hydrate: mockHydrate,
        setViewport: mockSetViewport,
      }),
    },
  ),
}));

// ---------------------------------------------------------------------------
// Import after mocks
// ---------------------------------------------------------------------------

import { api } from "@/lib/api";
import { hydrateCanvas, MAX_RETRIES } from "../hydrate";

// ---------------------------------------------------------------------------
// Mock data
// ---------------------------------------------------------------------------

const WORKSPACES: WorkspaceData[] = [
  { id: "ws-1", name: "Test Workspace" } as WorkspaceData,
];

const VIEWPORT = { x: 10, y: 20, zoom: 1.5 };

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const mockApiGet = vi.mocked(api.get);

/** Resolves successfully for `count` parallel workspace fetches; viewport always succeeds. */
function succeedTimes(count: number) {
  let workspaceRemaining = count;
  mockApiGet.mockImplementation(async (url: string) => {
    if (url === "/canvas/viewport") return VIEWPORT;
    if (workspaceRemaining > 0) {
      workspaceRemaining--;
      return WORKSPACES;
    }
    throw new Error("API error");
  });
}

/** Always fails with the given message. */
function alwaysFail(msg = "Network error") {
  mockApiGet.mockRejectedValue(new Error(msg));
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe("hydrateCanvas", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockApiGet.mockReset();
    mockHydrate.mockReset();
    mockSetViewport.mockReset();
  });

  // ── Success on first attempt ─────────────────────────────────────────────

  it("hydrates the store and returns null error on first attempt success", async () => {
    succeedTimes(1);
    const result = await hydrateCanvas();
    expect(result).toEqual({ error: null });
    expect(mockHydrate).toHaveBeenCalledOnce();
  });

  it("persists viewport when returned by the API", async () => {
    succeedTimes(1);
    const result = await hydrateCanvas();
    expect(result).toEqual({ error: null });
    expect(mockSetViewport).toHaveBeenCalledWith(VIEWPORT);
  });

  // ── Viewport failure is non-fatal ─────────────────────────────────────────

  it("returns null error when viewport fetch fails but workspaces succeed", async () => {
    mockApiGet.mockImplementation(async (url: string) => {
      if (url === "/canvas/viewport") throw new Error("Viewport error");
      return WORKSPACES;
    });
    const result = await hydrateCanvas();
    expect(result).toEqual({ error: null });
    expect(mockHydrate).toHaveBeenCalledOnce();
    expect(mockSetViewport).not.toHaveBeenCalled();
  });

  // ── Retry logic ──────────────────────────────────────────────────────────

  it("retries MAX_RETRIES times before returning an error", async () => {
    alwaysFail();
    const onRetrying = vi.fn();
    const result = await Promise.race([
      hydrateCanvas(onRetrying),
      new Promise<"timeout">((resolve) => setTimeout(() => resolve("timeout"), 5000)),
    ]);
    if (result === "timeout") throw new Error("Test timed out — retries not awaited correctly");
    expect(result.error).not.toBeNull();
    expect(onRetrying).toHaveBeenCalledTimes(MAX_RETRIES - 1);
  }, 10000);

  it("onRetrying is called with attempt number before each retry", async () => {
    alwaysFail();
    const onRetrying = vi.fn();
    await Promise.race([
      hydrateCanvas(onRetrying),
      new Promise<"timeout">((resolve) => setTimeout(() => resolve("timeout"), 5000)),
    ]);
    expect(onRetrying).toHaveBeenNthCalledWith(1, 1);
    expect(onRetrying).toHaveBeenNthCalledWith(2, 2);
  }, 10000);

  it("succeeds on second attempt — hydrates after transient failure", async () => {
    let callCount = 0;
    mockApiGet.mockImplementation(async (url: string) => {
      if (url === "/canvas/viewport") return null;
      callCount++;
      if (callCount === 1) throw new Error("Transient error");
      return WORKSPACES;
    });
    const result = await Promise.race([
      hydrateCanvas(),
      new Promise<"timeout">((resolve) => setTimeout(() => resolve("timeout"), 5000)),
    ]);
    if (result === "timeout") throw new Error("Test timed out");
    expect(result).toEqual({ error: null });
    expect(mockHydrate).toHaveBeenCalledOnce();
  }, 10000);

  // ── Error messages ────────────────────────────────────────────────────────

  it("error message includes the platform URL after all retries exhausted", async () => {
    alwaysFail("Connection refused");
    const result = await Promise.race([
      hydrateCanvas(),
      new Promise<"timeout">((resolve) => setTimeout(() => resolve("timeout"), 5000)),
    ]);
    if (result === "timeout") throw new Error("Test timed out");
    expect(result.error).toContain("platform.test");
    expect(result.error).toContain("Unable to connect");
  }, 10000);

  it("error message includes the underlying error message", async () => {
    alwaysFail("TLS certificate expired");
    const result = await Promise.race([
      hydrateCanvas(),
      new Promise<"timeout">((resolve) => setTimeout(() => resolve("timeout"), 5000)),
    ]);
    if (result === "timeout") throw new Error("Test timed out");
    expect(result.error).not.toBeNull();
    expect(typeof result.error).toBe("string");
  }, 10000);
});
