import { useCanvasStore } from "./canvas";
import { deriveWsBaseUrl } from "@/lib/ws-url";
import { emitSocketEvent } from "./socket-events";

// If explicit WS_URL is set, use it as-is (may include custom path).
// Otherwise derive base + append /ws.
export const WS_URL = process.env.NEXT_PUBLIC_WS_URL || (deriveWsBaseUrl() + "/ws");

export interface WSMessage {
  event: string;
  workspace_id: string;
  timestamp: string;
  payload: Record<string, unknown>;
}

/** Window during which a freshly-completed rehydrate is reused
 *  instead of firing a new GET. Picked to absorb the connect→health-
 *  check sequence (rehydrate runs once on onopen, then the first
 *  health-check tick fires immediately after — both should share the
 *  same fetch) without holding back legitimately-spaced rehydrates
 *  triggered by genuine WS silence later. */
const REHYDRATE_DEDUP_WINDOW_MS = 1_500;

/** Pure dedup gate for rehydrate(). Tracks two states:
 *
 *    - in-flight (between beginFetch and completeFetch): every
 *      shouldSkip returns true.
 *    - post-completion window (now < completedAt + windowMs):
 *      shouldSkip returns true.
 *
 *  Extracted from ReconnectingSocket so the gate is unit-testable
 *  without mocking dynamic imports or fake timers. The class itself
 *  is stateful but tiny — instances are not shared across sockets. */
export class RehydrateDedup {
  private inFlight = false;
  // -Infinity so the very first shouldSkip(now) call always passes
  // (now - (-Infinity) > windowMs). Initializing to 0 would false-
  // trip on test runs where now is also 0 (vi.useFakeTimers default
  // clock) AND on real runs in the first 1.5s after epoch on
  // clock-skewed systems.
  private completedAt = Number.NEGATIVE_INFINITY;
  constructor(private readonly windowMs: number) {}

  shouldSkip(now: number): boolean {
    if (this.inFlight) return true;
    if (now - this.completedAt < this.windowMs) return true;
    return false;
  }

  beginFetch(): void {
    this.inFlight = true;
  }

  completeFetch(now: number = Date.now()): void {
    this.inFlight = false;
    this.completedAt = now;
  }
}

/** Cadence for the HTTP fallback rehydrate that runs while the WS is
 *  in connecting/disconnected limbo. 10s is short enough that the user
 *  sees STARTING → ONLINE within one tick after the platform finishes
 *  provisioning, but long enough to not pound /workspaces if the
 *  network truly is down. The dedup gate inside rehydrate() collapses
 *  this against the post-onopen rehydrate, so reconnect doesn't pay
 *  for a duplicate fetch. */
export const FALLBACK_POLL_MS = 10_000;

class ReconnectingSocket {
  private ws: WebSocket | null = null;
  private attempt = 0;
  private url: string;
  private lastEventTime = 0;
  private healthCheckTimer: ReturnType<typeof setInterval> | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  // Polls /workspaces while the WS is unhealthy so the canvas reflects
  // truth even when realtime events aren't arriving. Without this the
  // store can stay frozen for minutes — e.g. workspaces transition
  // STARTING → ONLINE on the platform but the canvas keeps showing
  // STARTING until the WS finally reconnects, triggering false
  // "Provisioning Timeout" banners on already-online workspaces.
  private fallbackPollTimer: ReturnType<typeof setInterval> | null = null;
  // disposed signals that disconnect() has been called. Any in-flight
  // reconnect / handshake must abort early rather than attach to a
  // socket the caller no longer owns — otherwise React StrictMode's
  // effect double-invoke (and any future intentional disconnect)
  // leaves a zombie WebSocket alive forever.
  private disposed = false;
  // In-flight singleton + dedup window for rehydrate. Two reasons to
  // collapse rapid calls:
  //   1. connect.onopen fires rehydrate immediately, and the very next
  //      health-check tick may fire it again before the first GET
  //      returns — wasted round trip + rebuild churn that resets the
  //      mid-flight UI state (auto-rescue heuristics, grow passes).
  //   2. Future call sites (a manual "Refresh" button, post-import
  //      hydrate, error-recovery rehydrate) might pile up.
  // Keeping rehydrate idempotent at the call-site level means each
  // caller can fire-and-forget without coordinating.
  private rehydrateInFlight: Promise<void> | null = null;
  private rehydrateDedup = new RehydrateDedup(REHYDRATE_DEDUP_WINDOW_MS);

  constructor(url: string) {
    this.url = url;
  }

  connect() {
    if (this.disposed) return;
    useCanvasStore.getState().setWsStatus("connecting");
    // Start the HTTP fallback poll up-front, not just on onclose. Two
    // scenarios this guards against:
    //   1. The very first connect attempt — onclose hasn't fired yet
    //      because we never had a successful onopen.
    //   2. A failed handshake where the browser takes tens of seconds
    //      to surface as onclose (Chrome can hold a SYN-SENT WebSocket
    //      open for ~75s before giving up).
    // Idempotent — startFallbackPoll early-returns if a timer is
    // already running, so calling it from both places is cheap.
    this.startFallbackPoll();
    const ws = new WebSocket(this.url);
    this.ws = ws;

    ws.onopen = () => {
      if (this.disposed || this.ws !== ws) {
        // Late-open on an abandoned socket. Close it cleanly; the
        // caller already moved on.
        try { ws.close(); } catch { /* noop */ }
        return;
      }
      this.attempt = 0;
      this.lastEventTime = Date.now();
      useCanvasStore.getState().setWsStatus("connected");
      this.stopFallbackPoll();
      this.rehydrate();
      this.startHealthCheck();
    };

    ws.onmessage = (event) => {
      if (this.disposed || this.ws !== ws) return;
      this.lastEventTime = Date.now();
      try {
        const msg: WSMessage = JSON.parse(event.data);
        useCanvasStore.getState().applyEvent(msg);
        // Fan out to component-level subscribers so panels (Agent
        // Comms, MyChat activity feed) don't have to open their own
        // raw WebSocket — that pattern silently dropped events on
        // any reconnect because the per-component sockets had no
        // onclose / no backoff. See store/socket-events.ts.
        emitSocketEvent(msg);
      } catch {
        // Malformed WS message — skip silently
      }
    };

    ws.onclose = () => {
      // Fired on intentional close (disposed) OR server/network drop.
      // Only schedule a reconnect when the socket is still live AND
      // corresponds to the WS we just tore down (prevents a stale
      // onclose from a zombie socket from re-arming the loop).
      if (this.disposed || this.ws !== ws) return;
      this.stopHealthCheck();
      useCanvasStore.getState().setWsStatus("connecting");
      this.startFallbackPoll();
      const delay = Math.min(1000 * 2 ** this.attempt, 30000);
      this.attempt++;
      this.reconnectTimer = setTimeout(() => this.connect(), delay);
    };

    ws.onerror = () => {
      // Suppressed — onclose handles reconnection. onerror fires before onclose
      // and the Event object doesn't contain useful info (serializes to {}).
    };
  }

  /** Periodically re-fetch state in case WebSocket events were missed (e.g. agent
   *  status changed while the socket stayed open but no event was emitted). */
  private startHealthCheck() {
    this.stopHealthCheck();
    this.healthCheckTimer = setInterval(() => {
      const silenceSec = (Date.now() - this.lastEventTime) / 1000;
      // If no events for 30s, re-hydrate to catch missed status changes
      if (silenceSec > 30) {
        this.rehydrate();
        this.lastEventTime = Date.now(); // prevent rapid re-fetches
      }
    }, 30_000);
  }

  private stopHealthCheck() {
    if (this.healthCheckTimer) {
      clearInterval(this.healthCheckTimer);
      this.healthCheckTimer = null;
    }
  }

  /** While the WS is in connecting/disconnected limbo, poll /workspaces
   *  so the store stays fresh. The reconnect attempts continue in
   *  parallel; whichever recovers first wins. rehydrate()'s own dedup
   *  gate prevents this from racing with the open-time rehydrate. */
  private startFallbackPoll() {
    if (this.fallbackPollTimer) return;
    this.fallbackPollTimer = setInterval(() => {
      if (this.disposed) {
        this.stopFallbackPoll();
        return;
      }
      void this.rehydrate();
    }, FALLBACK_POLL_MS);
  }

  private stopFallbackPoll() {
    if (this.fallbackPollTimer) {
      clearInterval(this.fallbackPollTimer);
      this.fallbackPollTimer = null;
    }
  }

  private rehydrate(): Promise<void> {
    // Reuse an in-flight fetch — a second caller during the GET
    // shouldn't kick off a parallel one.
    if (this.rehydrateInFlight) return this.rehydrateInFlight;
    if (this.rehydrateDedup.shouldSkip(Date.now())) {
      return Promise.resolve();
    }

    // beginFetch lives INSIDE the IIFE's try so any future code added
    // between gate-check and IIFE-construction can't throw and leave
    // the gate stuck at inFlight=true forever. Today there's nothing
    // that can throw here, but the cost of being defensive is one
    // extra microtask of "in flight" status — negligible.
    const promise = (async () => {
      this.rehydrateDedup.beginFetch();
      try {
        const { api } = await import("@/lib/api");
        const workspaces = await api.get<WorkspaceData[]>("/workspaces");
        if (this.disposed) return;
        useCanvasStore.getState().hydrate(workspaces);
      } catch {
        // Rehydration failed — will retry on next health check cycle.
      } finally {
        this.rehydrateDedup.completeFetch(Date.now());
        this.rehydrateInFlight = null;
      }
    })();
    this.rehydrateInFlight = promise;
    return promise;
  }

  disconnect() {
    this.disposed = true;
    this.stopHealthCheck();
    this.stopFallbackPoll();
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.ws) {
      // Detach listeners before close() so we don't route the close
      // event through our onclose → scheduleReconnect path. Belt +
      // braces on top of the `disposed` check, because StrictMode
      // cycles through so fast that an attached onclose can fire
      // after disposed=true is set but before this assignment runs.
      this.ws.onopen = null;
      this.ws.onmessage = null;
      this.ws.onclose = null;
      this.ws.onerror = null;
      try { this.ws.close(); } catch { /* noop */ }
      this.ws = null;
    }
    useCanvasStore.getState().setWsStatus("disconnected");
  }
}

export interface WorkspaceData {
  id: string;
  name: string;
  role: string;
  tier: number;
  status: string;
  agent_card: Record<string, unknown> | null;
  url: string;
  parent_id: string | null;
  active_tasks: number;
  last_error_rate: number;
  last_sample_error: string;
  uptime_seconds: number;
  current_task: string;
  runtime: string;
  x: number;
  y: number;
  collapsed: boolean;
  /** USD spend ceiling set by the user; null = unlimited. Added by issue #541. */
  budget_limit: number | null;
  /** Cumulative USD spend for this workspace. Present when the platform tracks spend. */
  budget_used?: number | null;
  /** Server-declared provisioning-timeout override in milliseconds (#2054).
   *  Sourced from the workspace's template manifest at provision time —
   *  lets a slow runtime declare its cold-boot expectation without a
   *  canvas release. Falls through to the per-runtime profile in
   *  `@/lib/runtimeProfiles` when absent (the default behavior for any
   *  template that hasn't yet declared the field). */
  provision_timeout_ms?: number | null;
}

let socket: ReconnectingSocket | null = null;

export function connectSocket() {
  if (!socket) {
    socket = new ReconnectingSocket(WS_URL);
  }
  socket.connect();
}

export function disconnectSocket() {
  if (socket) {
    socket.disconnect();
    socket = null;
  }
}
