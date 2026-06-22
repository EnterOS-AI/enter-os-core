package handlers

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
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

	ctxJSON, marshalErr := json.Marshal(body.Context)
	if marshalErr != nil {
		log.Printf("Approvals create %s: json.Marshal context failed: %v", workspaceID, marshalErr)
	}
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

	if err := h.broadcaster.RecordAndBroadcast(ctx, string(events.EventApprovalRequested), workspaceID, map[string]interface{}{
		"approval_id": approvalID,
		"action":      body.Action,
		"reason":      body.Reason,
		"task_id":     body.TaskID,
	}); err != nil {
		log.Printf("approvals: failed to broadcast approval requested: %v", err)
	}

	// Auto-escalate to parent
	var parentID *string
	if err := db.DB.QueryRowContext(ctx, `SELECT parent_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&parentID); err != nil {
		log.Printf("approvals: failed to lookup parent for escalation: %v", err)
	}
	if parentID != nil {
		if err := h.broadcaster.RecordAndBroadcast(ctx, string(events.EventApprovalEscalated), *parentID, map[string]interface{}{
			"approval_id":       approvalID,
			"from_workspace_id": workspaceID,
			"action":            body.Action,
			"reason":            body.Reason,
		}); err != nil {
			log.Printf("approvals: failed to broadcast approval escalated: %v", err)
		}
	}

	c.JSON(http.StatusCreated, gin.H{"approval_id": approvalID, "status": "pending"})
}

// ListAll handles GET /approvals/pending
// Returns all pending approvals across all workspaces (for canvas polling).
// Approvals are long-lived until a human Decides (approve or deny); there is
// no time-based auto-expiry (CTO directive). A requester that no longer
// needs the approval can withdraw it via
// POST /workspaces/:id/approvals/:approvalId/withdraw (see Withdraw
// below) — the only path that moves a row out of 'pending' before a
// human acts on it.
func (h *ApprovalsHandler) ListAll(c *gin.Context) {
	ctx := c.Request.Context()

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
	if err := rows.Err(); err != nil {
		log.Printf("ListPendingApprovals rows.Err: %v", err)
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
	if err := rows.Err(); err != nil {
		log.Printf("ListApprovals rows.Err workspace=%s: %v", workspaceID, err)
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

	rows, err := result.RowsAffected()
	if err != nil {
		log.Printf("Approval decision RowsAffected error approval=%s workspace=%s: %v", approvalID, workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update"})
		return
	}
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "approval not found or already decided"})
		return
	}

	eventType := "APPROVAL_APPROVED"
	if body.Decision == "denied" {
		eventType = "APPROVAL_DENIED"
	}

	if err := h.broadcaster.RecordAndBroadcast(ctx, eventType, workspaceID, map[string]interface{}{
		"approval_id": approvalID,
		"decision":    body.Decision,
		"decided_by":  decidedBy,
	}); err != nil {
		log.Printf("approvals: failed to broadcast approval decision: %v", err)
	}

	c.JSON(http.StatusOK, gin.H{"status": body.Decision, "approval_id": approvalID})
}

// Withdraw handles POST /workspaces/:id/approvals/:approvalId/withdraw
// — the requester pulls back a pending approval before any human acts
// on it. Issue #66: closes the long-standing gap where a requester had
// no way to retract a request they'd raised but no longer needed
// (e.g. the underlying destructive op was abandoned, or the user
// changed their mind verbally and the agent wants to clear its own
// inbox row before the human approver wastes time on it).
//
// Authz model (PM/Researcher guardrail 7600d2ed): the caller's
// workspace token must match approval_requests.workspace_id (the
// CREATOR's workspace), NOT the URL path's :id. This matters for
// cross-workspace approval gates (core#2574, core#2593) where the
// approval row's workspace_id is the org-level gate, while the
// underlying request originates from a different workspace. Using
// the path's :id would (a) require every caller to know the
// approval's true creator workspace, and (b) be the wrong anchor
// for any audit trail.
//
// State guard: only status='pending' is withdrawable. An approval
// that has been approved/denied/escalated/withdrawn cannot be
// withdrawn — the human approver (or a prior withdraw) has already
// acted, and re-mutating the row would lose the audit signal.
//
// Event broadcast: APPROVAL_WITHDRAWN on the row's workspace_id
// (the creator's), matching the same convention Decide uses (so the
// canvas inbox can react uniformly).
func (h *ApprovalsHandler) Withdraw(c *gin.Context) {
	workspaceID := c.Param("id")
	approvalID := c.Param("approvalId")
	ctx := c.Request.Context()

	// Read the row to discover the creator workspace for authz.
	// We need the creator workspace before the UPDATE so we can
	// compare it to the caller's workspace token. (Decide skips
	// this step because the path's :id IS the creator workspace
	// in the non-cross-workspace case, and Decide doesn't authz
	// against the creator anyway — it authzs against the
	// approver, which is a different model.)
	var creatorWorkspaceID string
	err := db.DB.QueryRowContext(ctx, `
		SELECT workspace_id::text FROM approval_requests WHERE id = $1
	`, approvalID).Scan(&creatorWorkspaceID)
	if err != nil {
		// No row found (or the UUID is malformed) → 404. A
		// malformed UUID would error from pgx with a parse
		// failure rather than "no rows", so the caller can't
		// distinguish "approval not found" from "approval id
		// is invalid" — same response is fine for both: the
		// approval can't be withdrawn either way.
		c.JSON(http.StatusNotFound, gin.H{"error": "approval not found"})
		return
	}

	// Authz: the caller (workspace-token) must be the creator
	// workspace. We use the row's workspace_id, NOT the URL
	// path :id — this is the load-bearing authz anchor for
	// cross-workspace approval gates (#2574 / #2593), where
	// the path's :id is the gate's workspace and the row's
	// workspace_id is the underlying requester's workspace.
	if workspaceID != "" && workspaceID != creatorWorkspaceID {
		c.JSON(http.StatusForbidden, gin.H{"error": "not the requester"})
		return
	}

	// State guard + status update in one statement. The
	// WHERE status='pending' clause is the load-bearing
	// guarantee: if the row was already approved/denied/
	// escalated/withdrawn by a concurrent caller, the UPDATE
	// affects 0 rows and we return 409 (Conflict) — the
	// requester's withdraw raced with the human approver and
	// lost. Returning 404 instead would be a lie (the row
	// exists), and returning 200 would silently drop the
	// state-change.
	result, err := db.DB.ExecContext(ctx, `
		UPDATE approval_requests
		SET status = 'withdrawn', decided_by = 'requester', decided_at = now()
		WHERE id = $1 AND status = 'pending'
	`, approvalID)
	if err != nil {
		log.Printf("Withdraw UPDATE error approval=%s workspace=%s: %v", approvalID, workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to withdraw"})
		return
	}

	rows, err := result.RowsAffected()
	if err != nil {
		log.Printf("Withdraw RowsAffected error approval=%s workspace=%s: %v", approvalID, workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to withdraw"})
		return
	}
	if rows == 0 {
		// The approval exists (we read it above) but is no
		// longer 'pending' — a concurrent Decide/withdraw got
		// there first. Surface as 409 so the caller can
		// distinguish this from the not-found case.
		c.JSON(http.StatusConflict, gin.H{"error": "approval not pending (already decided or withdrawn)"})
		return
	}

	if err := h.broadcaster.RecordAndBroadcast(ctx, "APPROVAL_WITHDRAWN", creatorWorkspaceID, map[string]interface{}{
		"approval_id": approvalID,
		"decided_by":  "requester",
	}); err != nil {
		log.Printf("approvals: failed to broadcast approval withdrawal: %v", err)
	}

	c.JSON(http.StatusOK, gin.H{"status": "withdrawn", "approval_id": approvalID})
}
