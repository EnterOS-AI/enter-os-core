package events

// types.go — typed taxonomy of WebSocket event names emitted by the
// workspace-server.
//
// RFC #2945 PR-B. Pre-consolidation, every BroadcastOnly /
// RecordAndBroadcast call site passed a bare string literal:
//
//	h.broadcaster.BroadcastOnly(workspaceID, "AGENT_MESSAGE", payload)
//
// Producers (Go workspace-server, ~30 call sites across handlers/,
// scheduler/, registry/, bundle/) and consumers (canvas TS store +
// component listeners) duplicated the same string with no shared
// definition. A producer renaming an event silently broke every
// consumer — same drift class that produced the reno-stars data-loss
// regression on the persistence side. The fix on that side was the
// AgentMessageWriter SSOT (PR-A); the fix on this side is named
// constants.
//
// Why a typed string (not a plain enum / iota): the event name
// crosses the wire to TypeScript consumers as the literal string in
// `WSMessage.Event`. Iota integers would break the canvas store's
// switch (`case "AGENT_MESSAGE":`); a typed string preserves the
// wire contract while giving Go callers compile-time discipline.
//
// Mirror in canvas: a parity gate (PR-B-2 follow-up) will assert this
// constant set ≡ the TypeScript union members in
// `canvas/src/lib/ws-events.ts`. Today the canvas consumes the names
// via bare-string comparisons; the mirror lands separately to keep
// PR-B narrow.

// EventType is the wire-typed name of a WebSocket event the platform
// broadcasts. Always emit constants from this file rather than bare
// strings — the AST gate in events_types_drift_test.go guards
// against bare-string usage in the broadcaster surfaces.
type EventType string

// Event constants — the canonical taxonomy. New events MUST be added
// here AND mirrored in canvas/src/lib/ws-events.ts (parity gate
// pending in PR-B-2). Group by semantic family so the list stays
// scan-friendly as it grows.
const (
	// Chat / agent messaging — surfaces in canvas chat panels.
	EventAgentMessage EventType = "AGENT_MESSAGE"
	EventA2AResponse  EventType = "A2A_RESPONSE"
	EventActivityLogged EventType = "ACTIVITY_LOGGED"
	EventChannelMessage EventType = "CHANNEL_MESSAGE"

	// Workspace lifecycle.
	EventWorkspaceProvisioning    EventType = "WORKSPACE_PROVISIONING"
	EventWorkspaceProvisionFailed EventType = "WORKSPACE_PROVISION_FAILED"
	EventWorkspaceOnline          EventType = "WORKSPACE_ONLINE"
	EventWorkspaceOffline         EventType = "WORKSPACE_OFFLINE"
	EventWorkspaceDegraded        EventType = "WORKSPACE_DEGRADED"
	EventWorkspaceHibernated      EventType = "WORKSPACE_HIBERNATED"
	EventWorkspacePaused          EventType = "WORKSPACE_PAUSED"
	EventWorkspaceRemoved         EventType = "WORKSPACE_REMOVED"
	EventWorkspaceAwaitingAgent   EventType = "WORKSPACE_AWAITING_AGENT"
	EventWorkspaceHeartbeat       EventType = "WORKSPACE_HEARTBEAT"

	// Agent assignment + identity.
	EventAgentAssigned     EventType = "AGENT_ASSIGNED"
	EventAgentReplaced     EventType = "AGENT_REPLACED"
	EventAgentRemoved      EventType = "AGENT_REMOVED"
	EventAgentMoved        EventType = "AGENT_MOVED"
	EventAgentCardUpdated  EventType = "AGENT_CARD_UPDATED"

	// Delegation lifecycle.
	EventDelegationSent     EventType = "DELEGATION_SENT"
	EventDelegationStatus   EventType = "DELEGATION_STATUS"
	EventDelegationComplete EventType = "DELEGATION_COMPLETE"
	EventDelegationFailed   EventType = "DELEGATION_FAILED"

	// Task progression + scheduler.
	EventTaskUpdated EventType = "TASK_UPDATED"
	EventCronExecuted EventType = "CRON_EXECUTED"
	EventCronSkipped  EventType = "CRON_SKIPPED"

	// Approvals.
	EventApprovalRequested EventType = "APPROVAL_REQUESTED"
	EventApprovalEscalated EventType = "APPROVAL_ESCALATED"

	// Auth / credentials.
	EventExternalCredentialsRotated EventType = "EXTERNAL_CREDENTIALS_ROTATED"
)

// AllEventTypes lists every constant in this file. Used by the
// snapshot test (events_types_drift_test.go) to detect when a new
// constant is added without updating the snapshot — the catch-up
// step is mirroring the addition into canvas/src/lib/ws-events.ts so
// canvas consumers can switch on it.
//
// Keep in lexicographic order so the snapshot diff is stable on
// renames and the parity-with-TS comparison is order-independent.
var AllEventTypes = []EventType{
	EventA2AResponse,
	EventActivityLogged,
	EventAgentAssigned,
	EventAgentCardUpdated,
	EventAgentMessage,
	EventAgentMoved,
	EventAgentRemoved,
	EventAgentReplaced,
	EventApprovalEscalated,
	EventApprovalRequested,
	EventChannelMessage,
	EventCronExecuted,
	EventCronSkipped,
	EventDelegationComplete,
	EventDelegationFailed,
	EventDelegationSent,
	EventDelegationStatus,
	EventExternalCredentialsRotated,
	EventTaskUpdated,
	EventWorkspaceAwaitingAgent,
	EventWorkspaceDegraded,
	EventWorkspaceHeartbeat,
	EventWorkspaceHibernated,
	EventWorkspaceOffline,
	EventWorkspaceOnline,
	EventWorkspacePaused,
	EventWorkspaceProvisionFailed,
	EventWorkspaceProvisioning,
	EventWorkspaceRemoved,
}
