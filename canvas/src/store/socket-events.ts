/** Tiny pub/sub on top of the global ReconnectingSocket so feature
 *  components can subscribe to raw WS messages without each opening
 *  their own WebSocket. The previous pattern (each panel calling
 *  `new WebSocket(WS_URL)` in a useEffect with no onclose / no
 *  reconnect) silently dropped events whenever the underlying socket
 *  blipped — idle timeout, browser background-tab throttling, network
 *  jitter — and forced a refresh to recover.
 *
 *  The global ReconnectingSocket already owns reconnect, exponential
 *  backoff, health-check, and HTTP fallback poll. Routing component
 *  subscribers through it gives every consumer those guarantees for
 *  free, with one TCP connection instead of N.
 *
 *  Wiring: the socket's `ws.onmessage` calls `emitSocketEvent(msg)`
 *  after `useCanvasStore.getState().applyEvent(msg)`. Subscribers see
 *  events in arrival order; emit is synchronous so React's batched
 *  setState in handlers behaves the same as before.
 *
 *  Listeners are stored in a Set, so duplicate-subscribe is a no-op
 *  and unsubscribe is O(1). The bus survives the socket itself —
 *  intentional, since reconnect creates a new ws but listeners stay
 *  bound to the bus, not to any one ws instance. */

import type { WSMessage } from "./socket";

type Listener = (msg: WSMessage) => void;

const listeners = new Set<Listener>();

/** Fan a single decoded WS message out to every subscriber. Called by
 *  the socket's onmessage immediately after the store's applyEvent so
 *  derived store state and component handlers stay in lockstep. */
export function emitSocketEvent(msg: WSMessage): void {
  for (const listener of listeners) {
    try {
      listener(msg);
    } catch (err) {
      // One bad subscriber shouldn't break the others. Surface in dev,
      // swallow in prod — a thrown handler is a component bug, not a
      // socket bug.
      if (typeof console !== "undefined") {
        console.error("socket-events listener threw:", err);
      }
    }
  }
}

/** Register a subscriber. Returns an unsubscribe function the caller
 *  must invoke (typically from a useEffect cleanup). The listener is
 *  called for EVERY event — the caller is responsible for filtering by
 *  workspace_id, event type, etc. */
export function subscribeSocketEvents(listener: Listener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** Test-only: drop all subscribers. Lets unit tests reset state
 *  between cases without touching the singleton socket. */
export function _resetSocketEventListenersForTests(): void {
  listeners.clear();
}
