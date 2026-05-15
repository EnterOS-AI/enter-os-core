package handlers

// workspace_broadcast.go — POST /workspaces/:id/broadcast
//
// Allows a workspace with broadcast_enabled=true to send a message to every
// non-removed agent workspace in the SAME ORG.  The message is:
//
//   • Persisted in each recipient's activity_logs (type='broadcast_receive')
//     so poll-mode agents pick it up via GET /activity.
//   • Broadcast via WebSocket BROADCAST_MESSAGE event so canvas panels can
//     show a real-time banner for each recipient workspace.
//
// The sender's own workspace logs a 'broadcast_sent' activity row for
// traceability.
//
// Auth: WorkspaceAuth (the agent triggers this with its own bearer token).
// The handler re-validates broadcast_enabled inside the DB lookup to prevent
// TOCTOU — the middleware only proved the token is valid, not the ability.
//
// Org isolation (OFFSEC-015): recipients are scoped to the sender's org using
// a recursive CTE that walks the parent_id chain to find the org root. This
// prevents a compromised or misconfigured workspace from broadcasting to
// workspaces in other tenants' orgs.

import (
	"log"
	"net/http"
	"strconv"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/gin-gonic/gin"
)

// BroadcastHandler is constructed once and shared across requests.
type BroadcastHandler struct {
	broadcaster *events.Broadcaster
}

// NewBroadcastHandler creates a BroadcastHandler.
func NewBroadcastHandler(b *events.Broadcaster) *BroadcastHandler {
	return &BroadcastHandler{broadcaster: b}
}

// Broadcast handles POST /workspaces/:id/broadcast.
func (h *BroadcastHandler) Broadcast(c *gin.Context) {
	senderID := c.Param("id")
	if err := validateWorkspaceID(senderID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}

	var body struct {
		Message string `json:"message" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message is required"})
		return
	}

	ctx := c.Request.Context()

	// Verify sender exists and has broadcast_enabled=true.
	var senderName string
	var broadcastEnabled bool
	err := db.DB.QueryRowContext(ctx,
		`SELECT name, broadcast_enabled FROM workspaces WHERE id = $1 AND status != 'removed'`,
		senderID,
	).Scan(&senderName, &broadcastEnabled)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if !broadcastEnabled {
		c.JSON(http.StatusForbidden, gin.H{
			"error": "broadcast_disabled",
			"hint":  "This workspace does not have the broadcast ability. Ask a user or admin to enable it via PATCH /workspaces/:id/abilities.",
		})
		return
	}

	// Find the sender's org root by walking the parent_id chain.
	// Workspaces with parent_id = NULL are org roots; every other workspace
	// belongs to the org identified by its topmost ancestor.
	var orgRootID string
	err = db.DB.QueryRowContext(ctx, `
		WITH RECURSIVE org_chain AS (
			SELECT id, parent_id, id AS root_id
			FROM workspaces
			WHERE id = $1
			UNION ALL
			SELECT w.id, w.parent_id, c.root_id
			FROM workspaces w
			JOIN org_chain c ON w.id = c.parent_id
		)
		SELECT root_id FROM org_chain WHERE parent_id IS NULL LIMIT 1
	`, senderID).Scan(&orgRootID)
	if err != nil {
		log.Printf("Broadcast: org root lookup for %s: %v", senderID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	// Collect all non-removed agent workspaces in the SAME ORG (same root_id),
	// excluding the sender itself.
	rows, err := db.DB.QueryContext(ctx, `
		WITH RECURSIVE org_chain AS (
			SELECT id, parent_id, id AS root_id
			FROM workspaces
			WHERE parent_id IS NULL
			UNION ALL
			SELECT w.id, w.parent_id, c.root_id
			FROM workspaces w
			JOIN org_chain c ON w.parent_id = c.id
		)
		SELECT c.id
		FROM org_chain c
		WHERE c.root_id = $1
		  AND c.id != $2
		  AND EXISTS (
			  SELECT 1 FROM workspaces w
			  WHERE w.id = c.id AND w.status != 'removed'
		  )
	`, orgRootID, senderID)
	if err != nil {
		log.Printf("Broadcast: recipient query failed for %s: %v", senderID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	defer rows.Close()

	var recipientIDs []string
	for rows.Next() {
		var rid string
		if rows.Scan(&rid) == nil {
			recipientIDs = append(recipientIDs, rid)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("Broadcast: recipient rows error for %s: %v", senderID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	broadcastPayload := map[string]interface{}{
		"message":   body.Message,
		"sender_id": senderID,
		"sender":    senderName,
	}

	// Persist broadcast_receive in each recipient's activity log + emit WS event.
	delivered := 0
	for _, rid := range recipientIDs {
		if _, err := db.DB.ExecContext(ctx, `
			INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, summary, status)
			VALUES ($1, 'broadcast_receive', 'broadcast', $2, $3, 'ok')
		`, rid, senderID, "Broadcast from "+senderName+": "+broadcastTruncate(body.Message, 120)); err != nil {
			log.Printf("Broadcast: activity_logs insert for recipient %s: %v", rid, err)
			continue
		}
		h.broadcaster.BroadcastOnly(rid, "BROADCAST_MESSAGE", broadcastPayload)
		delivered++
	}

	// Record the send on the sender's own log.
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO activity_logs (workspace_id, activity_type, method, summary, status)
		VALUES ($1, 'broadcast_sent', 'broadcast', $2, 'ok')
	`, senderID, "Broadcast sent to "+strconv.Itoa(delivered)+" workspace(s)"); err != nil {
		log.Printf("Broadcast: sender activity_log for %s: %v", senderID, err)
	}

	c.JSON(http.StatusOK, gin.H{
		"status":    "sent",
		"delivered": delivered,
	})
}

func broadcastTruncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
