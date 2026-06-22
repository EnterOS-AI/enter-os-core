package handlers

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// CheckpointsHandler persists Temporal workflow step checkpoints so workflows
// can resume from the last completed step after a crash or restart (#788).
type CheckpointsHandler struct {
	db *sql.DB
}

// NewCheckpointsHandler wires the handler to the given database. Pass db.DB
// at router-setup time; pass a sqlmock DB in tests.
func NewCheckpointsHandler(database *sql.DB) *CheckpointsHandler {
	return &CheckpointsHandler{db: database}
}

// checkpointEntry is the canonical shape returned by List.
type checkpointEntry struct {
	ID          string          `json:"id"`
	WorkspaceID string          `json:"workspace_id"`
	WorkflowID  string          `json:"workflow_id"`
	StepName    string          `json:"step_name"`
	StepIndex   int             `json:"step_index"`
	CompletedAt string          `json:"completed_at"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

// callerMismatch guards against cross-workspace access in unit-test and
// middleware-injected scenarios. When the Gin context carries a
// "caller_workspace_id" key (set by middleware or a test), the value must
// match the URL :id param; otherwise the handler aborts with 403.
//
// In production the WorkspaceAuth middleware already validates that the
// bearer token belongs to :id (401 on mismatch), so this key is typically
// absent and the check is a no-op. The key exists so that future
// middleware layers and unit tests can exercise workspace-isolation logic
// at the handler level without modifying WorkspaceAuth.
func callerMismatch(c *gin.Context, workspaceID string) bool {
	if caller := c.GetString("caller_workspace_id"); caller != "" && caller != workspaceID {
		c.JSON(http.StatusForbidden, gin.H{"error": "workspace access denied"})
		return true
	}
	return false
}

// Upsert handles POST /workspaces/:id/checkpoints
//
// Body: { "workflow_id", "step_name", "step_index", "payload"? }
//
// On first call for a (workspace_id, workflow_id, step_name) triple: INSERT.
// On repeat call: UPDATE step_index + completed_at + payload in-place.
// Returns 201 with the checkpoint id on success.
func (h *CheckpointsHandler) Upsert(c *gin.Context) {
	workspaceID := c.Param("id")
	if callerMismatch(c, workspaceID) {
		return
	}
	ctx := c.Request.Context()

	var body struct {
		WorkflowID string          `json:"workflow_id" binding:"required"`
		StepName   string          `json:"step_name"   binding:"required"`
		StepIndex  int             `json:"step_index"`
		Payload    json.RawMessage `json:"payload"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Normalise payload: a missing or zero-length field is stored as JSON null.
	payloadStr := "null"
	if len(body.Payload) > 0 {
		payloadStr = string(body.Payload)
	}

	var id string
	err := h.db.QueryRowContext(ctx, `
		INSERT INTO workflow_checkpoints
		    (workspace_id, workflow_id, step_name, step_index, payload)
		VALUES ($1, $2, $3, $4, $5::jsonb)
		ON CONFLICT (workspace_id, workflow_id, step_name) DO UPDATE
		    SET step_index   = EXCLUDED.step_index,
		        completed_at = now(),
		        payload      = EXCLUDED.payload
		RETURNING id
	`, workspaceID, body.WorkflowID, body.StepName, body.StepIndex, payloadStr).Scan(&id)
	if err != nil {
		log.Printf("Upsert checkpoint error workspace=%s wf=%s step=%s: %v",
			workspaceID, body.WorkflowID, body.StepName, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to upsert checkpoint"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":          id,
		"workspace_id": workspaceID,
		"workflow_id": body.WorkflowID,
		"step_name":   body.StepName,
	})
}

// List handles GET /workspaces/:id/checkpoints/:wfid
//
// Returns all checkpoints for the given workflow ordered by step_index DESC
// so the most recently completed step is first.
// Returns 404 when no checkpoints exist for that workflow.
func (h *CheckpointsHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")
	if callerMismatch(c, workspaceID) {
		return
	}
	workflowID := c.Param("wfid")
	ctx := c.Request.Context()

	rows, err := h.db.QueryContext(ctx, `
		SELECT id, workspace_id, workflow_id, step_name, step_index, completed_at, payload
		FROM workflow_checkpoints
		WHERE workspace_id = $1 AND workflow_id = $2
		ORDER BY step_index DESC
	`, workspaceID, workflowID)
	if err != nil {
		log.Printf("List checkpoints error workspace=%s wf=%s: %v", workspaceID, workflowID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list checkpoints"})
		return
	}
	defer rows.Close()

	checkpoints := make([]checkpointEntry, 0)
	for rows.Next() {
		var e checkpointEntry
		var payload []byte
		if err := rows.Scan(
			&e.ID, &e.WorkspaceID, &e.WorkflowID,
			&e.StepName, &e.StepIndex, &e.CompletedAt, &payload,
		); err != nil {
			log.Printf("List checkpoints scan error workspace=%s wf=%s: %v", workspaceID, workflowID, err)
			continue
		}
		if len(payload) > 0 {
			e.Payload = json.RawMessage(payload)
		}
		checkpoints = append(checkpoints, e)
	}
	if err := rows.Err(); err != nil {
		log.Printf("List checkpoints rows.Err workspace=%s wf=%s: %v", workspaceID, workflowID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "checkpoint read failed"})
		return
	}

	if len(checkpoints) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no checkpoints found for workflow"})
		return
	}

	c.JSON(http.StatusOK, checkpoints)
}

// Latest handles GET /workspaces/:id/checkpoints/latest
//
// Returns the single most recently completed checkpoint across all workflows
// for this workspace — ordered by completed_at DESC.  The workspace-template
// Temporal resume path calls this on startup to inject the last known step
// into the agent context (issue #837 step 3/3, closes #583).
//
// 200 — checkpoint found; body is a single checkpointEntry JSON object.
// 404 — no checkpoints exist yet for this workspace.
func (h *CheckpointsHandler) Latest(c *gin.Context) {
	workspaceID := c.Param("id")
	if callerMismatch(c, workspaceID) {
		return
	}
	ctx := c.Request.Context()

	var e checkpointEntry
	var payload []byte
	err := h.db.QueryRowContext(ctx, `
		SELECT id, workspace_id, workflow_id, step_name, step_index, completed_at, payload
		FROM workflow_checkpoints
		WHERE workspace_id = $1
		ORDER BY completed_at DESC
		LIMIT 1
	`, workspaceID).Scan(
		&e.ID, &e.WorkspaceID, &e.WorkflowID,
		&e.StepName, &e.StepIndex, &e.CompletedAt, &payload,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "no checkpoints found for workspace"})
		return
	}
	if err != nil {
		log.Printf("Latest checkpoint error workspace=%s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch latest checkpoint"})
		return
	}

	if len(payload) > 0 {
		e.Payload = json.RawMessage(payload)
	}
	c.JSON(http.StatusOK, e)
}

// Delete handles DELETE /workspaces/:id/checkpoints/:wfid
//
// Removes all checkpoints for a workflow (clean shutdown path).
// Returns 404 if no checkpoints existed.
func (h *CheckpointsHandler) Delete(c *gin.Context) {
	workspaceID := c.Param("id")
	if callerMismatch(c, workspaceID) {
		return
	}
	workflowID := c.Param("wfid")
	ctx := c.Request.Context()

	result, err := h.db.ExecContext(ctx, `
		DELETE FROM workflow_checkpoints
		WHERE workspace_id = $1 AND workflow_id = $2
	`, workspaceID, workflowID)
	if err != nil {
		log.Printf("Delete checkpoints error workspace=%s wf=%s: %v", workspaceID, workflowID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete checkpoints"})
		return
	}

	n, err := result.RowsAffected()
	if err != nil {
		log.Printf("Delete checkpoints RowsAffected error workspace=%s wf=%s: %v", workspaceID, workflowID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete checkpoints"})
		return
	}
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no checkpoints found for workflow"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"deleted": n, "workflow_id": workflowID})
}
