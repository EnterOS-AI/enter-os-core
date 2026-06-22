/**
 * closeWebSocketGracefully closes a WebSocket without tripping the
 * browser console warning "WebSocket is closed before the connection is
 * established". That warning fires when `ws.close()` runs while
 * readyState is still CONNECTING (0) — most often triggered by React
 * StrictMode's double-invoked useEffect in dev, or any rapid
 * mount/unmount (tab switch, route change) during the WS handshake.
 *
 * Behaviour by state:
 *   - OPEN / CLOSING: close immediately (the normal path).
 *   - CONNECTING:     defer the close until 'open' fires, so the
 *                     browser sees a full handshake before the shutdown.
 *   - CLOSED:         no-op.
 *
 * Returns the ws unchanged for chaining.
 */
export function closeWebSocketGracefully(ws: WebSocket): WebSocket {
  const state = ws.readyState;
  if (state === WebSocket.OPEN || state === WebSocket.CLOSING) {
    ws.close();
    return ws;
  }
  if (state === WebSocket.CONNECTING) {
    const onOpen = () => {
      ws.close();
    };
    ws.addEventListener("open", onOpen, { once: true });
    // Also wire an error listener — if the handshake fails we don't
    // need to close (the browser already tore it down) and we should
    // clear the queued onOpen handler.
    ws.addEventListener(
      "error",
      () => ws.removeEventListener("open", onOpen),
      { once: true },
    );
  }
  return ws;
}
