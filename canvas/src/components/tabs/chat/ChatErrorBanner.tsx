"use client";

/**
 * ChatErrorBanner — error-state banner rendered under the chat
 * message list when an agent turn fails or the workspace is offline.
 *
 * internal#212 closes the "see workspace logs for details" pointer-to-
 * nowhere defect:
 *
 *   - The banner now renders the actionable, secret-safe failure
 *     reason that ws-server places on `ACTIVITY_LOGGED.error_detail`
 *     (provider HTTP status + error code + provider's own human
 *     message). The hook (`useChatSocket`) forwards this through
 *     `onSendError`, which the ChatTab routes into this banner's
 *     `message` prop. No hardcoded opaque text in this component.
 *
 *   - A "View activity log" button navigates the user to the Activity
 *     tab where the full row (request body, response body, timing,
 *     full error_detail) lives. Until internal#212, the banner
 *     mentioned "workspace logs" with no link — there is no separate
 *     Logs tab in the side panel; the Activity tab IS the workspace-
 *     logs surface. Routing through the existing tab makes the
 *     reference real instead of dangling.
 *
 *   - The existing Restart button (shown only when the workspace is
 *     offline) is preserved unchanged so the recovery affordance the
 *     old banner offered does not regress.
 *
 * Pure presentational — no socket subscription, no state machine. Easy
 * to unit-test in isolation and easy to compose into the ChatTab.
 */

import { useCanvasStore } from "@/store/canvas";

export interface ChatErrorBannerProps {
  /** The user-visible reason. Pass `null` to render nothing. */
  message: string | null;
  /** Workspace reachable state — gates the Restart affordance. */
  isOnline: boolean;
  /** Fires when the user clicks Restart (offline-only). */
  onRestart: () => void;
}

export function ChatErrorBanner({ message, isOnline, onRestart }: ChatErrorBannerProps) {
  // Pulled from the global store rather than threaded through props so
  // the chat tab does not need to know about the side-panel tab state.
  // Matches how Toolbar.tsx triggers the audit tab (the existing
  // precedent for cross-tab navigation).
  const setPanelTab = useCanvasStore((s) => s.setPanelTab);

  if (!message) return null;

  return (
    <div
      // role="alert" + aria-live mirrors the project's existing WCAG
      // 4.1.3 banner pattern (see fix/canvas-errors-aria-alert) — a
      // screen reader announces the failure as soon as it lands.
      role="alert"
      aria-live="assertive"
      className="px-3 py-2 bg-red-900/20 border-t border-red-800/30"
    >
      <div className="flex items-center justify-between gap-2">
        <span className="text-[10px] text-red-300 break-words flex-1">{message}</span>
        <div className="flex items-center gap-1.5 shrink-0">
          <button
            type="button"
            onClick={() => setPanelTab("activity")}
            className="text-[10px] px-2 py-0.5 bg-red-900/40 hover:bg-red-800/60 border border-red-700/40 text-red-200 rounded transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1"
          >
            View activity log
          </button>
          {!isOnline && (
            <button
              type="button"
              onClick={onRestart}
              className="text-[11px] px-2 py-0.5 bg-red-800 text-red-200 rounded hover:bg-red-700"
            >
              Restart
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
