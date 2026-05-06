package handlers

import (
	"database/sql"
	"log"
	"net/http"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/gin-gonic/gin"
)

type AgentHandler struct {
	broadcaster *events.Broadcaster
}

func NewAgentHandler(b *events.Broadcaster) *AgentHandler {
	return &AgentHandler{broadcaster: b}
}

// Assign handles POST /workspaces/:id/agent
func (h *AgentHandler) Assign(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var body struct {
		Model string `json:"model" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Check workspace exists
	var status string
	err := db.DB.QueryRowContext(ctx,
		`SELECT status FROM workspaces WHERE id = $1`, workspaceID).Scan(&status)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	// Check no active agent already assigned
	var existingCount int
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agents WHERE workspace_id = $1 AND status = 'active'`, workspaceID,
	).Scan(&existingCount); err != nil {
		log.Printf("Agent assign check error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}
	if existingCount > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "workspace already has an active agent, use PATCH to replace"})
		return
	}

	// Insert agent
	var agentID string
	err = db.DB.QueryRowContext(ctx,
		`INSERT INTO agents (workspace_id, model) VALUES ($1, $2) RETURNING id`, workspaceID, body.Model,
	).Scan(&agentID)
	if err != nil {
		log.Printf("Assign agent error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to assign agent"})
		return
	}

	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventAgentAssigned), workspaceID, map[string]interface{}{
		"agent_id": agentID,
		"model":    body.Model,
	})

	c.JSON(http.StatusCreated, gin.H{"agent_id": agentID, "model": body.Model})
}

// Replace handles PATCH /workspaces/:id/agent
func (h *AgentHandler) Replace(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var body struct {
		Model string `json:"model" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Deactivate current agent
	var oldModel string
	err := db.DB.QueryRowContext(ctx,
		`UPDATE agents SET status = 'replaced', removed_at = now(), removal_reason = 'model_replaced'
		 WHERE workspace_id = $1 AND status = 'active' RETURNING model`,
		workspaceID,
	).Scan(&oldModel)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "no active agent to replace"})
		return
	}
	if err != nil {
		log.Printf("Replace agent error (deactivate): %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to replace agent"})
		return
	}

	// Insert new agent
	var agentID string
	err = db.DB.QueryRowContext(ctx,
		`INSERT INTO agents (workspace_id, model) VALUES ($1, $2) RETURNING id`, workspaceID, body.Model,
	).Scan(&agentID)
	if err != nil {
		log.Printf("Replace agent error (insert): %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to assign replacement"})
		return
	}

	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventAgentReplaced), workspaceID, map[string]interface{}{
		"agent_id":  agentID,
		"model":     body.Model,
		"old_model": oldModel,
	})

	c.JSON(http.StatusOK, gin.H{"agent_id": agentID, "model": body.Model, "old_model": oldModel})
}

// Remove handles DELETE /workspaces/:id/agent
func (h *AgentHandler) Remove(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var agentID, model string
	err := db.DB.QueryRowContext(ctx,
		`UPDATE agents SET status = 'removed', removed_at = now(), removal_reason = 'manual_removal'
		 WHERE workspace_id = $1 AND status = 'active' RETURNING id, model`,
		workspaceID,
	).Scan(&agentID, &model)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "no active agent to remove"})
		return
	}
	if err != nil {
		log.Printf("Remove agent error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove agent"})
		return
	}

	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventAgentRemoved), workspaceID, map[string]interface{}{
		"agent_id": agentID,
		"model":    model,
	})

	c.JSON(http.StatusOK, gin.H{"status": "removed", "agent_id": agentID})
}

// Move handles POST /workspaces/:id/agent/move
func (h *AgentHandler) Move(c *gin.Context) {
	sourceID := c.Param("id")
	ctx := c.Request.Context()

	var body struct {
		TargetWorkspaceID string `json:"target_workspace_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Check target workspace exists
	var targetStatus string
	err := db.DB.QueryRowContext(ctx,
		`SELECT status FROM workspaces WHERE id = $1`, body.TargetWorkspaceID).Scan(&targetStatus)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "target workspace not found"})
		return
	}
	if err != nil {
		log.Printf("Move agent target lookup error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "target lookup failed"})
		return
	}

	// Check target doesn't already have an agent
	var targetAgentCount int
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agents WHERE workspace_id = $1 AND status = 'active'`, body.TargetWorkspaceID,
	).Scan(&targetAgentCount); err != nil {
		log.Printf("Move agent target check error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "target lookup failed"})
		return
	}
	if targetAgentCount > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "target workspace already has an active agent"})
		return
	}

	// Move the agent: update workspace_id
	var agentID, model string
	err = db.DB.QueryRowContext(ctx,
		`UPDATE agents SET workspace_id = $2
		 WHERE workspace_id = $1 AND status = 'active' RETURNING id, model`,
		sourceID, body.TargetWorkspaceID,
	).Scan(&agentID, &model)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "no active agent in source workspace"})
		return
	}
	if err != nil {
		log.Printf("Move agent error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to move agent"})
		return
	}

	// Broadcast on both workspaces
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventAgentMoved), sourceID, map[string]interface{}{
		"agent_id":            agentID,
		"model":               model,
		"target_workspace_id": body.TargetWorkspaceID,
	})
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventAgentMoved), body.TargetWorkspaceID, map[string]interface{}{
		"agent_id":            agentID,
		"model":               model,
		"source_workspace_id": sourceID,
	})

	c.JSON(http.StatusOK, gin.H{
		"agent_id":       agentID,
		"model":          model,
		"from_workspace": sourceID,
		"to_workspace":   body.TargetWorkspaceID,
	})
}
