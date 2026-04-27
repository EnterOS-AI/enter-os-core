// @vitest-environment jsdom
/**
 * Tests for the socket-events pub/sub bus that lets feature components
 * subscribe to global WS messages without each opening their own
 * WebSocket. The previous per-panel `new WebSocket(WS_URL)` pattern
 * silently dropped events on any reconnect because each raw socket
 * had no onclose handler.
 *
 * The bus contract:
 *   - Every emit fans out to every registered listener.
 *   - Subscribe returns an unsubscribe; calling it removes that listener.
 *   - A throwing listener does not prevent siblings from receiving the
 *     event (bug-tolerant fan-out).
 *   - The bus survives test cases — _resetSocketEventListenersForTests
 *     gives unit tests a clean slate.
 */
import { describe, it, expect, beforeEach, vi } from "vitest";
import {
  emitSocketEvent,
  subscribeSocketEvents,
  _resetSocketEventListenersForTests,
} from "../socket-events";
import type { WSMessage } from "../socket";

const sampleMsg: WSMessage = {
  event: "ACTIVITY_LOGGED",
  workspace_id: "ws-test",
  timestamp: "2026-04-27T19:00:00Z",
  payload: { activity_type: "a2a_send", source_id: "ws-test" },
};

beforeEach(() => {
  _resetSocketEventListenersForTests();
});

describe("socket-events bus", () => {
  it("delivers an emitted message to a single subscriber", () => {
    const listener = vi.fn();
    subscribeSocketEvents(listener);
    emitSocketEvent(sampleMsg);
    expect(listener).toHaveBeenCalledOnce();
    expect(listener).toHaveBeenCalledWith(sampleMsg);
  });

  it("fans out to every subscriber in registration order", () => {
    const order: number[] = [];
    subscribeSocketEvents(() => order.push(1));
    subscribeSocketEvents(() => order.push(2));
    subscribeSocketEvents(() => order.push(3));
    emitSocketEvent(sampleMsg);
    expect(order).toEqual([1, 2, 3]);
  });

  it("returned unsubscribe stops further delivery to that listener", () => {
    const a = vi.fn();
    const b = vi.fn();
    const unsubA = subscribeSocketEvents(a);
    subscribeSocketEvents(b);

    emitSocketEvent(sampleMsg);
    expect(a).toHaveBeenCalledOnce();
    expect(b).toHaveBeenCalledOnce();

    unsubA();
    emitSocketEvent(sampleMsg);
    expect(a).toHaveBeenCalledOnce(); // still 1 — unsubscribed
    expect(b).toHaveBeenCalledTimes(2);
  });

  it("a throwing listener does not break sibling listeners", () => {
    // Suppress the expected console.error so test output stays clean.
    const errSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const sibling = vi.fn();
    subscribeSocketEvents(() => {
      throw new Error("buggy handler");
    });
    subscribeSocketEvents(sibling);

    emitSocketEvent(sampleMsg);
    expect(sibling).toHaveBeenCalledOnce();
    expect(errSpy).toHaveBeenCalled();
    errSpy.mockRestore();
  });

  it("emit is a no-op when there are no subscribers", () => {
    // Just verifies it doesn't throw.
    expect(() => emitSocketEvent(sampleMsg)).not.toThrow();
  });

  it("re-subscribing the same listener instance is a no-op (Set semantics)", () => {
    const listener = vi.fn();
    const unsubA = subscribeSocketEvents(listener);
    subscribeSocketEvents(listener); // duplicate
    emitSocketEvent(sampleMsg);
    // Set dedupes — listener fires once per emit, not twice.
    expect(listener).toHaveBeenCalledOnce();
    // First unsubscribe removes it (Set delete is idempotent — second
    // unsub from the duplicate subscribe call is also a no-op).
    unsubA();
    emitSocketEvent(sampleMsg);
    expect(listener).toHaveBeenCalledOnce();
  });
});
