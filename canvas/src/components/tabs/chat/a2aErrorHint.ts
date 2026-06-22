/**
 * Maps an A2A delivery-failure detail string (the bit AFTER stripping
 * the [A2A_ERROR] sentinel prefix) to a one-line operator-actionable
 * hint. Pattern matches are lowercase substring checks, ordered most-
 * specific first so the right hint wins when multiple patterns
 * overlap (e.g. "control request timeout" wins over generic "timeout").
 *
 * Used by both the chat Agent Comms panel and the Activity tab so the
 * same symptom reads identically across surfaces. Two prior copies
 * had already drifted (Activity tab gained `not found`/`offline`
 * cases AgentCommsPanel never picked up) — this module is the merged
 * superset and the only place hint text should change.
 */
export function inferA2AErrorHint(detail: string): string {
  const t = detail.toLowerCase();

  // "control request timeout" is the specific Claude Code SDK init
  // wedge symptom. Pattern on the full phrase, not bare "initialize"
  // — a user task containing "failed to initialize database" would
  // false-positive into the SDK-wedge hint.
  if (t.includes("control request timeout")) {
    return "The remote agent's Claude Code SDK is wedged on initialization (often after a long idle period or OAuth refresh). A workspace restart usually clears it.";
  }
  if (
    t.includes("readtimeout") ||
    t.includes("connecttimeout") ||
    t.includes("deadline exceeded") ||
    t.includes("timeout")
  ) {
    return "The remote agent didn't respond within the proxy timeout. It may be busy with a long task, or the runtime is stuck — restart the workspace if this repeats.";
  }
  if (
    t.includes("connectionreset") ||
    t.includes("remoteprotocolerror") ||
    t.includes("connection reset") ||
    t.includes("no message")
  ) {
    return "The connection to the remote agent dropped before a reply arrived. Usually a transient network blip — retry once. If it repeats, the remote container may have crashed mid-request; check its logs.";
  }
  if (t.includes("agent error") || t.includes("exception")) {
    // internal#212 closeout: end users have no "container logs" surface
    // in the canvas; the Activity tab IS the user-visible logs surface
    // (full row carries request/response body + error_detail). Point
    // there so the hint is actionable from inside the product.
    return "The remote agent's runtime threw an exception. Open the Activity tab for the full row (request body, response, error_detail) — Restart usually clears transient runtime crashes.";
  }
  if (
    t.includes("not found") ||
    t.includes("not accessible") ||
    t.includes("offline")
  ) {
    return "The remote workspace can't be reached — it may be stopped, removed, or outside the access control list. Verify the peer is online before retrying.";
  }
  if (detail === "") {
    return "The remote agent returned no error detail (the underlying httpx exception had an empty message — typically a connection-reset or silent timeout). A workspace restart is the safe first move.";
  }
  // internal#212 closeout: "workspace logs" pointed at a tab that does
  // not exist — Activity tab is the in-product logs surface. Keep the
  // hint generic enough for the unrecognised-detail fallback but point
  // the user at a real affordance.
  return "The remote agent reported a delivery failure. Open the Activity tab for the full row, or try restarting the workspace.";
}
