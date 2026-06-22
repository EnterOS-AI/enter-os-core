/**
 * Derive WebSocket base URL. Priority:
 * 1. Explicit NEXT_PUBLIC_WS_URL (non-empty)
 * 2. Derived from NEXT_PUBLIC_PLATFORM_URL (http→ws)
 * 3. Derived from window.location (same-origin tenant image)
 * 4. Fallback to localhost
 *
 * Returns the base URL WITHOUT the /ws path suffix — callers append
 * their own path (/ws for the event stream, /workspaces/:id/terminal
 * for terminal sessions).
 */
export function deriveWsBaseUrl(): string {
  const explicit = process.env.NEXT_PUBLIC_WS_URL;
  if (explicit) return explicit.replace(/\/ws$/, "");

  const platform = process.env.NEXT_PUBLIC_PLATFORM_URL;
  if (platform) return platform.replace(/^http/, "ws");

  if (typeof window !== "undefined") {
    const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
    return `${proto}//${window.location.host}`;
  }

  return "ws://localhost:8080";
}
