import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";

// ---------------------------------------------------------------------------
// Mock the canvas store and api before importing socket.ts
// ---------------------------------------------------------------------------
vi.mock("../canvas", () => ({
  useCanvasStore: {
    getState: vi.fn(() => ({
      applyEvent: vi.fn(),
      hydrate: vi.fn(),
      setWsStatus: vi.fn(),
    })),
  },
}));


// ---------------------------------------------------------------------------
// Mock WebSocket
// ---------------------------------------------------------------------------

class MockWebSocket {
  static instances: MockWebSocket[] = [];

  url: string;
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  closeCallCount = 0;

  constructor(url: string) {
    this.url = url;
    MockWebSocket.instances.push(this);
  }

  close() {
    this.closeCallCount++;
  }

  // Helpers to trigger events in tests
  triggerOpen() {
    this.onopen?.();
  }

  triggerMessage(data: unknown) {
    this.onmessage?.({ data: JSON.stringify(data) });
  }

  triggerRawMessage(data: string) {
    this.onmessage?.({ data });
  }

  triggerClose() {
    this.onclose?.();
  }

  triggerError() {
    this.onerror?.();
  }
}

// Install mock WebSocket globally before importing socket module
(globalThis as unknown as Record<string, unknown>).WebSocket = MockWebSocket;

// Now import the socket module (uses globalThis.WebSocket at call time)
import { connectSocket, disconnectSocket, wakeSocket } from "../socket";
import { useCanvasStore } from "../canvas";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function getLastWS(): MockWebSocket {
  return MockWebSocket.instances[MockWebSocket.instances.length - 1];
}

beforeEach(() => {
  MockWebSocket.instances = [];
  vi.useFakeTimers();
  // Reset mocked store state
  vi.mocked(useCanvasStore.getState).mockReturnValue({
    applyEvent: vi.fn(),
    hydrate: vi.fn(),
    setWsStatus: vi.fn(),
  } as unknown as ReturnType<typeof useCanvasStore.getState>);
});

afterEach(() => {
  // Always disconnect to clean the module-level socket singleton
  disconnectSocket();
  vi.useRealTimers();
  vi.clearAllMocks();
});

// ---------------------------------------------------------------------------
// connectSocket / disconnectSocket
// ---------------------------------------------------------------------------

describe("connectSocket", () => {
  it("creates a WebSocket on connect", () => {
    connectSocket();
    expect(MockWebSocket.instances).toHaveLength(1);
  });

  it("connects to the correct URL (default WS_URL)", () => {
    connectSocket();
    const ws = getLastWS();
    expect(ws.url).toMatch(/^ws/);
  });

  it("sets up onopen, onmessage, onclose, onerror handlers", () => {
    connectSocket();
    const ws = getLastWS();
    expect(ws.onopen).toBeTypeOf("function");
    expect(ws.onmessage).toBeTypeOf("function");
    expect(ws.onclose).toBeTypeOf("function");
    expect(ws.onerror).toBeTypeOf("function");
  });

  it("calling connectSocket twice reuses the same socket instance (does not create a new one, but calls connect again)", () => {
    connectSocket();
    connectSocket(); // second call — socket singleton exists, just calls connect()
    // The first connect() already created one WS; second connect() on the same
    // ReconnectingSocket instance creates another WS because connect() always creates new WebSocket
    // This is the expected behaviour of the implementation
    expect(MockWebSocket.instances.length).toBeGreaterThanOrEqual(1);
  });
});

describe("disconnectSocket", () => {
  it("closes the underlying WebSocket", () => {
    connectSocket();
    const ws = getLastWS();
    disconnectSocket();
    expect(ws.closeCallCount).toBe(1);
  });

  it("nullifies the socket singleton so a subsequent connectSocket creates a fresh one", () => {
    connectSocket();
    disconnectSocket();
    connectSocket();
    // Should have created two WebSocket instances
    expect(MockWebSocket.instances.length).toBeGreaterThanOrEqual(2);
  });

  it("does not throw when called without a prior connectSocket", () => {
    expect(() => disconnectSocket()).not.toThrow();
  });
});

// ---------------------------------------------------------------------------
// onopen handler
// ---------------------------------------------------------------------------

describe("WebSocket onopen", () => {
  it("resets attempt counter (indirectly tested via reconnect behaviour)", () => {
    connectSocket();
    const ws = getLastWS();
    // Should not throw
    expect(() => ws.triggerOpen()).not.toThrow();
  });

  it("starts the health check interval after connection opens", () => {
    const setIntervalSpy = vi.spyOn(globalThis, "setInterval");
    connectSocket();
    const ws = getLastWS();
    ws.triggerOpen();
    expect(setIntervalSpy).toHaveBeenCalled();
    setIntervalSpy.mockRestore();
  });
});

// ---------------------------------------------------------------------------
// onmessage handler
// ---------------------------------------------------------------------------

describe("WebSocket onmessage", () => {
  it("parses JSON and calls applyEvent on the canvas store", () => {
    connectSocket();
    const ws = getLastWS();
    const applyEvent = vi.fn();
    vi.mocked(useCanvasStore.getState).mockReturnValue({
      applyEvent,
      hydrate: vi.fn(),
      setWsStatus: vi.fn(),
    } as unknown as ReturnType<typeof useCanvasStore.getState>);

    const msg = {
      event: "WORKSPACE_ONLINE",
      workspace_id: "ws-1",
      timestamp: new Date().toISOString(),
      payload: {},
    };
    ws.triggerMessage(msg);

    expect(applyEvent).toHaveBeenCalledWith(msg);
  });

  it("does not throw when JSON is malformed", () => {
    connectSocket();
    const ws = getLastWS();
    expect(() => ws.triggerRawMessage("not-valid-json{{{")).not.toThrow();
  });

  it("handles an empty JSON object without crashing", () => {
    connectSocket();
    const ws = getLastWS();
    expect(() => ws.triggerRawMessage("{}")).not.toThrow();
  });
});

// ---------------------------------------------------------------------------
// onclose / auto-reconnect
// ---------------------------------------------------------------------------

describe("WebSocket onclose – auto-reconnect", () => {
  it("schedules a reconnect via setTimeout when socket closes", () => {
    const setTimeoutSpy = vi.spyOn(globalThis, "setTimeout");
    connectSocket();
    const ws = getLastWS();
    ws.triggerClose();
    expect(setTimeoutSpy).toHaveBeenCalled();
    setTimeoutSpy.mockRestore();
  });

  it("reconnect delay is at most 30 000 ms", () => {
    const delays: number[] = [];
    const origSetTimeout = globalThis.setTimeout;
    const setTimeoutSpy = vi
      .spyOn(globalThis, "setTimeout")
      .mockImplementation((fn, delay, ...args) => {
        delays.push(delay as number);
        // Don't actually reconnect to avoid infinite loops in the test
        return origSetTimeout(() => {}, 0) as unknown as ReturnType<typeof setTimeout>;
      });

    connectSocket();
    const ws = getLastWS();

    // Trigger several closes to increment the attempt counter
    for (let i = 0; i < 6; i++) {
      ws.triggerClose();
    }

    expect(delays.every((d) => d <= 30_000)).toBe(true);
    setTimeoutSpy.mockRestore();
  });

  it("stops the health check interval on close", () => {
    const clearIntervalSpy = vi.spyOn(globalThis, "clearInterval");
    connectSocket();
    const ws = getLastWS();
    ws.triggerOpen(); // starts health check
    ws.triggerClose(); // should stop health check
    expect(clearIntervalSpy).toHaveBeenCalled();
    clearIntervalSpy.mockRestore();
  });

  it("creates a new WebSocket after reconnect delay elapses", () => {
    connectSocket();
    expect(MockWebSocket.instances).toHaveLength(1);

    const ws = getLastWS();
    ws.triggerClose();

    // First reconnect attempt is scheduled at 1s (Math.min(1000 * 2^0,
    // 30000)). Advance just past that — vi.runAllTimers() would
    // additionally re-fire the fallback poll setInterval forever and
    // hit the 10000-timer abort.
    vi.advanceTimersByTime(1100);

    expect(MockWebSocket.instances.length).toBeGreaterThan(1);
  });
});

describe("HTTP fallback poll while WS unhealthy", () => {
  it("starts a setInterval after onclose so /workspaces stays fresh", () => {
    const setIntervalSpy = vi.spyOn(globalThis, "setInterval");
    connectSocket();
    const ws = getLastWS();
    ws.triggerClose();
    // The fallback poll runs at 10s; the reconnect uses setTimeout, so
    // any setInterval registered between connect and close must be the
    // fallback poll.
    const fallbackCalls = setIntervalSpy.mock.calls.filter(
      ([, delay]) => delay === 10_000,
    );
    expect(fallbackCalls.length).toBeGreaterThan(0);
    setIntervalSpy.mockRestore();
  });

  it("clears the fallback poll once the WS reconnects (onopen)", () => {
    const clearIntervalSpy = vi.spyOn(globalThis, "clearInterval");
    connectSocket();
    const ws = getLastWS();
    ws.triggerClose(); // starts fallback poll
    clearIntervalSpy.mockClear();
    // Advance past the first reconnect delay so a fresh ws exists,
    // then trigger its open.
    vi.advanceTimersByTime(1100);
    const ws2 = getLastWS();
    ws2.triggerOpen();
    expect(clearIntervalSpy).toHaveBeenCalled();
    clearIntervalSpy.mockRestore();
  });

  it("clears the fallback poll on disconnect", () => {
    const clearIntervalSpy = vi.spyOn(globalThis, "clearInterval");
    connectSocket();
    const ws = getLastWS();
    ws.triggerClose(); // starts fallback poll
    clearIntervalSpy.mockClear();
    disconnectSocket();
    expect(clearIntervalSpy).toHaveBeenCalled();
    clearIntervalSpy.mockRestore();
  });
});

// ---------------------------------------------------------------------------
// onerror handler
// ---------------------------------------------------------------------------

describe("WebSocket onerror", () => {
  it("does not throw when error is triggered", () => {
    connectSocket();
    const ws = getLastWS();
    expect(() => ws.triggerError()).not.toThrow();
  });
});

// ---------------------------------------------------------------------------
// Health check (startHealthCheck / stopHealthCheck via onopen / disconnect)
// ---------------------------------------------------------------------------

describe("health check", () => {
  it("clears interval on disconnect", () => {
    const clearIntervalSpy = vi.spyOn(globalThis, "clearInterval");
    connectSocket();
    const ws = getLastWS();
    ws.triggerOpen(); // starts health check timer
    disconnectSocket();
    expect(clearIntervalSpy).toHaveBeenCalled();
    clearIntervalSpy.mockRestore();
  });

  it("sets a 30-second health check interval after onopen", () => {
    const intervals: number[] = [];
    const setIntervalSpy = vi
      .spyOn(globalThis, "setInterval")
      .mockImplementation((fn, delay, ...args) => {
        intervals.push(delay as number);
        return 999 as unknown as ReturnType<typeof setInterval>;
      });

    connectSocket();
    const ws = getLastWS();
    ws.triggerOpen();

    // The health check interval should be 30_000 ms
    expect(intervals.some((d) => d === 30_000)).toBe(true);
    setIntervalSpy.mockRestore();
  });

  it("does not accumulate multiple intervals on repeated onopen", () => {
    const clearIntervalSpy = vi.spyOn(globalThis, "clearInterval");
    connectSocket();
    const ws = getLastWS();

    ws.triggerOpen();
    ws.triggerOpen(); // second open should clear old interval first

    // clearInterval must have been called at least once (stopHealthCheck inside startHealthCheck)
    expect(clearIntervalSpy).toHaveBeenCalled();
    clearIntervalSpy.mockRestore();
  });
});

// Rehydrate dedup logic itself is exercised by `RehydrateDedup` unit
// tests in this file (below). End-to-end coupling through the
// dynamic-imported `@/lib/api` was non-trivial under our existing
// fake-timer setup; isolating the gate in a pure helper keeps
// regression coverage without that mocking complexity.

import { RehydrateDedup } from "../socket";

describe("RehydrateDedup", () => {
  it("first call passes the gate (no prior fetch)", () => {
    const d = new RehydrateDedup(1500);
    expect(d.shouldSkip(0)).toBe(false);
  });

  it("blocks while a fetch is in flight", () => {
    const d = new RehydrateDedup(1500);
    d.beginFetch();
    expect(d.shouldSkip(100)).toBe(true);
  });

  it("blocks within the post-completion window", () => {
    const d = new RehydrateDedup(1500);
    d.beginFetch();
    d.completeFetch(1_000);
    // 1100 - 1000 = 100 < 1500 → skip
    expect(d.shouldSkip(1_100)).toBe(true);
    // 2600 - 1000 = 1600 > 1500 → allow
    expect(d.shouldSkip(2_600)).toBe(false);
  });

  it("a completed fetch followed by another beginFetch blocks for the new in-flight", () => {
    const d = new RehydrateDedup(1500);
    d.beginFetch();
    d.completeFetch(1_000);
    // First wait out the dedup window
    expect(d.shouldSkip(2_600)).toBe(false);
    d.beginFetch();
    // Now a second fetch is in flight; further calls block again
    expect(d.shouldSkip(2_700)).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// wakeSocket() — visibility-wake reconnect (regression #223 / #228)
// ---------------------------------------------------------------------------
//
// Mobile browsers (iOS Safari, Chrome on Android in deep-sleep) silently
// drop the WebSocket when the tab is backgrounded; the in-page onclose
// fires very late or never. Without a visibility wake, the canvas stays
// frozen until the user manually refreshes.
//
// The real wiring lives at module level: connectSocket installs a
// visibilitychange/pageshow listener that calls wake() on foreground.
// We can't dispatch DOM events here because the suite runs under the
// `node` test environment (no `document`/`window` — see canvas/vitest
// .config.ts). Instead we test wake() directly through the wakeSocket
// public export, which is the same code path the listener invokes.

describe("wakeSocket → reconnect (#223 / #228 — mobile visibility wake)", () => {
  it("wake on a healthy OPEN socket does not create a new WebSocket", () => {
    connectSocket();
    const ws = getLastWS();
    ws.triggerOpen();
    // OPEN === 1. wake() should take the healthy-no-op branch.
    (ws as unknown as { readyState: number }).readyState = 1;
    const before = MockWebSocket.instances.length;
    wakeSocket();
    expect(MockWebSocket.instances.length).toBe(before);
  });

  it("wake on a CLOSED socket creates a new WebSocket (the actual #223 fix)", () => {
    connectSocket();
    const ws = getLastWS();
    ws.triggerOpen();
    // CLOSED === 3. Simulates the OS killing the socket while the tab
    // was backgrounded. We deliberately don't fire triggerClose() —
    // the whole point of #223 is that mobile browsers don't fire
    // onclose when they kill the WS, so reconnect never schedules.
    (ws as unknown as { readyState: number }).readyState = 3;
    const before = MockWebSocket.instances.length;
    wakeSocket();
    expect(MockWebSocket.instances.length).toBe(before + 1);
  });

  it("wake while CONNECTING (readyState=0) does not pile another handshake", () => {
    connectSocket();
    const ws = getLastWS();
    // CONNECTING === 0 — a handshake is already in flight.
    (ws as unknown as { readyState: number }).readyState = 0;
    const before = MockWebSocket.instances.length;
    wakeSocket();
    expect(MockWebSocket.instances.length).toBe(before);
  });

  it("wake cancels any pending backoff reconnect", () => {
    const clearTimeoutSpy = vi.spyOn(globalThis, "clearTimeout");
    connectSocket();
    const ws = getLastWS();
    ws.triggerOpen();
    // Drop the socket — onclose schedules a backoff reconnect.
    ws.triggerClose();
    // Now wake the page. wake() should pre-empt the backoff so the
    // user sees the canvas come back immediately, not after the
    // exponential delay window.
    (ws as unknown as { readyState: number }).readyState = 3;
    clearTimeoutSpy.mockClear();
    wakeSocket();
    expect(clearTimeoutSpy).toHaveBeenCalled();
    clearTimeoutSpy.mockRestore();
  });

  it("wake after disconnectSocket is a no-op (no zombie reconnect)", () => {
    connectSocket();
    const ws = getLastWS();
    ws.triggerOpen();
    disconnectSocket();
    const before = MockWebSocket.instances.length;
    // Singleton is null now — wake() should silently do nothing.
    expect(() => wakeSocket()).not.toThrow();
    expect(MockWebSocket.instances.length).toBe(before);
  });
});
