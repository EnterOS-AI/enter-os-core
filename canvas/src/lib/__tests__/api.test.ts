import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";

// ---------------------------------------------------------------------------
// Mock fetch globally BEFORE importing api.ts so the module picks it up
// ---------------------------------------------------------------------------

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

import { api, PlatformUnavailableError } from "../api";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const DEFAULT_PLATFORM_URL = "http://localhost:8080";

function mockSuccess(body: unknown, status = 200) {
  mockFetch.mockResolvedValueOnce({
    ok: true,
    status,
    json: () => Promise.resolve(body),
    text: () => Promise.resolve(JSON.stringify(body)),
  } as unknown as Response);
}

function mockFailure(status: number, text = "Internal Server Error") {
  mockFetch.mockResolvedValueOnce({
    ok: false,
    status,
    json: () => Promise.reject(new Error("no json")),
    text: () => Promise.resolve(text),
  } as unknown as Response);
}

function mockNetworkError(message = "Failed to fetch") {
  mockFetch.mockRejectedValueOnce(new Error(message));
}

// ---------------------------------------------------------------------------
// Setup / teardown
// ---------------------------------------------------------------------------

beforeEach(() => {
  vi.clearAllMocks();
});

afterEach(() => {
  vi.clearAllMocks();
});

// ---------------------------------------------------------------------------
// GET
// ---------------------------------------------------------------------------

describe("api.get", () => {
  it("calls fetch with GET method and the correct URL", async () => {
    mockSuccess({ id: 1 });
    await api.get("/workspaces");

    expect(mockFetch).toHaveBeenCalledOnce();
    const [url, options] = mockFetch.mock.calls[0];
    expect(url).toBe(`${DEFAULT_PLATFORM_URL}/workspaces`);
    expect(options.method).toBe("GET");
  });

  it("sends Content-Type: application/json header", async () => {
    mockSuccess({});
    await api.get("/test");

    const [, options] = mockFetch.mock.calls[0];
    expect(options.headers).toMatchObject({ "Content-Type": "application/json" });
  });

  it("does not include a body", async () => {
    mockSuccess({});
    await api.get("/test");

    const [, options] = mockFetch.mock.calls[0];
    expect(options.body).toBeUndefined();
  });

  it("returns parsed JSON response", async () => {
    const payload = [{ id: "ws-1", name: "Alpha" }];
    mockSuccess(payload);
    const result = await api.get("/workspaces");
    expect(result).toEqual(payload);
  });

  it("throws on non-ok response", async () => {
    mockFailure(404, "Not Found");
    await expect(api.get("/missing")).rejects.toThrow("404");
  });

  it("includes path and method in error message", async () => {
    mockFailure(500, "boom");
    await expect(api.get("/broken")).rejects.toThrow("GET /broken");
  });
});

// ---------------------------------------------------------------------------
// POST
// ---------------------------------------------------------------------------

describe("api.post", () => {
  it("calls fetch with POST method", async () => {
    mockSuccess({});
    await api.post("/workspaces");

    const [, options] = mockFetch.mock.calls[0];
    expect(options.method).toBe("POST");
  });

  it("serializes body as JSON string", async () => {
    mockSuccess({});
    const body = { name: "New WS", tier: 2 };
    await api.post("/workspaces", body);

    const [, options] = mockFetch.mock.calls[0];
    expect(options.body).toBe(JSON.stringify(body));
  });

  it("sends no body when body argument is omitted", async () => {
    mockSuccess({});
    await api.post("/workspaces/ws-1/restart");

    const [, options] = mockFetch.mock.calls[0];
    expect(options.body).toBeUndefined();
  });

  it("returns parsed JSON response", async () => {
    const created = { id: "ws-new" };
    mockSuccess(created, 201);
    const result = await api.post("/workspaces", { name: "New" });
    expect(result).toEqual(created);
  });

  it("throws on non-ok response", async () => {
    mockFailure(400, "bad request");
    await expect(api.post("/workspaces", {})).rejects.toThrow("400");
  });
});

// ---------------------------------------------------------------------------
// PATCH
// ---------------------------------------------------------------------------

describe("api.patch", () => {
  it("calls fetch with PATCH method", async () => {
    mockSuccess({});
    await api.patch("/workspaces/ws-1", { x: 10, y: 20 });

    const [, options] = mockFetch.mock.calls[0];
    expect(options.method).toBe("PATCH");
  });

  it("serializes body correctly", async () => {
    mockSuccess({});
    const body = { parent_id: "parent-ws" };
    await api.patch("/workspaces/ws-1", body);

    const [, options] = mockFetch.mock.calls[0];
    expect(options.body).toBe(JSON.stringify(body));
  });

  it("sends to the correct URL", async () => {
    mockSuccess({});
    await api.patch("/workspaces/ws-1", {});

    const [url] = mockFetch.mock.calls[0];
    expect(url).toBe(`${DEFAULT_PLATFORM_URL}/workspaces/ws-1`);
  });

  it("throws on non-ok response", async () => {
    mockFailure(403, "Forbidden");
    await expect(api.patch("/workspaces/ws-1", {})).rejects.toThrow("403");
  });
});

// ---------------------------------------------------------------------------
// PUT
// ---------------------------------------------------------------------------

describe("api.put", () => {
  it("calls fetch with PUT method", async () => {
    mockSuccess({});
    await api.put("/canvas/viewport", { x: 0, y: 0, zoom: 1 });

    const [, options] = mockFetch.mock.calls[0];
    expect(options.method).toBe("PUT");
  });

  it("serializes body correctly", async () => {
    mockSuccess({});
    const body = { x: 5, y: 10, zoom: 1.5 };
    await api.put("/canvas/viewport", body);

    const [, options] = mockFetch.mock.calls[0];
    expect(options.body).toBe(JSON.stringify(body));
  });

  it("sends to the correct URL", async () => {
    mockSuccess({});
    await api.put("/canvas/viewport", {});

    const [url] = mockFetch.mock.calls[0];
    expect(url).toBe(`${DEFAULT_PLATFORM_URL}/canvas/viewport`);
  });

  it("returns parsed JSON response", async () => {
    const updated = { ok: true };
    mockSuccess(updated);
    const result = await api.put("/canvas/viewport", {});
    expect(result).toEqual(updated);
  });

  it("throws on non-ok response", async () => {
    mockFailure(500, "Server Error");
    await expect(api.put("/canvas/viewport", {})).rejects.toThrow("500");
  });
});

// ---------------------------------------------------------------------------
// DELETE (del)
// ---------------------------------------------------------------------------

describe("api.del", () => {
  it("calls fetch with DELETE method", async () => {
    mockSuccess({});
    await api.del("/workspaces/ws-1");

    const [, options] = mockFetch.mock.calls[0];
    expect(options.method).toBe("DELETE");
  });

  it("does not include a body", async () => {
    mockSuccess({});
    await api.del("/workspaces/ws-1");

    const [, options] = mockFetch.mock.calls[0];
    expect(options.body).toBeUndefined();
  });

  it("sends to the correct URL", async () => {
    mockSuccess({});
    await api.del("/workspaces/ws-1");

    const [url] = mockFetch.mock.calls[0];
    expect(url).toBe(`${DEFAULT_PLATFORM_URL}/workspaces/ws-1`);
  });

  it("throws on non-ok response", async () => {
    mockFailure(404, "Not Found");
    await expect(api.del("/workspaces/missing")).rejects.toThrow("404");
  });
});

// ---------------------------------------------------------------------------
// Network / fetch-level errors
// ---------------------------------------------------------------------------

describe("api – network errors", () => {
  it("propagates network errors from fetch", async () => {
    mockNetworkError("Network request failed");
    await expect(api.get("/workspaces")).rejects.toThrow("Network request failed");
  });

  it("propagates network errors on POST", async () => {
    mockNetworkError("Failed to connect");
    await expect(api.post("/workspaces", {})).rejects.toThrow("Failed to connect");
  });

  it("propagates network errors on PATCH", async () => {
    mockNetworkError("ECONNREFUSED");
    await expect(api.patch("/workspaces/ws-1", {})).rejects.toThrow("ECONNREFUSED");
  });
});

// ---------------------------------------------------------------------------
// Error message format
// ---------------------------------------------------------------------------

describe("api – error message format", () => {
  it("error message includes HTTP method, path, status and response text", async () => {
    mockFailure(422, "validation failed");
    let errorMessage = "";
    try {
      await api.post("/workspaces", {});
    } catch (e) {
      errorMessage = (e as Error).message;
    }
    expect(errorMessage).toContain("POST");
    expect(errorMessage).toContain("/workspaces");
    expect(errorMessage).toContain("422");
    expect(errorMessage).toContain("validation failed");
  });
});

// ---------------------------------------------------------------------------
// PLATFORM_URL from environment variable (default fallback)
// ---------------------------------------------------------------------------

describe("api – PLATFORM_URL default", () => {
  it("uses http://localhost:8080 as default base URL", async () => {
    mockSuccess({});
    await api.get("/health");

    const [url] = mockFetch.mock.calls[0];
    expect(url).toContain("localhost:8080");
  });
});

// ---------------------------------------------------------------------------
// 15s timeout via AbortSignal.timeout (regression for #982 /
// fix/canvas-api-fetch-timeout). The signal prevents a hung backend from
// leaving the UI spinning forever. These assertions pin the behaviour so
// a future edit can't drop the signal without breaking tests.
// ---------------------------------------------------------------------------

describe("api – request timeout signal", () => {
  it("GET passes an AbortSignal to fetch", async () => {
    mockSuccess({});
    await api.get("/workspaces");
    const [, options] = mockFetch.mock.calls[0];
    expect(options.signal).toBeDefined();
    expect(options.signal).toBeInstanceOf(AbortSignal);
  });

  it("POST passes an AbortSignal to fetch", async () => {
    mockSuccess({});
    await api.post("/workspaces", { name: "x" });
    const [, options] = mockFetch.mock.calls[0];
    expect(options.signal).toBeDefined();
    expect(options.signal).toBeInstanceOf(AbortSignal);
  });

  it("PATCH passes an AbortSignal to fetch", async () => {
    mockSuccess({});
    await api.patch("/workspaces/ws-1", { x: 0 });
    const [, options] = mockFetch.mock.calls[0];
    expect(options.signal).toBeInstanceOf(AbortSignal);
  });

  it("PUT passes an AbortSignal to fetch", async () => {
    mockSuccess({});
    await api.put("/canvas/viewport", { x: 0, y: 0, zoom: 1 });
    const [, options] = mockFetch.mock.calls[0];
    expect(options.signal).toBeInstanceOf(AbortSignal);
  });

  it("DELETE passes an AbortSignal to fetch", async () => {
    mockSuccess({});
    await api.del("/workspaces/ws-1");
    const [, options] = mockFetch.mock.calls[0];
    expect(options.signal).toBeInstanceOf(AbortSignal);
  });

  it("AbortError from timeout is propagated to the caller", async () => {
    // Simulate the browser firing a TimeoutError when AbortSignal.timeout
    // expires — the fetch promise rejects with a DOMException (name=TimeoutError).
    const abortErr =
      typeof DOMException === "function"
        ? new DOMException("signal timed out", "TimeoutError")
        : Object.assign(new Error("signal timed out"), { name: "TimeoutError" });
    mockFetch.mockRejectedValueOnce(abortErr);

    await expect(api.get("/slow")).rejects.toMatchObject({ name: "TimeoutError" });
  });

  it("each request installs its own signal (not a shared module-level controller)", async () => {
    mockSuccess({});
    mockSuccess({});
    await api.get("/a");
    await api.get("/b");
    const sigA = mockFetch.mock.calls[0][1].signal;
    const sigB = mockFetch.mock.calls[1][1].signal;
    // AbortSignal.timeout() returns a fresh signal per call — they must
    // not be the same reference, otherwise one slow request could cancel
    // a subsequent fast request.
    expect(sigA).not.toBe(sigB);
  });
});

// ---------------------------------------------------------------------------
// PlatformUnavailableError classification
// ---------------------------------------------------------------------------
//
// When the platform's wsauth middleware can't reach Postgres/Redis to
// validate a token, it returns 503 + {error, code:"platform_unavailable"}.
// api.ts must surface that as a typed error so the page-level renderer
// can show a dedicated diagnostic instead of a generic 5xx toast.

describe("PlatformUnavailableError classification", () => {
  beforeEach(() => {
    mockFetch.mockReset();
  });

  function mock503Platform(detail = "platform datastore unavailable — retry shortly") {
    const body = JSON.stringify({ error: detail, code: "platform_unavailable" });
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 503,
      json: () => Promise.reject(new Error("not used")),
      text: () => Promise.resolve(body),
    } as unknown as Response);
  }

  it("throws PlatformUnavailableError on 503 + code=platform_unavailable", async () => {
    mock503Platform();
    let thrown: unknown;
    try {
      await api.get("/workspaces");
    } catch (e) {
      thrown = e;
    }
    expect(thrown).toBeInstanceOf(PlatformUnavailableError);
    expect((thrown as PlatformUnavailableError).code).toBe("platform_unavailable");
  });

  it("preserves the server-provided error string as the Error message", async () => {
    mock503Platform("Postgres unreachable");
    try {
      await api.get("/workspaces");
    } catch (e) {
      expect(e).toBeInstanceOf(PlatformUnavailableError);
      expect((e as Error).message).toBe("Postgres unreachable");
      return;
    }
    throw new Error("expected to throw");
  });

  it("does NOT classify a generic 503 (no platform_unavailable code) as PlatformUnavailableError", async () => {
    // Generic upstream-busy 503 — should keep the legacy generic-Error
    // path so existing busy-retry UX isn't disrupted.
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 503,
      json: () => Promise.reject(new Error("not used")),
      text: () => Promise.resolve(JSON.stringify({ error: "upstream busy" })),
    } as unknown as Response);
    try {
      await api.get("/workspaces/x/a2a");
    } catch (e) {
      expect(e).not.toBeInstanceOf(PlatformUnavailableError);
      expect((e as Error).message).toContain("503");
      return;
    }
    throw new Error("expected to throw");
  });

  it("does NOT classify on 500 (server kept legacy 500 for true internal errors)", async () => {
    mockFailure(500, "boom");
    try {
      await api.get("/workspaces");
    } catch (e) {
      expect(e).not.toBeInstanceOf(PlatformUnavailableError);
      return;
    }
    throw new Error("expected to throw");
  });

  it("falls back to generic Error when 503 body isn't JSON", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 503,
      json: () => Promise.reject(new Error("not used")),
      text: () => Promise.resolve("Service Unavailable"),
    } as unknown as Response);
    try {
      await api.get("/workspaces");
    } catch (e) {
      expect(e).not.toBeInstanceOf(PlatformUnavailableError);
      expect((e as Error).message).toContain("503");
      return;
    }
    throw new Error("expected to throw");
  });
});
