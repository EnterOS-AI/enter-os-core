// @vitest-environment jsdom
import { describe, it, expect, vi } from "vitest";
import { closeWebSocketGracefully } from "../ws-close";

// Minimal test-double for WebSocket. jsdom doesn't ship a
// spec-compliant WebSocket, so we roll our own with just the bits the
// helper touches: readyState, close(), addEventListener("open") /
// ("error"). This lets us verify the graceful-close semantics without
// a live server.
function makeFakeWS(initialState: number) {
  const listeners: Record<string, Array<() => void>> = {};
  const ws = {
    readyState: initialState,
    close: vi.fn(),
    addEventListener: vi.fn(
      (type: string, handler: () => void, _opts?: { once?: boolean }) => {
        (listeners[type] ??= []).push(handler);
      },
    ),
    removeEventListener: vi.fn(
      (type: string, handler: () => void) => {
        const arr = listeners[type];
        if (!arr) return;
        const idx = arr.indexOf(handler);
        if (idx >= 0) arr.splice(idx, 1);
      },
    ),
    // Helpers for tests to fire the queued listeners.
    fire(type: string) {
      (listeners[type] ?? []).slice().forEach((h) => h());
    },
  };
  return ws as unknown as WebSocket & { fire(type: string): void };
}

describe("closeWebSocketGracefully", () => {
  it("calls close() immediately when the socket is OPEN", () => {
    const ws = makeFakeWS(WebSocket.OPEN);
    closeWebSocketGracefully(ws);
    expect(ws.close).toHaveBeenCalledOnce();
  });

  it("calls close() immediately when the socket is CLOSING", () => {
    const ws = makeFakeWS(WebSocket.CLOSING);
    closeWebSocketGracefully(ws);
    expect(ws.close).toHaveBeenCalledOnce();
  });

  it("is a no-op when the socket is already CLOSED", () => {
    const ws = makeFakeWS(WebSocket.CLOSED);
    closeWebSocketGracefully(ws);
    expect(ws.close).not.toHaveBeenCalled();
    expect(ws.addEventListener).not.toHaveBeenCalled();
  });

  it("defers close until 'open' when the socket is CONNECTING", () => {
    const ws = makeFakeWS(WebSocket.CONNECTING);
    closeWebSocketGracefully(ws);

    // close() NOT called yet — handshake hasn't completed.
    expect(ws.close).not.toHaveBeenCalled();
    // Two listeners queued: one for 'open' (close on connect), one
    // for 'error' (cancel the queued close if handshake fails).
    expect(ws.addEventListener).toHaveBeenCalledWith(
      "open", expect.any(Function), { once: true },
    );
    expect(ws.addEventListener).toHaveBeenCalledWith(
      "error", expect.any(Function), { once: true },
    );

    // Simulate the handshake completing — close() should fire now.
    (ws as unknown as { fire: (t: string) => void }).fire("open");
    expect(ws.close).toHaveBeenCalledOnce();
  });

  it("does NOT call close() when the CONNECTING socket errors instead of opening", () => {
    const ws = makeFakeWS(WebSocket.CONNECTING);
    closeWebSocketGracefully(ws);

    // Simulate handshake failure — the browser has already torn the
    // socket down, no explicit close() needed.
    (ws as unknown as { fire: (t: string) => void }).fire("error");
    expect(ws.close).not.toHaveBeenCalled();
  });
});
