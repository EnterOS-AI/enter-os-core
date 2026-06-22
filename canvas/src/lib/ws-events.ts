/**
 * Canvas-side mirror of the Go `events.EventType` taxonomy
 * (workspace-server/internal/events/types.go). The Go side is the SSOT;
 * this union keeps the TS consumers (the socket bus + feature panels)
 * honest about which `WSMessage.event` strings can arrive, so a typo in
 * a handler `case` is a compile error rather than a silently-dropped
 * event.
 *
 * Keep this list in sync with the Go `AllEventTypes` slice. When P1 added
 * the unified requests inbox it introduced REQUEST_CREATED / REQUEST_RESPONDED
 * / REQUEST_MESSAGE to the Go taxonomy; those are mirrored here so the canvas
 * Tasks/Approvals tabs can react to them live (RFC unified-requests-inbox, P3).
 *
 * Only the event names the canvas actually consumes need to be exhaustive
 * for type-safety; this file intentionally lists the full known set so the
 * union reads as the contract, not a subset.
 */
export const WS_EVENTS = {
  WorkspaceOnline: "WORKSPACE_ONLINE",
  WorkspaceOffline: "WORKSPACE_OFFLINE",
  WorkspacePaused: "WORKSPACE_PAUSED",
  WorkspaceDegraded: "WORKSPACE_DEGRADED",
  WorkspaceProvisioning: "WORKSPACE_PROVISIONING",
  WorkspaceProvisionFailed: "WORKSPACE_PROVISION_FAILED",
  WorkspaceRemoved: "WORKSPACE_REMOVED",
  AgentCardUpdated: "AGENT_CARD_UPDATED",
  AgentMessage: "AGENT_MESSAGE",
  TaskUpdated: "TASK_UPDATED",
  A2AResponse: "A2A_RESPONSE",
  // --- Unified requests inbox (RFC P1; mirrored from events/types.go) ---
  RequestCreated: "REQUEST_CREATED",
  RequestResponded: "REQUEST_RESPONDED",
  RequestMessage: "REQUEST_MESSAGE",
} as const;

/** The event-name string union — the values of WS_EVENTS. */
export type WsEventName = (typeof WS_EVENTS)[keyof typeof WS_EVENTS];

/**
 * REQUEST_* event names the canvas Tasks/Approvals tabs refresh on. A single
 * Set keeps the ConciergeShell subscriber's membership test O(1) and the list
 * declarative — adding a fourth request event is one line here.
 */
export const REQUEST_EVENT_NAMES: ReadonlySet<string> = new Set<string>([
  WS_EVENTS.RequestCreated,
  WS_EVENTS.RequestResponded,
  WS_EVENTS.RequestMessage,
]);

/** True when a WS message's event is one of the unified-requests events. */
export function isRequestEvent(event: string): boolean {
  return REQUEST_EVENT_NAMES.has(event);
}
