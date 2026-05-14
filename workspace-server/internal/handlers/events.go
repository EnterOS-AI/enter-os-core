package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/gin-gonic/gin"
)

type EventsHandler struct{}

func NewEventsHandler() *EventsHandler {
	return &EventsHandler{}
}

// List handles GET /events
func (h *EventsHandler) List(c *gin.Context) {
	rows, err := db.DB.QueryContext(c.Request.Context(), `
		SELECT id, event_type, workspace_id, payload, created_at
		FROM structure_events
		ORDER BY created_at DESC
		LIMIT 100
	`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	events := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, eventType string
		var workspaceID *string
		var payload []byte
		var createdAt time.Time

		if err := rows.Scan(&id, &eventType, &workspaceID, &payload, &createdAt); err != nil {
			log.Printf("Events scan error: %v", err)
			continue
		}
		events = append(events, map[string]interface{}{
			"id":           id,
			"event_type":   eventType,
			"workspace_id": workspaceID,
			"payload":      json.RawMessage(payload),
			"created_at":   createdAt,
		})
	}
	c.JSON(http.StatusOK, events)
}

// ListByWorkspace handles GET /events/:workspaceId
func (h *EventsHandler) ListByWorkspace(c *gin.Context) {
	workspaceID := c.Param("workspaceId")

	rows, err := db.DB.QueryContext(c.Request.Context(), `
		SELECT id, event_type, workspace_id, payload, created_at
		FROM structure_events
		WHERE workspace_id = $1
		ORDER BY created_at DESC
		LIMIT 100
	`, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	events := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, eventType string
		var wsID *string
		var payload []byte
		var createdAt time.Time

		if err := rows.Scan(&id, &eventType, &wsID, &payload, &createdAt); err != nil {
			continue
		}
		events = append(events, map[string]interface{}{
			"id":           id,
			"event_type":   eventType,
			"workspace_id": wsID,
			"payload":      json.RawMessage(payload),
			"created_at":   createdAt,
		})
	}
	c.JSON(http.StatusOK, events)
}
