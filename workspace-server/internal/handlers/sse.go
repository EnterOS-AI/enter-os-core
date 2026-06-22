package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/gin-gonic/gin"
)

// aguiEvent is the AG-UI envelope written to the SSE stream.
// Spec: {"type":"<event_name>","timestamp":<unix_ms>,"data":{...}}
type aguiEvent struct {
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp"` // Unix milliseconds
	Data      json.RawMessage `json:"data"`
}

// SSEHandler streams workspace events as AG-UI-compatible Server-Sent Events.
type SSEHandler struct {
	broadcaster *events.Broadcaster
}

// NewSSEHandler returns an SSEHandler that sources events from b.
func NewSSEHandler(b *events.Broadcaster) *SSEHandler {
	return &SSEHandler{broadcaster: b}
}

// StreamEvents handles GET /workspaces/:id/events/stream.
//
// Authentication is enforced by the upstream WorkspaceAuth middleware (bearer
// token bound to :id). This handler only needs to:
//  1. Verify the workspace exists (returns 404 if not).
//  2. Set SSE headers.
//  3. Subscribe to the in-process broadcaster and relay events until the
//     client disconnects (context cancellation).
//
// AG-UI envelope per event:
//
//	data: {"type":"<event>","timestamp":<unix_ms>,"data":{...}}\n\n
func (h *SSEHandler) StreamEvents(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// Verify the workspace exists — 404 early rather than serving an empty stream.
	var exists bool
	if err := db.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM workspaces WHERE id = $1)`,
		workspaceID,
	).Scan(&exists); err != nil {
		log.Printf("SSE: workspace existence check failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify workspace"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	// SSE response headers.
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	// Instruct nginx / reverse-proxies to disable buffering so events reach
	// the client immediately rather than being held in a proxy buffer.
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		// Should never happen with gin's responseWriter, but guard defensively.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	ch, cancel := h.broadcaster.SubscribeSSE(workspaceID)
	defer cancel()

	// Send an initial SSE comment so the client knows the stream is live.
	fmt.Fprintf(c.Writer, ": ping\n\n")
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			env := aguiEvent{
				Type:      msg.Event,
				Timestamp: msg.Timestamp.UnixMilli(),
				Data:      msg.Payload,
			}
			b, err := json.Marshal(env)
			if err != nil {
				log.Printf("SSE: marshal error for workspace %s event %s: %v", workspaceID, msg.Event, err)
				continue
			}
			fmt.Fprintf(c.Writer, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}
