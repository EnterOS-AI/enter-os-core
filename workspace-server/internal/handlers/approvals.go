package handlers

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
	"github.com/gin-gonic/gin"
)

type ApprovalsHandler struct {
	broadcaster *events.Broadcaster
}

func NewApprovalsHandler(b *events.Broadcaster) *ApprovalsHandler {
	return &ApprovalsHandler{broadcaster: b}
}

// Create handles POST /workspaces/:id/approvals
func (h *ApprovalsHandler) Create(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var body struct {
		TaskID  string                 `json:"task_id"`
		Action  string                 `json:"action" binding:"required"`
		Reason  string                 `json:"reason"`
		Context map[string]interface{} `json:"context"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	ctxJSON, _ := json.Marshal(body.Context)
	if ctxJSON == nil {
		ctxJSON = []byte("{}")
	}

	var approvalID string
	err := db.DB.QueryRowContext(ctx, `
		INSERT INTO approval_requests (workspace_id, task_id, action, reason, context)
		VALUES ($1, $2, $3, $4, $5::jsonb)
		RETURNING id
	`, workspaceID, body.TaskID, body.Action, body.Reason, string(ctxJSON)).Scan(&approvalID)
	if err != nil {
		log.Printf("Create approval error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create approval"})
		return
	}

	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventApprovalRequested), workspaceID, map[string]interface{}{
		"approval_id": approvalID,
		"action":      body.Action,
		"reason":      body.Reason,
		"task_id":     body.TaskID,
	})

	// Auto-escalate to parent
	var parentID *string
	db.DB.QueryRowContext(ctx, `SELECT parent_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&parentID)
	if parentID != nil {
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventApprovalEscalated), *parentID, map[string]interface{}{
			"approval_id":       approvalID,
			"from_workspace_id": workspaceID,
			"action":            body.Action,
			"reason":            body.Reason,
		})
	}

	c.JSON(http.StatusCreated, gin.H{"approval_id": approvalID, "status": "pending"})
}

// ListAll handles GET /approvals/pending
// Returns all pending approvals across all workspaces (for canvas polling).
// Auto-expires approvals older than 10 minutes.
func (h *ApprovalsHandler) ListAll(c *gin.Context) {
	ctx := c.Request.Context()

	// Auto-expire stale approvals (older than 10 min)
	db.DB.ExecContext(ctx, `
		UPDATE approval_requests SET status = 'denied', decided_by = 'auto-expired', decided_at = now()
		WHERE status = 'pending' AND created_at < now() - interval '10 minutes'
	`)

	rows, err := db.DB.QueryContext(ctx, `
		SELECT a.id, a.workspace_id, w.name, a.action, a.reason, a.status, a.created_at
		FROM approval_requests a
		JOIN workspaces w ON w.id = a.workspace_id
		WHERE a.status = 'pending'
		ORDER BY a.created_at DESC
		LIMIT 50
	`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	approvals := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, wsID, wsName, action, status, createdAt string
		var reason *string
		if rows.Scan(&id, &wsID, &wsName, &action, &reason, &status, &createdAt) != nil {
			continue
		}
		approvals = append(approvals, map[string]interface{}{
			"id":             id,
			"workspace_id":   wsID,
			"workspace_name": wsName,
			"action":         action,
			"reason":         reason,
			"status":         status,
			"created_at":     createdAt,
		})
	}

	c.JSON(http.StatusOK, approvals)
}

// List handles GET /workspaces/:id/approvals
func (h *ApprovalsHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, task_id, action, reason, status, decided_by, decided_at, created_at
		FROM approval_requests WHERE workspace_id = $1
		ORDER BY created_at DESC LIMIT 50
	`, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	approvals := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, action, status, createdAt string
		var taskID, reason, decidedBy *string
		var decidedAt *string
		if rows.Scan(&id, &taskID, &action, &reason, &status, &decidedBy, &decidedAt, &createdAt) != nil {
			continue
		}
		approvals = append(approvals, map[string]interface{}{
			"id":         id,
			"task_id":    taskID,
			"action":     action,
			"reason":     reason,
			"status":     status,
			"decided_by": decidedBy,
			"decided_at": decidedAt,
			"created_at": createdAt,
		})
	}

	c.JSON(http.StatusOK, approvals)
}

// Decide handles POST /workspaces/:id/approvals/:approvalId/decide
func (h *ApprovalsHandler) Decide(c *gin.Context) {
	workspaceID := c.Param("id")
	approvalID := c.Param("approvalId")
	ctx := c.Request.Context()

	var body struct {
		Decision  string `json:"decision" binding:"required"`
		DecidedBy string `json:"decided_by"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if body.Decision != "approved" && body.Decision != "denied" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "decision must be 'approved' or 'denied'"})
		return
	}

	decidedBy := body.DecidedBy
	if decidedBy == "" {
		decidedBy = "human"
	}

	result, err := db.DB.ExecContext(ctx, `
		UPDATE approval_requests
		SET status = $1, decided_by = $2, decided_at = now()
		WHERE id = $3 AND workspace_id = $4 AND status = 'pending'
	`, body.Decision, decidedBy, approvalID, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update"})
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "approval not found or already decided"})
		return
	}

	eventType := "APPROVAL_APPROVED"
	if body.Decision == "denied" {
		eventType = "APPROVAL_DENIED"
	}

	h.broadcaster.RecordAndBroadcast(ctx, eventType, workspaceID, map[string]interface{}{
		"approval_id": approvalID,
		"decision":    body.Decision,
		"decided_by":  decidedBy,
	})

	c.JSON(http.StatusOK, gin.H{"status": body.Decision, "approval_id": approvalID})
}
