/**
 * Sliding-window log for the in-chat activity feed (the live progress
 * lines under the spinner while a chat reply is in flight).
 *
 * Sized to fit the spinner area without forcing a scroll; per-tool-use
 * rows from the workspace's _report_tool_use can fire dozens per turn
 * (Read 5 files + Grep + Bash + Edits + delegations), so a too-small
 * window flushes useful early context before the user can read it.
 *
 * Consecutive identical lines collapse to a single entry — the same
 * tool repeated on the same target (e.g. Read of the same file twice
 * within a turn) is noise, not new progress.
 */
export const ACTIVITY_LOG_WINDOW = 20;

export function appendActivityLine(prev: string[], line: string): string[] {
  if (prev[prev.length - 1] === line) return prev; // collapse duplicates
  const next =
    prev.length >= ACTIVITY_LOG_WINDOW
      ? prev.slice(-(ACTIVITY_LOG_WINDOW - 1))
      : prev;
  return [...next, line];
}
