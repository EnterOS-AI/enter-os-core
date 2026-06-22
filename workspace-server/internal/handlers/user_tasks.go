package handlers

import (
	"errors"
	"log"
	"net/http"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/events"
	"github.com/gin-gonic/gin"
)

// UserTasksHandler serves the "user tasks" primitive — structured asks an
// agent raises for the human user (e.g. "Review the draft", "Provide the API
// key"). It mirrors ApprovalsHandler but resolving a task has no enforcement
// effect; it is a worklist signal. See docs/design/rfc-user-tasks.md.
type UserTasksHandler struct {
	broadcaster *events.Broadcaster
}

// --- OpenAPI doc shapes (used by swaggo; the handlers emit gin.H inline) ---

// CreateUserTaskRequest is the body of POST /workspaces/{id}/user-tasks.
type CreateUserTaskRequest struct {
	Title  string `json:"title" binding:"required"`
	Detail string `json:"detail"`
}

// CreateUserTaskResponse is returned by POST /workspaces/{id}/user-tasks.
type CreateUserTaskResponse struct {
	UserTaskID string `json:"user_task_id"`
	Status     string `json:"status"`
}

// ResolveUserTaskRequest is the body of
// POST /workspaces/{id}/user-tasks/{taskId}/resolve.
type ResolveUserTaskRequest struct {
	Status     string `json:"status" binding:"required" enums:"done,dismissed"`
	ResolvedBy string `json:"resolved_by"`
}

// ResolveUserTaskResponse is returned by the resolve endpoint.
type ResolveUserTaskResponse struct {
	Status     string `json:"status"`
	UserTaskID string `json:"user_task_id"`
}

// UpdateUserTaskRequest is the body of
// PATCH /workspaces/{id}/user-tasks/{taskId}. All fields are optional;
// only provided keys are updated (COALESCE).
type UpdateUserTaskRequest struct {
	Title  *string `json:"title"`
	Detail *string `json:"detail"`
	Status *string `json:"status" enums:"pending,done,dismissed"`
}

// UserTaskMutationResponse is the {status, user_task_id} echo returned by
// the update and delete endpoints.
type UserTaskMutationResponse struct {
	Status     string `json:"status"`
	UserTaskID string `json:"user_task_id"`
}

// UserTask is a single ask a workspace raised, as returned by
// GET /workspaces/{id}/user-tasks. detail/resolved_at/resolved_by are
// null until the task is resolved.
type UserTask struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Detail     *string `json:"detail"`
	Status     string  `json:"status" enums:"pending,done,dismissed"`
	CreatedAt  string  `json:"created_at"`
	ResolvedAt *string `json:"resolved_at"`
	ResolvedBy *string `json:"resolved_by"`
}

// PendingUserTask is one row of the cross-workspace pending list returned by
// GET /user-tasks/pending (joined with the workspace name).
type PendingUserTask struct {
	ID            string  `json:"id"`
	WorkspaceID   string  `json:"workspace_id"`
	WorkspaceName string  `json:"workspace_name"`
	Title         string  `json:"title"`
	Detail        *string `json:"detail"`
	Status        string  `json:"status" enums:"pending"`
	CreatedAt     string  `json:"created_at"`
}

func NewUserTasksHandler(b *events.Broadcaster) *UserTasksHandler {
	return &UserTasksHandler{broadcaster: b}
}

// store builds a UserTaskStore over the live global db.DB per request.
// Constructed per call (rather than cached on the handler) so the global-DB
// swap the test harness performs in setupTestDB is observed — the same shape
// AgentMessageWriter is used with in activity.go.
func (h *UserTasksHandler) store() *UserTaskStore {
	return NewUserTaskStore(db.DB, h.broadcaster)
}

// Create handles POST /workspaces/:id/user-tasks — an agent raises an ask.
//
//	@Summary	Raise a user task
//	@Tags		user-tasks
//	@Accept		json
//	@Produce	json
//	@Param		id		path		string					true	"Workspace ID"
//	@Param		body	body		CreateUserTaskRequest	true	"Task fields"
//	@Success	201		{object}	CreateUserTaskResponse
//	@Failure	400		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Router		/workspaces/{id}/user-tasks [post]
//	@Security	BearerAuth && OrgSlugAuth
func (h *UserTasksHandler) Create(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var body struct {
		Title  string `json:"title" binding:"required"`
		Detail string `json:"detail"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	taskID, err := h.store().Create(ctx, workspaceID, body.Title, body.Detail)
	if err != nil {
		log.Printf("Create user task error workspace=%s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create user task"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"user_task_id": taskID, "status": "pending"})
}

// ListAll handles GET /user-tasks/pending — all pending asks across the org
// (for the concierge Tasks tab). Cross-workspace, so AdminAuth-gated.
//
//	@Summary	List pending user tasks across all workspaces
//	@Tags		user-tasks
//	@Produce	json
//	@Success	200	{array}		PendingUserTask
//	@Failure	500	{object}	ErrorResponse
//	@Router		/user-tasks/pending [get]
//	@Security	BearerAuth
func (h *UserTasksHandler) ListAll(c *gin.Context) {
	ctx := c.Request.Context()

	rows, err := db.DB.QueryContext(ctx, `
		SELECT t.id, t.workspace_id, w.name, t.title, t.detail, t.status, t.created_at
		FROM user_tasks t
		JOIN workspaces w ON w.id = t.workspace_id
		WHERE t.status = 'pending'
		ORDER BY t.created_at DESC
		LIMIT 50
	`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	tasks := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, wsID, wsName, title, status, createdAt string
		var detail *string
		if rows.Scan(&id, &wsID, &wsName, &title, &detail, &status, &createdAt) != nil {
			continue
		}
		tasks = append(tasks, map[string]interface{}{
			"id":             id,
			"workspace_id":   wsID,
			"workspace_name": wsName,
			"title":          title,
			"detail":         detail,
			"status":         status,
			"created_at":     createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		log.Printf("ListAll user tasks rows.Err: %v", err)
	}

	c.JSON(http.StatusOK, tasks)
}

// Resolve handles POST /workspaces/:id/user-tasks/:taskId/resolve — the user
// marks an ask done or dismissed.
//
//	@Summary	Resolve a user task
//	@Tags		user-tasks
//	@Accept		json
//	@Produce	json
//	@Param		id		path		string					true	"Workspace ID"
//	@Param		taskId	path		string					true	"User task ID"
//	@Param		body	body		ResolveUserTaskRequest	true	"Resolution"
//	@Success	200		{object}	ResolveUserTaskResponse
//	@Failure	400		{object}	ErrorResponse
//	@Failure	404		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Router		/workspaces/{id}/user-tasks/{taskId}/resolve [post]
//	@Security	BearerAuth && OrgSlugAuth
func (h *UserTasksHandler) Resolve(c *gin.Context) {
	workspaceID := c.Param("id")
	taskID := c.Param("taskId")
	ctx := c.Request.Context()

	var body struct {
		Status     string `json:"status" binding:"required"`
		ResolvedBy string `json:"resolved_by"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if body.Status != "done" && body.Status != "dismissed" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be 'done' or 'dismissed'"})
		return
	}

	if _, err := h.store().Resolve(ctx, workspaceID, taskID, body.Status, body.ResolvedBy); err != nil {
		if errors.Is(err, ErrUserTaskNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user task not found or already resolved"})
			return
		}
		if errors.Is(err, ErrInvalidUserTaskStatus) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "status must be 'done' or 'dismissed'"})
			return
		}
		log.Printf("User task resolve error task=%s workspace=%s: %v", taskID, workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": body.Status, "user_task_id": taskID})
}

// List handles GET /workspaces/:id/user-tasks — the asks a workspace itself
// raised (any status). Lets an agent read back its own created tasks.
//
//	@Summary	List a workspace's own user tasks
//	@Tags		user-tasks
//	@Produce	json
//	@Param		id	path		string	true	"Workspace ID"
//	@Success	200	{array}		UserTask
//	@Failure	500	{object}	ErrorResponse
//	@Router		/workspaces/{id}/user-tasks [get]
//	@Security	BearerAuth && OrgSlugAuth
func (h *UserTasksHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	tasks, err := h.store().List(ctx, workspaceID)
	if err != nil {
		log.Printf("List user tasks error workspace=%s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	c.JSON(http.StatusOK, tasks)
}

// Update handles PATCH /workspaces/:id/user-tasks/:taskId — a workspace edits
// its own ask (title / detail / status). The workspace_id scope means an
// agent can only touch tasks it raised. Fields are optional (COALESCE).
//
//	@Summary	Update a workspace's own user task
//	@Tags		user-tasks
//	@Accept		json
//	@Produce	json
//	@Param		id		path		string					true	"Workspace ID"
//	@Param		taskId	path		string					true	"User task ID"
//	@Param		body	body		UpdateUserTaskRequest	true	"Partial task fields (only provided keys are updated)"
//	@Success	200		{object}	UserTaskMutationResponse
//	@Failure	400		{object}	ErrorResponse
//	@Failure	404		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Router		/workspaces/{id}/user-tasks/{taskId} [patch]
//	@Security	BearerAuth && OrgSlugAuth
func (h *UserTasksHandler) Update(c *gin.Context) {
	workspaceID := c.Param("id")
	taskID := c.Param("taskId")
	ctx := c.Request.Context()

	var body struct {
		Title  *string `json:"title"`
		Detail *string `json:"detail"`
		Status *string `json:"status"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if err := h.store().Update(ctx, workspaceID, taskID, body.Title, body.Detail, body.Status); err != nil {
		if errors.Is(err, ErrInvalidUserTaskStatus) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "status must be 'pending', 'done' or 'dismissed'"})
			return
		}
		if errors.Is(err, ErrUserTaskNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user task not found"})
			return
		}
		log.Printf("User task update error task=%s workspace=%s: %v", taskID, workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "updated", "user_task_id": taskID})
}

// Delete handles DELETE /workspaces/:id/user-tasks/:taskId — a workspace
// removes its own ask. Scoped by workspace_id so agents can only delete
// tasks they raised.
//
//	@Summary	Delete a workspace's own user task
//	@Tags		user-tasks
//	@Produce	json
//	@Param		id		path		string	true	"Workspace ID"
//	@Param		taskId	path		string	true	"User task ID"
//	@Success	200		{object}	UserTaskMutationResponse
//	@Failure	404		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Router		/workspaces/{id}/user-tasks/{taskId} [delete]
//	@Security	BearerAuth && OrgSlugAuth
func (h *UserTasksHandler) Delete(c *gin.Context) {
	workspaceID := c.Param("id")
	taskID := c.Param("taskId")
	ctx := c.Request.Context()

	if err := h.store().Delete(ctx, workspaceID, taskID); err != nil {
		if errors.Is(err, ErrUserTaskNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user task not found"})
			return
		}
		log.Printf("User task delete error task=%s workspace=%s: %v", taskID, workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deleted", "user_task_id": taskID})
}
