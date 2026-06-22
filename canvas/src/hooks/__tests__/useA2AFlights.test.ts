// @vitest-environment jsdom
/** Unit tests for useA2AFlights — the event→flight lifecycle that drives the
 *  envelope animations on the canvas (MessageFlightLayer) and the concierge
 *  home (MessageFlightHome). useSocketEvent is mocked so we can drive the
 *  ACTIVITY_LOGGED handler directly. */
import { renderHook, act } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";

// Capture the handler the hook registers with the socket bus. vi.hoisted is
// required because vi.mock factories are hoisted above normal declarations and
// may only close over hoisted state.
const h = vi.hoisted(() => ({ captured: null as ((msg: unknown) => void) | null }));
vi.mock("@/hooks/useSocketEvent", () => ({
  useSocketEvent: (cb: (msg: unknown) => void) => {
    h.captured = cb;
  },
}));

import {
  useA2AFlights,
  FLIGHT_DURATION_MS,
  BOUNCE_DURATION_MS,
  RECEIVE_BOUNCE_DELAY_MS,
} from "@/hooks/useA2AFlights";

function setReducedMotion(reduce: boolean) {
  window.matchMedia = vi.fn().mockImplementation((q: string) => ({
    matches: reduce && q.includes("reduce"),
    media: q,
    onchange: null,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    addListener: vi.fn(),
    removeListener: vi.fn(),
    dispatchEvent: vi.fn(),
  }));
}

const msg = (payload: Record<string, unknown>, event = "ACTIVITY_LOGGED") => ({
  event,
  workspace_id: "a",
  timestamp: "2026-06-08T00:00:00Z",
  payload,
});
const a2aSend = (over: Record<string, unknown> = {}) =>
  msg({ activity_type: "a2a_send", source_id: "a", target_id: "b", ...over });

describe("useA2AFlights", () => {
  beforeEach(() => {
    h.captured = null;
    vi.useRealTimers();
    setReducedMotion(false);
  });

  it("emits a flight for an a2a_send between two distinct agents", () => {
    const { result } = renderHook(() => useA2AFlights());
    act(() => h.captured?.(a2aSend()));
    expect(result.current).toHaveLength(1);
    expect(result.current[0]).toMatchObject({ sourceId: "a", targetId: "b", kind: "send" });
  });

  it("maps a2a_receive / task_update to their kinds", () => {
    const { result } = renderHook(() => useA2AFlights());
    act(() => h.captured?.(a2aSend({ activity_type: "a2a_receive" })));
    act(() => h.captured?.(a2aSend({ activity_type: "task_update" })));
    const kinds = result.current.map((f) => f.kind);
    expect(kinds).toContain("receive");
    expect(kinds).toContain("task");
  });

  it("ignores non-A2A activity and non-ACTIVITY_LOGGED events", () => {
    const { result } = renderHook(() => useA2AFlights());
    act(() => h.captured?.(msg({ activity_type: "status_change", source_id: "a", target_id: "b" })));
    act(() => h.captured?.(a2aSend({}, )));
    act(() => h.captured?.({ event: "WORKSPACE_UPDATED", workspace_id: "a", payload: {} }));
    expect(result.current.every((f) => f.kind === "send")).toBe(true);
    expect(result.current).toHaveLength(1); // only the one valid a2aSend
  });

  it("skips self-loops and flights with no target", () => {
    const { result } = renderHook(() => useA2AFlights());
    act(() => h.captured?.(a2aSend({ target_id: "a" }))); // self-loop
    act(() => h.captured?.(a2aSend({ target_id: "" }))); // missing target
    expect(result.current).toHaveLength(0);
  });

  it("emits nothing when prefers-reduced-motion is set", () => {
    setReducedMotion(true);
    const { result } = renderHook(() => useA2AFlights());
    act(() => h.captured?.(a2aSend()));
    expect(result.current).toHaveLength(0);
  });

  it("emits nothing when disabled", () => {
    const { result } = renderHook(() => useA2AFlights(false));
    act(() => h.captured?.(a2aSend()));
    expect(result.current).toHaveLength(0);
  });

  it("expires a flight after the TTL (outliving the receiver landing bounce)", () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useA2AFlights());
    act(() => h.captured?.(a2aSend()));
    expect(result.current).toHaveLength(1);
    // The flight must SURVIVE past the envelope traversal — the receiver's
    // landing bounce is still playing (it starts at ~0.82 of the flight and
    // runs BOUNCE_DURATION_MS). Unmounting here would cut the catch mid-air.
    act(() => {
      vi.advanceTimersByTime(FLIGHT_DURATION_MS + 100);
    });
    expect(result.current).toHaveLength(1);
    // ...and expire once the landing bounce has finished too.
    act(() => {
      vi.advanceTimersByTime(RECEIVE_BOUNCE_DELAY_MS + BOUNCE_DURATION_MS - FLIGHT_DURATION_MS + 200);
    });
    expect(result.current).toHaveLength(0);
  });
});