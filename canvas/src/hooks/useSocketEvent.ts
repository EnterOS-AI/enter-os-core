/** React hook to subscribe to global WS events without opening a new
 *  WebSocket connection. Subscribers are routed through the singleton
 *  ReconnectingSocket in store/socket.ts so they inherit its
 *  reconnect, backoff, and HTTP fallback for free.
 *
 *  Usage:
 *
 *    useSocketEvent((msg) => {
 *      if (msg.workspace_id !== workspaceId) return;
 *      if (msg.event !== "ACTIVITY_LOGGED") return;
 *      // ... handle ...
 *    });
 *
 *  The handler is captured into a ref on every render so the latest
 *  closure (with its current state / props) is always invoked, while
 *  the actual subscription is registered exactly once per mount.
 *  Without the ref, an inline-defined handler would re-subscribe on
 *  every render, churning Set add/delete and risking missed events
 *  during the gap.
 *
 *  The handler is responsible for its own filtering — by event type,
 *  workspace_id, payload shape, etc. The bus is intentionally untyped
 *  beyond the WSMessage envelope; coupling each consumer to a typed
 *  per-event schema would defeat the "tiny pub/sub" goal. */

import { useEffect, useRef } from "react";
import type { WSMessage } from "@/store/socket";
import { subscribeSocketEvents } from "@/store/socket-events";

export function useSocketEvent(handler: (msg: WSMessage) => void): void {
  const handlerRef = useRef(handler);
  handlerRef.current = handler;
  useEffect(() => {
    return subscribeSocketEvents((msg) => handlerRef.current(msg));
  }, []);
}
