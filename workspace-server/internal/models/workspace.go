package models

import (
	"database/sql"
	"encoding/json"
	"time"
)

// DefaultMaxConcurrentTasks mirrors the workspaces.max_concurrent_tasks
// schema default. Handlers that resolve a 0/omitted payload value write
// this constant so the read-side (scheduler capacity check) sees a
// guaranteed non-zero column on every row.
const DefaultMaxConcurrentTasks = 1

type Workspace struct {
	ID                 string          `json:"id" db:"id"`
	Name               string          `json:"name" db:"name"`
	Role               sql.NullString  `json:"role" db:"role"`
	Tier               int             `json:"tier" db:"tier"`
	AwarenessNamespace sql.NullString  `json:"awareness_namespace" db:"awareness_namespace"`
	Status             string          `json:"status" db:"status"`
	SourceBundleID     sql.NullString  `json:"source_bundle_id" db:"source_bundle_id"`
	AgentCard          json.RawMessage `json:"agent_card" db:"agent_card"`
	URL                sql.NullString  `json:"url" db:"url"`
	ParentID           *string         `json:"parent_id" db:"parent_id"`
	ForwardedTo        *string         `json:"forwarded_to" db:"forwarded_to"`
	LastHeartbeatAt    *time.Time      `json:"last_heartbeat_at" db:"last_heartbeat_at"`
	LastErrorRate      float64         `json:"last_error_rate" db:"last_error_rate"`
	LastSampleError    sql.NullString  `json:"last_sample_error" db:"last_sample_error"`
	ActiveTasks        int             `json:"active_tasks" db:"active_tasks"`
	MaxConcurrentTasks int             `json:"max_concurrent_tasks" db:"max_concurrent_tasks"`
	UptimeSeconds      int             `json:"uptime_seconds" db:"uptime_seconds"`
	CreatedAt          time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at" db:"updated_at"`
	// DeliveryMode: "push" (synchronous to URL — default) or "poll" (logged
	// to activity_logs, agent reads via GET /activity?since_id=). See
	// migration 045 + RFC #2339.
	DeliveryMode       string          `json:"delivery_mode" db:"delivery_mode"`
	// BroadcastEnabled: when true the workspace may call POST /broadcast to
	// deliver a message to all non-removed agent workspaces in the org.
	// Default false — only privileged orchestrators should hold this ability.
	BroadcastEnabled   bool            `json:"broadcast_enabled" db:"broadcast_enabled"`
	// TalkToUserEnabled: when false the workspace's send_message_to_user calls
	// and POST /notify requests are rejected with HTTP 403 so the agent is
	// forced to route updates through a parent workspace. Default true
	// (preserves existing behaviour for all workspaces).
	TalkToUserEnabled  bool            `json:"talk_to_user_enabled" db:"talk_to_user_enabled"`
	// Canvas layout fields (from JOIN)
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Collapsed bool    `json:"collapsed"`
}

// Delivery mode constants. Matches the CHECK constraint in migration 045.
const (
	DeliveryModePush = "push"
	DeliveryModePoll = "poll"
)

// IsValidDeliveryMode reports whether s is one of the recognised
// delivery modes. Empty string is NOT valid here — callers must
// resolve the default ("push") before calling.
func IsValidDeliveryMode(s string) bool {
	return s == DeliveryModePush || s == DeliveryModePoll
}

type RegisterPayload struct {
	ID string `json:"id" binding:"required"`
	// URL is required for push-mode workspaces; optional / unused for
	// poll-mode (the platform never dispatches to it). The handler
	// enforces the conditional requirement based on the resolved
	// delivery mode (payload value, falling back to the row's existing
	// value, falling back to "push").
	URL          string          `json:"url"`
	AgentCard    json.RawMessage `json:"agent_card" binding:"required"`
	// DeliveryMode is optional. Empty string means "keep the existing
	// value on the workspace row, or default to push for new rows".
	// When set, must be one of DeliveryModePush / DeliveryModePoll.
	DeliveryMode string          `json:"delivery_mode,omitempty"`
}

type HeartbeatPayload struct {
	WorkspaceID   string  `json:"workspace_id" binding:"required"`
	ErrorRate     float64 `json:"error_rate"`
	SampleError   string  `json:"sample_error"`
	ActiveTasks   int     `json:"active_tasks"`
	UptimeSeconds int     `json:"uptime_seconds"`
	CurrentTask   string  `json:"current_task"`
	// MonthlySpend is cumulative USD spend for the current calendar month,
	// denominated in cents (e.g. 1500 = $15.00). Zero means "no update" —
	// the heartbeat handler never writes zero to avoid accidentally clearing
	// a previously-reported spend value. Any non-zero value is clamped to
	// [0, maxMonthlySpend] before the DB write. (#615)
	MonthlySpend int64 `json:"monthly_spend"`
	// RuntimeState is a self-reported runtime health flag separate from
	// "is the heartbeat task firing at all". The heartbeat task lives in
	// its own asyncio task and keeps pinging even when the agent runtime
	// is wedged (e.g. claude_agent_sdk's `Control request timeout:
	// initialize` leaves the SDK in a permanent error state for the
	// process lifetime). RuntimeState is how the workspace tells the
	// platform "I'm alive but my Claude runtime is broken — flip me to
	// degraded so the canvas can show a Restart hint."
	//
	// Empty string = healthy / no signal. The only currently-recognised
	// non-empty value is "wedged"; future values can extend this without
	// migration.
	RuntimeState string `json:"runtime_state"`

	// RuntimeMetadata is the adapter-declared capability map + per-
	// capability override values. The Python runtime builds this from
	// BaseAdapter.capabilities() + per-hook methods (e.g.
	// idle_timeout_override()) — see workspace/heartbeat.py:
	// _runtime_metadata_payload. Optional: missing means "use platform
	// defaults for everything", matching pre-2026-04 behavior.
	//
	// Pointer (not value) so a missing JSON field is nil rather than a
	// zero-value RuntimeMetadata{} that would falsely claim "all caps =
	// false declared explicitly". Lets the platform distinguish "adapter
	// said no native ownership" from "old runtime version, didn't say".
	RuntimeMetadata *RuntimeMetadata `json:"runtime_metadata,omitempty"`
}

// RuntimeMetadata is the adapter-declared capability + override block
// the Python runtime sends in the heartbeat payload. New fields can be
// added with `omitempty` without breaking older runtime versions.
//
// See project memory `project_runtime_native_pluggable.md` for the
// principle and workspace/adapter_base.py:RuntimeCapabilities for the
// Python source of truth.
type RuntimeMetadata struct {
	// Capabilities maps capability name → "adapter owns it natively".
	// Keys (heartbeat, scheduler, session, status_mgmt, retry,
	// activity_decoration, channel_dispatch) match
	// RuntimeCapabilities.to_dict() in adapter_base.py — keep in sync.
	Capabilities map[string]bool `json:"capabilities,omitempty"`

	// IdleTimeoutSeconds, when set, overrides the per-dispatch silence
	// window in a2a_proxy.go for this workspace's A2A traffic. Pointer
	// so nil means "no override; use the global default". Zero / negative
	// is treated as nil by the consumer (a2a_proxy.go).
	IdleTimeoutSeconds *int `json:"idle_timeout_seconds,omitempty"`
}

type UpdateCardPayload struct {
	WorkspaceID string          `json:"workspace_id" binding:"required"`
	AgentCard   json.RawMessage `json:"agent_card" binding:"required"`
}

// MemorySeed represents an initial memory to seed into a workspace at creation time.
// Used by both the POST /workspaces API and org template import to pre-populate
// agent memories from config (issue #1050).
type MemorySeed struct {
	Content string `json:"content" yaml:"content"`
	Scope   string `json:"scope" yaml:"scope"` // LOCAL, TEAM, GLOBAL
}

type CreateWorkspacePayload struct {
	Name     string  `json:"name" binding:"required"`
	Role     string  `json:"role"`
	Template string  `json:"template"` // workspace-configs-templates folder name
	Tier     int     `json:"tier"`
	Model    string  `json:"model"`
	Runtime      string  `json:"runtime"`       // "langgraph" (default), "claude-code", etc.
	External     bool    `json:"external"`      // true = no Docker container, just a registered URL
	URL          string  `json:"url"`           // for external workspaces: the A2A endpoint URL (push mode only — omit for poll)
	// DeliveryMode: "push" (default) sends inbound A2A to URL synchronously;
	// "poll" records inbound to activity_logs for the agent to consume via
	// GET /activity?since_id=. Poll mode does not require a URL. See #2339.
	DeliveryMode string  `json:"delivery_mode,omitempty"`
	WorkspaceDir    string  `json:"workspace_dir"`    // host path to mount as /workspace (empty = isolated volume)
	WorkspaceAccess string  `json:"workspace_access"` // "none" (default), "read_only", or "read_write" — see #65
	ParentID        *string `json:"parent_id"`
	// BudgetLimit is the optional monthly spend ceiling in USD cents.
	// NULL (omitted) means no limit. budget_limit=500 means $5.00/month.
	BudgetLimit *int64 `json:"budget_limit"`
	// Secrets is an optional map of key→plaintext-value pairs to persist as
	// workspace secrets at creation time.  Stored encrypted (same path as
	// POST /workspaces/:id/secrets).  Nil/empty map is a no-op.
	Secrets map[string]string `json:"secrets"`
	// MaxConcurrentTasks caps parallel A2A + cron dispatch. 0 means use
	// DefaultMaxConcurrentTasks. Leaders typically set 3.
	MaxConcurrentTasks int `json:"max_concurrent_tasks"`
	Canvas   struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	} `json:"canvas"`
	// InitialMemories is an optional list of memories to seed into the
	// workspace immediately after creation. Each entry is inserted into
	// agent_memories with the workspace's awareness namespace. Issue #1050.
	InitialMemories []MemorySeed `json:"initial_memories"`
}

type CheckAccessPayload struct {
	CallerID string `json:"caller_id" binding:"required"`
	TargetID string `json:"target_id" binding:"required"`
}
