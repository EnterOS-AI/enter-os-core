package models

import (
	"encoding/json"
	"time"
)

type StructureEvent struct {
	ID          string          `json:"id" db:"id"`
	EventType   string          `json:"event_type" db:"event_type"`
	WorkspaceID *string         `json:"workspace_id" db:"workspace_id"`
	AgentID     *string         `json:"agent_id" db:"agent_id"`
	TargetID    *string         `json:"target_id" db:"target_id"`
	Payload     json.RawMessage `json:"payload" db:"payload"`
	CreatedAt   time.Time       `json:"created_at" db:"created_at"`
}

// WSMessage is the JSON sent over WebSocket to clients.
type WSMessage struct {
	Event       string          `json:"event"`
	WorkspaceID string          `json:"workspace_id"`
	Timestamp   time.Time       `json:"timestamp"`
	Payload     json.RawMessage `json:"payload"`
}
