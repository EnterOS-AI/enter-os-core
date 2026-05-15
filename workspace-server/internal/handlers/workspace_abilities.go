package handlers

// workspace_abilities.go — PATCH /workspaces/:id/abilities
//
// Allows users and admin agents to toggle two workspace-level ability flags:
//
//   broadcast_enabled   — workspace may POST /broadcast to send org-wide messages
//   talk_to_user_enabled — workspace may deliver canvas chat messages via
//                          send_message_to_user / POST /notify
//
// Gated behind AdminAuth so workspace agents cannot self-modify their own
// ability flags (that would let any agent grant itself broadcast rights or
// suppress its own chat-silence constraint).

import (
	"log"
	"net/http"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/gin-gonic/gin"
)

// AbilitiesPayload carries the subset of ability flags the caller wants to
// update. Fields are pointers so that the handler can distinguish "caller
// supplied false" from "caller omitted the field" (omitempty semantics).
type AbilitiesPayload struct {
	BroadcastEnabled   *bool `json:"broadcast_enabled"`
	TalkToUserEnabled  *bool `json:"talk_to_user_enabled"`
}

// PatchAbilities handles PATCH /workspaces/:id/abilities (AdminAuth).
func PatchAbilities(c *gin.Context) {
	id := c.Param("id")
	if err := validateWorkspaceID(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}

	var body AbilitiesPayload
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if body.BroadcastEnabled == nil && body.TalkToUserEnabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one ability field required"})
		return
	}

	ctx := c.Request.Context()

	var exists bool
	if err := db.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM workspaces WHERE id = $1 AND status != 'removed')`, id,
	).Scan(&exists); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	if body.BroadcastEnabled != nil {
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET broadcast_enabled = $2, updated_at = now() WHERE id = $1`,
			id, *body.BroadcastEnabled,
		); err != nil {
			log.Printf("PatchAbilities broadcast_enabled for %s: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}
	}

	if body.TalkToUserEnabled != nil {
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET talk_to_user_enabled = $2, updated_at = now() WHERE id = $1`,
			id, *body.TalkToUserEnabled,
		); err != nil {
			log.Printf("PatchAbilities talk_to_user_enabled for %s: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "updated"})
}
