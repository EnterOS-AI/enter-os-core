package handlers

// chat_session.go — POST /workspaces/:id/chat-session/new
// (core#2697).
//
// Implements the "New session" soft-boundary primitive: rotates
// workspaces.chat_session_started_at to now() so subsequent
// /chat-history reads filter out pre-marker rows, and broadcasts a
// SESSION_RESET event so every connected device clears its local
// view in lockstep.
//
// Soft boundary, not destructive: the underlying activity_logs rows
// are NOT deleted. A future "history" affordance can still read
// pre-marker rows by querying the table directly (bypassing the
// /chat-history filter). The CTO decision in the dispatch was
// explicit on this — a "new session" in the chat panel is a UX
// re-orientation, not a data wipe.
//
// Auth: same wsAuth chain as /chat-history. The handler that owns
// the canvas path already covers tenant ADMIN_TOKEN +
// X-Molecule-Org-Id; no new trust boundary.
//
// Why a dedicated handler file (not folded into chat_history.go):
// the chat-history read is a thin adapter over MessageStore; the
// reset is a write with a side-effect broadcast. Keeping them in
// separate files matches the "thin adapter / domain logic" split
// used elsewhere in the handlers package.

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ChatSessionNewResponse is the wire shape for POST /chat-session/new.
// The marker field is the new chat_session_started_at value; canvas
// can use it to align its own "session X started at..." UI without
// re-fetching the workspace row.
type ChatSessionNewResponse struct {
	WorkspaceID  string    `json:"workspace_id"`
	Marker       time.Time `json:"chat_session_started_at"`
	BroadcastSeq int64     `json:"broadcast_seq,omitempty"`
}

// ChatSessionHandler exposes the soft-boundary rotate endpoint.
// No fields today — the handler is stateless; a future
// "list all sessions" affordance would land here as a second method.
type ChatSessionHandler struct {
	broadcaster events.EventEmitter
}

// NewChatSessionHandler wires the broadcaster the handler uses for
// the cross-device SESSION_RESET event. Tests inject a stub via
// the same constructor (handler_test.go).
func NewChatSessionHandler(broadcaster events.EventEmitter) *ChatSessionHandler {
	return &ChatSessionHandler{broadcaster: broadcaster}
}

// NewSession handles POST /workspaces/:id/chat-session/new. Sets
// chat_session_started_at = now() and broadcasts SESSION_RESET so
// other devices clear their local view.
//
// Idempotency: repeated calls in quick succession all succeed; the
// marker advances to the latest now(). Canvas's confirm dialog is
// the gate against accidental multi-press, so the server doesn't
// need a debounce token here.
//
// Error surface:
//   - 400 if the id isn't a UUID (trust boundary)
//   - 404 if the workspace row doesn't exist
//   - 502 if the DB write fails (infra)
//   - 200 on success with the new marker
func (h *ChatSessionHandler) NewSession(c *gin.Context) {
	workspaceID := c.Param("id")
	if _, err := uuid.Parse(workspaceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace id must be a UUID"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	// Verify the workspace exists and grab the existing marker
	// (so we can distinguish "first-ever session" from "rotated
	// session" if a future audit needs that).
	var prevMarker sql.NullTime
	err := db.DB.QueryRowContext(ctx,
		`SELECT chat_session_started_at FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&prevMarker)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
		log.Printf("chat_session: pre-update lookup failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "chat session update unavailable"})
		return
	}

	marker := time.Now().UTC()
	_, err = db.DB.ExecContext(ctx,
		`UPDATE workspaces SET chat_session_started_at = $1 WHERE id = $2`,
		marker, workspaceID,
	)
	if err != nil {
		log.Printf("chat_session: marker update failed for %s: %v", workspaceID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "chat session update failed"})
		return
	}

	// Cross-device fan-out: every other device connected to this
	// workspace clears its local view in lockstep. Origin device
	// also receives the event (it broadcasts to ALL subscribers,
	// not "other subscribers") — the canvas listener is idempotent
	// (clearing an already-cleared view is a no-op).
	if h.broadcaster != nil {
		h.broadcaster.BroadcastOnly(workspaceID, string(events.EventSessionReset), map[string]interface{}{
			"workspace_id":          workspaceID,
			"chat_session_started_at": marker.Format(time.RFC3339Nano),
			"prev_marker_set":       prevMarker.Valid,
		})
	}

	c.JSON(http.StatusOK, ChatSessionNewResponse{
		WorkspaceID: workspaceID,
		Marker:      marker,
	})
}
